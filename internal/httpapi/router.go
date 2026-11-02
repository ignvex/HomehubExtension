package httpapi

import (
	"io/fs"
	"log/slog"
	"time"

	"github.com/ignvex/HomehubExtension/internal/auth"
	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/config"
	"github.com/ignvex/HomehubExtension/internal/events"
	"github.com/ignvex/HomehubExtension/internal/paste"
	"github.com/ignvex/HomehubExtension/internal/rss"
	"github.com/ignvex/HomehubExtension/internal/scheduler"
	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/gin-gonic/gin"
)

// BuildInfo 由 -ldflags 注入，开发时保持零值。
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// Deps 是路由层的全部依赖。nil 字段表示对应模块未启用。
type Deps struct {
	Cfg         config.Config
	Store       store.Store
	Blobs       *blob.Store
	Auth        *auth.Service
	HubVerifier *auth.HubVerifier
	Paste       *paste.Service
	RSS         *rss.Service
	Scheduler   *scheduler.Scheduler
	Bus         *events.Bus
	// Limiter 限制认证端点的失败次数。nil 表示不限制（仅测试环境）。
	Limiter *auth.AttemptLimiter
	Log     *slog.Logger
	Build   BuildInfo
	Started time.Time
	WebFS   fs.FS
	DevMode bool
}

type server struct {
	Deps
	spa *SPA
}

// NewRouter 组装 gin engine。
//
// 用 gin.New() 而不是 gin.Default()：后者的日志格式与 slog 冲突，
// Recovery 也不符合统一错误结构。
func NewRouter(d Deps) *gin.Engine {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.HandleMethodNotAllowed = true
	r.MaxMultipartMemory = 1 << 20

	// 只信任回环：Caddy 就在本机，其余来源的 X-Forwarded-For 不可信。
	_ = r.SetTrustedProxies(d.Cfg.Server.TrustedProxies)

	s := &server{Deps: d, spa: NewSPA(d.WebFS, d.DevMode)}

	r.Use(RequestID())
	r.Use(Recovery(d.Log))
	r.Use(AccessLog(d.Log, d.Cfg.Log.AccessLog))
	r.Use(SecurityHeaders(d.DevMode))
	r.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		if c.Request.ContentLength > d.Cfg.Server.MaxBodyBytes && d.Cfg.Server.MaxBodyBytes > 0 {
			failWith(c, 413, "payload_too_large", "请求体过大")
			return
		}
		c.Next()
	})

	r.NoMethod(func(c *gin.Context) {
		failWith(c, 405, "method_not_allowed", "不支持该请求方法")
	})

	s.registerProbes(r)
	s.registerAuth(r)
	s.registerHub(r)
	s.registerPaste(r)
	s.registerRSS(r)
	s.registerSystem(r)
	s.registerEvents(r)

	// 所有未命中路由的请求交给 SPA。保留路径在 SPA 内部返回 JSON 404。
	r.NoRoute(s.spa.Handler())
	r.GET("/", s.spa.Handler())

	return r
}

func (s *server) registerProbes(r *gin.Engine) {
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	r.GET("/readyz", s.handleReady)
}

func (s *server) registerAuth(r *gin.Engine) {
	// 三个接受未认证输入的端点都过限流。
	// me/logout/refresh 不需要：它们要么已认证，要么依赖 cookie。
	g := r.Group("/api/v1/auth")
	limited := RequireAttemptLimit(s.Limiter, s.Log)
	g.POST("/enroll", limited, s.handleEnroll)
	g.POST("/challenge", limited, s.handleChallenge)
	g.POST("/token", limited, s.handleToken)
	// 密钥兑换也要限流：它是一把长期共享凭证，一旦泄露就是可以
	// 反复尝试的口令。99 bit 熵下爆破不现实，但限流能让"某个 IP
	// 在狂试"变成一个看得见的信号，而不是静默的噪音。
	g.POST("/key", limited, s.handleKeyLogin)
	g.POST("/refresh", s.handleRefresh)
	g.POST("/logout", s.handleLogout)
	g.GET("/server-key", s.handleServerKey)
	g.GET("/me", RequireAuth(s.Auth, false), s.handleMe)

	cl := r.Group("/api/v1/clients", RequireAuth(s.Auth, false), RequireCSRF())
	cl.GET("", s.handleListClients)
	cl.DELETE("/:id", s.handleRevokeClient)
}

// registerHub 是 Homehub 宿主调用的入站接口。
//
// 全部走请求签名而不是 Bearer：这是机器对机器的通道，Homehub 持有私钥，
// Extension 只存公钥。签名把 method/path/query/body 都覆盖进去，
// 一条合法请求无法被挪到另一个端点重放。
func (s *server) registerHub(r *gin.Engine) {
	signed := r.Group("/api/v1/hub",
		RequireHubSignature(s.HubVerifier, s.Cfg.Server.MaxBodyBytes))

	// 订阅通道额外要求 RSS 模块启用：模块关掉时四个端点统一 404，
	// 宿主不必逐个猜哪个还活着。
	subs := signed.Group("", RequireRSSEnabled(s.Cfg.RSS.Enabled, s.RSS))
	subs.POST("/subscriptions", s.handleHubUpsert)
	subs.GET("/subscriptions", s.handleHubList)
	subs.DELETE("/subscriptions/:id", s.handleHubDelete)
	subs.POST("/subscriptions/:id/refresh", s.handleHubRefresh)

	// 批量对账：宿主启动时一次推送几百条订阅，逐个 POST 就是几百个
	// 签名验算与往返。注册在 :id 之前，避免 batch 被当成 id。
	subs.POST("/subscriptions/batch", s.handleHubBatch)

	// 宿主读内容：没有这组，Homehub 只能推订阅、看状态，
	// 无法自己渲染阅读界面。
	subs.GET("/entries", s.handleHubEntries)
	subs.GET("/stats", s.handleHubStats)

	// 密钥签发只依赖签名，与 RSS 是否启用无关：
	// 它是一个纯粹的凭证管理操作，跟订阅通道没有业务关系。
	signed.POST("/keys", s.handleHubCreateKey)
	signed.GET("/keys", s.handleHubListKeys)
	signed.DELETE("/keys/:id", s.handleHubRevokeKey)
}

// registerPaste 挂载 pastebin。
//
// 整组不要求登录：匿名也能创建、查看与管理自己的片段。
// 归属靠 X-Owner-Token（匿名）或 Bearer（已登录）推导，
// 见 middleware.OwnerHash。写操作仍然过 CSRF——那是防止跨站
// 借用浏览器 cookie 的，与"是否登录"无关。
func (s *server) registerPaste(r *gin.Engine) {
	if !s.Cfg.Pastebin.Enabled {
		return
	}

	g := r.Group("/api/v1/pastes", OptionalAuth(s.Auth), RequireCSRF())
	g.POST("", s.handlePasteCreate)
	g.GET("", s.handlePasteList)
	g.GET("/:slug", s.handlePasteGet)
	g.PUT("/:slug", s.handlePasteUpdate)
	g.DELETE("/:slug", s.handlePasteDelete)

	// 公开分享页不鉴权：分享链接必须能被 curl 与爬虫读取。
	r.GET("/p/:slug", OptionalAuth(s.Auth), s.handlePublicPaste)
}

func (s *server) registerRSS(r *gin.Engine) {
	if !s.Cfg.RSS.Enabled {
		return
	}
	authn := RequireAuth(s.Auth, false)
	csrf := RequireCSRF()

	r.GET("/media/:name", s.handleMedia)

	// 订阅源管理需要登录：抓取会主动访问外部地址，
	// 匿名开放等于把服务变成开放的 SSRF 跳板。
	g := r.Group("/api/v1", authn, csrf)
	g.POST("/feeds", s.handleFeedCreate)
	g.GET("/feeds", s.handleFeedList)

	// OPML 迁移端点。注册在 /feeds/:id 之前：gin 的路由树里静态段
	// 优先于参数，顺序不影响匹配，但放前面读起来更清楚——
	// 否则容易以为 /feeds/export 会被 :id 吃掉。
	g.GET("/feeds/export", s.handleFeedExport)
	g.POST("/feeds/import", s.handleFeedImport)

	g.GET("/feeds/:id", s.handleFeedGet)
	g.PATCH("/feeds/:id", s.handleFeedUpdate)
	g.POST("/feeds/:id/refresh", s.handleFeedRefresh)
	g.DELETE("/feeds/:id", s.handleFeedDelete)

	g.GET("/entries", s.handleEntryList)
	g.PATCH("/entries", s.handleEntryBulk)
	g.GET("/entries/:id", s.handleEntryGet)
	g.PATCH("/entries/:id", s.handleEntryPatch)
	g.GET("/entries/:id/original", s.handleEntryOriginal)
}

func (s *server) registerSystem(r *gin.Engine) {
	authn := RequireAuth(s.Auth, false)
	csrf := RequireCSRF()

	// 公开端点：工具台首页要在未登录时就能渲染，
	// 否则用户看到的第一个页面是登录墙，不知道这服务能干什么。
	//
	// 用 OptionalAuth 而不是完全跳过鉴权：有凭证时要能算出「你的」
	// 片段数，否则角标会显示全局计数而列表是空的。
	pub := r.Group("/api/v1", OptionalAuth(s.Auth))
	pub.GET("/apps", s.handleApps)
	pub.GET("/system/info", s.handleInfo)
	pub.GET("/system/stats", s.handleStats)

	// 其余保持鉴权：配置视图、任务历史、手动触发清理。
	g := r.Group("/api/v1", authn)
	g.GET("/system/jobs", s.handleJobs)
	g.GET("/system/config", s.handleConfig)

	w := r.Group("/api/v1", authn, csrf)
	w.POST("/system/jobs/:name/run", s.handleRunJob)
	w.POST("/prefs/theme", ThemeCookie)
}

func (s *server) registerEvents(r *gin.Engine) {
	// SSE 用 cookie 认证：EventSource 无法设置 Authorization 头。
	r.GET("/api/v1/events/stream", RequireAuth(s.Auth, true), s.handleEvents)
}
