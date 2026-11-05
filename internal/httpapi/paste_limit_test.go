package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ignvex/HomehubExtension/internal/paste"
)

// withPublicPost 挂上 POST /p/:slug。生产路由由 router.go 注册：
//
//	r.POST("/p/:slug", OptionalAuth(s.Auth), s.handlePublicPaste)
//
// 这里手工挂同一行，让测试不依赖 router.go 的改动先落地。
func (h *harness) withPublicPost(t *testing.T) {
	t.Helper()
	s := &server{Deps: Deps{Cfg: h.cfg, Store: h.db, Auth: h.svc, Paste: h.pasteSvc}}
	defer func() {
		// router.go 已注册该路由时 gin 会 panic，此时直接用已有路由即可。
		_ = recover()
	}()
	h.router.POST("/p/:slug", OptionalAuth(h.svc), s.handlePublicPaste)
}

func (h *harness) createPaste(t *testing.T, body map[string]any, mutate func(*http.Request)) string {
	t.Helper()
	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", body, mutate)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Slug string `json:"slug"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return out.Slug
}

func getWithPassword(slug, pw string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+slug, nil)
	if pw != "" {
		req.Header.Set("X-Paste-Password", pw)
	}
	return req
}

func postForm(slug, pw string) *http.Request {
	form := url.Values{"password": {pw}}
	req := httptest.NewRequest(http.MethodPost, "/p/"+slug, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestPastePasswordAPIRateLimitedAfterFiveFailures(t *testing.T) {
	h := newHarness(t)
	slug := h.createPaste(t, map[string]any{"body": "secret", "password": "right"}, nil)

	for i := 0; i < paste.PasswordAttemptLimit; i++ {
		if w := h.do(t, getWithPassword(slug, "wrong")); w.Code != http.StatusForbidden {
			t.Fatalf("第 %d 次错误口令 = %d, want 403", i+1, w.Code)
		}
	}
	// 第 6 次，即便口令正确也被拦截。
	w := h.do(t, getWithPassword(slug, "right"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("超限后 = %d, want 429: %s", w.Code, w.Body.String())
	}
	if ra, _ := strconv.Atoi(w.Header().Get("Retry-After")); ra < 1 {
		t.Errorf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
}

func TestPastePasswordAPISuccessResetsCounter(t *testing.T) {
	h := newHarness(t)
	slug := h.createPaste(t, map[string]any{"body": "secret", "password": "right"}, nil)
	for round := 0; round < 2; round++ {
		for i := 0; i < paste.PasswordAttemptLimit-1; i++ {
			h.do(t, getWithPassword(slug, "wrong"))
		}
		if w := h.do(t, getWithPassword(slug, "right")); w.Code != http.StatusOK {
			t.Fatalf("round %d 正确口令 = %d, want 200", round, w.Code)
		}
	}
}

func TestPastePasswordQueryParamIgnored(t *testing.T) {
	h := newHarness(t)
	slug := h.createPaste(t, map[string]any{"body": "secret", "password": "right"}, nil)
	w := h.do(t, httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+slug+"?password=right", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("?password= 仍被接受: %d", w.Code)
	}
	if w := h.do(t, getWithPassword(slug, "right")); w.Code != http.StatusOK {
		t.Fatalf("X-Paste-Password = %d, want 200", w.Code)
	}
}

func TestPublicPastePasswordFormVerifiesPassword(t *testing.T) {
	h := newHarness(t)
	h.withPublicPost(t)
	slug := h.createPaste(t, map[string]any{"body": "form-secret", "password": "right"}, nil)

	// GET 显示口令页，不泄露正文。
	w := h.do(t, httptest.NewRequest(http.MethodGet, "/p/"+slug, nil))
	if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "form-secret") {
		t.Fatalf("GET = %d", w.Code)
	}

	w = h.do(t, postForm(slug, "wrong"))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "口令不正确") {
		t.Fatalf("错误口令 = %d: %s", w.Code, w.Body.String())
	}

	w = h.do(t, postForm(slug, "right"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "form-secret") {
		t.Fatalf("正确口令 = %d: %s", w.Code, w.Body.String())
	}
}

func TestPublicPastePasswordFormRateLimited(t *testing.T) {
	h := newHarness(t)
	h.withPublicPost(t)
	slug := h.createPaste(t, map[string]any{"body": "form-secret", "password": "right"}, nil)

	for i := 0; i < paste.PasswordAttemptLimit; i++ {
		h.do(t, postForm(slug, "wrong"))
	}
	w := h.do(t, postForm(slug, "right"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("超限后 = %d, want 429", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(w.Body.String(), "尝试次数过多") {
		t.Fatalf("应渲染带提示的口令页: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "form-secret") {
		t.Fatal("被限流时泄露了正文")
	}
	// API 与表单共用计数。
	if w := h.do(t, getWithPassword(slug, "right")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("API 侧 = %d, want 429（与表单共用计数）", w.Code)
	}
}

func TestAnonymousPasteCreateRateLimited(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < paste.AnonCreatePerMinute; i++ {
		h.createPaste(t, map[string]any{"body": "x"}, nil)
	}
	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{"body": "x"}, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次匿名创建 = %d, want 429", paste.AnonCreatePerMinute+1, w.Code)
	}
	if ra, _ := strconv.Atoi(w.Header().Get("Retry-After")); ra < 1 || ra > 60 {
		t.Errorf("Retry-After = %q", w.Header().Get("Retry-After"))
	}

	// 另一个来源不受影响。
	w = h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{"body": "x"},
		func(r *http.Request) { r.RemoteAddr = "198.51.100.7:1234" })
	if w.Code != http.StatusCreated {
		t.Fatalf("其他来源 = %d, want 201", w.Code)
	}
}

func TestLoggedInPasteCreateNotRateLimited(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)
	for i := 0; i < paste.AnonCreatePerMinute+5; i++ {
		h.createPaste(t, map[string]any{"body": "x"}, bearer(token))
	}
}

func TestAnonymousPermanentPasteRejected(t *testing.T) {
	h := newHarness(t)
	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes",
		map[string]any{"body": "x", "expires_in": "0"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("匿名永久 = %d, want 400: %s", w.Code, w.Body.String())
	}

	token, _ := h.login(t)
	w = h.doJSON(t, http.MethodPost, "/api/v1/pastes",
		map[string]any{"body": "x", "expires_in": "0"}, bearer(token))
	if w.Code != http.StatusCreated {
		t.Fatalf("登录永久 = %d, want 201: %s", w.Code, w.Body.String())
	}
}

func TestPasteUpdateInvalidVisibilityIs400(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)
	slug := h.createPaste(t, map[string]any{"body": "x"}, bearer(token))
	w := h.doJSON(t, http.MethodPut, "/api/v1/pastes/"+slug,
		map[string]any{"visibility": "everyone"}, bearer(token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 visibility = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestAnonymousPasteListAndBadgeEmpty(t *testing.T) {
	h := newHarness(t)
	h.createPaste(t, map[string]any{"body": "x"}, nil)
	w := h.do(t, httptest.NewRequest(http.MethodGet, "/api/v1/pastes", nil))
	var out struct {
		Items []any `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || len(out.Items) != 0 {
		t.Fatalf("无 owner token 的列表 = %d, %d 条", w.Code, len(out.Items))
	}
}
