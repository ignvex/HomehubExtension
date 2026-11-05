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
	cutoffTime := j.now().Add(-grace)
	cutoff := cutoffTime.Unix()

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
		orphans, orphanBytes, err := j.sweepOrphans(ctx, cutoffTime, true)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("dry_run: %d blobs would be purged, %d orphan files (%.1f MB) would be removed",
			len(unref), orphans, float64(orphanBytes)/(1<<20)), nil
	}

	var purged int
	var freed int64
	for _, b := range unref {
		if ctx.Err() != nil {
			break
		}
		// 查询之后、删除之前，新条目可能去重命中了这份文件（会刷新 mtime）。
		// 文件按 mtime 复检、行按 ref_count 与 last_seen_at 复检，
		// 任何一处显示它刚被用过就跳过，留给下一轮重新判断。
		// 文件已经不在（上一轮删了文件、删行失败）时直接去删行。
		removed, _, err := j.Blobs.RemoveIfOlder(b.SHA256, b.Ext, cutoffTime)
		if err != nil {
			continue
		}
		if !removed {
			if _, statErr := os.Stat(j.Blobs.Path(b.SHA256, b.Ext)); !os.IsNotExist(statErr) {
				continue
			}
		}
		if ok, err := j.Store.Blob().DeleteUnreferenced(ctx, b.SHA256, cutoff); err != nil || !ok {
			continue
		}
		purged++
		freed += b.Size
	}

	// 孤儿文件放在行级 GC 之后：上面刚删掉行、但文件删除失败的那些，
	// 这里会作为孤儿再被捞一次。
	orphans, orphanBytes, err := j.sweepOrphans(ctx, cutoffTime, false)
	if err != nil {
		return "", err
	}

	msg := fmt.Sprintf("purged %d blobs, freed %.1f MB, removed %d orphan files (%.1f MB)",
		purged, float64(freed)/(1<<20), orphans, float64(orphanBytes)/(1<<20))
	if recalibrated > 0 {
		msg += fmt.Sprintf(", recalibrated %d counters", recalibrated)
	}
	return msg, nil
}

// orphanBatch 是孤儿判定时一次查询的 sha 数，与 BlobStore.Known 的上限一致。
const orphanBatch = 500

// sweepOrphans 删除磁盘上存在、blob 表里没有行的文件。
//
// 来源：下载落盘后、blob 行入库前进程崩溃；或 blob 行被删但文件删除失败。
// 这些文件占着配额却永远不会被行级 GC 看到。
//
// 宽限保护：只删 mtime 早于 cutoff 的文件。抓取管线是「先落盘、后入库」，
// 刚写入的文件在入库前的几秒里看起来就是孤儿；去重命中会刷新 mtime，
// 所以被复用的旧文件同样受保护。最终的 mtime 判定与删除在
// blob.Store 的锁内完成（RemoveIfOlder），不会与并发落位交叠。
func (j *Jobs) sweepOrphans(ctx context.Context, cutoff time.Time, dryRun bool) (count int, bytes int64, err error) {
	if j.Blobs == nil {
		return 0, 0, nil
	}
	var batch []blob.FileInfo
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		shas := make([]string, len(batch))
		for i, f := range batch {
			shas[i] = f.SHA256
		}
		known, err := j.Store.Blob().Known(ctx, shas)
		if err != nil {
			return err
		}
		for _, f := range batch {
			if known[f.SHA256] {
				continue
			}
			if dryRun {
				count++
				bytes += f.Size
				continue
			}
			removed, size, err := j.Blobs.RemoveIfOlder(f.SHA256, f.Ext, cutoff)
			if err != nil || !removed {
				continue // 删不掉就留到下一轮
			}
			count++
			bytes += size
		}
		batch = batch[:0]
		return nil
	}

	err = j.Blobs.WalkFiles(ctx, func(f blob.FileInfo) error {
		// 宽限期内的文件不进候选：省一次查询，也让 dry_run 计数准确。
		if !f.ModTime.Before(cutoff) {
			return nil
		}
		batch = append(batch, f)
		if len(batch) >= orphanBatch {
			return flush()
		}
		return nil
	})
	if err != nil {
		return count, bytes, err
	}
	return count, bytes, flush()
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
