package httpapi

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// SPA 提供前端静态资源与 History API fallback。
//
// 关键边界：/api/、/media/、/p/、/healthz、/readyz 不参与 fallback。
// 少了这条，一个打错的 API 路径会拿到 200 + HTML，客户端解析 JSON 时才报错，
// 排查成本很高。
type SPA struct {
	dist    fs.FS
	devMode bool

	indexHTML []byte // 启动时读一次，之后只做占位符替换
}

// NewSPA 构造静态资源处理器。dist 为 nil 时返回提示页。
func NewSPA(dist fs.FS, devMode bool) *SPA {
	s := &SPA{dist: dist, devMode: devMode}
	if dist != nil {
		if raw, err := fs.ReadFile(dist, "index.html"); err == nil {
			s.indexHTML = raw
		}
	}
	return s
}

// placeholder 由 index.html 提供，Go 在响应时替换成用户当前主题。
// 这样首帧就是正确配色，不需要内联脚本，CSP 可以保持 script-src 'self'。
const themePlaceholder = "__THEME__"

// Handler 返回兜底路由：静态资源直出，其余回退 index.html。
// /api 与 /media 等保留前缀返回 JSON 404，不能被 SPA 吞掉。
func (s *SPA) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Request.URL.Path

		if isReservedPath(p) {
			failWith(c, http.StatusNotFound, "not_found", "接口不存在")
			return
		}

		if s.devMode {
			// 开发期前端由 Vite 提供，后端只提示去哪里。
			c.String(http.StatusOK, "dev mode: run `pnpm --dir web dev` and open http://127.0.0.1:5173")
			return
		}

		if strings.HasPrefix(p, "/_app/") {
			s.serveAsset(c, strings.TrimPrefix(p, "/"))
			return
		}
		if p != "/" && s.serveIfFile(c, strings.TrimPrefix(p, "/")) {
			return
		}
		s.serveIndex(c)
	}
}

func isReservedPath(p string) bool {
	switch p {
	case "/healthz", "/readyz":
		return true
	}
	return strings.HasPrefix(p, "/api/") ||
		strings.HasPrefix(p, "/media/") ||
		strings.HasPrefix(p, "/p/")
}

func (s *SPA) serveAsset(c *gin.Context, name string) {
	if s.dist == nil {
		c.Status(http.StatusNotFound)
		return
	}
	f, err := s.dist.Open(path.Clean(name))
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		c.Status(http.StatusNotFound)
		return
	}
	// 文件名带内容哈希，可以放心长缓存。
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(c.Writer, c.Request, info.Name(), info.ModTime(), f.(io.ReadSeeker))
}

func (s *SPA) serveIfFile(c *gin.Context, name string) bool {
	if s.dist == nil || name == "" || name == "index.html" {
		return false
	}
	f, err := s.dist.Open(path.Clean(name))
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		return false
	}
	c.Header("Cache-Control", "public, max-age=3600")
	http.ServeContent(c.Writer, c.Request, info.Name(), info.ModTime(), rs)
	return true
}

func (s *SPA) serveIndex(c *gin.Context) {
	c.Header("Cache-Control", "no-cache")
	if s.indexHTML == nil {
		c.String(http.StatusOK, "前端尚未构建：运行 `pnpm --dir web build` 后重新编译")
		return
	}
	theme := c.GetHeader("X-Theme")
	if ck, err := c.Cookie("hh_theme"); err == nil && ck != "" {
		theme = ck
	}
	switch theme {
	case "light", "dark":
	default:
		theme = "auto"
	}
	// 替换失败不影响可用性：保留占位符时浏览器会落到 prefers-color-scheme 分支。
	out := bytes.Replace(s.indexHTML, []byte(themePlaceholder), []byte(theme), 1)
	c.Data(http.StatusOK, "text/html; charset=utf-8", out)
}

// ThemeCookie 记录三态主题偏好。不设 HttpOnly：前端切换时要能直接写。
func ThemeCookie(c *gin.Context) {
	theme := c.Param("theme")
	switch theme {
	case "light", "dark", "auto":
	default:
		failWith(c, http.StatusBadRequest, "invalid_request", "theme 必须是 light、dark 或 auto")
		return
	}
	// SameSite=Lax + 一年有效期，仅作为渲染提示，不承载身份。
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("hh_theme", theme, 31536000, "/", "", c.Request.TLS != nil, false)
	c.JSON(http.StatusOK, gin.H{"theme": theme})
}
