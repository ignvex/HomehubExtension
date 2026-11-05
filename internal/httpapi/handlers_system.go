package httpapi

import (
	"net/http"
	"runtime"
	"strings"
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
// 卡片列表就在这个函数里逐项追加，不是一个可插拔的注册表：
// 新增一个模块要同时改两处——这里加一张卡，前端 app.ts 的路由表
// 加对应的一条。只改这里的话，卡片会指向 404。
//
// 放在服务端的好处是 enabled 能跟着配置走，前端不必重新发布。
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
			// 不回显剩余字节数：/readyz 不鉴权，磁盘容量属于运维信息，
			// 探针只需要就绪与否。具体数字登录后看 /system/stats。
			checks["storage"] = "ok"
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

// handleInfo 返回构建与运行信息。
//
// 路由挂在 OptionalAuth 下，匿名也能访问。版本、commit、Go 版本与
// schema 版本对攻击者是现成的指纹（对照已知漏洞版本），匿名时只返回
// 前端渲染必需的 public_url；登录后行为不变。
func (s *server) handleInfo(c *gin.Context) {
	if clientFrom(c) == nil {
		c.JSON(http.StatusOK, gin.H{"public_url": s.Cfg.Server.PublicURL})
		return
	}
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
	if !loggedIn {
		// 匿名只给首页概览需要的四个计数（web/src/views/home.ts）。
		// blob 数量与字节、待处理媒体、磁盘容量是运维信息，
		// 只有登录后的设置页才用得到。
		c.JSON(http.StatusOK, gin.H{
			"pastes":  myPastes,
			"feeds":   0,
			"entries": 0,
			"unread":  0,
		})
		return
	}

	out := gin.H{
		"pastes":        myPastes,
		"feeds":         stats.Feeds,
		"entries":       stats.Entries,
		"unread":        stats.Unread,
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
		// 不回显 err.Error()：任务执行失败的原因可能带路径、SQL 或
		// 外部地址。已知的两类调用方错误给固定文案，其余只进日志，
		// 详情可在任务历史里查看。
		//
		// scheduler 没有导出哨兵错误，这里按 RunByName 的错误文案判别；
		// 改动那边的措辞时要同步这里（system_test.go 有用例兜底）。
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, "unknown job "):
			failWith(c, http.StatusNotFound, "not_found", "任务不存在")
		case strings.HasSuffix(msg, " is already running"):
			failWith(c, http.StatusConflict, "conflict", "任务正在运行")
		default:
			logFrom(c).Error("manual job run failed", "job", name, "err", err)
			failWith(c, http.StatusInternalServerError, "internal", "任务执行失败")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": name, "detail": detail})
}

// handleConfig 返回脱敏后的生效配置。密钥路径只显示"(configured)"。
func (s *server) handleConfig(c *gin.Context) {
	c.JSON(http.StatusOK, s.Cfg.Redacted())
}
