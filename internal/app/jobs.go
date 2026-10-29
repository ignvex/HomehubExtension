package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/rss"
	"github.com/ignvex/HomehubExtension/internal/store"
)

// Jobs 持有清理任务需要的依赖。
//
// 直接依赖具体类型而不是接口：这里是装配层，不是领域层，
// 多一层抽象只会让签名对不上时更难排查。
type Jobs struct {
	Store  store.Store
	Blobs  *blob.Store
	RSS    *rss.Service
	Vacuum func(ctx context.Context, full bool) (int64, error)
	Now    func() time.Time

	// GCGrace 是 storage.gc_grace 的值：引用归零后等多久才删文件。
	// 必须从这里读，而不是在任务里硬编码兜底值——否则用户改了配置
	// 却毫无效果，比没有这个配置项更糟。
	GCGrace time.Duration
	// CatchUp 对应 cleanup.catch_up。
	CatchUp bool
}

func (j *Jobs) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

// params 里的值来自 config.json，类型已由校验器确认；这里仍做一次
// 类型断言，缺失时回落到默认值而不是 panic。
func durParam(p map[string]any, key string, def time.Duration) time.Duration {
	v, ok := p[key].(string)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return def
	}
	return d
}

func intParam(p map[string]any, key string, def int) int {
	switch v := p[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func boolParam(p map[string]any, key string, def bool) bool {
	if v, ok := p[key].(bool); ok {
		return v
	}
	return def
}

// PasteExpired 物理删除已过期或软删超过保留期的片段。
func (j *Jobs) PasteExpired(ctx context.Context, p map[string]any) (string, error) {
	now := j.now()
	retention := durParam(p, "deleted_retention", time.Hour)
	n, err := j.Store.Paste().PurgeExpired(ctx, now.Unix(), now.Add(-retention).Unix())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("purged %d pastes", n), nil
}

// SessionExpired 清理过期会话、挑战与配对码。
func (j *Jobs) SessionExpired(ctx context.Context, p map[string]any) (string, error) {
	now := j.now().Unix()
	sessions, err := j.Store.Auth().PurgeSessions(ctx, now)
	if err != nil {
		return "", err
	}
	enrollments, err := j.Store.Auth().PurgeEnrollments(ctx, now)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("purged %d sessions/challenges, %d enrollments", sessions, enrollments), nil
}

// RSSPrune 删除超期条目。引用计数由 entry_blob 的 AFTER DELETE 触发器回退，
// 所以这里必须让 Purge 走"先删 entry_blob 再删 entry"的路径。
func (j *Jobs) RSSPrune(ctx context.Context, p map[string]any) (string, error) {
	olderThan := durParam(p, "older_than", 90*24*time.Hour)
	keepStarred := boolParam(p, "keep_starred", true)
	before := j.now().Add(-olderThan).Unix()

	n, err := j.Store.Entry().Purge(ctx, before, keepStarred)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("purged %d entries older than %s (keep_starred=%v)", n, olderThan, keepStarred), nil
}

// MediaRetry 重试下载失败的媒体资源。
func (j *Jobs) MediaRetry(ctx context.Context, p map[string]any) (string, error) {
	if j.RSS == nil {
		return "skipped: rss module disabled", nil
	}
	maxAttempts := intParam(p, "max_attempts", 3)
	within := durParam(p, "within", 72*time.Hour)

	res, err := j.RSS.RetryMedia(ctx, maxAttempts, within, 200)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("entries=%d tried=%d ok=%d failed=%d",
		res.Entries, res.Tried, res.OK, res.Failed), nil
}

// BlobGC 回收无引用文件并清理孤儿。
//
// 顺序很重要：先校准引用计数，再删文件。校准能修复任何遗漏的删除路径，
// 避免因为一次漏删导致文件永久占着配额。
func (j *Jobs) BlobGC(ctx context.Context, p map[string]any) (string, error) {
	// 任务级 params 可以覆盖，但默认值来自 storage.gc_grace，
	// 而不是写死的常量。
	def := j.GCGrace
	if def <= 0 {
		def = time.Hour
	}
	grace := durParam(p, "grace", def)
	dryRun := boolParam(p, "dry_run", false)
	cutoff := j.now().Add(-grace).Unix()

	// 先校准：任何遗漏的删除路径都会让 ref_count 偏高，
	// 校准之后本轮 GC 才能把这些 blob 捞出来。
	// 用类型断言而不是加到 store.BlobStore 接口里，
	// 因为这只是 sqlite 实现的能力，不是所有存储后端都必须提供。
	recalibrated := int64(0)
	if recal, ok := j.Store.Blob().(interface {
		Recalibrate(ctx context.Context) (int64, error)
	}); ok {
		recalibrated, _ = recal.Recalibrate(ctx)
	}

	unref, err := j.Store.Blob().Unreferenced(ctx, cutoff, 500)
	if err != nil {
		return "", err
	}
	if dryRun {
		return fmt.Sprintf("dry_run: %d blobs would be purged", len(unref)), nil
	}

	var purged int
	var freed int64
	for _, b := range unref {
		if ctx.Err() != nil {
			break
		}
		if err := j.Blobs.Delete(b.SHA256, b.Ext); err != nil && !os.IsNotExist(err) {
			continue // 删不掉就留着，下一轮再试，不要因为一个文件中断整轮
		}
		if err := j.Store.Blob().Delete(ctx, b.SHA256); err != nil {
			continue
		}
		purged++
		freed += b.Size
	}

	msg := fmt.Sprintf("purged %d blobs, freed %.1f MB", purged, float64(freed)/(1<<20))
	if recalibrated > 0 {
		msg += fmt.Sprintf(", recalibrated %d counters", recalibrated)
	}
	return msg, nil
}

// DBVacuum 回收空闲页。full 模式会阻塞写入，默认只做 incremental。
func (j *Jobs) DBVacuum(ctx context.Context, p map[string]any) (string, error) {
	if j.Vacuum == nil {
		return "skipped: vacuum not available", nil
	}
	mode, _ := p["mode"].(string)
	full := mode == "full"
	freed, err := j.Vacuum(ctx, full)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("vacuum mode=%s freed %.1f MB", orDefault(mode, "incremental"), float64(freed)/(1<<20)), nil
}

// JobLogPrune 清理任务执行历史。
func (j *Jobs) JobLogPrune(ctx context.Context, p map[string]any) (string, error) {
	keep := durParam(p, "keep", 30*24*time.Hour)
	n, err := j.Store.Job().Purge(ctx, j.now().Add(-keep).Unix())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("purged %d job runs", n), nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
