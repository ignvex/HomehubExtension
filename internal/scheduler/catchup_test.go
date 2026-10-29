package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

func newCatchUpScheduler(t *testing.T) (*Scheduler, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db.Job(), time.UTC, nil), db
}

// 进程 downtime 会让 cron 的触发点丢失。没有补跑，
// 一台每天重启的机器上「凌晨 4 点」的任务永远不会执行。
func TestCatchUpRunsMissedJobs(t *testing.T) {
	s, db := newCatchUpScheduler(t)
	ctx := context.Background()

	ran := false
	s.Register(Task{Name: "rss-prune", Run: func(ctx context.Context, p map[string]any) (string, error) {
		ran = true
		return "purged 5", nil
	}})

	// 日级任务。
	if err := s.Apply([]Plan{{Name: "rss-prune", Schedule: "30 4 * * *", Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	// 造一条「两天前成功过」的记录。
	twoDaysAgo := time.Now().Add(-48 * time.Hour).Unix()
	id, err := db.Job().Start(ctx, "rss-prune", twoDaysAgo)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Job().Finish(ctx, id, "ok", "purged 3", twoDaysAgo+1); err != nil {
		t.Fatal(err)
	}

	got := s.CatchUp(ctx, time.Now())
	if !ran {
		t.Fatal("超过一个周期的任务应该被补跑")
	}
	if len(got) != 1 || got[0] != "rss-prune" {
		t.Errorf("ran = %v, want [rss-prune]", got)
	}
}

func TestCatchUpSkipsRecentJobs(t *testing.T) {
	s, db := newCatchUpScheduler(t)
	ctx := context.Background()

	ran := false
	s.Register(Task{Name: "rss-prune", Run: func(ctx context.Context, p map[string]any) (string, error) {
		ran = true
		return "", nil
	}})
	if err := s.Apply([]Plan{{Name: "rss-prune", Schedule: "30 4 * * *", Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	// 一小时前刚跑过：远未到一个周期，不该补跑。
	id, _ := db.Job().Start(ctx, "rss-prune", time.Now().Add(-time.Hour).Unix())
	db.Job().Finish(ctx, id, "ok", "", time.Now().Unix())

	s.CatchUp(ctx, time.Now())
	if ran {
		t.Error("周期内刚跑过的任务不该补跑")
	}
}

// 从未成功执行过的任务不补跑：刚部署时不该把四个重任务一起拉起来。
func TestCatchUpSkipsJobsWithoutHistory(t *testing.T) {
	s, _ := newCatchUpScheduler(t)

	ran := false
	s.Register(Task{Name: "blob-gc", Run: func(ctx context.Context, p map[string]any) (string, error) {
		ran = true
		return "", nil
	}})
	if err := s.Apply([]Plan{{Name: "blob-gc", Schedule: "0 5 * * *", Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	s.CatchUp(context.Background(), time.Now())
	if ran {
		t.Error("没有历史记录的任务不该在启动时立刻补跑")
	}
}

// db-vacuum 与 media-retry 明确排除：
// 前者会在最不该卡的时候阻塞写入，后者会立刻发起大量出站请求。
func TestCatchUpExcludesHeavyJobs(t *testing.T) {
	s, db := newCatchUpScheduler(t)
	ctx := context.Background()

	heavy := map[string]bool{}
	s.Register(Task{Name: "db-vacuum", Run: func(ctx context.Context, p map[string]any) (string, error) {
		heavy["db-vacuum"] = true
		return "", nil
	}})
	s.Register(Task{Name: "media-retry", Run: func(ctx context.Context, p map[string]any) (string, error) {
		heavy["media-retry"] = true
		return "", nil
	}})

	if err := s.Apply([]Plan{
		{Name: "db-vacuum", Schedule: "30 5 * * 0", Enabled: true},
		{Name: "media-retry", Schedule: "*/30 * * * *", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	// 给两者都造很久以前的历史。
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	for _, name := range []string{"db-vacuum", "media-retry"} {
		id, _ := db.Job().Start(ctx, name, old)
		db.Job().Finish(ctx, id, "ok", "", old+1)
	}

	s.CatchUp(ctx, time.Now())
	if len(heavy) != 0 {
		t.Errorf("重任务不该补跑，实际跑了 %v", heavy)
	}
}

// 周期估算要正确：日级任务约 24h，周级约 168h。
func TestScheduleInterval(t *testing.T) {
	cases := []struct {
		spec string
		want time.Duration
	}{
		{"30 4 * * *", 24 * time.Hour},
		{"0 5 * * 0", 7 * 24 * time.Hour},
		{"0 * * * *", time.Hour},
	}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

	for _, c := range cases {
		sched, err := parseSpec(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		got := scheduleInterval(sched, now)
		if got != c.want {
			t.Errorf("%s: interval = %v, want %v", c.spec, got, c.want)
		}
	}
}
