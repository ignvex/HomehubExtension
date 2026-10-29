package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/gin-gonic/gin"
)

// hubUpsertRequest 是 Homehub 推送订阅的载荷。
//
// Homehub 是主动方：它知道用户在宿主的哪个角落订阅了什么，把地址与
// 抓取间隔一起推过来。Extension 只负责抓取、本地化与呈现。
type hubUpsertRequest struct {
	URL      string `json:"url" binding:"required"`
	Title    string `json:"title"`
	Interval string `json:"interval"`
	Enabled  *bool  `json:"enabled"`
	FetchNow *bool  `json:"fetch_now"`
}

type hubSubscription struct {
	ID            int64  `json:"id"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	SiteURL       string `json:"site_url"`
	Enabled       bool   `json:"enabled"`
	FetchInterval string `json:"fetch_interval"`
	NextFetchAt   *int64 `json:"next_fetch_at"`
	LastFetchAt   *int64 `json:"last_fetch_at"`
	LastStatus    int    `json:"last_status"`
	LastError     string `json:"last_error"`
	ErrorCount    int    `json:"error_count"`
	UnreadCount   int64  `json:"unread_count"`
	CreatedAt     *int64 `json:"created_at"`
}

func toHubSubscription(f *store.Feed) hubSubscription {
	return hubSubscription{
		ID:            f.ID,
		URL:           f.URL,
		Title:         f.Title,
		SiteURL:       f.SiteURL,
		Enabled:       f.Enabled,
		FetchInterval: (time.Duration(f.FetchInterval) * time.Second).String(),
		NextFetchAt:   optTS(f.NextFetchAt),
		LastFetchAt:   optTS(f.LastFetchAt),
		LastStatus:    f.LastStatus,
		LastError:     f.LastError,
		ErrorCount:    f.ErrorCount,
		UnreadCount:   f.UnreadCount,
		CreatedAt:     ts(f.CreatedAt),
	}
}

// handleHubUpsert 幂等：同一个 URL 重复推送只更新间隔与启用状态，
// 不会重复建订阅，也不会清空已经学到的标题与站点地址。
func (s *server) handleHubUpsert(c *gin.Context) {
	if !s.Cfg.RSS.Enabled {
		failWith(c, http.StatusNotFound, "not_found", "RSS 模块未启用")
		return
	}
	var req hubUpsertRequest
	if !bindJSON(c, &req) {
		return
	}

	interval := s.Cfg.RSS.DefaultInterval.Duration
	if req.Interval != "" {
		d, err := time.ParseDuration(req.Interval)
		if err != nil {
			failWith(c, http.StatusBadRequest, "invalid_request", "interval 格式不正确",
				FieldError{Field: "interval", Reason: `需要形如 "30m" 的时长`})
			return
		}
		interval = d
	}
	// 下限保护站点，上限防止一个源被无限期冷落。
	if interval < s.Cfg.RSS.MinInterval.Duration {
		interval = s.Cfg.RSS.MinInterval.Duration
	}
	if interval > s.Cfg.RSS.MaxInterval.Duration {
		interval = s.Cfg.RSS.MaxInterval.Duration
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	now := time.Now()
	// 首推时 next_fetch_at 设为一个间隔之后，而不是「立刻到期」。
	//
	// 下面的异步抓取已经覆盖了"马上抓一次"的需求；如果这里也设成 now，
	// poller 在 30 秒内扫到它就会重复抓一遍，浪费目标站的带宽。
	next := now.Add(interval).Unix()
	feed := &store.Feed{
		URL:           req.URL,
		Title:         req.Title,
		Source:        "hub",
		Enabled:       enabled,
		FetchInterval: int64(interval / time.Second),
		NextFetchAt:   &next,
		CreatedAt:     now.Unix(),
	}

	saved, created, err := s.Store.Feed().Upsert(c.Request.Context(), feed)
	if err != nil {
		fail(c, err)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}

	// 首推默认立即抓一次，让 Homehub 侧不必再等一个周期。
	// RSS 服务为 nil 说明模块未装配（配置禁用或测试环境），跳过即可。
	if s.RSS != nil && (req.FetchNow == nil || *req.FetchNow) {
		go func(id int64) {
			// 用独立 context：请求返回后仍要跑完，否则首页会一直是空的。
			ctx, cancel := contextWithTimeout(5 * time.Minute)
			defer cancel()
			if _, err := s.RSS.Fetch(ctx, id); err != nil {
				s.Log.Warn("initial fetch failed", "feed_id", id, "err", err)
			}
		}(saved.ID)
	}

	c.JSON(status, toHubSubscription(&saved))
}

func (s *server) handleHubList(c *gin.Context) {
	feeds, err := s.Store.Feed().List(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]hubSubscription, 0, len(feeds))
	for i := range feeds {
		out = append(out, toHubSubscription(&feeds[i]))
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (s *server) handleHubDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "id 必须是整数")
		return
	}
	if err := s.Store.Feed().Delete(c.Request.Context(), id); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *server) handleHubRefresh(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "id 必须是整数")
		return
	}
	ctx, cancel := contextWithTimeout(2 * time.Minute)
	defer cancel()
	res, err := s.RSS.Fetch(ctx, id)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   gin.H{"code": "upstream_error", "message": err.Error()},
			"feed_id": id,
		})
		return
	}
	c.JSON(http.StatusOK, res)
}
