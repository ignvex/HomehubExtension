package httpapi

import (
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/paste"

	"github.com/gin-gonic/gin"
)

type pasteResponse struct {
	Slug          string `json:"slug"`
	Title         string `json:"title"`
	Body          string `json:"body,omitempty"`
	Language      string `json:"language"`
	Visibility    string `json:"visibility"`
	SizeBytes     int64  `json:"size_bytes"`
	ExpiresAt     *int64 `json:"expires_at"`
	BurnAfterRead bool   `json:"burn_after_read"`
	HasPassword   bool   `json:"has_password"`
	CreatedAt     *int64 `json:"created_at"`
	UpdatedAt     *int64 `json:"updated_at,omitempty"`
	URL           string `json:"url,omitempty"`
	// OwnerToken 只在创建响应里出现一次，且仅匿名创建时有值。
	OwnerToken string `json:"owner_token,omitempty"`
}

func toPasteResponse(p *pasteModel, withBody bool) pasteResponse {
	out := pasteResponse{
		Slug:          p.Slug,
		Title:         p.Title,
		Language:      p.Language,
		Visibility:    p.Visibility,
		SizeBytes:     p.SizeBytes,
		ExpiresAt:     optTS(p.ExpiresAt),
		BurnAfterRead: p.BurnAfterRead,
		HasPassword:   p.PasswordHash != "",
		CreatedAt:     ts(p.CreatedAt),
		UpdatedAt:     ts(p.UpdatedAt),
	}
	if withBody {
		out.Body = p.Body
	}
	return out
}

type createPasteRequest struct {
	Title         string `json:"title"`
	Body          string `json:"body" binding:"required"`
	Language      string `json:"language"`
	Visibility    string `json:"visibility"`
	Password      string `json:"password"`
	ExpiresIn     string `json:"expires_in"`
	BurnAfterRead bool   `json:"burn_after_read"`
}

// failPaste 在通用映射之前处理 paste 包里 errors.go 尚未覆盖的校验错误。
func failPaste(c *gin.Context, err error) {
	if errors.Is(err, paste.ErrBadVisibility) {
		failWith(c, http.StatusBadRequest, "invalid_request", "visibility 不合法",
			FieldError{Field: "visibility", Reason: `只接受 "public"、"unlisted"、"private"`})
		return
	}
	fail(c, err)
}

// failRateLimited 返回 429 并带上 Retry-After（秒，至少 1）。
func failRateLimited(c *gin.Context, retryAfter time.Duration, msg string) {
	c.Header("Retry-After", strconv.Itoa(retryAfterSeconds(retryAfter)))
	failWith(c, http.StatusTooManyRequests, "rate_limited", msg)
}

func retryAfterSeconds(d time.Duration) int {
	sec := int((d + time.Second - 1) / time.Second)
	if sec < 1 {
		sec = 1
	}
	return sec
}

func (s *server) handlePasteCreate(c *gin.Context) {
	// 匿名创建按来源限速，放在绑定与哈希之前：被限流的请求不该再消耗资源。
	// 登录用户不受限——他们的写入有身份可追溯。
	cl := clientFrom(c)
	if cl == nil {
		if ok, wait := s.Paste.AllowAnonymousCreate(c.ClientIP()); !ok {
			failRateLimited(c, wait, "创建过于频繁，请稍后再试")
			return
		}
	}

	var req createPasteRequest
	if !bindJSON(c, &req) {
		return
	}

	expiry, err := parseExpiry(req.ExpiresIn)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "expires_in 格式不正确",
			FieldError{Field: "expires_in", Reason: `需要形如 "168h" 的时长，或 "0" 表示永久`})
		return
	}

	// 归属：稳定身份 > 设备 id > 匿名 token，与读取侧的 OwnerHash 一致。
	// 两边必须用同一套优先级，否则会出现"刚创建就读不到"这种
	// 自相矛盾的状态。
	var clientID, ownerID string
	if cl != nil {
		clientID = cl.ID
		ownerID = cl.OwnerID
	}

	p, ownerToken, err := s.Paste.Create(c.Request.Context(), paste.CreateRequest{
		Title:         req.Title,
		Body:          req.Body,
		Language:      req.Language,
		Visibility:    req.Visibility,
		Password:      req.Password,
		ExpiresIn:     expiry,
		BurnAfterRead: req.BurnAfterRead,
		OwnerClientID: clientID,
		OwnerID:       ownerID,
		OwnerToken:    c.GetHeader(headerOwnerToken),
	})
	if err != nil {
		failPaste(c, err)
		return
	}

	resp := toPasteResponse(p, true)
	resp.URL = s.publicURL("/p/" + p.Slug)
	// 始终回显当前有效的 owner_token：首次创建时是服务端新生成的，
	// 之后是客户端提交的原值。前端每次都用最新返回值覆盖本地存储，
	// 这样即使 token 轮换也不会丢失归属。服务端只留哈希，丢了找不回。
	resp.OwnerToken = ownerToken
	c.Header("Location", "/p/"+p.Slug)
	c.JSON(http.StatusCreated, resp)
}

func (s *server) handlePasteList(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, next, err := s.Paste.List(c.Request.Context(), OwnerHash(c), limit, c.Query("cursor"))
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]pasteResponse, 0, len(items))
	for i := range items {
		out = append(out, toPasteResponse(&items[i], false))
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "next_cursor": cursorOrNil(next)})
}

func (s *server) handlePasteGet(c *gin.Context) {
	slug := c.Param("slug")
	pw := pastePassword(c)
	ip := c.ClientIP()
	// 只有带了口令的请求才参与计数：不带口令的请求不会触发 argon2，
	// 也不该让前端的首次探测把自己锁住。
	if pw != "" {
		if ok, wait := s.Paste.PasswordAttemptAllowed(ip, slug); !ok {
			failRateLimited(c, wait, "口令尝试次数过多，请稍后再试")
			return
		}
	}
	p, err := s.Paste.Read(c.Request.Context(), slug, pw, OwnerHash(c))
	if pw != "" {
		switch {
		case errors.Is(err, paste.ErrBadPassword):
			s.Paste.RecordPasswordResult(ip, slug, false)
		case err == nil:
			s.Paste.RecordPasswordResult(ip, slug, true)
		}
	}
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toPasteResponse(p, true))
}

type updatePasteRequest struct {
	Title      *string `json:"title"`
	Body       *string `json:"body"`
	Language   *string `json:"language"`
	Visibility *string `json:"visibility"`
	Password   *string `json:"password"`
	ExpiresIn  *string `json:"expires_in"`
}

func (s *server) handlePasteUpdate(c *gin.Context) {
	var req updatePasteRequest
	if !bindJSON(c, &req) {
		return
	}
	var expiry *time.Duration
	if req.ExpiresIn != nil {
		d, err := parseExpiry(*req.ExpiresIn)
		if err != nil {
			failWith(c, http.StatusBadRequest, "invalid_request", "expires_in 格式不正确")
			return
		}
		expiry = d
	}
	p, err := s.Paste.Update(c.Request.Context(), c.Param("slug"), paste.UpdateRequest{
		Title:      req.Title,
		Body:       req.Body,
		Language:   req.Language,
		Visibility: req.Visibility,
		Password:   req.Password,
		ExpiresIn:  expiry,
		// 永久片段只允许有登录身份的调用方设置，与创建侧一致。
		Authenticated: clientFrom(c) != nil,
	}, OwnerHash(c))
	if err != nil {
		failPaste(c, err)
		return
	}
	c.JSON(http.StatusOK, toPasteResponse(p, true))
}

func (s *server) handlePasteDelete(c *gin.Context) {
	if err := s.Paste.Delete(c.Request.Context(), c.Param("slug"), OwnerHash(c)); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handlePublicPaste 服务端渲染分享页。
//
// 不依赖前端 bundle：分享链接必须能被 curl、爬虫、以及 JS 加载失败时的
// 浏览器正常读取。页面零外部资源，CSP 比主站更严格。
func (s *server) handlePublicPaste(c *gin.Context) {
	slug := c.Param("slug")

	// 先取元信息判断形态，避免一次页面预取就把 burn_after_read 烧掉。
	meta, err := s.Paste.Meta(c.Request.Context(), slug, OwnerHash(c))
	if err != nil {
		s.renderPasteError(c, err)
		return
	}

	// 表单提交（口令页）必须在「有口令就显示口令页」之前处理，
	// 否则 POST 永远只会拿回空白口令页，口令根本不会被校验。
	if c.Request.Method == http.MethodPost {
		s.handlePublicPasteSubmit(c, meta)
		return
	}

	if c.Query("raw") == "1" && meta.PasswordHash == "" {
		p, err := s.Paste.Read(c.Request.Context(), slug, "", OwnerHash(c))
		if err != nil {
			s.renderPasteError(c, err)
			return
		}
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusOK, p.Body)
		return
	}

	if meta.PasswordHash != "" {
		s.renderPasswordPrompt(c, meta.Slug, "", http.StatusUnauthorized)
		return
	}

	p, err := s.Paste.Read(c.Request.Context(), slug, "", OwnerHash(c))
	if err != nil {
		s.renderPasteError(c, err)
		return
	}
	s.renderPaste(c, p)
}

// handlePublicPasteSubmit 处理口令页的表单提交。
//
// 失败按 (来源 IP, slug) 计数，与 API 侧共用同一个计数器：
// 换个入口不应换来一轮新的配额。
func (s *server) handlePublicPasteSubmit(c *gin.Context, meta *pasteModel) {
	slug := meta.Slug
	ip := c.ClientIP()
	protected := meta.PasswordHash != ""
	pw := ""
	if protected {
		if ok, wait := s.Paste.PasswordAttemptAllowed(ip, slug); !ok {
			c.Header("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			s.renderPasswordPrompt(c, slug, "尝试次数过多，请稍后再试", http.StatusTooManyRequests)
			return
		}
		pw = c.PostForm("password")
	}

	p, err := s.Paste.Read(c.Request.Context(), slug, pw, OwnerHash(c))
	if errors.Is(err, paste.ErrBadPassword) {
		s.Paste.RecordPasswordResult(ip, slug, false)
		s.renderPasswordPrompt(c, slug, "口令不正确", http.StatusUnauthorized)
		return
	}
	if err != nil {
		s.renderPasteError(c, err)
		return
	}
	if protected {
		s.Paste.RecordPasswordResult(ip, slug, true)
	}
	s.renderPaste(c, p)
}

func (s *server) renderPaste(c *gin.Context, p *pasteModel) {
	// 覆盖全局 CSP：这个页面用内联样式，且不允许任何脚本。
	// 用户内容全部经 html.EscapeString 转义，不存在注入面。
	c.Header("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; "+
			"form-action 'self'; frame-ancestors 'none'")
	c.Header("Cache-Control", "no-store")

	title := p.Title
	if title == "" {
		title = p.Slug
	}

	expiry := "永久"
	if p.ExpiresAt != nil {
		expiry = time.Unix(*p.ExpiresAt, 0).Local().Format("2006-01-02 15:04")
	}

	var sb strings.Builder
	sb.WriteString(htmlHead(html.EscapeString(title), publicPageCSS))
	sb.WriteString(`<main><header><h1>` + html.EscapeString(title) + `</h1><span class="meta">`)
	sb.WriteString(html.EscapeString(p.Language) + " · ")
	sb.WriteString(strconv.Itoa(len(p.Body)) + " 字符 · 到期 ")
	sb.WriteString(html.EscapeString(expiry))
	if p.BurnAfterRead {
		sb.WriteString(" · 一次性")
	}
	sb.WriteString(`</span></header><pre><code>`)
	sb.WriteString(html.EscapeString(p.Body))
	sb.WriteString(`</code></pre><footer><a href="?raw=1">纯文本</a></footer></main></body></html>`)

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, sb.String())
}

// renderPasswordPrompt 渲染口令表单。
//
// 表单不带 CSRF 令牌：它是同源的公开页表单，提交只做口令校验与读取，
// 不依赖也不修改任何 cookie 会话状态（/p/:slug 不读会话 cookie，
// OptionalAuth 只认 Bearer），跨站伪造提交拿不到任何受害者身份。
// 会话 cookie 本身是 SameSite=Lax，跨站 POST 也不会携带它。
func (s *server) renderPasswordPrompt(c *gin.Context, slug, errMsg string, status int) {
	c.Header("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'")
	c.Header("Cache-Control", "no-store")

	var sb strings.Builder
	sb.WriteString(htmlHead("需要口令", centerPageCSS))
	sb.WriteString(`<form method="post"><strong>该片段需要口令</strong>`)
	if errMsg != "" {
		sb.WriteString(`<span class="err">` + html.EscapeString(errMsg) + `</span>`)
	}
	sb.WriteString(`<input type="password" name="password" autofocus required>`)
	sb.WriteString(`<button type="submit">查看</button></form></body></html>`)

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(status, sb.String())
}

func (s *server) renderPasteError(c *gin.Context, err error) {
	status, code, msg := codeFor(err)
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	c.Header("Cache-Control", "no-store")

	body := htmlHead("不可用", centerPageCSS) +
		`<main><h1>` + html.EscapeString(msg) + `</h1><p>` +
		html.EscapeString(code) + `</p></main></body></html>`

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(status, body)
}

// pastePassword 只从 X-Paste-Password 头读取口令。
// 不接受 ?password=：query 会进访问日志、浏览器历史与 Referer。
func pastePassword(c *gin.Context) string {
	return c.GetHeader("X-Paste-Password")
}

// parseExpiry 解析 "168h" / "0" / 空字符串。
func parseExpiry(s string) (*time.Duration, error) {
	if s == "" {
		return nil, nil
	}
	if s == "0" {
		d := time.Duration(0)
		return &d, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return nil, err
	}
	if d < 0 {
		return nil, paste.ErrBadExpiry
	}
	return &d, nil
}

func (s *server) publicURL(path string) string {
	base := strings.TrimRight(s.Cfg.Server.PublicURL, "/")
	if base == "" {
		return path
	}
	return base + path
}
