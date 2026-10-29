package app

import (
	"context"
	"sync"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"
)

// feedPoller 定期扫描到期的订阅并抓取。
//
// 这是一个独立的后台循环，不是 cron 任务，原因有两点：
//  1. 精度：feed 的最小抓取间隔是 10 分钟，30 秒的扫描精度足够，
//     而 5 段 cron 表达式最小只到分钟。
//  2. 日志：它每分钟都要跑，塞进 job_run 会瞬间淹没真正的清理记录。
//
// 没有这个循环，订阅只在「Homehub 首次推送」和「用户手动刷新」时抓取
// 一次，之后 next_fetch_at 一直被写入却从没人消费——订阅永远停在第一次的内容。
type feedPoller struct {
	store  store.Store
	fetch  func(ctx context.Context, feedID int64) error
	conc   int
	window int
	log    logger

	scanMu sync.Mutex // 保证两轮扫描不重叠

	// inflight 需要单独一把锁：worker goroutine 在 scan 返回之后
	// 仍会继续增删条目。用 scanMu 保护它是典型的错误——那把锁
	// 在 wg.Wait() 之前就已经随 scan 的 defer 释放了。
	inflightMu sync.Mutex
	inflight   map[int64]struct{}
}

// logger 是 slog.Logger 的最小接口，避免这里直接依赖具体类型。
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

const pollInterval = 30 * time.Second

func newFeedPoller(st store.Store, fetch func(context.Context, int64) error, concurrency int, log logger) *feedPoller {
	if concurrency < 1 {
		concurrency = 1
	}
	return &feedPoller{
		store:    st,
		fetch:    fetch,
		conc:     concurrency,
		window:   20, // 单轮最多取 20 个，避免一次积压把单核占满
		log:      log,
		inflight: make(map[int64]struct{}),
	}
}

// Run 阻塞运行直到 ctx 取消。
func (p *feedPoller) Run(ctx context.Context) {
	// 启动时先等一个短延迟：让 HTTP 服务与其他初始化先完成。
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.scan(ctx)
		}
	}
}

// scan 抓取一轮到期的订阅。
func (p *feedPoller) scan(ctx context.Context) {
	// 上一轮还没跑完就跳过：抓取有 90 秒超时，20 个 feed 分 2 个并发
	// 可能超过 30 秒的扫描间隔。排队没有意义，下一轮会重新取。
	if !p.scanMu.TryLock() {
		return
	}
	defer p.scanMu.Unlock()

	feeds, err := p.store.Feed().Due(ctx, time.Now().Unix(), p.window)
	if err != nil {
		p.log.Error("scan due feeds", "err", err)
		return
	}
	if len(feeds) == 0 {
		return
	}

	p.log.Info("fetching due feeds", "count", len(feeds))

	sem := make(chan struct{}, p.conc)
	var wg sync.WaitGroup

	for _, f := range feeds {
		if ctx.Err() != nil {
			break
		}
		// 手动刷新可能与定时抓取撞上同一个 feed。
		// 跳过比并发抓两遍好：抓取会消耗目标站带宽，也会让
		// next_fetch_at 被后完成的那次覆盖。
		if !p.markInflight(f.ID) {
			continue
		}

		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			defer p.clearInflight(id)

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			if err := p.fetch(ctx, id); err != nil {
				// 失败已经由 rss.Service 记录到 feed.last_error 并排了退避，
				// 这里只留一条日志。
				p.log.Warn("fetch feed failed", "feed_id", id, "err", err)
			}
		}(f.ID)
	}

	wg.Wait()
}

func (p *feedPoller) markInflight(id int64) bool {
	p.inflightMu.Lock()
	defer p.inflightMu.Unlock()
	if _, busy := p.inflight[id]; busy {
		return false
	}
	p.inflight[id] = struct{}{}
	return true
}

func (p *feedPoller) clearInflight(id int64) {
	p.inflightMu.Lock()
	defer p.inflightMu.Unlock()
	delete(p.inflight, id)
}
