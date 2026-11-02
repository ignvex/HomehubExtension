package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/auth"

	"github.com/gin-gonic/gin"
)

type enrollRequest struct {
	Token     string `json:"token" binding:"required"`
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	PublicKey string `json:"public_key" binding:"required"`
}

func (s *server) handleEnroll(c *gin.Context) {
	var req enrollRequest
	if !bindJSON(c, &req) {
		return
	}
	res, err := s.Auth.Enroll(c.Request.Context(), req.Token, req.Name, req.Algorithm, req.PublicKey)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"client_id":  res.Client.ID,
		"name":       res.Client.Name,
		"server_key": res.ServerKey,
		"kid":        res.KID,
		"algorithm":  res.Client.Algorithm,
	})
}

type challengeRequest struct {
	ClientID string `json:"client_id" binding:"required"`
}

func (s *server) handleChallenge(c *gin.Context) {
	var req challengeRequest
	if !bindJSON(c, &req) {
		return
	}
	res, err := s.Auth.Challenge(c.Request.Context(), req.ClientID)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"nonce":      res.Nonce,
		"expires_at": res.ExpiresAt.Unix(),
		// 客户端要用它拼签名消息，回显能减少前端拼错的机会。
		"message": string(auth.AuthMessage(req.ClientID, res.Nonce)),
	})
}

type tokenRequest struct {
	ClientID   string `json:"client_id" binding:"required"`
	Nonce      string `json:"nonce" binding:"required"`
	Signature  string `json:"signature" binding:"required"`
	ClientKind string `json:"client_kind"`
}

func (s *server) handleToken(c *gin.Context) {
	var req tokenRequest
	if !bindJSON(c, &req) {
		return
	}
	tokens, err := s.Auth.Exchange(c.Request.Context(), req.ClientID, req.Nonce, req.Signature, c.Request.UserAgent())
	if err != nil {
		fail(c, err)
		return
	}
	s.respondTokens(c, tokens, req.ClientKind == "machine")
}

func (s *server) handleRefresh(c *gin.Context) {
	refresh, err := c.Cookie(cookieSession)
	if err != nil || refresh == "" {
		failWith(c, http.StatusUnauthorized, "unauthenticated", "会话不存在")
		return
	}
	tokens, err := s.Auth.Refresh(c.Request.Context(), refresh, c.Request.UserAgent())
	if err != nil {
		s.clearAuthCookies(c)
		fail(c, err)
		return
	}
	s.respondTokens(c, tokens, false)
}

// cookieSecure 判断下发的 cookie 是否要带 Secure。
//
// 两个条件任一成立：
//   - 到应用层就是 TLS（直连，不经反代）；
//   - public_url 是 https（生产部署：TLS 在 Caddy 终止，
//     到 Go 这里是明文 HTTP，Request.TLS 永远为 nil）。
//
// 设置与清除必须用同一个判断。清除时只看 Request.TLS 的话，
// 反代部署下会下发一个不带 Secure 的同名 cookie，
// 而浏览器（RFC 6265bis 的完整性保护）拒绝用非 Secure cookie
// 覆盖已存在的 Secure cookie —— 结果就是「点了登出，
// refresh token 还在浏览器里」。
func (s *server) cookieSecure(c *gin.Context) bool {
	return c.Request.TLS != nil || strings.HasPrefix(s.Cfg.Server.PublicURL, "https://")
}

// respondTokens 统一签发响应。
//
// refresh_token 默认只写 HttpOnly cookie，不进响应体：
// 放进 JSON 的话 XSS 一个 fetch 就能带走长期凭证，HttpOnly 就白设了。
// 机器客户端显式声明 client_kind=machine 时才在 body 里返回。
func (s *server) respondTokens(c *gin.Context, t *auth.Tokens, machine bool) {
	secure := s.cookieSecure(c)
	sessionTTL := int(s.Cfg.Auth.SessionTTL.Duration / time.Second)

	body := gin.H{
		"access_token": t.AccessToken,
		"token_type":   "Bearer",
		"expires_in":   int(s.Auth.AccessTTL() / time.Second),
		"expires_at":   t.AccessExpires.Unix(),
		"csrf_token":   t.CSRF,
		"client":       gin.H{"id": t.Client.ID, "name": t.Client.Name},
	}

	if machine {
		body["refresh_token"] = t.RefreshToken
		c.JSON(http.StatusOK, body)
		return
	}

	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(cookieSession, t.RefreshToken, sessionTTL, "/", "", secure, true)
	c.SetCookie(cookieCSRF, t.CSRF, sessionTTL, "/", "", secure, false)
	c.JSON(http.StatusOK, body)
}

func (s *server) clearAuthCookies(c *gin.Context) {
	secure := s.cookieSecure(c)
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(cookieSession, "", -1, "/", "", secure, true)
	c.SetCookie(cookieCSRF, "", -1, "/", "", secure, false)
}

func (s *server) handleLogout(c *gin.Context) {
	if refresh, err := c.Cookie(cookieSession); err == nil {
		_ = s.Auth.Logout(c.Request.Context(), refresh)
	}
	s.clearAuthCookies(c)
	c.Status(http.StatusNoContent)
}

func (s *server) handleMe(c *gin.Context) {
	cl := clientFrom(c)
	if cl == nil {
		fail(c, auth.ErrBadCredentials)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"client":     gin.H{"id": cl.ID, "name": cl.Name},
		"expires_in": int(s.Auth.AccessTTL() / time.Second),
	})
}

func (s *server) handleServerKey(c *gin.Context) {
	signer := s.Auth.Signer()
	c.JSON(http.StatusOK, gin.H{
		"kid":        signer.KID(),
		"algorithm":  auth.AlgEd25519,
		"public_key": b64Std(signer.PublicKey()),
	})
}

func (s *server) handleListClients(c *gin.Context) {
	clients, err := s.Store.Auth().ListClients(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]gin.H, 0, len(clients))
	for _, cl := range clients {
		item := gin.H{
			"id":         cl.ID,
			"name":       cl.Name,
			"algorithm":  cl.Algorithm,
			"created_at": ts(cl.CreatedAt),
			"revoked":    !cl.Active(),
		}
		if cl.LastSeenAt != nil {
			item["last_seen_at"] = ts(*cl.LastSeenAt)
		}
		out = append(out, item)
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (s *server) handleRevokeClient(c *gin.Context) {
	id := c.Param("id")
	if err := s.Store.Auth().RevokeClient(c.Request.Context(), id, time.Now().Unix()); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type keyLoginRequest struct {
	Key string `json:"key" binding:"required"`
}

// handleKeyLogin 用宿主签发的长期密钥换一组会话令牌。
//
// 和 /auth/token 的区别只在于"怎么证明自己"：那边是设备私钥签的
// 挑战消息，这里是直接出示密钥。拿到令牌之后一切相同——同一个
// issue()、同一个 access token、同一个 refresh cookie、同一个 owner_hash。
// 下游没有任何分支需要知道这次登录走的是哪条路。
func (s *server) handleKeyLogin(c *gin.Context) {
	var req keyLoginRequest
	if !bindJSON(c, &req) {
		return
	}
	tokens, err := s.Auth.RedeemAccessKey(c.Request.Context(), req.Key, c.Request.UserAgent())
	if err != nil {
		fail(c, err)
		return
	}
	// 不走 machine 分支：密钥登录本质上仍是"人在这台浏览器上进入"，
	// refresh token 应该留在 HttpOnly cookie 里，而不是进响应体。
	s.respondTokens(c, tokens, false)
}
