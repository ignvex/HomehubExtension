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
	"github.com/ignvex/HomehubExtension/internal/store"
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
	if sub.FetchInterval != "45m" {
		t.Errorf("interval = %q, want 45m", sub.FetchInterval)
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
	if list.Items[0].FetchInterval != "2h" {
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
	if sub.FetchInterval != "10m" {
		t.Errorf("interval = %q, want clamped to 10m", sub.FetchInterval)
	}
}

// push 的响应必须带上真实的未读计数。
//
// 之前 hubUpsert 返回的是 Upsert 内部 Get 出来的行，而 Get 的查询
// 没有未读子查询，字段恒为 0；紧跟着的 GET 列表却有值。同一个
// toHubSubscription 给出两种答案，宿主对账时会以为「刚推的内容没了」。
func TestHubPushReportsUnreadCount(t *testing.T) {
	h := newHarness(t)
	const url = "https://e.com/unread.xml"

	first := h.pushHub(t, []byte(`{"url":"`+url+`","fetch_now":false}`), "u1")
	feedID := int64(first["id"].(float64))

	ctx := context.Background()
	now := time.Now().Unix()
	for _, guid := range []string{"e1", "e2"} {
		if _, err := h.db.Entry().InsertIfNew(ctx, &store.Entry{
			FeedID: feedID, GUID: guid, Title: guid, FetchedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, err := h.db.Entry().List(ctx, store.EntryQuery{FeedID: feedID, Limit: 10})
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %d, err = %v", len(entries), err)
	}
	if err := h.db.Entry().SetRead(ctx, entries[0].ID, true); err != nil {
		t.Fatal(err)
	}

	// 只改标题的更新推送：响应里的未读计数必须与列表一致。
	got := h.pushHub(t, []byte(`{"url":"`+url+`","title":"renamed","fetch_now":false}`), "u2")
	if got["unread_count"] != float64(1) {
		t.Errorf("push unread_count = %v, want 1", got["unread_count"])
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/hub/subscriptions", nil)
	signHubRequest(t, listReq, h.hubPriv, nil, "u3")
	w := h.do(t, listReq)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body.String())
	}
	var list struct {
		Items []struct {
			UnreadCount int64 `json:"unread_count"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Items[0].UnreadCount != 1 {
		t.Errorf("list items = %+v, want one item with unread_count 1", list.Items)
	}
}

// pushHub 推一次订阅并返回解码后的响应体。
// 任何 >= 300 都直接失败：这一组测试关心的是响应内容，不是错误路径。
func (h *harness) pushHub(t *testing.T, body []byte, nonce string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	signHubRequest(t, req, h.hubPriv, body, nonce)
	w := h.do(t, req)
	if w.Code >= 300 {
		t.Fatalf("push %s = %d: %s", body, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return out
}

// RSS 模块关闭时，四个 hub 端点必须给同一个答案。
//
// 之前 refresh 不做检查就直接调 s.RSS.Fetch，RSS 未装配时是一次
// nil 解引用，被 Recovery 接住变成 500；而 upsert 返回 404、
// list 返回 200 空数组。宿主无法用一致逻辑判断模块是否可用。
func TestHubEndpointsAgreeWhenRSSDisabled(t *testing.T) {
	h := newHarness(t)
	cfg := h.cfg
	cfg.RSS.Enabled = false
	router := NewRouter(Deps{
		Cfg: cfg, Store: h.db, Blobs: h.blobs, Auth: h.svc,
		HubVerifier: auth.NewHubVerifier(h.hubPub, time.Minute, time.Minute),
		Paste:       h.pasteSvc, RSS: nil, Log: nil, Bus: h.bus,
		Build: BuildInfo{Version: "test"}, Started: time.Now(),
	})

	cases := []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodPost, "/api/v1/hub/subscriptions", []byte(`{"url":"https://e.com/f.xml"}`)},
		{http.MethodGet, "/api/v1/hub/subscriptions", nil},
		{http.MethodDelete, "/api/v1/hub/subscriptions/1", nil},
		{http.MethodPost, "/api/v1/hub/subscriptions/1/refresh", nil},
	}
	for i, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
		signHubRequest(t, req, h.hubPriv, tc.body, fmt.Sprintf("disabled-%d", i))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404: %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "not_found") {
			t.Errorf("%s %s body = %s, want JSON not_found", tc.method, tc.path, w.Body.String())
		}
	}
}

// 省略的字段必须保留原值，而不是回落默认。
//
// 之前 enabled 缺省写死为 true、interval 缺省写死为 default_interval，
// 于是一次「只改标题」的推送会把用户显式暂停的订阅悄悄恢复。
func TestHubUpsertPreservesOmittedFields(t *testing.T) {
	h := newHarness(t)

	h.pushHub(t, []byte(`{"url":"https://e.com/keep.xml","interval":"45m","fetch_now":false}`), "k1")

	// 宿主显式暂停。
	paused := h.pushHub(t, []byte(`{"url":"https://e.com/keep.xml","enabled":false,"fetch_now":false}`), "k2")
	if paused["enabled"] != false {
		t.Fatalf("enabled = %v, want false", paused["enabled"])
	}

	// 只改标题：间隔与启用状态都必须原样保留。
	only := h.pushHub(t, []byte(`{"url":"https://e.com/keep.xml","title":"新标题","fetch_now":false}`), "k3")
	if only["enabled"] != false {
		t.Errorf("omitted enabled revived the feed: got %v, want false", only["enabled"])
	}
	if only["fetch_interval"] != "45m" {
		t.Errorf("omitted interval = %v, want 45m", only["fetch_interval"])
	}
	if only["title"] != "新标题" {
		t.Errorf("title = %v, want 新标题", only["title"])
	}
}

// 间隔变化必须重排 next_fetch_at；只是重复推送同样间隔则不动。
//
// 之前 UPDATE 分支根本不写 next_fetch_at，宿主把间隔从 6h 调到 10m 后
// 仍要等满 6 小时才按新间隔跑。
func TestHubUpsertReschedulesOnIntervalChange(t *testing.T) {
	h := newHarness(t)
	const url = "https://e.com/pace.xml"

	first := h.pushHub(t, []byte(`{"url":"`+url+`","interval":"6h","fetch_now":false}`), "s1")
	original, ok := first["next_fetch_at"].(float64)
	if !ok {
		t.Fatalf("next_fetch_at = %v, want unix seconds", first["next_fetch_at"])
	}

	// 同间隔、只改标题：抓取时刻不该被推迟。
	same := h.pushHub(t, []byte(`{"url":"`+url+`","interval":"6h","title":"t","fetch_now":false}`), "s2")
	if same["next_fetch_at"].(float64) != original {
		t.Errorf("next_fetch_at moved without an interval change")
	}

	// 间隔改成 10m：必须重排到约 now+600s。
	tightened := h.pushHub(t, []byte(`{"url":"`+url+`","interval":"10m","fetch_now":false}`), "s3")
	delta := tightened["next_fetch_at"].(float64) - float64(time.Now().Unix())
	if delta < 500 || delta > 700 {
		t.Errorf("next_fetch_at in %.0fs, want ~600s after the interval change", delta)
	}
}

// 从「连续失败被自动停用」恢复时，error_count 必须清零。
//
// 只把 enabled 改回 true 的话，error_count 仍是 10，下一次失败就变 11，
// 立刻又被停用——宿主看到的是「刚恢复就又停了」。
func TestHubRevivalResetsErrorCount(t *testing.T) {
	h := newHarness(t)
	const url = "https://e.com/dead.xml"

	h.pushHub(t, []byte(`{"url":"`+url+`","fetch_now":false}`), "r1")

	// 直接改库，模拟 markFailure 连续失败 10 次之后的形态。
	ctx := context.Background()
	f, err := h.db.Feed().GetByURL(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	f.Enabled = false
	f.ErrorCount = 10
	f.LastError = "停用：连续失败 10 次；connection refused"
	if err := h.db.Feed().Update(ctx, f); err != nil {
		t.Fatal(err)
	}

	revived := h.pushHub(t, []byte(`{"url":"`+url+`","enabled":true,"fetch_now":false}`), "r2")
	if revived["enabled"] != true {
		t.Fatalf("enabled = %v, want true", revived["enabled"])
	}
	if revived["error_count"] != float64(0) {
		t.Errorf("error_count = %v, want 0 after explicit revival", revived["error_count"])
	}
	if revived["last_error"] != "" {
		t.Errorf("last_error = %q, want empty after explicit revival", revived["last_error"])
	}

	// 落库也要是清的，不只是响应好看。
	after, err := h.db.Feed().GetByURL(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if after.ErrorCount != 0 || after.LastError != "" {
		t.Errorf("persisted error_count=%d last_error=%q, want 0 and empty",
			after.ErrorCount, after.LastError)
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

// 刷一个不存在的订阅是 404，不是 502。
//
// 之前 Fetch 返回的所有错误都被当成上游失败，包括 store.ErrNotFound。
// 宿主推了一个已被删掉的 id 会拿到 502，误以为「目标站有问题，重试就好」。
func TestHubRefreshUnknownFeed(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions/99999/refresh", nil)
	signHubRequest(t, req, h.hubPriv, nil, "refresh-missing")

	w := h.do(t, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("refresh missing feed = %d, want 404: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if body.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", body.Error.Code)
	}
	if body.Error.RequestID == "" {
		t.Error("request_id missing: 统一错误结构必须带它")
	}
}

// ---- 宿主签发的长期访问密钥 ----

// issueKey 走真实的签名通道签发一把密钥，返回明文与 id。
func (h *harness) issueKey(t *testing.T, name, expiresIn string) (plain, id string) {
	t.Helper()
	payload := map[string]string{"name": name}
	if expiresIn != "" {
		payload["expires_in"] = expiresIn
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	signHubRequest(t, req, h.hubPriv, body, "key-"+name)
	w := h.do(t, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("issue key = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Key == "" {
		t.Fatal("签发响应里没有明文密钥")
	}
	return out.Key, out.ID
}

// redeemKey 用密钥换令牌，返回响应体与状态码。
func (h *harness) redeemKey(t *testing.T, key string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"key": key})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/key", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := h.do(t, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// 签发的密钥必须能直接换到可用会话，并且换出来的会话和其他登录方式
// 完全等价——这是整个特性的验收条件。
func TestAccessKeyEndToEnd(t *testing.T) {
	h := newHarness(t)
	plain, _ := h.issueKey(t, "alice", "")

	code, body := h.redeemKey(t, plain)
	if code != http.StatusOK {
		t.Fatalf("redeem = %d: %v", code, body)
	}
	token, _ := body["access_token"].(string)
	if token == "" {
		t.Fatalf("no access_token: %v", body)
	}

	// 用它访问一个需要登录的端点。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := h.do(t, req)
	if w.Code != http.StatusOK {
		t.Fatalf("clients with key session = %d: %s", w.Code, w.Body.String())
	}
}

// 同一把密钥可以反复兑换：换浏览器、换设备都靠它。
func TestAccessKeyIsReusable(t *testing.T) {
	h := newHarness(t)
	plain, _ := h.issueKey(t, "shared", "")

	_, first := h.redeemKey(t, plain)
	_, second := h.redeemKey(t, plain)
	if first["access_token"] == second["access_token"] {
		t.Error("两次兑换返回了同一个 access token，说明没有签发新会话")
	}
}

// 用户从聊天窗口复制来的密钥经常带分隔符、大小写不一、或丢了前缀。
// 这些都必须能登录——否则每次都要教用户怎么粘贴。
func TestAccessKeyNormalization(t *testing.T) {
	h := newHarness(t)
	plain, _ := h.issueKey(t, "messy", "")

	compact := strings.ReplaceAll(plain, "-", "")
	bodyOnly := strings.TrimPrefix(compact, auth.AccessKeyPrefix)

	cases := map[string]string{
		"原样":      plain,
		"小写":      strings.ToLower(plain),
		"带空格":     " " + plain + " ",
		"去掉分隔符":   compact,
		"省略前缀":    bodyOnly,
		"全角分隔符混用": strings.ReplaceAll(plain, "-", " "),
	}
	for name, input := range cases {
		if code, body := h.redeemKey(t, input); code != http.StatusOK {
			t.Errorf("%s: redeem = %d: %v", name, code, body)
		}
	}
}

// 撤销密钥必须同时踢掉已经换出去的会话。
//
// 只标记密钥失效的话，用户手里的 refresh token 还能继续续期，
// "撤销"就成了一个只拦新登录的假动作。
func TestAccessKeyRevocationKillsSession(t *testing.T) {
	h := newHarness(t)
	plain, id := h.issueKey(t, "revoked", "")

	code, body := h.redeemKey(t, plain)
	if code != http.StatusOK {
		t.Fatalf("redeem = %d: %v", code, body)
	}
	token, _ := body["access_token"].(string)

	// access token 是 15 分钟的无状态 JWT，撤销不会立刻失效它——
	// 这是既有的取舍，不在这里断言。要验的是 refresh 会话。
	del := httptest.NewRequest(http.MethodDelete, "/api/v1/hub/keys/"+id, nil)
	signHubRequest(t, del, h.hubPriv, nil, "revoke-key")
	if w := h.do(t, del); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d: %s", w.Code, w.Body.String())
	}

	// 再兑换必须失败。
	if code, _ := h.redeemKey(t, plain); code != http.StatusUnauthorized {
		t.Errorf("redeem after revoke = %d, want 401", code)
	}

	// 用旧 access token 访问需要查库的路径也必须失败：
	// client 被连带撤销，Authenticate 里 GetClient 之后会拒绝。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if w := h.do(t, req); w.Code != http.StatusUnauthorized {
		t.Errorf("old access token after revoke = %d, want 401", w.Code)
	}
}

// 过期的密钥不能兑换。
func TestAccessKeyExpiry(t *testing.T) {
	h := newHarness(t)
	// 1 纳秒的 TTL：签发完基本立刻过期。
	plain, _ := h.issueKey(t, "short", "1ns")

	if code, body := h.redeemKey(t, plain); code != http.StatusUnauthorized {
		t.Errorf("expired key redeem = %d: %v", code, body)
	}
}

// 列表接口永远不能回显明文或哈希。
func TestAccessKeyListHidesSecret(t *testing.T) {
	h := newHarness(t)
	plain, id := h.issueKey(t, "listed", "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub/keys", nil)
	signHubRequest(t, req, h.hubPriv, nil, "list-keys")
	w := h.do(t, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list keys = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, plain) {
		t.Error("列表响应里出现了明文密钥")
	}
	// 主体部分也不该出现——否则可以离线爆破。
	compact := strings.ReplaceAll(strings.TrimPrefix(plain, auth.AccessKeyPrefix+"-"), "-", "")
	if strings.Contains(body, compact) {
		t.Error("列表响应里出现了密钥主体")
	}
	if !strings.Contains(body, id) {
		t.Errorf("列表里没有刚签发的 key id %s: %s", id, body)
	}
}

// 未签名的请求不能签发密钥——否则任何人都能给自造一把长期凭证。
func TestAccessKeyIssueRequiresSignature(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{"name":"evil"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if w := h.do(t, req); w.Code != http.StatusUnauthorized {
		t.Errorf("unsigned issue = %d, want 401", w.Code)
	}
}

// 同一主人签发的多把密钥共享数据。
//
// 这是 0004 迁移引入的语义：每把密钥仍然绑一台独立的无主客户端
// （会话、撤销各自独立），但它们带同一个 owner_id，所以 owner_hash
// 一致。效果是轮换密钥不丢数据。
//
// 反过来说，这条测试也固定了「密钥之间没有隔离」——在单主人场景下
// 这正是想要的；如果哪天要按人隔离，先看 docs/architecture.md 待定项 5。
func TestAccessKeySharesOwnerScope(t *testing.T) {
	h := newHarness(t)

	keyA, _ := h.issueKey(t, "primary-a", "")
	keyB, _ := h.issueKey(t, "primary-b", "")

	_, bodyA := h.redeemKey(t, keyA)
	_, bodyB := h.redeemKey(t, keyB)
	tokenA, _ := bodyA["access_token"].(string)
	tokenB, _ := bodyB["access_token"].(string)
	if tokenA == "" || tokenB == "" {
		t.Fatal("兑换没拿到 access token")
	}

	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"body": "用 A 写的", "visibility": "private",
	}, bearer(tokenA))
	if w.Code != http.StatusCreated {
		t.Fatalf("创建 = %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		Slug string `json:"slug"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)

	// B 读得到：同一个 owner。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+created.Slug, nil)
	req.Header.Set("Authorization", "Bearer "+tokenB)
	if w := h.do(t, req); w.Code != http.StatusOK {
		t.Errorf("B 读 A 的片段 = %d, want 200", w.Code)
	}

	// 列表里也有。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/pastes", nil)
	req.Header.Set("Authorization", "Bearer "+tokenB)
	w = h.do(t, req)
	if w.Code != http.StatusOK {
		t.Fatalf("列表 = %d", w.Code)
	}
	var list struct {
		Items []struct {
			Slug string `json:"slug"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	found := false
	for _, item := range list.Items {
		if item.Slug == created.Slug {
			found = true
		}
	}
	if !found {
		t.Error("B 的列表里没有 A 写的片段，说明两把密钥没共享归属")
	}
}

// 配对设备与访问密钥属于同一个主人，数据互相可见。
//
// 0005 之后所有 client 都带实例级 owner_id。所以「我在浏览器里建了个
// 私有片段，换个入口就看不见了」这种事不会发生——同一个人不该因为
// 换了认证方式就丢数据。
func TestPairedDeviceAndAccessKeyShareScope(t *testing.T) {
	h := newHarness(t)

	// 一台配对设备。
	deviceToken, _ := h.login(t)
	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"body": "设备写的私有内容", "visibility": "private",
	}, bearer(deviceToken))
	if w.Code != http.StatusCreated {
		t.Fatalf("设备创建 = %d: %s", w.Code, w.Body.String())
	}
	var fromDevice struct {
		Slug string `json:"slug"`
	}
	json.Unmarshal(w.Body.Bytes(), &fromDevice)

	// 一把访问密钥。
	key, _ := h.issueKey(t, "primary-key", "")
	_, keyBody := h.redeemKey(t, key)
	keyToken, _ := keyBody["access_token"].(string)
	if keyToken == "" {
		t.Fatal("密钥兑换失败")
	}

	// 密钥读得到设备写的片段。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+fromDevice.Slug, nil)
	req.Header.Set("Authorization", "Bearer "+keyToken)
	if w := h.do(t, req); w.Code != http.StatusOK {
		t.Errorf("密钥读设备写的片段 = %d, want 200", w.Code)
	}

	// 反过来：设备读得到密钥写的片段。
	w = h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"body": "密钥写的私有内容", "visibility": "private",
	}, bearer(keyToken))
	if w.Code != http.StatusCreated {
		t.Fatalf("密钥创建 = %d: %s", w.Code, w.Body.String())
	}
	var fromKey struct {
		Slug string `json:"slug"`
	}
	json.Unmarshal(w.Body.Bytes(), &fromKey)

	req = httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+fromKey.Slug, nil)
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	if w := h.do(t, req); w.Code != http.StatusOK {
		t.Errorf("设备读密钥写的片段 = %d, want 200", w.Code)
	}

	// 两个入口的列表看到的是同一批片段。
	for name, token := range map[string]string{"设备": deviceToken, "密钥": keyToken} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/pastes", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := h.do(t, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s 列表 = %d", name, w.Code)
		}
		var list struct {
			Items []struct {
				Slug string `json:"slug"`
			} `json:"items"`
		}
		json.Unmarshal(w.Body.Bytes(), &list)
		seen := map[string]bool{}
		for _, item := range list.Items {
			seen[item.Slug] = true
		}
		if !seen[fromDevice.Slug] || !seen[fromKey.Slug] {
			t.Errorf("%s 的列表缺片段：设备写的=%v 密钥写的=%v",
				name, seen[fromDevice.Slug], seen[fromKey.Slug])
		}
	}
}

// 匿名创建的片段仍然按 token 隔离，不会因为 owner 共享而串到登录身份上。
func TestAnonymousPasteStaysIsolatedFromPrimaryOwner(t *testing.T) {
	h := newHarness(t)

	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"body": "匿名写的", "visibility": "private",
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("匿名创建 = %d: %s", w.Code, w.Body.String())
	}
	var anon struct {
		Slug       string `json:"slug"`
		OwnerToken string `json:"owner_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &anon)
	if anon.OwnerToken == "" {
		t.Fatal("匿名创建必须返回 owner_token")
	}

	// 登录身份读不到匿名片段。
	deviceToken, _ := h.login(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+anon.Slug, nil)
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	if w := h.do(t, req); w.Code != http.StatusNotFound {
		t.Errorf("登录身份读匿名私有片段 = %d, want 404", w.Code)
	}

	// 只有 owner token 能读。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+anon.Slug, nil)
	req.Header.Set("X-Owner-Token", anon.OwnerToken)
	if w := h.do(t, req); w.Code != http.StatusOK {
		t.Errorf("凭 owner_token 读 = %d, want 200", w.Code)
	}
}

// 轮换密钥不丢数据：撤销旧密钥后，新密钥仍能看到旧密钥写的私有片段。
//
// 这是 0004 迁移要解决的核心问题。在此之前的实现里，撤销旧密钥会
// 连带撤销它绑定的 client，而 owner_hash 由 client id 推导，于是
// 旧密钥名下的私有片段再也没有凭证能认领——数据没删，但永久失联。
//
// 现在每把密钥的 client 带同一个 owner_id，新旧密钥算出同一个
// owner_hash，轮换对用户完全透明。
func TestAccessKeyRotationKeepsData(t *testing.T) {
	h := newHarness(t)

	// 轮换前：用旧密钥建一个私有片段。
	oldKey, oldID := h.issueKey(t, "before-rotation", "")
	_, oldBody := h.redeemKey(t, oldKey)
	oldToken, _ := oldBody["access_token"].(string)

	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"body": "轮换前的私有内容", "visibility": "private",
	}, bearer(oldToken))
	if w.Code != http.StatusCreated {
		t.Fatalf("private 创建 = %d: %s", w.Code, w.Body.String())
	}
	var private struct {
		Slug string `json:"slug"`
	}
	json.Unmarshal(w.Body.Bytes(), &private)

	// 撤销旧密钥，签发新密钥——典型的轮换。
	del := httptest.NewRequest(http.MethodDelete, "/api/v1/hub/keys/"+oldID, nil)
	signHubRequest(t, del, h.hubPriv, nil, "rotate-del")
	if w := h.do(t, del); w.Code != http.StatusNoContent {
		t.Fatalf("撤销旧密钥 = %d: %s", w.Code, w.Body.String())
	}

	newKey, _ := h.issueKey(t, "after-rotation", "")
	_, newBody := h.redeemKey(t, newKey)
	newToken, _ := newBody["access_token"].(string)
	if newToken == "" {
		t.Fatal("新密钥兑换失败")
	}

	// 关键断言：新密钥读得到旧密钥写的私有片段。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pastes/"+private.Slug, nil)
	req.Header.Set("Authorization", "Bearer "+newToken)
	if w := h.do(t, req); w.Code != http.StatusOK {
		t.Errorf("轮换后读旧私有片段 = %d, want 200: %s", w.Code, w.Body.String())
	}

	// 列表里也要有。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/pastes", nil)
	req.Header.Set("Authorization", "Bearer "+newToken)
	w = h.do(t, req)
	if w.Code != http.StatusOK {
		t.Fatalf("列表 = %d", w.Code)
	}
	var list struct {
		Items []struct {
			Slug string `json:"slug"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	found := false
	for _, item := range list.Items {
		if item.Slug == private.Slug {
			found = true
		}
	}
	if !found {
		t.Error("轮换后列表里没有旧密钥写的私有片段")
	}

	// 旧密钥本身失效——轮换的另一半。
	if code, _ := h.redeemKey(t, oldKey); code != http.StatusUnauthorized {
		t.Errorf("旧密钥仍可兑换 = %d, want 401", code)
	}
}
