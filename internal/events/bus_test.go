package events

import (
	"sync"
	"testing"
	"time"
)

// Publish 会在慢订阅者的通道塞满时删除并关闭它。这段逻辑之前用读锁
// 保护 map，多个 Publish 并发时既会并发写 map（data race / fatal），
// 也会在 send 与 close 之间形成竞态（send on closed channel panic）。
// 两个都只在真实并发下暴露，单线程调用永远看不到。
func TestPublishConcurrentDropIsSafe(t *testing.T) {
	b := New()

	// 订阅但不消费：64 条缓冲塞满后，后续 Publish 必须走 drop 分支。
	_, cancel := b.Subscribe()
	defer cancel()

	const writers = 32
	const perWriter = 500

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				b.Publish("test", map[string]int{"j": j})
			}
		}()
	}
	wg.Wait()

	// drop 掉之后订阅表应当收敛到空，而不是残留已关闭的通道。
	b.mu.RLock()
	remaining := len(b.subs)
	b.mu.RUnlock()
	if remaining != 0 {
		t.Errorf("慢订阅者应被移除，subs 剩余 %d 个", remaining)
	}
}

// 订阅与取消和 Publish 并发，是最容易踩到 send-on-closed-channel 的组合。
func TestSubscribeCancelConcurrentWithPublish(t *testing.T) {
	b := New()

	stop := make(chan struct{})
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				b.Publish("tick", nil)
			}
		}
	}()

	var subWG sync.WaitGroup
	subWG.Add(1)
	go func() {
		defer subWG.Done()
		for i := 0; i < 200; i++ {
			ch, cancel := b.Subscribe()
			// 交替消费与直接取消：两种路径都要覆盖。
			if i%2 == 0 {
				select {
				case <-ch:
				case <-time.After(2 * time.Millisecond):
				}
				cancel()
			} else {
				cancel()
			}
		}
	}()

	// 订阅方先跑完；再停写入方。反过来会死锁：writer 等 stop，
	// 而 stop 要等 wg.Wait() 之后才关闭。
	subWG.Wait()
	close(stop)
	writerWG.Wait()

	b.mu.RLock()
	remaining := len(b.subs)
	b.mu.RUnlock()
	if remaining != 0 {
		t.Errorf("所有订阅都已取消，subs 剩余 %d 个", remaining)
	}
}

// 重复 cancel 必须幂等：SSE handler 里 defer cancel，同时慢消费者
// 路径可能已经把它从表里摘掉了。
func TestCancelIsIdempotent(t *testing.T) {
	b := New()
	_, cancel := b.Subscribe()
	cancel()
	cancel()
	cancel()
}

// 事件 ID 必须严格单调：前端用 Last-Event-ID 续传，回退或重号会让
// 客户端漏事件或重复处理。
func TestEventIDMonotonic(t *testing.T) {
	b := New()

	ch, cancel := b.Subscribe()
	defer cancel()

	done := make(chan []Event, 1)
	go func() {
		var got []Event
		for ev := range ch {
			got = append(got, ev)
			if len(got) == 50 {
				done <- got
				return
			}
		}
		done <- got
	}()

	for i := 0; i < 50; i++ {
		b.Publish("e", i)
	}

	var got []Event
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("未在超时内收到 50 条事件")
	}

	if len(got) != 50 {
		t.Fatalf("收到 %d 条事件, want 50", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].ID != got[i-1].ID+1 {
			t.Fatalf("事件 ID 不连续: %d → %d", got[i-1].ID, got[i].ID)
		}
	}
}

// 关闭时要主动断开所有订阅：http.Server.Shutdown 不会取消在途请求的
// context，SSE handler 如果只等请求 context，长连接会把关闭拖到超时。
// Close 之后每个订阅者的通道都必须关闭，handler 才能立刻返回。
func TestCloseDisconnectsAllSubscribers(t *testing.T) {
	b := New()

	const n = 5
	chans := make([]<-chan Event, 0, n)
	for i := 0; i < n; i++ {
		ch, cancel := b.Subscribe()
		defer cancel()
		chans = append(chans, ch)
	}

	b.Close()

	for i, ch := range chans {
		select {
		case _, open := <-ch:
			if open {
				t.Errorf("订阅 %d 在 Close 后仍收到事件", i)
			}
		case <-time.After(time.Second):
			t.Errorf("订阅 %d 的通道未关闭，SSE handler 会挂住", i)
		}
	}

	b.mu.RLock()
	remaining := len(b.subs)
	b.mu.RUnlock()
	if remaining != 0 {
		t.Errorf("Close 后 subs 剩余 %d 个", remaining)
	}
}

// Close 可能被 shutdown 路径重复调用，不能 double close。
func TestCloseIsIdempotent(t *testing.T) {
	b := New()
	_, cancel := b.Subscribe()
	defer cancel()

	b.Close()
	b.Close()
	b.Close()
}

// Close 与 Publish 并发：不能出现 send on closed channel。
func TestCloseConcurrentWithPublish(t *testing.T) {
	b := New()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				b.Publish("tick", nil)
			}
		}
	}()

	for i := 0; i < 50; i++ {
		_, cancel := b.Subscribe()
		cancel()
	}
	b.Close()
	close(stop)
	wg.Wait()
}
