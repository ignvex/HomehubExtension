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
	g := r.Group("/api/v1/hub",
		RequireHubSignature(s.HubVerifier, s.Cfg.Server.MaxBodyBytes))
	g.POST("/subscriptions", s.handleHubUpsert)
	g.GET("/subscriptions", s.handleHubList)
	g.DELETE("/subscriptions/:id", s.handleHubDelete)
	g.POST("/subscriptions/:id/refresh", s.handleHubRefresh)
}

func (s *server) registerPaste(r *gin.Engine) {
	if !s.Cfg.Pastebin.Enabled {
		return
	}
	authn := RequireAuth(s.Auth, false)
	csrf := RequireCSRF()

	g := r.Group("/api/v1/pastes", authn, csrf)
	g.POST("", s.handlePasteCreate)
	g.GET("", s.handlePasteList)
	g.GET("/:slug", s.handlePasteGet)
	g.PUT("/:slug", s.handlePasteUpdate)
	g.DELETE("/:slug", s.handlePasteDelete)

	// 公开分享页不鉴权：分享链接必须能被 curl 与爬虫读取。
	r.GET("/p/:slug", s.handlePublicPaste)
}

func (s *server) registerRSS(r *gin.Engine) {
	if !s.Cfg.RSS.Enabled {
		return
	}
	authn := RequireAuth(s.Auth, false)
	csrf := RequireCSRF()

	r.GET("/media/:name", s.handleMedia)

	g := r.Group("/api/v1", authn, csrf)
	g.GET("/feeds", s.handleFeedList)
	g.GET("/feeds/:id", s.handleFeedGet)
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

	g := r.Group("/api/v1", authn)
	g.GET("/system/info", s.handleInfo)
	g.GET("/system/stats", s.handleStats)
	g.GET("/system/jobs", s.handleJobs)
	g.GET("/system/config", s.handleConfig)
	g.GET("/apps", s.handleApps)

	w := r.Group("/api/v1", authn, csrf)
	w.POST("/system/jobs/:name/run", s.handleRunJob)
	w.POST("/prefs/theme", ThemeCookie)
}

func (s *server) registerEvents(r *gin.Engine) {
	// SSE 用 cookie 认证：EventSource 无法设置 Authorization 头。
	r.GET("/api/v1/events/stream", RequireAuth(s.Auth, true), s.handleEvents)
}
