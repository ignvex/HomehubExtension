// Package app 负责装配依赖、编排启动顺序与优雅关闭。
//
// 启动顺序是有意为之：配置 → 日志 → 数据库 → 文件存储 → 密钥 → 领域服务 →
// 路由 → 调度器 → HTTP。任一步失败都直接退出，不留半启动状态。
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ignvex/HomehubExtension/internal/auth"
	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/config"
	"github.com/ignvex/HomehubExtension/internal/events"
	"github.com/ignvex/HomehubExtension/internal/httpapi"
	"github.com/ignvex/HomehubExtension/internal/paste"
	"github.com/ignvex/HomehubExtension/internal/rss"
	"github.com/ignvex/HomehubExtension/internal/scheduler"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
	"github.com/ignvex/HomehubExtension/internal/systemd"
)

// Options 是 app.Run 的装配参数。
type Options struct {
	Config  config.Config
	Log     *slog.Logger
	Build   httpapi.BuildInfo
	WebFS   fs.FS
	DevMode bool
}

// App 持有进程内所有长生命周期组件。
type App struct {
	cfg     config.Config
	log     *slog.Logger
	db      *sqlite.DB
	blobs   *blob.Store
	auth    *auth.Service
	hub     *auth.HubVerifier
	paste   *paste.Service
	rss     *rss.Service
	sched   *scheduler.Scheduler
	bus     *events.Bus
	signer  *auth.Signer
	limiter *auth.AttemptLimiter
	server  *http.Server
	srvErr  chan error
	started time.Time

	// rootCtx 是全部后台工作（轮询、启动补跑）的根，shutdown 第一步取消。
	// 它不派生自 Run 的 ctx：后台工作的停止时机由 shutdown 的顺序决定，
	// 而不是信号到达的那一刻，这样等待与关库的先后才可控。
	rootCtx    context.Context
	rootCancel context.CancelFunc
	// workers 跟踪 rootCtx 下的 goroutine；关库前必须全部返回。
	workers sync.WaitGroup
	// bg 跟踪 HTTP 请求触发的后台任务（如首推抓取）。
	bg *httpapi.Background
}

// 关闭总预算。systemd 的 TimeoutStopSec 必须大于它。
const shutdownBudget = 20 * time.Second

// serverReadTimeout 是读取整个请求（头 + body）的上限，防慢速 body 攻击。
//
// 暂用常量而不是配置项（server.read_timeout），默认值 30s 已覆盖 pastebin
// 与 OPML 上传的正常场景。它不会切断 SSE：net/http 在 handler 开始前
// （无 body 的 GET）或 body 读完时启动后台读，并在那一刻把读 deadline 清零
// （net/http/server.go connReader.startBackgroundRead 的
// SetReadDeadline(time.Time{})），ReadTimeout 之后不再作用于该连接。
// internal/httpapi/readtimeout_test.go 锁定这一行为。
const serverReadTimeout = 30 * time.Second

// Run 启动完整服务，阻塞直到 ctx 取消，然后按序关闭。
// Run 启动完整服务，阻塞直到 ctx 取消，然后按序关闭。
//
// 装配顺序是有意为之：配置 → 日志 → 数据库 → 文件存储 → 密钥 → 领域服务 →
// 路由 → 调度器 → HTTP。任一步失败都直接退出，不留半启动状态。
func Run(ctx context.Context, opts Options) error {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	a := &App{
		cfg:     opts.Config,
		log:     log,
		started: time.Now(),
		bus:     events.Default,
	}
	a.rootCtx, a.rootCancel = context.WithCancel(context.Background())
	defer a.rootCancel()
	a.bg = httpapi.NewBackground(a.rootCtx)

	if err := a.openInfra(ctx); err != nil {
		return err
	}
	// 基础设施起来之后任何失败都要回收已打开的资源。
	defer a.closeInfra()

	if err := a.buildServices(); err != nil {
		return err
	}
	if err := a.setupScheduler(); err != nil {
		return err
	}

	// 心跳覆盖启动与运行全程：systemd 用 WatchdogSec 发现「进程还在但
	// 已经不干活」的情况，这是 Restart=on-failure 看不到的一类故障。
	go systemd.KeepWatchdog(ctx, log)

	if err := a.startServers(opts); err != nil {
		return err
	}

	select {
	case err := <-a.srvErr:
		a.shutdown()
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	a.shutdown()
	return nil
}

// openInfra 打开数据库、文件存储与服务端密钥。
//
// 这三者是后续一切的依赖，失败必须让整个进程退出而不是降级运行——
// 一个打不开数据库的实例对外只会返回 500。
func (a *App) openInfra(ctx context.Context) error {
	cfg := a.cfg

	db, err := sqlite.Open(ctx, cfg.Database.Path, sqlite.Options{
		BusyTimeout: cfg.Database.BusyTimeout.Duration,
		ReadConns:   cfg.Database.ReadConns,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	a.db = db

	if schema, err := db.SchemaVersion(ctx); err == nil {
		a.log.Info("database ready", "path", cfg.Database.Path, "schema_version", schema)
	}

	// 文件存储的 Open 会顺带清空 tmp/：崩溃残留的下载不该占用配额。
	blobs, err := blob.Open(blob.Options{
		Root:         cfg.Storage.BlobDir,
		TmpDir:       cfg.Storage.TmpDir,
		MaxSize:      cfg.Storage.MaxBlobBytes,
		Quota:        cfg.Storage.QuotaBytes,
		MinFreeBytes: cfg.Storage.MinFreeBytes,
	})
	if err != nil {
		return fmt.Errorf("open blob store: %w", err)
	}
	a.blobs = blobs

	signer, err := auth.LoadOrCreateSigner(cfg.Auth.ServerKeyPath)
	if err != nil {
		return fmt.Errorf("load server key: %w", err)
	}
	a.signer = signer
	a.log.Info("server key ready", "kid", signer.KID(), "path", cfg.Auth.ServerKeyPath)

	a.limiter = auth.NewAttemptLimiter(
		cfg.Auth.FailedAttemptLimit,
		cfg.Auth.FailedAttemptWindow.Duration,
	)
	return nil
}

// buildServices 构造领域服务。
//
// Homehub 入站公钥缺失时不拒绝启动，只禁用该通道：
// 很多部署一开始只用浏览器访问，没有宿主对接需求。
func (a *App) buildServices() error {
	cfg := a.cfg

	a.auth = auth.NewService(a.db.Auth(), a.signer, auth.Settings{
		SessionTTL:    cfg.Auth.SessionTTL.Duration,
		ChallengeTTL:  cfg.Auth.ChallengeTTL.Duration,
		EnrollmentTTL: cfg.Auth.EnrollmentTTL.Duration,
	})

	if cfg.Auth.Hub.Enabled && cfg.Auth.Hub.PublicKey != "" {
		pub, err := auth.ParseHubPublicKey([]byte(cfg.Auth.Hub.PublicKey))
		if err != nil {
			a.log.Warn("hub signature channel disabled", "err", err)
		} else {
			a.hub = auth.NewHubVerifier(pub,
				cfg.Auth.Hub.ClockSkew.Duration, cfg.Auth.Hub.NonceTTL.Duration)
			a.log.Info("hub signature channel ready",
				"fingerprint", auth.HubKeyFingerprint(pub))
		}
	}

	a.paste = paste.NewService(a.db, paste.Config{
		MaxBodyBytes:  cfg.Pastebin.MaxBodyBytes,
		DefaultExpiry: cfg.Pastebin.DefaultExpiry.Duration,
		MaxExpiry:     cfg.Pastebin.MaxExpiry.Duration,
		SlugLength:    cfg.Pastebin.SlugLength,
		AllowPublic:   cfg.Pastebin.AllowPublic,
		Languages:     toSet(cfg.Pastebin.LanguageList),
	})

	if cfg.RSS.Enabled {
		a.rss = a.newRSSService()
	}
	return nil
}

// newRSSService 构造抓取服务并接线到事件总线。
//
// OnEntryCreated / OnFeedFetched 是 SSE 的唯一事件来源：
// 少了这两行，实时推送就是一条只会发心跳的死管道。
func (a *App) newRSSService() *rss.Service {
	cfg := a.cfg
	return rss.NewService(a.db, a.blobs, rss.Config{
		UserAgent:         cfg.RSS.UserAgent,
		RequestTimeout:    cfg.RSS.RequestTimeout.Duration,
		FetchTimeout:      cfg.RSS.FetchTimeout.Duration,
		MaxFeedBytes:      cfg.RSS.MaxFeedBytes,
		MaxEntryBytes:     cfg.RSS.MaxEntryBytes,
		MaxBlobBytes:      cfg.Storage.MaxBlobBytes,
		AllowPrivateHosts: cfg.RSS.AllowPrivateHosts,
		RefererMode:       cfg.RSS.Referer,
		MediaConcurrency:  cfg.RSS.MediaConcurrency,
		FetchConcurrency:  cfg.RSS.FetchConcurrency,
		DefaultInterval:   cfg.RSS.DefaultInterval.Duration,
		MinInterval:       cfg.RSS.MinInterval.Duration,
		MaxInterval:       cfg.RSS.MaxInterval.Duration,
		OnEntryCreated: func(entryID, feedID int64, title string) {
			a.bus.Publish("entry.created", map[string]any{
				"entry_id": entryID, "feed_id": feedID, "title": title,
			})
		},
		OnFeedFetched: func(feedID int64, status, newEntries int, errMsg string) {
			a.bus.Publish("feed.updated", map[string]any{
				"feed_id": feedID, "status": status,
				"new_entries": newEntries, "error": errMsg,
			})
		},
	}, a.log)
}

func (a *App) setupScheduler() error {
	a.sched = scheduler.New(a.db.Job(), loadLocation(a.cfg.Cleanup.Timezone), a.log)
	a.sched.SetOnFinish(func(job, status, detail string) {
		a.bus.Publish("job.finished", map[string]any{
			"job": job, "status": status, "detail": detail,
		})
	})
	jobs := &Jobs{
		Store:   a.db,
		Blobs:   a.blobs,
		RSS:     a.rss,
		GCGrace: a.cfg.Storage.GCGrace.Duration,
		CatchUp: a.cfg.Cleanup.CatchUp,
		Vacuum: func(ctx context.Context, full bool) (int64, error) {
			return a.db.Vacuum(ctx, full)
		},
	}
	a.sched.Register(scheduler.Task{Name: "paste-expired", Run: jobs.PasteExpired})
	a.sched.Register(scheduler.Task{Name: "session-expired", Run: jobs.SessionExpired})
	a.sched.Register(scheduler.Task{Name: "rss-prune", Run: jobs.RSSPrune})
	a.sched.Register(scheduler.Task{Name: "media-retry", Run: jobs.MediaRetry})
	a.sched.Register(scheduler.Task{Name: "blob-gc", Run: jobs.BlobGC})
	a.sched.Register(scheduler.Task{Name: "db-vacuum", Run: jobs.DBVacuum})
	a.sched.Register(scheduler.Task{Name: "job-log-prune", Run: jobs.JobLogPrune})

	plans := make([]scheduler.Plan, 0, len(a.cfg.Cleanup.Jobs))
	for _, j := range a.cfg.Cleanup.Jobs {
		plans = append(plans, scheduler.Plan{
			Name:     j.Name,
			Schedule: j.Schedule,
			Timeout:  j.Timeout.Duration,
			Enabled:  j.IsEnabled(),
			Params:   j.Params,
		})
	}
	if err := a.sched.Apply(plans); err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	return nil
}

// startServers 构造路由、启动调度器与轮询器，然后开始监听。
func (a *App) startServers(opts Options) error {
	router := httpapi.NewRouter(httpapi.Deps{
		Cfg:         a.cfg,
		Store:       a.db,
		Blobs:       a.blobs,
		Auth:        a.auth,
		HubVerifier: a.hub,
		Paste:       a.paste,
		RSS:         a.rss,
		Scheduler:   a.sched,
		Bus:         a.bus,
		Limiter:     a.limiter,
		Log:         a.log,
		Build:       opts.Build,
		Started:     a.started,
		WebFS:       opts.WebFS,
		DevMode:     opts.DevMode,
	})

	a.server = &http.Server{
		Addr:    a.cfg.Server.Addr,
		Handler: router,
		// 读超时防慢速攻击；写超时必须为 0，否则 SSE 会被周期性切断。
		// ReadTimeout 不影响 SSE，依据见 serverReadTimeout 的注释。
		ReadHeaderTimeout: a.cfg.Server.ReadHeaderTimeout.Duration,
		IdleTimeout:       a.cfg.Server.IdleTimeout.Duration,
		WriteTimeout:      0,
		ReadTimeout:       serverReadTimeout,
		// 请求 context 携带后台任务组：handler 触发的、比请求活得久的
		// goroutine 经它登记，关停时在关库之前取消并等待。
		BaseContext: func(net.Listener) context.Context {
			return httpapi.WithBackground(context.Background(), a.bg)
		},
	}

	// 先显式 bind 再 Serve。
	//
	// ListenAndServe 把 bind 和 serve 揉在一起，bind 的失败只能在后台
	// goroutine 里经 srvErr 传回来。而下面几行就要发 READY=1——沿用旧写法
	// 的话，端口被占时进程会先朝 systemd 发一条 READY=1 再退出：
	// systemd 认为服务就绪，实际上没有任何端口在监听。
	// 同步 bind 之后，READY 只可能出现在「确实绑定成功」的路径上。
	ln, err := net.Listen("tcp", a.cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", a.cfg.Server.Addr, err)
	}

	a.srvErr = make(chan error, 1)
	go func() {
		if err := a.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.srvErr <- err
		}
	}()

	a.startBackgroundWorkers(a.rootCtx)

	a.log.Info("http server listening",
		"addr", a.cfg.Server.Addr,
		"public_url", a.cfg.Server.PublicURL,
		"version", opts.Build.Version)

	// 端口已绑定、迁移与依赖装配全部完成，此刻才算「可以接流量」。
	if err := systemd.Ready(); err != nil {
		a.log.Warn("sd_notify ready failed", "err", err)
	}
	if err := systemd.Status("serving on %s", a.cfg.Server.Addr); err != nil {
		a.log.Warn("sd_notify status failed", "err", err)
	}
	return nil
}

// startBackgroundWorkers 启动轮询器、调度器与启动补跑。
func (a *App) startBackgroundWorkers(ctx context.Context) {
	cfg := a.cfg

	// 定时抓取订阅。没有它，next_fetch_at 会被写入却从没人消费，
	// 订阅永远停在首次推送时的内容。
	if a.rss != nil {
		poller := newFeedPoller(a.db, func(ctx context.Context, id int64) error {
			_, err := a.rss.Fetch(ctx, id)
			return err
		}, cfg.RSS.FetchConcurrency, a.log)
		a.goWorker(func() { poller.Run(ctx) })
		a.log.Info("feed poller started",
			"interval", pollInterval.String(),
			"concurrency", cfg.RSS.FetchConcurrency)
	}

	if a.sched == nil {
		return
	}
	a.sched.Start()

	// 启动补跑：cron 随进程启停，downtime 期间的触发点全部丢失。
	// 没有这一步，一台每天重启的机器上「凌晨 4 点」的任务永远不会执行。
	if !cfg.Cleanup.CatchUp {
		return
	}
	a.goWorker(func() {
		// 延迟 30 秒：先让服务可用，再跑清理。
		select {
		case <-time.After(30 * time.Second):
		case <-ctx.Done():
			return
		}
		ran := a.sched.CatchUp(ctx, time.Now())
		if len(ran) > 0 {
			a.log.Info("startup catch-up completed", "jobs", ran)
		}
	})
}

// goWorker 在 rootCtx 生命周期内启动一个受 workers 跟踪的 goroutine。
func (a *App) goWorker(fn func()) {
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		fn()
	}()
}

// waitWorkers 等待 rootCtx 下的 goroutine 返回，超出 d 后放弃并返回 false。
func (a *App) waitWorkers(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		a.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// closeInfra 回收基础设施。只用于启动失败时的清理；
// 正常关闭走 shutdown，那条路径还会处理 HTTP 与调度器。
//
// 正常路径下 shutdown 已经关过库并把 a.db 置 nil，这里不会重复关闭。
func (a *App) closeInfra() {
	if a.db != nil {
		if err := a.db.Close(); err != nil {
			a.log.Warn("close database", "err", err)
		}
	}
}

// shutdown 按序关闭。先取消 SSE，再停 HTTP，否则 Shutdown 会一直
// 等着那些长连接直到超时。
func (a *App) shutdown() {
	// 先声明进入关闭流程：`systemctl status` 会显示这条状态，
	// 运维看到的是「正在关闭」而不是一次莫名其妙的连接中断。
	if err := systemd.Stopping(); err != nil {
		a.log.Warn("sd_notify stopping failed", "err", err)
	}
	if err := systemd.Status("shutting down"); err != nil {
		a.log.Warn("sd_notify status failed", "err", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()

	// 顺序：
	//  1. 取消根 context：轮询器停止取新任务，在途 fetch 与启动补跑收到取消；
	//  2. 停调度器：取消并等待运行中的清理任务；
	//  3. 关事件总线，再 Shutdown HTTP（等在途请求）；
	//  4. 取消并等待请求触发的后台任务（必须在 HTTP Shutdown 之后，
	//     否则 Shutdown 期间仍在跑的请求还能登记新任务）；
	//  5. 等轮询器 / 补跑 goroutine 返回；
	//  6. 最后才关数据库——此时所有后台写都已返回或已放弃等待。
	a.rootCancel()

	if a.sched != nil {
		a.sched.Stop(10 * time.Second)
	}

	// 主动断开 SSE：http.Server.Shutdown 不会取消在途请求的 context，
	// 不先关掉订阅通道，长连接会一直把 Shutdown 拖到 15 秒超时。
	// 放在 Shutdown 之前，顺序不能反。
	if a.bus != nil {
		a.bus.Close()
	}

	if a.server != nil {
		// 给在途请求 15 秒；SSE 连接由客户端断开或进程退出终止。
		sctx, scancel := context.WithTimeout(ctx, 15*time.Second)
		defer scancel()
		if err := a.server.Shutdown(sctx); err != nil {
			a.log.Warn("http shutdown incomplete", "err", err)
		}
	}

	if a.bg != nil {
		if err := a.bg.Shutdown(ctx); err != nil {
			a.log.Warn("background tasks did not finish before shutdown budget", "err", err)
		}
	}

	if !a.waitWorkers(remaining(ctx)) {
		a.log.Warn("background workers did not finish before shutdown budget")
	}

	if a.db != nil {
		if err := a.db.Close(); err != nil {
			a.log.Warn("close database", "err", err)
		}
		a.db = nil
	}
	a.log.Info("shutdown complete")
}

// remaining 返回 ctx 截止前的剩余时间；至少 1 秒，给已取消的任务留出返回的余地。
func remaining(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > time.Second {
			return d
		}
	}
	return time.Second
}

func toSet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}

func loadLocation(name string) *time.Location {
	if name == "" || name == "Local" {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	return loc
}
