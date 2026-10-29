// Package scheduler 负责 cron 调度与任务执行记录。
//
// 调度器不实现任何清理逻辑，只负责：按表达式触发、保证同名任务不并发、
// 记录每次执行的结果。具体任务由 app 层注册。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/robfig/cron/v3"
)

// Task 是一个可调度的清理任务。
type Task struct {
	Name    string
	Timeout time.Duration
	Params  map[string]any
	Run     func(ctx context.Context, params map[string]any) (string, error)
}

// Scheduler 把配置里的 jobs 绑定到已注册的任务上。
type Scheduler struct {
	cron  *cron.Cron
	jobs  store.JobStore
	log   *slog.Logger
	tasks map[string]Task

	mu       sync.Mutex
	running  map[string]bool
	planned  []plannedTask
	onFinish func(job, status, detail string)
}

// plannedTask 保存调度项与其解析后的 schedule，
// 供启动补跑时计算「多久跑一次」。
type plannedTask struct {
	task     Task
	schedule cron.Schedule
}

// SetOnFinish 注册任务结束回调，供 SSE 推送。
// 在 Apply/Start 之前调用；为 nil 时静默跳过。
func (s *Scheduler) SetOnFinish(fn func(job, status, detail string)) {
	s.onFinish = fn
}

// New 构造调度器。loc 为 nil 时用本地时区。
func New(jobs store.JobStore, loc *time.Location, log *slog.Logger) *Scheduler {
	if loc == nil {
		loc = time.Local
	}
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		cron:    cron.New(cron.WithLocation(loc)),
		jobs:    jobs,
		log:     log,
		tasks:   make(map[string]Task),
		running: make(map[string]bool),
	}
}

// Register 注册一个可调度的任务实现。必须在 Apply 之前调用。
func (s *Scheduler) Register(t Task) {
	if t.Timeout <= 0 {
		t.Timeout = 10 * time.Minute
	}
	s.tasks[t.Name] = t
}

// Plan 是按配置构造的一条调度项。
type Plan struct {
	Name     string
	Schedule string
	Timeout  time.Duration
	Enabled  bool
	Params   map[string]any
}

// Apply 注册全部调度项。未知任务名说明配置与代码不同步，必须报错。
func (s *Scheduler) Apply(plans []Plan) error {
	var errs []error
	for _, p := range plans {
		if !p.Enabled {
			s.log.Info("job disabled by config", "job", p.Name)
			continue
		}
		task, ok := s.tasks[p.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("job %q is not implemented", p.Name))
			continue
		}
		if p.Timeout > 0 {
			task.Timeout = p.Timeout
		}
		if p.Params != nil {
			task.Params = p.Params
		}
		name := p.Name
		// 自己解析而不是用 AddFunc：需要保留 Schedule 对象来计算调度周期，
		// 启动补跑据此判断「错过了几轮」。AddFunc 只返回 EntryID。
		sched, err := cron.ParseStandard(p.Schedule)
		if err != nil {
			errs = append(errs, fmt.Errorf("job %q: %w", name, err))
			continue
		}
		// 闭包捕获 task 的副本；cron 每次触发都调用同一个函数，
		// 并发保护在 runOnce 里做，而不是依赖 cron 的 chain。
		s.cron.Schedule(sched, cron.FuncJob(func() { s.runOnce(task) }))
		s.planned = append(s.planned, plannedTask{task: task, schedule: sched})
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Start 启动 cron 循环。已注册的任务按各自的表达式触发。
func (s *Scheduler) Start() { s.cron.Start() }

// Stop 停止调度并等待运行中的任务，超出 wait 后放弃。
func (s *Scheduler) Stop(wait time.Duration) {
	ctx := s.cron.Stop()
	select {
	case <-ctx.Done():
	case <-time.After(wait):
		s.log.Warn("scheduler stop timed out", "waited", wait)
	}
}

func (s *Scheduler) runOnce(task Task) {
	// 同一任务上一轮未结束就跳过：清理任务没有"补一次"的必要，
	// 排队只会让下一轮也变慢。
	s.mu.Lock()
	if s.running[task.Name] {
		s.mu.Unlock()
		s.log.Info("job skipped, previous run still active", "job", task.Name)
		return
	}
	s.running[task.Name] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, task.Name)
		s.mu.Unlock()
	}()

	s.RunNow(context.Background(), task)
}

// RunNow 同步执行一次任务并写 job_run。手动触发也走这条路径。
func (s *Scheduler) RunNow(ctx context.Context, task Task) (string, error) {
	started := time.Now()
	runID, err := s.jobs.Start(ctx, task.Name, started.Unix())
	if err != nil {
		return "", fmt.Errorf("record job start: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, task.Timeout)
	defer cancel()

	detail, runErr := task.Run(runCtx, task.Params)
	status := "ok"
	if runErr != nil {
		switch {
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			status = "timeout"
		case errors.Is(runErr, context.Canceled):
			status = "timeout"
		default:
			status = "failed"
		}
		if detail == "" {
			detail = truncate(runErr.Error(), 500)
		} else {
			detail = truncate(detail+" | "+runErr.Error(), 500)
		}
	}

	finish := time.Now()
	if err := s.jobs.Finish(ctx, runID, status, detail, finish.Unix()); err != nil {
		s.log.Error("record job finish", "job", task.Name, "err", err)
	}
	s.log.Info("job finished", "job", task.Name, "status", status,
		"took", finish.Sub(started).String(), "detail", detail)
	if s.onFinish != nil {
		s.onFinish(task.Name, status, detail)
	}
	return detail, runErr
}

// RunByName 供 POST /system/jobs/{name}/run 使用。
func (s *Scheduler) RunByName(ctx context.Context, name string) (string, error) {
	task, ok := s.tasks[name]
	if !ok {
		return "", fmt.Errorf("unknown job %q", name)
	}
	s.mu.Lock()
	if s.running[name] {
		s.mu.Unlock()
		return "", fmt.Errorf("job %q is already running", name)
	}
	s.running[name] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, name)
		s.mu.Unlock()
	}()
	return s.RunNow(ctx, task)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// catchUpExcluded 是不参与启动补跑的任务。
//
// db-vacuum：全量 VACUUM 需要约两倍数据库大小的临时空间且全程阻塞写入，
// 启动时立刻跑会在最不该卡的时候卡住。
// media-retry：会立刻发起大量出站请求，启动阶段应该先让服务可用。
var catchUpExcluded = map[string]bool{
	"db-vacuum":   true,
	"media-retry": true,
}

// CatchUp 补跑已错过的任务。
//
// 语义：对每个已注册的调度项，算出它「应该多久跑一次」（用两次 Next 的间隔
// 近似），再查 job_run 里最后一次成功执行的时间。如果距现在已经超过一个完整
// 周期，就立即跑一次。
//
// 为什么需要这个：cron 调度器随进程启停，进程 downtime 期间的触发点全部丢失。
// 一台每天重启一次的机器上，定在凌晨 4 点的 rss-prune 永远不会执行——
// 过期条目会无限堆积，直到磁盘被撑满。
func (s *Scheduler) CatchUp(ctx context.Context, now time.Time) (ran []string) {
	s.mu.Lock()
	planned := append([]plannedTask(nil), s.planned...)
	s.mu.Unlock()

	for _, p := range planned {
		if catchUpExcluded[p.task.Name] {
			continue
		}
		if ctx.Err() != nil {
			return ran
		}

		interval := scheduleInterval(p.schedule, now)
		if interval <= 0 {
			continue
		}

		lastOK, ok := s.lastSuccess(ctx, p.task.Name)
		if !ok {
			// 从未成功执行过。刚部署时不该立刻把四个重任务一起跑起来，
			// 记录一条日志即可，让正常调度接手。
			s.log.Info("job has no success record, skipping catch-up", "job", p.task.Name)
			continue
		}
		elapsed := now.Sub(lastOK)
		if elapsed < interval {
			continue
		}

		s.log.Info("catch-up running missed job",
			"job", p.task.Name, "last_success", lastOK.Format(time.RFC3339),
			"elapsed", elapsed.Round(time.Minute).String(),
			"interval", interval.Round(time.Minute).String())

		s.mu.Lock()
		busy := s.running[p.task.Name]
		if !busy {
			s.running[p.task.Name] = true
		}
		s.mu.Unlock()
		if busy {
			continue
		}

		func() {
			defer func() {
				s.mu.Lock()
				delete(s.running, p.task.Name)
				s.mu.Unlock()
			}()
			s.RunNow(ctx, p.task)
		}()
		ran = append(ran, p.task.Name)
	}
	return ran
}

// scheduleInterval 用连续两个触发点的时间差近似调度周期。
//
// 对 5 段 cron 与 @daily 这类描述符都成立；
// 对带月份的表达式（如 "0 0 1 1 *"）会给出一年，这正是我们想要的语义。
func scheduleInterval(sched cron.Schedule, now time.Time) time.Duration {
	next := sched.Next(now)
	if next.IsZero() {
		return 0
	}
	after := sched.Next(next)
	if after.IsZero() {
		return 0
	}
	return after.Sub(next)
}

// lastSuccess 查询某任务最近一次成功执行的时间。
func (s *Scheduler) lastSuccess(ctx context.Context, job string) (time.Time, bool) {
	runs, err := s.jobs.Recent(ctx, 200)
	if err != nil {
		return time.Time{}, false
	}
	for _, r := range runs {
		if r.Job != job {
			continue
		}
		if r.Status != "ok" {
			continue
		}
		return time.Unix(r.StartedAt, 0), true
	}
	return time.Time{}, false
}

// parseSpec 是 cron.ParseStandard 的薄包装，便于测试与将来换实现。
func parseSpec(spec string) (cron.Schedule, error) {
	return cron.ParseStandard(spec)
}
