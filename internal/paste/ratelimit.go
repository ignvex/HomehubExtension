package paste

import (
	"sync"
	"time"
)

// 匿名创建与口令尝试的限额。
//
// 不进配置：这是安全底线（AGENTS.md「匿名可写的接口必须有限流」），
// 不是需要按部署调的业务参数。
const (
	// AnonCreatePerMinute 是单个来源每分钟最多能匿名创建的片段数。
	AnonCreatePerMinute = 10
	// PasswordAttemptLimit 是同一 (来源, slug) 在窗口内允许的口令失败次数。
	PasswordAttemptLimit = 5
	// PasswordAttemptWindow 是口令失败计数的窗口。
	PasswordAttemptWindow = 15 * time.Minute

	// maxRateKeys 是固定窗口计数器最多记录的来源数。
	// 超出时新来源直接被拒——宁可误伤，也不能让伪造来源把内存撑爆。
	maxRateKeys = 10000
)

// fixedWindow 是按 key 计数的固定窗口限流器。
//
// 窗口粒度粗（窗口边界处最多放过 2×limit），但实现简单、内存有界，
// 对「每分钟十条」这种量级足够。
type fixedWindow struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	maxKeys   int
	now       func() time.Time
	counts    map[string]*windowCount
	lastSweep time.Time
}

type windowCount struct {
	start time.Time
	n     int
}

func newFixedWindow(limit int, window time.Duration, maxKeys int, now func() time.Time) *fixedWindow {
	return &fixedWindow{
		limit:   limit,
		window:  window,
		maxKeys: maxKeys,
		now:     now,
		counts:  make(map[string]*windowCount),
	}
}

// take 消耗一次配额。允许时返回 (true, 0)，
// 拒绝时返回 (false, 距窗口结束的时长)。
func (w *fixedWindow) take(key string) (bool, time.Duration) {
	now := w.now()

	w.mu.Lock()
	defer w.mu.Unlock()

	w.sweepLocked(now)

	rec, ok := w.counts[key]
	if !ok || now.Sub(rec.start) >= w.window {
		if !ok && len(w.counts) >= w.maxKeys {
			// 表满且清扫后仍满：拒绝新来源，等最早的窗口过去。
			return false, w.window
		}
		w.counts[key] = &windowCount{start: now, n: 1}
		return true, 0
	}
	if rec.n >= w.limit {
		return false, rec.start.Add(w.window).Sub(now)
	}
	rec.n++
	return true, 0
}

// sweepLocked 回收过期窗口。每半个窗口最多扫一次；表满时强制扫。
func (w *fixedWindow) sweepLocked(now time.Time) {
	if now.Sub(w.lastSweep) < w.window/2 && len(w.counts) < w.maxKeys {
		return
	}
	for k, rec := range w.counts {
		if now.Sub(rec.start) >= w.window {
			delete(w.counts, k)
		}
	}
	w.lastSweep = now
}

func (w *fixedWindow) size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.counts)
}

// AllowAnonymousCreate 为一次匿名创建消耗来源 key 的配额。
// 返回 false 时第二个值是建议的 Retry-After。
// 登录用户不应调用它：限额只针对匿名写入。
func (s *Service) AllowAnonymousCreate(key string) (bool, time.Duration) {
	return s.createLimit.take(key)
}

// passwordKey 把来源与 slug 组合成限流 key。
// 按 slug 分开计：一个来源猜错 A 的口令不应影响它读 B。
func passwordKey(source, slug string) string {
	return source + "\x00" + slug
}

// PasswordAttemptAllowed 判断该来源对该 slug 是否还能尝试口令。
// 返回 false 时第二个值是建议的 Retry-After。
func (s *Service) PasswordAttemptAllowed(source, slug string) (bool, time.Duration) {
	key := passwordKey(source, slug)
	if s.pwAttempts.Allow(key) {
		return true, 0
	}
	_, resetIn := s.pwAttempts.Remaining(key)
	return false, resetIn
}

// RecordPasswordResult 记录一次口令校验结果：失败计数，成功清零。
func (s *Service) RecordPasswordResult(source, slug string, ok bool) {
	key := passwordKey(source, slug)
	if ok {
		s.pwAttempts.Reset(key)
		return
	}
	s.pwAttempts.Fail(key)
}
