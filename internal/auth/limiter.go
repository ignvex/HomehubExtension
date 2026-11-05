package auth

import (
	"sync"
	"time"
)

// AttemptLimiter 按来源限制认证失败次数。
//
// 存在的理由：配对码、challenge、token 三个端点都接受未认证的输入，
// 没有限制就可以被无限次尝试。虽然配对码空间够大（12 个字符，字符集 31，
// 约 2.5e17 种），但 challenge 端点允许对已知 client_id 反复索取 nonce，
// 而 token 端点接受签名——两者都值得设一道闸。
//
// 计数只在失败时保留。成功不消耗配额，5xx 也不惩罚客户端
// （那是服务端自己的问题）。HTTP 中间件用 Reserve/Release 原子预占，
// Allow/Fail 保留给不存在并发问题的调用方。
type AttemptLimiter struct {
	mu        sync.Mutex
	attempts  map[string]*attemptRecord
	limit     int
	window    time.Duration
	lastSweep time.Time
}

type attemptRecord struct {
	count int
	first time.Time
}

// NewAttemptLimiter 构造失败次数限流器。limit 或 window 非正数时使用默认值。
func NewAttemptLimiter(limit int, window time.Duration) *AttemptLimiter {
	if limit <= 0 {
		limit = 10
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	return &AttemptLimiter{
		attempts: make(map[string]*attemptRecord),
		limit:    limit,
		window:   window,
	}
}

// Allow 判断该来源当前是否还能尝试。
//
// 返回 false 表示窗口内失败次数已达上限，调用方应直接返回 429，
// 不要再去碰数据库或算哈希——被限流的请求不该消耗任何资源。
func (l *AttemptLimiter) Allow(key string) bool {
	if key == "" {
		return true
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	rec, ok := l.attempts[key]
	if !ok {
		return true
	}
	// 窗口过期，视为新一轮。
	if now.Sub(rec.first) > l.window {
		delete(l.attempts, key)
		return true
	}
	return rec.count < l.limit
}

// Fail 记录一次失败。返回 true 表示这次失败让该来源进入了限流状态，
// 调用方可以据此写一条审计日志——只在"刚刚被拦住"时记录，
// 避免持续暴力尝试把日志刷满。
func (l *AttemptLimiter) Fail(key string) bool {
	if key == "" {
		return false
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	rec, ok := l.attempts[key]
	if !ok || now.Sub(rec.first) > l.window {
		l.attempts[key] = &attemptRecord{count: 1, first: now}
		return false
	}

	rec.count++
	return rec.count == l.limit
}

// Reserve 原子地检查并预占一次失败配额。
//
// 与 Allow + Fail 的"先查后计"不同：Allow 与 Fail 之间存在窗口，
// 同一来源的并发请求可以全部通过 Allow，再各自计一次失败，
// 实际放行的失败次数会远超 limit。Reserve 在同一把锁里完成检查与计数，
// 任意时刻已预占（含进行中）的次数都不会超过 limit。
//
// 返回 false 表示配额已满，调用方应直接返回 429。返回 true 后，
// 若请求最终不算失败，调用方必须调用 Release 退还；算失败则什么都不做，
// 计数就此保留。
func (l *AttemptLimiter) Reserve(key string) bool {
	if key == "" {
		return true
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	rec, ok := l.attempts[key]
	if !ok || now.Sub(rec.first) > l.window {
		l.attempts[key] = &attemptRecord{count: 1, first: now}
		return true
	}
	if rec.count >= l.limit {
		return false
	}
	rec.count++
	return true
}

// Release 退还一次 Reserve 预占的配额，用于请求最终没有失败的情况
// （成功、4xx 非鉴权错误、5xx）。计数降到 0 时删除条目。
//
// 若预占之后窗口已过期、条目已被清掉或被 Reset，这里什么都不做：
// 不会把计数减成负数，也不会凭空制造新条目。
func (l *AttemptLimiter) Release(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	rec, ok := l.attempts[key]
	if !ok {
		return
	}
	rec.count--
	if rec.count <= 0 {
		delete(l.attempts, key)
	}
}

// Reset 在认证成功后清空该来源的计数。
//
// 必须调用：否则一个用户打错几次密码、成功后继续操作，
// 几分钟内的正常使用会被之前的历史失败误伤。
func (l *AttemptLimiter) Reset(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// Remaining 返回该来源在窗口内还能失败几次，供 429 响应的
// Retry-After 与调试使用。
func (l *AttemptLimiter) Remaining(key string) (int, time.Duration) {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	rec, ok := l.attempts[key]
	if !ok || now.Sub(rec.first) > l.window {
		return l.limit, 0
	}
	remaining := l.limit - rec.count
	if remaining < 0 {
		remaining = 0
	}
	resetIn := l.window - now.Sub(rec.first)
	if resetIn < 0 {
		resetIn = 0
	}
	return remaining, resetIn
}

// sweepLocked 回收过期条目。与 ReplayGuard 同理：不能等表满了才清理，
// 否则低频部署下会一直占着内存。
func (l *AttemptLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) <= l.window/2 {
		return
	}
	for k, rec := range l.attempts {
		if now.Sub(rec.first) > l.window {
			delete(l.attempts, k)
		}
	}
	l.lastSweep = now
}

// Size 返回当前记录数，供测试使用。
func (l *AttemptLimiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.attempts)
}
