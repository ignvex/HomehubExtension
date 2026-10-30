package httpapi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/auth"
	"github.com/ignvex/HomehubExtension/internal/paste"

	"github.com/gin-gonic/gin"
)

const (
	ctxRequestID = "request_id"
	ctxClient    = "client"
	ctxLogger    = "logger"
	ctxHubBody   = "hub_body"

	cookieSession    = "hh_session"
	cookieCSRF       = "hh_csrf"
	headerCSRF       = "X-CSRF-Token"
	headerRequestID  = "X-Request-ID"
	headerOwnerToken = "X-Owner-Token"
)

func requestID(c *gin.Context) string {
	if v, ok := c.Get(ctxRequestID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func logFrom(c *gin.Context) *slog.Logger {
	if v, ok := c.Get(ctxLogger); ok {
		if l, ok := v.(*slog.Logger); ok {
			return l
		}
	}
	return slog.Default()
}

func clientFrom(c *gin.Context) *authClient {
	if v, ok := c.Get(ctxClient); ok {
		if cl, ok := v.(*authClient); ok {
			return cl
		}
	}
	return nil
}

type authClient struct {
	ID   string
	Name string
}

// RequestID 为每个请求分配 ID，并在响应头回写，便于和日志对账。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(headerRequestID)
		if id == "" || len(id) > 64 {
			buf := make([]byte, 8)
			if _, err := rand.Read(buf); err == nil {
				id = hex.EncodeToString(buf)
			} else {
				id = time.Now().Format("20060102150405.000")
			}
		}
		c.Set(ctxRequestID, id)
		c.Header(headerRequestID, id)
		c.Next()
	}
}

// AccessLog 记录请求。探针与媒体路径不记，避免刷屏。
func AccessLog(log *slog.Logger, enabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		l := log.With("request_id", requestID(c))
		c.Set(ctxLogger, l)

		if !enabled || skipLog(c.FullPath()) {
			c.Next()
			return
		}
		start := time.Now()
		c.Next()
		l.Info("http",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"bytes", c.Writer.Size(),
			"took_ms", time.Since(start).Milliseconds(),
			"ip", c.ClientIP(),
		)
	}
}

func skipLog(route string) bool {
	switch route {
	case "/healthz", "/readyz", "/media/:name":
		return true
	}
	return strings.HasPrefix(route, "/_app/")
}

// Recovery 把 panic 转成 500，并记录堆栈。
func Recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic", "request_id", requestID(c), "panic", r,
					"path", c.Request.URL.Path)
				failWith(c, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
		}()
		c.Next()
	}
}

// SecurityHeaders 下发 CSP 与其余安全头。
//
// CSP 里没有任何 'unsafe-inline'：正文里的 style 属性在清洗阶段已删除，
// 前端构建产物也不含内联脚本。CSP 是第二道防线，不是唯一防线。
func SecurityHeaders(devMode bool) gin.HandlerFunc {
	csp := strings.Join([]string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data:",
		"media-src 'self'",
		"font-src 'self'",
		"connect-src 'self'",
		"frame-src 'none'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")

	return func(c *gin.Context) {
		h := c.Writer.Header()
		if devMode {
			// Vite HMR 需要内联脚本与 ws 连接，仅开发模式放宽。
			h.Set("Content-Security-Policy", strings.ReplaceAll(csp,
				"script-src 'self'", "script-src 'self' 'unsafe-inline'"))
		} else {
			h.Set("Content-Security-Policy", csp)
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		c.Next()
	}
}

// RequireAuth 校验 Bearer token；SSE 路径额外允许 refresh cookie。
func RequireAuth(svc *auth.Service, allowCookie bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.ParseBearer(c.GetHeader("Authorization"))
		if token != "" {
			client, err := svc.Authenticate(c.Request.Context(), token)
			if err == nil {
				c.Set(ctxClient, &authClient{ID: client.ID, Name: client.Name})
				// Bearer 请求豁免 CSRF：浏览器不会自动附加这个头。
				c.Set("auth_via_bearer", true)
				c.Next()
				return
			}
		}
		if allowCookie {
			if ck, err := c.Cookie(cookieSession); err == nil && ck != "" {
				client, err := svc.SessionClient(c.Request.Context(), ck)
				if err == nil {
					c.Set(ctxClient, &authClient{ID: client.ID, Name: client.Name})
					c.Next()
					return
				}
			}
		}
		fail(c, auth.ErrBadCredentials)
	}
}

// RequireCSRF 对「浏览器会自动携带凭证」的写请求做双提交校验。
//
// 豁免三种情况，判据都是「有没有可被借用的凭证」而不是「是不是写请求」：
//
//  1. 安全方法（GET/HEAD/OPTIONS）本身无副作用。
//  2. Bearer 认证：攻击者无法让浏览器自动附加 Authorization 头。
//  3. 完全匿名的请求：既没有 Bearer 也没有 session cookie。
//     此时不存在被冒用身份的凭证，CSRF 攻击也就无从谈起——
//     这正是「不登录也能创建片段」能成立的前提。
//
// 第 3 条容易漏。漏了的症状是匿名创建被 403 挡住，
// 而错误信息是「缺少 CSRF 令牌」——看起来像前端忘了带 header。
func RequireCSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		if v, ok := c.Get("auth_via_bearer"); ok {
			if b, _ := v.(bool); b {
				c.Next()
				return
			}
		}
		// 没有任何会话凭证：匿名请求，没有可借用的身份。
		if _, err := c.Cookie(cookieSession); err != nil {
			c.Next()
			return
		}
		cookie, err := c.Cookie(cookieCSRF)
		if err != nil || cookie == "" {
			failWith(c, http.StatusForbidden, "forbidden", "缺少 CSRF 令牌")
			return
		}
		if subtleCompare(c.GetHeader(headerCSRF), cookie) == false {
			failWith(c, http.StatusForbidden, "forbidden", "CSRF 令牌不匹配")
			return
		}
		c.Next()
	}
}

func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// RequireHubSignature 校验 Homehub 入站请求的签名。
//
// 需要原始 body 参与验签，所以先把 body 读进内存并重置：
// 这些接口的请求体都很小（订阅描述），几十 KB 的缓冲可以接受。
func RequireHubSignature(verifier *auth.HubVerifier, maxBody int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if verifier == nil {
			fail(c, auth.ErrNoHubKey)
			return
		}
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody+1))
		if err != nil {
			failWith(c, http.StatusBadRequest, "invalid_request", "无法读取请求体")
			return
		}
		if int64(len(body)) > maxBody {
			failWith(c, http.StatusRequestEntityTooLarge, "payload_too_large", "请求体过大")
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		if err := verifier.Verify(c.Request, body); err != nil {
			fail(c, err)
			return
		}
		c.Next()
	}
}

// RequireAttemptLimit 对认证端点按来源限制失败次数。
//
// 计数发生在请求处理完之后：只有 401/403 才算一次失败。
// 成功会清空计数（否则正常用户会被自己的历史失误误伤），
// 5xx 不计数（那是服务端的问题，不该惩罚客户端）。
func RequireAttemptLimit(limiter *auth.AttemptLimiter, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limiter == nil {
			c.Next()
			return
		}
		key := c.ClientIP()
		if !limiter.Allow(key) {
			_, resetIn := limiter.Remaining(key)
			seconds := int(resetIn.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			// Retry-After 让客户端知道什么时候可以再试，
			// 比单纯返回 429 更有用。
			c.Header("Retry-After", strconvItoa(seconds))
			failWith(c, http.StatusTooManyRequests, "rate_limited",
				"尝试次数过多，请稍后再试")
			return
		}

		c.Next()

		switch c.Writer.Status() {
		case http.StatusUnauthorized, http.StatusForbidden:
			if justBlocked := limiter.Fail(key); justBlocked {
				// 只在刚进入限流状态时记一条：持续的暴力尝试不该刷满日志。
				log.Warn("auth attempts blocked",
					"ip", key, "path", c.Request.URL.Path)
			}
		case http.StatusOK, http.StatusCreated, http.StatusNoContent:
			limiter.Reset(key)
		}
	}
}

func strconvItoa(v int) string {
	return strconv.Itoa(v)
}

// OptionalAuth 尝试识别调用方，但从不拒绝请求。
//
// 用于「匿名也能用，登录后行为更完整」的端点：
//   - paste 创建：匿名可建，登录则归属到设备
//   - paste 列表：匿名凭 owner token 看自己的，登录看设备的
//
// 与 RequireAuth 的关键区别：凭证无效时不是 401，而是当作匿名继续。
// 这样 token 过期的用户不会突然失去所有能力，只是退回匿名。
func OptionalAuth(svc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token := auth.ParseBearer(c.GetHeader("Authorization")); token != "" {
			if client, err := svc.Authenticate(c.Request.Context(), token); err == nil {
				c.Set(ctxClient, &authClient{ID: client.ID, Name: client.Name})
				c.Set("auth_via_bearer", true)
			}
		}
		c.Next()
	}
}

// OwnerHash 从请求上下文推导归属哈希。
//
// 优先级：已登录设备 > 客户端提交的 owner token。两者都没有则返回空，
// 调用方据此决定是「拒绝」还是「按匿名处理」。
//
// owner token 走 header 而不是 query：query 会进访问日志，
// 而这些日志通常会被收集到别处。
func OwnerHash(c *gin.Context) string {
	if cl := clientFrom(c); cl != nil {
		return paste.OwnerHashForClient(cl.ID)
	}
	if token := c.GetHeader(headerOwnerToken); token != "" {
		return paste.OwnerHashForToken(token)
	}
	return ""
}
