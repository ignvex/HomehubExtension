package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Stop 必须取消运行中任务的 context 并等它返回：app 在 Stop 之后
// 立刻关闭数据库，任务若还在后台写库就会撞上已关闭的连接。
func TestStopCancelsRunningTask(t *testing.T) {
	s, db := newScheduler(t)
	started := make(chan struct{})
	returned := make(chan error, 1)
	s.Register(Task{Name: "rss-prune", Timeout: time.Hour, Run: func(ctx context.Context, p map[string]any) (string, error) {
		close(started)
		<-ctx.Done()
		returned <- ctx.Err()
		return "", ctx.Err()
	}})

	task := s.tasks["rss-prune"]
	go s.runOnce(task) // 与 cron 触发同一路径

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("task did not start")
	}

	begin := time.Now()
	s.Stop(5 * time.Second)
	if took := time.Since(begin); took > 2*time.Second {
		t.Fatalf("Stop took %v; running task was not cancelled", took)
	}
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("task ctx err = %v, want Canceled", err)
		}
	default:
		t.Fatal("Stop returned before the running task did")
	}

	// 被中断的任务也要落终态，不能永远显示运行中。
	runs, err := db.Job().Recent(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status == "" || runs[0].Status == "running" {
		t.Fatalf("job_run not finalized: %+v", runs)
	}
}

// 手动触发用的是请求 context，Stop 同样要能取消它。
func TestStopCancelsManualRun(t *testing.T) {
	s, _ := newScheduler(t)
	started := make(chan struct{})
	s.Register(Task{Name: "blob-gc", Timeout: time.Hour, Run: func(ctx context.Context, p map[string]any) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}})

	done := make(chan error, 1)
	go func() {
		_, err := s.RunByName(context.Background(), "blob-gc")
		done <- err
	}()
	<-started
	s.Stop(5 * time.Second)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunByName err = %v, want Canceled", err)
		}
	default:
		t.Fatal("Stop returned before the manual run did")
	}
}

// Stop 之后不再接受新的执行。
func TestRunAfterStopRejected(t *testing.T) {
	s, _ := newScheduler(t)
	s.Register(Task{Name: "paste-expired", Run: func(ctx context.Context, p map[string]any) (string, error) {
		t.Error("task must not run after Stop")
		return "", nil
	}})
	s.Stop(time.Second)
	if _, err := s.RunByName(context.Background(), "paste-expired"); !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
}
