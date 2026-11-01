package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

func newScheduler(t *testing.T) (*Scheduler, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db.Job(), time.UTC, nil), db
}

func TestRunNowRecordsResult(t *testing.T) {
	s, db := newScheduler(t)
	s.Register(Task{Name: "paste-expired", Run: func(ctx context.Context, p map[string]any) (string, error) {
		return "purged 3", nil
	}})

	detail, err := s.RunByName(context.Background(), "paste-expired")
	if err != nil {
		t.Fatalf("RunByName: %v", err)
	}
	if detail != "purged 3" {
		t.Errorf("detail = %q", detail)
	}

	runs, err := db.Job().Recent(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("job runs = %d, want 1", len(runs))
	}
	if runs[0].Status != "ok" {
		t.Errorf("status = %q", runs[0].Status)
	}
	if runs[0].FinishedAt == nil {
		t.Error("finished_at should be set")
	}
}

func TestFailureRecorded(t *testing.T) {
	s, db := newScheduler(t)
	s.Register(Task{Name: "blob-gc", Run: func(ctx context.Context, p map[string]any) (string, error) {
		return "", errors.New("disk on fire")
	}})

	if _, err := s.RunByName(context.Background(), "blob-gc"); err == nil {
		t.Fatal("expected error")
	}
	runs, _ := db.Job().Recent(context.Background(), 10)
	if runs[0].Status != "failed" {
		t.Errorf("status = %q, want failed", runs[0].Status)
	}
	if runs[0].Detail == "" {
		t.Error("error detail should be recorded")
	}
}

func TestTimeoutRecorded(t *testing.T) {
	s, _ := newScheduler(t)
	s.Register(Task{
		Name:    "rss-prune",
		Timeout: 50 * time.Millisecond,
		Run: func(ctx context.Context, p map[string]any) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
				return "done", nil
			}
		},
	})

	_, err := s.RunByName(context.Background(), "rss-prune")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestUnknownJobRejected(t *testing.T) {
	s, _ := newScheduler(t)
	if _, err := s.RunByName(context.Background(), "nope"); err == nil {
		t.Fatal("unknown job should be rejected")
	}
}

func TestConcurrentRunRejected(t *testing.T) {
	s, _ := newScheduler(t)
	var started atomic.Int32
	release := make(chan struct{})

	s.Register(Task{Name: "media-retry", Run: func(ctx context.Context, p map[string]any) (string, error) {
		started.Add(1)
		<-release
		return "ok", nil
	}})

	done := make(chan struct{})
	go func() {
		s.RunByName(context.Background(), "media-retry")
		close(done)
	}()

	// 等第一个真正跑起来
	for started.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	// 同一任务不允许并发：第二次直接报错，而不是排队。
	if _, err := s.RunByName(context.Background(), "media-retry"); err == nil {
		t.Error("concurrent run of the same job should be rejected")
	}
	close(release)
	<-done
}

func TestApplyRejectsUnimplementedJob(t *testing.T) {
	s, _ := newScheduler(t)
	err := s.Apply([]Plan{{Name: "does-not-exist", Schedule: "0 * * * *", Enabled: true}})
	if err == nil {
		t.Fatal("unimplemented job should fail Apply")
	}
}

func TestApplyAcceptsValidCron(t *testing.T) {
	s, _ := newScheduler(t)
	s.Register(Task{Name: "job-log-prune", Run: func(ctx context.Context, p map[string]any) (string, error) {
		return "", nil
	}})
	if err := s.Apply([]Plan{{Name: "job-log-prune", Schedule: "0 6 * * 1", Enabled: true}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// cron 在自己的 goroutine 里跑任务，未恢复的 panic 会终止整个进程。
// 调度器必须自己兜住，并且照样写 job_run 的终态——否则任务会永远
// 停在 running，前端「系统 → 任务」里看不到任何结束时间。
func TestPanicIsRecoveredAndRecorded(t *testing.T) {
	s, db := newScheduler(t)
	s.Register(Task{Name: "blob-gc", Run: func(ctx context.Context, p map[string]any) (string, error) {
		panic("nil map write")
	}})

	// 关键断言：调用本身不能把进程带走。
	_, err := s.RunByName(context.Background(), "blob-gc")
	if err == nil {
		t.Fatal("panic 应当以 error 形式返回")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("err = %v, 应带 panic 字样", err)
	}

	runs, err := db.Job().Recent(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("job runs = %d, want 1", len(runs))
	}
	if runs[0].Status != "failed" {
		t.Errorf("status = %q, want failed", runs[0].Status)
	}
	if runs[0].FinishedAt == nil {
		t.Error("panic 后必须写 finished_at，否则任务永远显示为运行中")
	}
	if runs[0].Detail == "" {
		t.Error("panic 信息应写进 detail")
	}
}

// 一旦 panic 过，同一任务的下一次运行不能被上一次残留的 running 标记挡住。
func TestPanicDoesNotBlockNextRun(t *testing.T) {
	s, _ := newScheduler(t)
	var calls atomic.Int32
	s.Register(Task{Name: "media-retry", Run: func(ctx context.Context, p map[string]any) (string, error) {
		if calls.Add(1) == 1 {
			panic("first run explodes")
		}
		return "ok", nil
	}})

	if _, err := s.RunByName(context.Background(), "media-retry"); err == nil {
		t.Fatal("首次应当 panic 成 error")
	}
	detail, err := s.RunByName(context.Background(), "media-retry")
	if err != nil {
		t.Fatalf("第二次运行失败: %v", err)
	}
	if detail != "ok" {
		t.Errorf("detail = %q, want ok", detail)
	}
}
