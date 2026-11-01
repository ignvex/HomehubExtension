// Package events 提供进程内的事件广播，供 SSE 使用。
//
// 设计上刻意简单：非阻塞发送，缓冲满就断开慢客户端而不是阻塞生产者。
// 一个卡住的浏览器标签页不应该拖慢抓取任务。
package events

import (
	"sync"
	"sync/atomic"
	"time"
)

// Event 是一条广播事件。ID 单调递增，供 SSE 的 Last-Event-ID 使用。
type Event struct {
	ID   int64  `json:"-"`
	Type string `json:"type"`
	Data any    `json:"data"`
	At   int64  `json:"at"`
}

// Bus 是进程内的事件广播中心。发送永不阻塞。
type Bus struct {
	mu     sync.RWMutex
	subs   map[uint64]chan Event
	nextID uint64
	seq    atomic.Int64
}

// New 构造一个空的事件总线。
func New() *Bus {
	return &Bus{subs: make(map[uint64]chan Event)}
}

var Default = New()

// Publish 广播一个事件。没有订阅者时开销接近零。
func (b *Bus) Publish(kind string, data any) {
	ev := Event{
		ID:   b.seq.Add(1),
		Type: kind,
		Data: data,
		At:   time.Now().Unix(),
	}

	// 必须用写锁：循环里可能在 dropLocked 中 delete(map) 与 close(ch)。
	// 用读锁时多个 Publish 会并发遍历同一个 map 并互相删除，触发
	// data race 甚至 "concurrent map writes" 直接崩溃。
	//
	// 用整段持写锁而不是「读锁收集 + 写锁删除」两阶段：两阶段之间
	// 仍可能有其它 Publish 往即将被 close 的通道发送，导致
	// "send on closed channel" panic。持同一把锁才能保证 send 与 close 互斥。
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// 缓冲满：这个连接已经跟不上，直接断开比阻塞广播更合适。
			b.dropLocked(id)
		}
	}
}

// Subscribe 返回事件通道与取消函数。通道带 64 条缓冲。
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	ch := make(chan Event, 64)
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
			b.mu.Unlock()
		})
	}
	return ch, cancel
}

// Close 关闭所有订阅，让阻塞在通道上的 SSE handler 立即返回。
//
// 优雅关闭时需要它：http.Server.Shutdown 只停止接受新连接并等待在途
// 请求完成，不会取消已有请求的 context。SSE 是长连接，不主动断开就会
// 一直拖到 Shutdown 超时。
//
// 幂等：重复调用不会 double close（map 里已经没有条目）。
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}

func (b *Bus) dropLocked(id uint64) {
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}
