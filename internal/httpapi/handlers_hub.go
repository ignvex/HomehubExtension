package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
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
		FetchInterval: formatInterval(f.FetchInterval),
		NextFetchAt:   optTS(f.NextFetchAt),
		LastFetchAt:   optTS(f.LastFetchAt),
		LastStatus:    f.LastStatus,
		LastError:     f.LastError,
		ErrorCount:    f.ErrorCount,
		UnreadCount:   f.UnreadCount,
		CreatedAt:     ts(f.CreatedAt),
	}
}

// handleHubUpsert 幂等：同一个 URL 重复推送只更新它说了的字段。
//
// 三个字段的省略语义必须分清：
//   - title / site_url 为空表示「没说」，由 SQL 里的 CASE 保留原值；
//   - interval 省略沿用已存的间隔，新建时用 default_interval；
//   - enabled 省略沿用已存的状态，新建时默认 true。
//
// 后两条如果直接回落默认值，宿主一次「只改标题」的推送就会把用户
// 显式暂停的订阅重新启用，并把抓取间隔重置回 30 分钟。
func (s *server) handleHubUpsert(c *gin.Context) {
	var req hubUpsertRequest
	if !bindJSON(c, &req) {
		return
	}

	ctx := c.Request.Context()

	// 入库前校验地址，与网页端新建订阅保持同一条规则。
	//
	// 宿主通道有签名保护，不是未授权的 SSRF 入口；但宿主推来的 URL
	// 往往来自用户输入（用户在宿主里粘什么就推什么），而抓取时
	// 连接会被 Transport 拦住——于是内网地址会入库、每次抓取都失败、
	// error_count 累积到被自动停用。在入口就拒绝比让它入库后
	// 反复失败更早、也更好排查。
	if err := s.RSS.ValidateURL(req.URL); err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "订阅地址不可用",
			FieldError{Field: "url", Reason: err.Error()})
		return
	}

	existing, err := s.Store.Feed().GetByURL(ctx, req.URL)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(c, err)
		return
	}

	interval, ok := s.resolveInterval(c, req.Interval, existing)
	if !ok {
		return
	}

	feed := planUpsert(req, existing, interval, time.Now())

	saved, created, err := s.Store.Feed().Upsert(ctx, feed)
	if err != nil {
		fail(c, err)
		return
	}

	// 从「连续失败被自动停用」恢复时清零失败计数。
	//
	// 只把 enabled 改回 true 没有意义：error_count 还是 10，下一次
	// 抓取一失败就变成 11，立刻又被停用。宿主看到的是「刚恢复就又停了」，
	// 而这种源往往正好是抓不动的那个。
	if req.Enabled != nil && *req.Enabled && existing != nil && !existing.Enabled {
		s.reviveFeed(ctx, &saved)
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}

	// 首推默认立即抓一次，让 Homehub 侧不必再等一个周期。
	if req.FetchNow == nil || *req.FetchNow {
		s.kickInitialFetch(ctx, saved.ID)
	}

	c.JSON(status, toHubSubscription(&saved))
}

// planUpsert 根据推送内容与已存状态算出要写入的 feed。
//
// 三个字段的省略语义必须分清：
//   - title / site_url 为空表示「没说」，由 SQL 里的 CASE 保留原值；
//   - enabled 省略沿用已存状态，新建时默认 true；
//   - next_fetch_at 只在间隔真变时才重排。
//
// 最后一条容易漏：只改标题的推送如果也重排，会把下一次抓取整段
// 推迟一个周期；反过来，间隔变了却不重排，宿主调小间隔后要等
// 旧周期（最长 max_interval）才生效。
func planUpsert(req hubUpsertRequest, existing *store.Feed, interval time.Duration, now time.Time) *store.Feed {
	enabled := true
	if existing != nil {
		enabled = existing.Enabled
	}
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	var next *int64
	if existing != nil {
		next = existing.NextFetchAt
	}
	if existing == nil || existing.FetchInterval != int64(interval/time.Second) {
		v := now.Add(interval).Unix()
		next = &v
	}

	return &store.Feed{
		URL:           req.URL,
		Title:         req.Title,
		Source:        "hub",
		Enabled:       enabled,
		FetchInterval: int64(interval / time.Second),
		NextFetchAt:   next,
		CreatedAt:     now.Unix(),
	}
}

// reviveFeed 在宿主显式重新启用一条被自动停用的订阅时清零失败计数。
func (s *server) reviveFeed(ctx context.Context, saved *store.Feed) {
	saved.ErrorCount = 0
	saved.LastError = ""
	if err := s.Store.Feed().Update(ctx, saved); err != nil {
		s.Log.Warn("reset error count after revival", "feed_id", saved.ID, "err", err)
	}
}

// kickInitialFetch 在请求返回后异步抓一次。
//
// 不能用请求 context：请求结束后仍要跑完，否则宿主推完第一次就再也
// 看不到内容。但也不能完全脱离进程生命周期——关停时它必须被取消并
// 等它返回，否则会在数据库关闭之后继续写库。所以 goroutine 交给
// 请求 context 里携带的 Background（由 app 经 http.Server.BaseContext
// 注入）管理；没有注入时（测试）退回脱离取消的请求 context。
func (s *server) kickInitialFetch(reqCtx context.Context, feedID int64) {
	run := func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if _, err := s.RSS.Fetch(ctx, feedID); err != nil {
			s.Log.Warn("initial fetch failed", "feed_id", feedID, "err", err)
		}
	}
	if bg := BackgroundFrom(reqCtx); bg != nil {
		if !bg.Go(run) {
			s.Log.Info("initial fetch skipped, shutting down", "feed_id", feedID)
		}
		return
	}
	// 没有注入 Background（测试）：脱离请求取消，但保留请求上的值。
	go run(context.WithoutCancel(reqCtx))
}

// Background 跟踪请求触发、但比请求活得更久的后台任务。
//
// 它是 httpapi 与 app 之间的关停契约：app 用 NewBackground 创建，
// 经 WithBackground 放进 http.Server.BaseContext，关停时在关闭数据库
// 之前调用 Shutdown——取消全部任务并等待它们返回。
// 走 context 而不是 Deps 字段，是为了不改动路由层的依赖结构。
type Background struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	wg      sync.WaitGroup
	stopped bool
}

// NewBackground 构造一个派生自 parent 的后台任务组。
func NewBackground(parent context.Context) *Background {
	ctx, cancel := context.WithCancel(parent)
	return &Background{ctx: ctx, cancel: cancel}
}

// Go 在后台运行 fn，fn 收到的 context 在 Shutdown 时取消。
// 已进入关停时不再启动，返回 false。
func (b *Background) Go(fn func(ctx context.Context)) bool {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return false
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		fn(b.ctx)
	}()
	return true
}

// Shutdown 取消全部后台任务并等待返回；ctx 到期时放弃等待并返回其错误。
func (b *Background) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	b.stopped = true
	b.mu.Unlock()
	b.cancel()

	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type backgroundKey struct{}

// WithBackground 把 b 挂到 ctx 上，供 http.Server.BaseContext 使用。
func WithBackground(ctx context.Context, b *Background) context.Context {
	return context.WithValue(ctx, backgroundKey{}, b)
}

// BackgroundFrom 取出 ctx 上的 Background；没有时返回 nil。
func BackgroundFrom(ctx context.Context) *Background {
	b, _ := ctx.Value(backgroundKey{}).(*Background)
	return b
}

// resolveInterval 决定这条推送用哪个抓取间隔，并夹到配置区间内。
//
// 缺省值取决于是不是第一次推送：新建用 default_interval，更新则沿用
// 原值。返回 false 表示已经写过错误响应。
func (s *server) resolveInterval(c *gin.Context, raw string, existing *store.Feed) (time.Duration, bool) {
	interval := s.Cfg.RSS.DefaultInterval.Duration
	if existing != nil && existing.FetchInterval > 0 {
		interval = time.Duration(existing.FetchInterval) * time.Second
	}
	if raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			failWith(c, http.StatusBadRequest, "invalid_request", "interval 格式不正确",
				FieldError{Field: "interval", Reason: `需要形如 "30m" 的时长`})
			return 0, false
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
	return interval, true
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
	ctx, cancel := refreshContext(c.Request.Context(), 2*time.Minute)
	defer cancel()
	res, err := s.RSS.Fetch(ctx, id)
	if err != nil {
		// 订阅不存在是 404，不是 502：宿主推了一个已被删掉的 id，
		// 重试不会让它变好。
		if errors.Is(err, store.ErrNotFound) {
			fail(c, err)
			return
		}
		// 目标站的问题用 502。走统一错误结构，客户端只认 code——
		// 之前这里手拼了一个缺 request_id、且把错误文本直接回显的 JSON，
		// 是所有端点里的唯一例外。
		logFrom(c).Warn("hub refresh failed", "feed_id", id, "err", err)
		failWith(c, http.StatusBadGateway, "upstream_error", "抓取订阅失败")
		return
	}
	c.JSON(http.StatusOK, res)
}

// ---- 宿主签发的长期访问密钥 ----

type hubKeyRequest struct {
	Name string `json:"name"`
	// ExpiresIn 是时长字符串，"0" 或省略表示永不过期。
	ExpiresIn string `json:"expires_in"`
}

// handleHubCreateKey 签发一把用户可直接使用的登录密钥。
//
// 明文只在这一次响应里出现：库里存的是 sha256，之后任何接口都拿不回它，
// 也拿不回原文的哈希。丢了只能重新签发——与配对码的取舍一致。
func (s *server) handleHubCreateKey(c *gin.Context) {
	var req hubKeyRequest
	if !bindJSON(c, &req) {
		return
	}

	var ttl time.Duration
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil {
			failWith(c, http.StatusBadRequest, "invalid_request", "expires_in 格式不正确",
				FieldError{Field: "expires_in", Reason: `需要形如 "720h" 的时长，省略表示永不过期`})
			return
		}
		if d <= 0 {
			failWith(c, http.StatusBadRequest, "invalid_request", "expires_in 必须为正",
				FieldError{Field: "expires_in", Reason: "省略该字段表示永不过期，不要用 0 或负数表达"})
			return
		}
		ttl = d
	}

	res, err := s.Auth.GenerateAccessKey(c.Request.Context(), req.Name, ttl)
	if err != nil {
		fail(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":         res.Key.ID,
		"name":       res.Key.Name,
		"key":        res.Plain,
		"client_id":  res.Key.ClientID,
		"created_at": ts(res.Key.CreatedAt),
		"expires_at": optTS(res.Key.ExpiresAt),
	})
}

// handleHubListKeys 对账用。永远不含明文，也不含哈希。
func (s *server) handleHubListKeys(c *gin.Context) {
	keys, err := s.Store.Auth().ListAccessKeys(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	now := time.Now().Unix()
	out := make([]gin.H, 0, len(keys))
	for i := range keys {
		k := &keys[i]
		out = append(out, gin.H{
			"id":           k.ID,
			"name":         k.Name,
			"client_id":    k.ClientID,
			"created_at":   ts(k.CreatedAt),
			"expires_at":   optTS(k.ExpiresAt),
			"last_used_at": optTS(k.LastUsedAt),
			"revoked":      k.RevokedAt != nil,
			// live 让宿主不必自己算过期：它只看这一个布尔。
			"live": k.Live(now),
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// handleHubRevokeKey 撤销密钥，并连带踢掉它换出去的会话。
func (s *server) handleHubRevokeKey(c *gin.Context) {
	if err := s.Store.Auth().RevokeAccessKey(
		c.Request.Context(), c.Param("id"), time.Now().Unix()); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
