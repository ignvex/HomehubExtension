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

// 同步刷新不随客户端断开中止，但必须随进程关停取消。
func TestRefreshContextDetachedFromRequestButStopsOnShutdown(t *testing.T) {
	b := NewBackground(context.Background())
	req, cancelReq := context.WithCancel(WithBackground(context.Background(), b))

	ctx, cancel := refreshContext(req, time.Minute)
	defer cancel()

	cancelReq()
	select {
	case <-ctx.Done():
		t.Fatal("客户端断开不应取消刷新")
	case <-time.After(20 * time.Millisecond):
	}

	sctx, scancel := context.WithTimeout(context.Background(), time.Second)
	defer scancel()
	if err := b.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("关停后刷新 ctx 仍未取消")
	}
}

// 没有注入 Background 时退回独立时限，不 panic。
func TestRefreshContextWithoutBackground(t *testing.T) {
	ctx, cancel := refreshContext(context.Background(), 10*time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("超时未生效")
	}
}
