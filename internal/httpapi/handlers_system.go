package httpapi

import (
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
)

type appBadge struct {
	Count int64  `json:"count"`
	Label string `json:"label"`
}

type appCard struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Icon        string    `json:"icon"`
	Path        string    `json:"path"`
	Enabled     bool      `json:"enabled"`
	Badge       *appBadge `json:"badge"`
}

// handleApps 驱动工具台首页。
//
// 前端不硬编码应用列表：新增一个模块只需要在代码里注册，
// 首页自动多出一张卡。这也是"会有很多本地工具台应用"这条需求的落点。
func (s *server) handleApps(c *gin.Context) {
	stats, err := s.Store.Stats(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}

	// 角标必须与列表口径一致：列表按归属过滤，角标就不能是全局计数。
	// 否则用户会看到「首页说 2 条、列表是空的」这种自相矛盾的界面。
	owner := OwnerHash(c)
	myPastes, err := s.Store.Paste().CountByOwner(c.Request.Context(), owner)
	if err != nil {
		fail(c, err)
		return
	}

	// RSS 数据只有登录用户才看得到，匿名时不要把全局未读数当角标——
	// 那会让匿名用户以为有内容却进不去。
	loggedIn := clientFrom(c) != nil
	unread := stats.Unread
	if !loggedIn {
		unread = 0
	}

	apps := make([]appCard, 0, 4)
	if s.Cfg.Pastebin.Enabled {
		apps = append(apps, appCard{
			ID: "pastebin", Name: "Pastebin", Icon: "paste",
			Description: "自托管文本片段，支持过期、口令与一次性读取",
			Path:        "/pastes", Enabled: true,
			Badge: &appBadge{Count: myPastes, Label: "条"},
		})
	}
	if s.Cfg.RSS.Enabled {
		apps = append(apps, appCard{
			ID: "rss", Name: "RSS 阅读", Icon: "rss",
			Description: "订阅由 Homehub 推送，媒体全部本地化，零第三方请求",
			Path:        "/entries", Enabled: true,
			Badge: &appBadge{Count: unread, Label: "未读"},
		})
	}
	// 开发工具：纯前端计算，不碰服务器数据。
	// 放在 Pastebin 与 RSS 之后——它们是本项目的两个主功能，
	// 工具集是附带的便利。
	apps = append(apps, appCard{
		ID: "tools", Name: "开发工具", Icon: "tools",
		Description: "JSON、正则、Base64、哈希、二维码等，全部在浏览器本地运行",
		Path:        "/tools", Enabled: true,
	})
	apps = append(apps, appCard{
		ID: "system", Name: "系统", Icon: "settings",
		Description: "运行状态、存储占用与清理任务记录",
		Path:        "/settings", Enabled: true,
	})
	apps = append(apps, appCard{
		ID: "feeds", Name: "订阅源", Icon: "list",
		Description: "查看 Homehub 推送的订阅与最近抓取结果",
		Path:        "/feeds", Enabled: s.Cfg.RSS.Enabled,
	})

	c.JSON(http.StatusOK, gin.H{"apps": apps})
}

// handleReady 检查依赖是否就绪。失败原因放在响应体里便于 curl 排查。
func (s *server) handleReady(c *gin.Context) {
	ctx := c.Request.Context()
	checks := gin.H{}
	ready := true

	if err := s.Store.Ping(ctx); err != nil {
		checks["database"] = "error"
		ready = false
	} else {
		checks["database"] = "ok"
	}

	if v, err := s.Store.SchemaVersion(ctx); err != nil {
		checks["schema"] = "error"
		ready = false
	} else {
		checks["schema"] = v
	}

	if s.Blobs != nil {
		space, err := s.Blobs.Space()
		if err != nil {
			checks["storage"] = "error"
		} else if space.Free < s.Cfg.Storage.MinFreeBytes {
			// 磁盘紧张时进入降级：可以读，但拒绝新下载。
			checks["storage"] = "low_disk"
			ready = false
		} else {
			checks["storage"] = "ok"
			checks["free_bytes"] = space.Free
		}
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{
		"ready":          ready,
		"checks":         checks,
		"uptime_seconds": int64(time.Since(s.Started).Seconds()),
	})
}

func (s *server) handleInfo(c *gin.Context) {
	schema, _ := s.Store.SchemaVersion(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"version":        s.Build.Version,
		"commit":         s.Build.Commit,
		"build_time":     s.Build.BuildTime,
		"go_version":     runtime.Version(),
		"schema_version": schema,
		"uptime_seconds": int64(time.Since(s.Started).Seconds()),
		"public_url":     s.Cfg.Server.PublicURL,
	})
}

func (s *server) handleStats(c *gin.Context) {
	stats, err := s.Store.Stats(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}

	// pastes 按归属统计，与列表一致。
	// feeds/entries 只有登录用户看得到，匿名时返回 0 而不是全局数字。
	owner := OwnerHash(c)
	myPastes, err := s.Store.Paste().CountByOwner(c.Request.Context(), owner)
	if err != nil {
		fail(c, err)
		return
	}
	loggedIn := clientFrom(c) != nil
	feeds, entries, unread := stats.Feeds, stats.Entries, stats.Unread
	if !loggedIn {
		feeds, entries, unread = 0, 0, 0
	}

	out := gin.H{
		"pastes":        myPastes,
		"feeds":         feeds,
		"entries":       entries,
		"unread":        unread,
		"blobs":         stats.Blobs,
		"blob_bytes":    stats.BlobBytes,
		"pending_media": stats.PendingMedia,
	}
	if s.Blobs != nil {
		if space, err := s.Blobs.Space(); err == nil {
			out["disk_total"] = space.Total
			out["disk_free"] = space.Free
		}
	}
	c.JSON(http.StatusOK, out)
}

func (s *server) handleJobs(c *gin.Context) {
	runs, err := s.Store.Job().Recent(c.Request.Context(), 50)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"jobs": runs})
}

func (s *server) handleRunJob(c *gin.Context) {
	name := c.Param("name")
	if s.Scheduler == nil {
		failWith(c, http.StatusServiceUnavailable, "unavailable", "调度器未启动")
		return
	}
	detail, err := s.Scheduler.RunByName(c.Request.Context(), name)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": name, "detail": detail})
}

// handleConfig 返回脱敏后的生效配置。密钥路径只显示"(configured)"。
func (s *server) handleConfig(c *gin.Context) {
	c.JSON(http.StatusOK, s.Cfg.Redacted())
}
