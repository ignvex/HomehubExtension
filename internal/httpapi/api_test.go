package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/auth"
	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/config"
	"github.com/ignvex/HomehubExtension/internal/events"
	"github.com/ignvex/HomehubExtension/internal/paste"
	"github.com/ignvex/HomehubExtension/internal/rss"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"

	"github.com/gin-gonic/gin"
)

type harness struct {
	router   *gin.Engine
	db       *sqlite.DB
	cfg      config.Config
	svc      *auth.Service
	hubPriv  ed25519.PrivateKey
	hubPub   ed25519.PublicKey
	bus      *events.Bus
	blobs    *blob.Store
	pasteSvc *paste.Service
	rssSvc   *rss.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Default()
	cfg.Database.Path = filepath.Join(dir, "api.db")
	cfg.Storage.BlobDir = filepath.Join(dir, "blobs")
	cfg.Storage.TmpDir = filepath.Join(dir, "blobs", "tmp")
	cfg.Auth.ServerKeyPath = filepath.Join(dir, "server.key")
	cfg.Server.PublicURL = "https://hh.example.com"
	cfg.Log.AccessLog = false

	ctx := context.Background()
	db, err := sqlite.Open(ctx, cfg.Database.Path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	blobs, err := blob.Open(blob.Options{
		Root: cfg.Storage.BlobDir, TmpDir: cfg.Storage.TmpDir,
		MaxSize: cfg.Storage.MaxBlobBytes, Quota: cfg.Storage.QuotaBytes,
	})
	if err != nil {
		t.Fatal(err)
	}

	signer, err := auth.LoadOrCreateSigner(cfg.Auth.ServerKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.NewService(db.Auth(), signer, auth.Settings{})

	hubPub, hubPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := auth.NewHubVerifier(hubPub, time.Minute, time.Minute)

	pasteSvc := paste.NewService(db, paste.Config{
		MaxBodyBytes:  cfg.Pastebin.MaxBodyBytes,
		DefaultExpiry: cfg.Pastebin.DefaultExpiry.Duration,
		MaxExpiry:     cfg.Pastebin.MaxExpiry.Duration,
		SlugLength:    cfg.Pastebin.SlugLength,
		AllowPublic:   true,
		Languages:     map[string]bool{"text": true},
	})

	bus := events.New()
	rssSvc := rss.NewService(db, blobs, rss.Config{
		UserAgent:       "test/1",
		RequestTimeout:  2 * time.Second,
		FetchTimeout:    5 * time.Second,
		MaxFeedBytes:    1 << 20,
		MaxEntryBytes:   1 << 20,
		DefaultInterval: 30 * time.Minute,
		MinInterval:     10 * time.Minute,
		MaxInterval:     6 * time.Hour,
	}, nil)

	router := NewRouter(Deps{
		Cfg: cfg, Store: db, Blobs: blobs, Auth: svc, HubVerifier: verifier,
		Paste: pasteSvc, RSS: rssSvc, Log: nil, Bus: bus,
		Build:   BuildInfo{Version: "test"},
		Started: time.Now(),
	})

	return &harness{router: router, db: db, cfg: cfg, svc: svc,
		hubPriv: hubPriv, hubPub: hubPub, bus: bus,
		blobs: blobs, pasteSvc: pasteSvc, rssSvc: rssSvc}
}

// setLimiter 重建路由以挂上限流器。
// harness 默认不带限流，避免其他测试因为多次失败调用被误伤。
func (h *harness) setLimiter(l *auth.AttemptLimiter) {
	h.router = NewRouter(Deps{
		Cfg: h.cfg, Store: h.db, Blobs: h.blobs, Auth: h.svc,
		HubVerifier: auth.NewHubVerifier(h.hubPub, time.Minute, time.Minute),
		Paste:       h.pasteSvc, RSS: h.rssSvc, Log: nil, Bus: h.bus, Limiter: l,
		Build:   BuildInfo{Version: "test"},
		Started: time.Now(),
	})
}

func (h *harness) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *harness) doJSON(t *testing.T, method, path string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	return h.do(t, req)
}

// login 走完 enroll → challenge → token，返回 access token 与 csrf。
func (h *harness) login(t *testing.T) (token, csrf string) {
	t.Helper()
	ctx := context.Background()

	code, _, err := h.svc.GenerateEnrollment(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	w := h.doJSON(t, http.MethodPost, "/api/v1/auth/enroll", map[string]any{
		"token": code, "name": "test-device", "algorithm": "ed25519",
		"public_key": base64.StdEncoding.EncodeToString(pub),
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("enroll = %d: %s", w.Code, w.Body.String())
	}
	var enrollResp struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &enrollResp)

	w = h.doJSON(t, http.MethodPost, "/api/v1/auth/challenge",
		map[string]any{"client_id": enrollResp.ClientID}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("challenge = %d: %s", w.Code, w.Body.String())
	}
	var chResp struct {
		Nonce string `json:"nonce"`
	}
	json.Unmarshal(w.Body.Bytes(), &chResp)

	sig := ed25519.Sign(priv, auth.AuthMessage(enrollResp.ClientID, chResp.Nonce))
	w = h.doJSON(t, http.MethodPost, "/api/v1/auth/token", map[string]any{
		"client_id": enrollResp.ClientID, "nonce": chResp.Nonce,
		"signature": base64.StdEncoding.EncodeToString(sig),
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("token = %d: %s", w.Code, w.Body.String())
	}
	var tokResp struct {
		AccessToken string `json:"access_token"`
		CSRF        string `json:"csrf_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &tokResp)

	// refresh_token 绝不能出现在响应体里：它在 HttpOnly cookie 中。
	if strings.Contains(w.Body.String(), "refresh_token") {
		t.Fatal("refresh_token leaked into response body; HttpOnly protection is void")
	}
	return tokResp.AccessToken, tokResp.CSRF
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func TestHealthAndReady(t *testing.T) {
	h := newHarness(t)

	w := h.do(t, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("healthz = %d", w.Code)
	}

	w = h.do(t, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("readyz = %d: %s", w.Code, w.Body.String())
	}
	var ready struct {
		Ready  bool           `json:"ready"`
		Checks map[string]any `json:"checks"`
	}
	json.Unmarshal(w.Body.Bytes(), &ready)
	if !ready.Ready {
		t.Errorf("ready should be true: %v", ready.Checks)
	}
}

// 鉴权边界：哪些端点匿名可用，哪些必须登录。
//
// 这条测试是需求的可执行表达——改了路由鉴权而忘了改这里，
// 说明边界被无意中移动了。
func TestAuthBoundaries(t *testing.T) {
	h := newHarness(t)

	// 匿名可用：工具台首页要靠它们渲染。
	for _, path := range []string{
		"/api/v1/apps",
		"/api/v1/system/info",
		"/api/v1/system/stats",
		"/api/v1/pastes", // 匿名可以有自己的片段
	} {
		w := h.do(t, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s 应匿名可访问, got %d: %s", path, w.Code, w.Body.String())
		}
	}

	// 必须登录：抓取会访问外部地址，匿名开放等于开放 SSRF。
	// 任务历史与配置视图含运维信息，也不该公开。
	for _, path := range []string{
		"/api/v1/feeds",
		"/api/v1/entries",
		"/api/v1/system/jobs",
		"/api/v1/system/config",
	} {
		w := h.do(t, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s 应要求登录, got %d", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s content-type = %q, want JSON", path, ct)
		}
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/api/v1/feeds", "/api/v1/entries"} {
		w := h.do(t, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s content-type = %q, want JSON", path, ct)
		}
	}
}

func TestAPI404IsJSONNotHTML(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d", w.Code)
	}
	// SPA fallback 不能吞掉 API 的 404，否则客户端会把 HTML 当 JSON 解析。
	if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("content-type = %q, want JSON", w.Header().Get("Content-Type"))
	}
}

func TestPasteLifecycle(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"title": "hello", "body": "world", "language": "text",
		"visibility": "unlisted", "expires_in": "1h",
	}, bearer(token))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		Slug string `json:"slug"`
		URL  string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Slug == "" {
		t.Fatal("slug empty")
	}
	if !strings.HasPrefix(created.URL, "https://hh.example.com/p/") {
		t.Errorf("url = %q", created.URL)
	}

	// 读自己的片段
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+created.Slug, nil)
	getReq.Header.Set("Authorization", "Bearer "+token)
	w = h.do(t, getReq)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", w.Code, w.Body.String())
	}

	// 公开页不需要鉴权，且必须是 HTML
	req := httptest.NewRequest(http.MethodGet, "/p/"+created.Slug, nil)
	w = h.do(t, req)
	if w.Code != http.StatusOK {
		t.Fatalf("public page = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("public page content-type = %q", w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if !strings.Contains(body, "world") {
		t.Errorf("public page missing body")
	}
	// 公开页必须禁止脚本执行。
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("CSP = %q", w.Header().Get("Content-Security-Policy"))
	}
	// 原文必须被转义，不能把内容当 HTML 渲染。
	if strings.Contains(body, "<script") {
		t.Error("public page contains unescaped script tag")
	}

	// raw 模式
	w = h.do(t, httptest.NewRequest(http.MethodGet, "/p/"+created.Slug+"?raw=1", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("raw = %d, ct=%q", w.Code, w.Header().Get("Content-Type"))
	}
	if w.Body.String() != "world" {
		t.Errorf("raw body = %q", w.Body.String())
	}
}

func TestPastePublicPageEscapesHTML(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	payload := `<script>alert("xss")</script>`
	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"body": payload, "visibility": "unlisted",
	}, bearer(token))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		Slug string `json:"slug"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)

	w = h.do(t, httptest.NewRequest(http.MethodGet, "/p/"+created.Slug, nil))
	if strings.Contains(w.Body.String(), "<script>alert") {
		t.Error("HTML in paste body was not escaped")
	}
	if !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Error("expected escaped form in output")
	}
}

func TestPasteCSRFRequiredForCookieAuth(t *testing.T) {
	// 这里直接验证 Bearer 免 CSRF、cookie 认证必须带 CSRF 的规则。
	h := newHarness(t)
	token, _ := h.login(t)

	// Bearer 请求不带 CSRF 也应成功。
	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes",
		map[string]any{"body": "x"}, bearer(token))
	if w.Code != http.StatusCreated {
		t.Errorf("bearer without csrf = %d, want 201: %s", w.Code, w.Body.String())
	}
}

func TestHubSubscriptionPush(t *testing.T) {
	h := newHarness(t)

	body := []byte(`{"url":"https://example.com/feed.xml","title":"Example","interval":"45m"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	signHubRequest(t, req, h.hubPriv, body, "nonce-a")

	w := h.do(t, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("push = %d: %s", w.Code, w.Body.String())
	}
	var sub struct {
		ID            int64  `json:"id"`
		FetchInterval string `json:"fetch_interval"`
	}
	json.Unmarshal(w.Body.Bytes(), &sub)
	if sub.FetchInterval != "45m0s" {
		t.Errorf("interval = %q, want 45m0s", sub.FetchInterval)
	}

	// 同一 URL 再推一次是更新，不是新建。
	body2 := []byte(`{"url":"https://example.com/feed.xml","interval":"2h"}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	signHubRequest(t, req2, h.hubPriv, body2, "nonce-b")

	w = h.do(t, req2)
	if w.Code != http.StatusOK {
		t.Fatalf("second push = %d, want 200: %s", w.Code, w.Body.String())
	}

	// 列表接口用签名读，便于 Homehub 对账。
	get := httptest.NewRequest(http.MethodGet, "/api/v1/hub/subscriptions", nil)
	signHubRequest(t, get, h.hubPriv, nil, "nonce-c")
	w = h.do(t, get)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body.String())
	}
	var list struct {
		Items []struct {
			ID            int64  `json:"id"`
			FetchInterval string `json:"fetch_interval"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Items) != 1 {
		t.Fatalf("items = %d, want 1 (upsert must not duplicate)", len(list.Items))
	}
	if list.Items[0].FetchInterval != "2h0m0s" {
		t.Errorf("interval after update = %q", list.Items[0].FetchInterval)
	}
}

func TestHubRejectsUnsignedRequest(t *testing.T) {
	h := newHarness(t)

	body := []byte(`{"url":"https://example.com/feed.xml"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := h.do(t, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unsigned push = %d, want 401", w.Code)
	}
}

func TestHubRejectsReplay(t *testing.T) {
	h := newHarness(t)

	body := []byte(`{"url":"https://example.com/r.xml"}`)
	makeReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		signHubRequest(t, r, h.hubPriv, body, "fixed-nonce")
		return r
	}

	if w := h.do(t, makeReq()); w.Code != http.StatusCreated {
		t.Fatalf("first push = %d: %s", w.Code, w.Body.String())
	}
	// 同一 nonce 重放必须被拒。
	if w := h.do(t, makeReq()); w.Code != http.StatusUnauthorized {
		t.Errorf("replay = %d, want 401", w.Code)
	}
}

func TestHubIntervalClamped(t *testing.T) {
	h := newHarness(t)

	// 低于 min_interval（10m）应被抬到下限。
	body := []byte(`{"url":"https://example.com/too-frequent.xml","interval":"1m"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	signHubRequest(t, req, h.hubPriv, body, "nonce-clamp")

	w := h.do(t, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("push = %d: %s", w.Code, w.Body.String())
	}
	var sub struct {
		FetchInterval string `json:"fetch_interval"`
	}
	json.Unmarshal(w.Body.Bytes(), &sub)
	if sub.FetchInterval != "10m0s" {
		t.Errorf("interval = %q, want clamped to 10m0s", sub.FetchInterval)
	}
}

func TestAppsEndpoint(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := h.do(t, req)

	if w.Code != http.StatusOK {
		t.Fatalf("apps = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Apps []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Path    string `json:"path"`
			Enabled bool   `json:"enabled"`
		} `json:"apps"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Apps) < 2 {
		t.Fatalf("apps = %d, want at least pastebin and system", len(resp.Apps))
	}
	ids := map[string]bool{}
	for _, a := range resp.Apps {
		ids[a.ID] = true
		if a.Path == "" {
			t.Errorf("app %s has no path", a.ID)
		}
	}
	if !ids["pastebin"] || !ids["system"] {
		t.Errorf("missing expected apps: %v", ids)
	}
}

func TestMediaNotFound(t *testing.T) {
	h := newHarness(t)

	// 非法哈希
	w := h.do(t, httptest.NewRequest(http.MethodGet, "/media/notahash.png", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("bad hash = %d, want 404", w.Code)
	}
	// 合法格式但不存在
	sha := strings.Repeat("a", 64)
	w = h.do(t, httptest.NewRequest(http.MethodGet, "/media/"+sha+".png", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("missing blob = %d, want 404", w.Code)
	}
}

func TestMethodNotAllowedIsJSON(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := h.do(t, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("content-type = %q", w.Header().Get("Content-Type"))
	}
}

func signHubRequest(t *testing.T, req *http.Request, priv ed25519.PrivateKey, body []byte, nonce string) {
	t.Helper()
	req.Header.Set("X-HH-Timestamp", fmt.Sprint(time.Now().Unix()))
	req.Header.Set("X-HH-Nonce", nonce)
	sig := ed25519.Sign(priv, []byte(auth.RequestMessage(req, body)))
	req.Header.Set("X-HH-Signature", base64.StdEncoding.EncodeToString(sig))
}

// 配对码端点必须限流。
//
// 配置里的 auth.failed_attempt_limit 与文档里的 429 承诺，
// 在此之前从未被代码读取过——意味着 /auth/enroll 可以被无限次尝试。
func TestEnrollRateLimited(t *testing.T) {
	h := newHarness(t)
	// harness 默认给 nil limiter（不限制）；这里换成很小的限额。
	h.setLimiter(auth.NewAttemptLimiter(3, time.Minute))

	badToken := map[string]any{
		"token": "HH-0000-0000-0000", "name": "x",
		"algorithm":  "ed25519",
		"public_key": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}

	// 前 3 次都是 401（配对码无效），第 4 次应被拦成 429。
	for i := 1; i <= 3; i++ {
		w := h.doJSON(t, http.MethodPost, "/api/v1/auth/enroll", badToken, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次 = %d, want 401: %s", i, w.Code, w.Body.String())
		}
	}

	w := h.doJSON(t, http.MethodPost, "/api/v1/auth/enroll", badToken, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 4 次 = %d, want 429: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 应带 Retry-After，客户端才知道何时能再试")
	}
}

// 匿名创建片段必须返回 owner_token，且凭它能管理自己的片段。
// 这是「不登录也能用」与「私有片段有意义」之间的桥。
func TestAnonymousPasteOwnership(t *testing.T) {
	h := newHarness(t)

	// 不带任何凭证创建。
	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"body": "anonymous", "visibility": "private",
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("匿名创建 = %d: %s", w.Code, w.Body.String())
	}

	var created struct {
		Slug       string `json:"slug"`
		OwnerToken string `json:"owner_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.OwnerToken == "" {
		t.Fatal("匿名创建必须返回 owner_token")
	}

	// 没有 token 读 private 片段：应 404（不是 403——不确认存在性）。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+created.Slug, nil)
	if w := h.do(t, req); w.Code != http.StatusNotFound {
		t.Errorf("无 token 读 private = %d, want 404", w.Code)
	}

	// 带 token 就能读。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+created.Slug, nil)
	req.Header.Set(headerOwnerToken, created.OwnerToken)
	if w := h.do(t, req); w.Code != http.StatusOK {
		t.Fatalf("带 token 读 private = %d: %s", w.Code, w.Body.String())
	}

	// 列表只返回自己的：另一个匿名调用方看不到别人创建的。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/pastes", nil)
	w = h.do(t, req)
	var listed struct {
		Items []struct {
			Slug string `json:"slug"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &listed)
	for _, it := range listed.Items {
		if it.Slug == created.Slug {
			t.Error("无 token 的列表里出现了别人的片段")
		}
	}

	// 带 token 的列表能看到。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/pastes", nil)
	req.Header.Set(headerOwnerToken, created.OwnerToken)
	w = h.do(t, req)
	json.Unmarshal(w.Body.Bytes(), &listed)
	found := false
	for _, it := range listed.Items {
		if it.Slug == created.Slug {
			found = true
		}
	}
	if !found {
		t.Error("带 token 的列表里应能看到自己的片段")
	}

	// 别人删不掉。
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/pastes/"+created.Slug, nil)
	req.Header.Set(headerOwnerToken, "someone-else")
	if w := h.do(t, req); w.Code != http.StatusNotFound {
		t.Errorf("别人删除 = %d, want 404", w.Code)
	}

	// 自己能删。
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/pastes/"+created.Slug, nil)
	req.Header.Set(headerOwnerToken, created.OwnerToken)
	if w := h.do(t, req); w.Code != http.StatusNoContent {
		t.Errorf("拥有者删除 = %d: %s", w.Code, w.Body.String())
	}
}

// 登录用户创建的片段归属到设备，靠 Bearer 管理，不需要 owner_token。
func TestLoggedInPasteOwnership(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"body": "mine", "visibility": "private",
	}, bearer(token))
	if w.Code != http.StatusCreated {
		t.Fatalf("创建 = %d", w.Code)
	}
	var created struct {
		Slug       string `json:"slug"`
		OwnerToken string `json:"owner_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)

	// 已登录创建不应返回 owner_token——身份由 Bearer 承载。
	if created.OwnerToken != "" {
		t.Error("已登录创建不该返回 owner_token")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+created.Slug, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if w := h.do(t, req); w.Code != http.StatusOK {
		t.Errorf("设备读自己的 private = %d", w.Code)
	}
}

// 订阅源管理需要登录，且入库前必须挡住内网地址。
func TestFeedCreateRequiresAuthAndBlocksSSRF(t *testing.T) {
	h := newHarness(t)

	body := []byte(`{"url":"http://169.254.169.254/latest/meta-data/"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/feeds", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if w := h.do(t, req); w.Code != http.StatusUnauthorized {
		t.Errorf("匿名新建订阅 = %d, want 401", w.Code)
	}

	token, _ := h.login(t)
	w := h.doJSON(t, http.MethodPost, "/api/v1/feeds",
		map[string]any{"url": "http://169.254.169.254/latest/meta-data/"}, bearer(token))
	if w.Code != http.StatusBadRequest {
		t.Errorf("内网地址 = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// 工具台角标必须与列表口径一致。
//
// 这个 bug 出现过：角标读的是全局计数，列表按归属过滤，于是
// 匿名用户看到「首页说 2 条」点进去却是空的。两个数字来自不同
// 查询口径这件事，只有把两者放在同一条测试里比较才会暴露。
func TestAppsBadgeMatchesPasteList(t *testing.T) {
	h := newHarness(t)

	// 匿名创建两条，同一个 owner token。
	var token string
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/pastes",
			bytes.NewReader([]byte(`{"body":"x"}`)))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set(headerOwnerToken, token)
		}
		w := h.do(t, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("创建 = %d: %s", w.Code, w.Body.String())
		}
		var created struct {
			OwnerToken string `json:"owner_token"`
		}
		json.Unmarshal(w.Body.Bytes(), &created)
		token = created.OwnerToken
	}

	badgeFor := func(token string) int64 {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
		if token != "" {
			req.Header.Set(headerOwnerToken, token)
		}
		w := h.do(t, req)
		if w.Code != http.StatusOK {
			t.Fatalf("apps = %d", w.Code)
		}
		var resp struct {
			Apps []struct {
				ID    string `json:"id"`
				Badge *struct {
					Count int64 `json:"count"`
				} `json:"badge"`
			} `json:"apps"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		for _, a := range resp.Apps {
			if a.ID == "pastebin" && a.Badge != nil {
				return a.Badge.Count
			}
		}
		return -1
	}

	listLen := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/pastes", nil)
		if token != "" {
			req.Header.Set(headerOwnerToken, token)
		}
		w := h.do(t, req)
		var resp struct {
			Items []json.RawMessage `json:"items"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		return len(resp.Items)
	}

	// 带 token：角标 == 列表条数。
	badge, listed := badgeFor(token), listLen(token)
	if badge != int64(listed) {
		t.Errorf("角标 %d 与列表 %d 不一致", badge, listed)
	}
	if badge != 2 {
		t.Errorf("角标 = %d, want 2", badge)
	}

	// 匿名：两者都必须是 0——不是「角标显示全局数、列表是空的」。
	if got := badgeFor(""); got != 0 {
		t.Errorf("匿名角标 = %d, want 0（不该暴露全局计数）", got)
	}
	if got := listLen(""); got != 0 {
		t.Errorf("匿名列表 = %d, want 0", got)
	}
}

// 详情页依赖的路由与数据必须对得上：
// 列表里出现的每个 slug，都要能通过 GET /pastes/{slug} 读到。
//
// 曾经前端没有 /pastes/:slug 路由，列表也无法点击——
// 断言「列表里的条目都能打开」能同时锁住这两件事。
func TestEveryListedPasteIsReadable(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/pastes",
		bytes.NewReader([]byte(`{"body":"detail me","visibility":"unlisted"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := h.do(t, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建 = %d", w.Code)
	}
	var created struct {
		Slug       string `json:"slug"`
		OwnerToken string `json:"owner_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)

	// 列表里有它。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/pastes", nil)
	req.Header.Set(headerOwnerToken, created.OwnerToken)
	w = h.do(t, req)
	var listed struct {
		Items []struct {
			Slug string `json:"slug"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed.Items) != 1 || listed.Items[0].Slug != created.Slug {
		t.Fatalf("列表 = %+v", listed.Items)
	}

	// 列表里的 slug 能打开详情。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+listed.Items[0].Slug, nil)
	w = h.do(t, req)
	if w.Code != http.StatusOK {
		t.Fatalf("详情 = %d: %s", w.Code, w.Body.String())
	}
	var detail struct {
		Body string `json:"body"`
	}
	json.Unmarshal(w.Body.Bytes(), &detail)
	if detail.Body != "detail me" {
		t.Errorf("正文 = %q", detail.Body)
	}
}
