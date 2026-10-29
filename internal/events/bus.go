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

	b.mu.RLock()
	defer b.mu.RUnlock()
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

func (b *Bus) dropLocked(id uint64) {
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}
