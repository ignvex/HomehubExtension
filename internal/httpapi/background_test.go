package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Shutdown 必须取消在途任务并等待它返回：app 紧接着就要关库。
func TestBackgroundShutdownCancelsAndWaits(t *testing.T) {
	b := NewBackground(context.Background())
	started := make(chan struct{})
	finished := make(chan error, 1)
	if !b.Go(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // 模拟取消后的收尾写入
		finished <- ctx.Err()
	}) {
		t.Fatal("Go refused before Shutdown")
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("task ctx err = %v", err)
		}
	default:
		t.Fatal("Shutdown returned before the task did")
	}

	if b.Go(func(context.Context) { t.Error("must not run after Shutdown") }) {
		t.Fatal("Go accepted a task after Shutdown")
	}
}

// 经 BaseContext 注入后，请求 context 上能取回同一个 Background。
func TestBackgroundFromContext(t *testing.T) {
	if BackgroundFrom(context.Background()) != nil {
		t.Fatal("empty ctx should carry no Background")
	}
	b := NewBackground(context.Background())
	if BackgroundFrom(WithBackground(context.Background(), b)) != b {
		t.Fatal("BackgroundFrom lost the Background")
	}
}
