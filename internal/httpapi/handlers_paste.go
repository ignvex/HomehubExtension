package httpapi

import (
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

func (s *server) handlePasteCreate(c *gin.Context) {
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

	p, err := s.Paste.Create(c.Request.Context(), paste.CreateRequest{
		Title:         req.Title,
		Body:          req.Body,
		Language:      req.Language,
		Visibility:    req.Visibility,
		Password:      req.Password,
		ExpiresIn:     expiry,
		BurnAfterRead: req.BurnAfterRead,
	})
	if err != nil {
		fail(c, err)
		return
	}

	resp := toPasteResponse(p, true)
	resp.URL = s.publicURL("/p/" + p.Slug)
	c.Header("Location", "/p/"+p.Slug)
	c.JSON(http.StatusCreated, resp)
}

func (s *server) handlePasteList(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, next, err := s.Paste.List(c.Request.Context(), limit, c.Query("cursor"))
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
	p, err := s.Paste.Read(c.Request.Context(), c.Param("slug"), pastePassword(c))
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
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toPasteResponse(p, true))
}

func (s *server) handlePasteDelete(c *gin.Context) {
	if err := s.Paste.Delete(c.Request.Context(), c.Param("slug")); err != nil {
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
	meta, err := s.Paste.Meta(c.Request.Context(), slug)
	if err != nil {
		s.renderPasteError(c, err)
		return
	}

	if c.Query("raw") == "1" && meta.PasswordHash == "" {
		p, err := s.Paste.Read(c.Request.Context(), slug, "")
		if err != nil {
			s.renderPasteError(c, err)
			return
		}
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusOK, p.Body)
		return
	}

	if meta.PasswordHash != "" {
		s.renderPasswordPrompt(c, meta.Slug, "")
		return
	}
	if c.Request.Method == http.MethodPost {
		// 表单提交（口令页）走这里。
		p, err := s.Paste.Read(c.Request.Context(), slug, c.PostForm("password"))
		if err != nil {
			s.renderPasswordPrompt(c, meta.Slug, "口令不正确")
			return
		}
		s.renderPaste(c, p)
		return
	}

	p, err := s.Paste.Read(c.Request.Context(), slug, "")
	if err != nil {
		s.renderPasteError(c, err)
		return
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

func (s *server) renderPasswordPrompt(c *gin.Context, slug, errMsg string) {
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
	c.String(http.StatusUnauthorized, sb.String())
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

func pastePassword(c *gin.Context) string {
	if v := c.GetHeader("X-Paste-Password"); v != "" {
		return v
	}
	return c.Query("password")
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
