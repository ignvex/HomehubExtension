package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAttemptLimiterBlocksAfterLimit(t *testing.T) {
	l := NewAttemptLimiter(3, time.Minute)

	for i := 0; i < 2; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("第 %d 次失败后不该被拦", i+1)
		}
		if l.Fail("1.2.3.4") {
			t.Fatalf("第 %d 次失败不该返回 justBlocked", i+1)
		}
	}

	// 第三次失败刚刚达到上限。
	if l.Fail("1.2.3.4") != true {
		t.Error("达到上限时应返回 justBlocked=true")
	}
	if l.Allow("1.2.3.4") {
		t.Error("达到上限后应被拦截")
	}

	// 另一个来源不受影响——限流必须按来源隔离。
	if !l.Allow("5.6.7.8") {
		t.Error("其他来源不该被牵连")
	}
}

func TestAttemptLimiterResetOnSuccess(t *testing.T) {
	l := NewAttemptLimiter(2, time.Minute)
	l.Fail("1.1.1.1")
	l.Fail("1.1.1.1")

	if l.Allow("1.1.1.1") {
		t.Fatal("应该已被拦截")
	}

	// 成功登录后清空：否则用户打错几次、之后成功，
	// 几分钟内的正常使用会被历史失败误伤。
	l.Reset("1.1.1.1")
	if !l.Allow("1.1.1.1") {
		t.Error("Reset 后应恢复")
	}
}

func TestAttemptLimiterWindowExpiry(t *testing.T) {
	l := NewAttemptLimiter(1, 80*time.Millisecond)
	l.Fail("9.9.9.9")
	if l.Allow("9.9.9.9") {
		t.Fatal("窗口内应被拦截")
	}

	time.Sleep(120 * time.Millisecond)
	if !l.Allow("9.9.9.9") {
		t.Error("窗口过期后应恢复")
	}
}

func TestAttemptLimiterSelfSweeps(t *testing.T) {
	l := NewAttemptLimiter(100, 60*time.Millisecond)
	for _, ip := range []string{"a", "b", "c"} {
		l.Fail(ip)
	}
	if got := l.Size(); got != 3 {
		t.Fatalf("Size = %d, want 3", got)
	}

	// 窗口过后，任意一次检查都会触发清理，旧条目应被回收。
	// Allow 本身不插入记录——只有失败才留痕，成功的请求不该在表里占位。
	time.Sleep(90 * time.Millisecond)
	l.Allow("d")
	if got := l.Size(); got != 0 {
		t.Errorf("清理后 Size = %d, want 0", got)
	}
}

func TestAttemptLimiterRemaining(t *testing.T) {
	l := NewAttemptLimiter(5, time.Minute)
	if n, _ := l.Remaining("x"); n != 5 {
		t.Errorf("初始剩余 = %d, want 5", n)
	}
	l.Fail("x")
	l.Fail("x")
	n, resetIn := l.Remaining("x")
	if n != 3 {
		t.Errorf("剩余 = %d, want 3", n)
	}
	if resetIn <= 0 || resetIn > time.Minute {
		t.Errorf("resetIn = %v", resetIn)
	}
}

func TestAttemptLimiterEmptyKeyAllowed(t *testing.T) {
	// 拿不到 IP 时不该把所有人当成同一个来源。
	l := NewAttemptLimiter(1, time.Minute)
	if !l.Allow("") {
		t.Error("空 key 应始终允许")
	}
	l.Fail("")
	if !l.Allow("") {
		t.Error("空 key 不该被计数")
	}
}

func TestAttemptLimiterReserveRelease(t *testing.T) {
	l := NewAttemptLimiter(2, time.Minute)
	const k = "9.9.9.9"

	if !l.Reserve(k) || !l.Reserve(k) {
		t.Fatal("前两次预占应成功")
	}
	if l.Reserve(k) {
		t.Fatal("配额用完后预占应失败")
	}
	// 退还一次后又能预占一次。
	l.Release(k)
	if !l.Reserve(k) {
		t.Fatal("退还后应能再次预占")
	}
	l.Release(k)
	l.Release(k)
	if l.Size() != 0 {
		t.Errorf("计数归零后应删除条目，size=%d", l.Size())
	}
	// 多余的 Release 不能把计数减成负数、也不能制造条目。
	l.Release(k)
	if l.Size() != 0 {
		t.Errorf("多余的 Release 不应制造条目，size=%d", l.Size())
	}
	if rem, _ := l.Remaining(k); rem != 2 {
		t.Errorf("remaining = %d, want 2", rem)
	}
	// 空 key 永远放行。
	if !l.Reserve("") {
		t.Error("空 key 应放行")
	}
}

// 并发预占：无论多少 goroutine 同时到达，成功预占（且不退还）的次数
// 都不能超过 limit。这正是 Allow+Fail 先查后计做不到的。
func TestAttemptLimiterReserveConcurrent(t *testing.T) {
	const limit, workers = 5, 50
	l := NewAttemptLimiter(limit, time.Minute)

	var wg sync.WaitGroup
	var granted atomic.Int64
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if l.Reserve("1.2.3.4") {
				granted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := granted.Load(); got != limit {
		t.Fatalf("granted = %d, want exactly %d", got, limit)
	}
}
