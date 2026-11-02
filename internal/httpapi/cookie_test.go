package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// findSetCookie 抽出某个名字的 Set-Cookie 原始值。
func findSetCookie(resp *http.Response, name string) (string, bool) {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c.Raw, true
		}
	}
	return "", false
}

// 登录与登出的 cookie 属性必须一致。
//
// 生产部署是 Caddy 反代：TLS 在 Caddy 终止，到 Go 这里是明文 HTTP，
// 所以 Request.TLS 为 nil。设置 cookie 时用的是
// `TLS != nil || public_url 是 https`，清除时如果只看 Request.TLS，
// 就会下发一个不带 Secure 的同名 cookie。
//
// 浏览器（Chrome/Firefox 都实现了 RFC 6265bis 的完整性保护）
// 拒绝用非 Secure cookie 覆盖已存在的 Secure cookie，
// 于是登出后 refresh token 仍留在浏览器里。
func TestLogoutClearsSecureCookiesConsistently(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	// 反代场景：到应用层是明文。
	if strings.HasPrefix(h.cfg.Server.PublicURL, "https://") == false {
		t.Fatalf("harness 的 public_url 应当是 https，实际 %q", h.cfg.Server.PublicURL)
	}

	// 先确认登录响应里的 cookie 带 Secure。
	loginResp := httptest.NewRecorder()
	_ = loginResp // login 用的是 h.doJSON，拿不到 Response 头；直接构造登出请求。

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-CSRF-Token", "x")
	w := h.do(t, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("logout = %d: %s", w.Code, w.Body.String())
	}

	resp := w.Result()
	sessionCookie, ok := findSetCookie(resp, cookieSession)
	if !ok {
		t.Fatal("登出没有清除 session cookie")
	}
	if !strings.Contains(strings.ToLower(sessionCookie), "; secure") {
		t.Errorf("登出下发的 session cookie 缺 Secure，浏览器会拒绝覆盖: %s", sessionCookie)
	}
	if !strings.Contains(strings.ToLower(sessionCookie), "httponly") {
		t.Errorf("session cookie 缺 HttpOnly: %s", sessionCookie)
	}
	// 过期时间必须让它立刻失效。
	if !strings.Contains(sessionCookie, "Max-Age=0") && !strings.Contains(sessionCookie, "Expires=Thu, 01 Jan 1970") {
		t.Errorf("session cookie 未被置为过期: %s", sessionCookie)
	}

	csrfCookie, ok := findSetCookie(resp, cookieCSRF)
	if !ok {
		t.Fatal("登出没有清除 csrf cookie")
	}
	if !strings.Contains(strings.ToLower(csrfCookie), "; secure") {
		t.Errorf("登出下发的 csrf cookie 缺 Secure: %s", csrfCookie)
	}
}
