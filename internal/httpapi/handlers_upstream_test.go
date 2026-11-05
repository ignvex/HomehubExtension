package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/auth"
	"github.com/ignvex/HomehubExtension/internal/upstream"
)

type fakeUpstream struct {
	feed      upstream.FeedResponse
	feedErr   error
	avatar    upstream.AvatarResponse
	avatarErr error

	gotFeedURL string
	gotAvatar  upstream.AvatarRequest
}

func (f *fakeUpstream) FetchFeed(_ context.Context, rawURL string) (upstream.FeedResponse, error) {
	f.gotFeedURL = rawURL
	return f.feed, f.feedErr
}

func (f *fakeUpstream) FetchAvatar(_ context.Context, req upstream.AvatarRequest) (upstream.AvatarResponse, error) {
	f.gotAvatar = req
	return f.avatar, f.avatarErr
}

func (h *harness) useUpstream(u Upstream) {
	h.router = NewRouter(Deps{
		Cfg: h.cfg, Store: h.db, Blobs: h.blobs, Auth: h.svc,
		HubVerifier: auth.NewHubVerifier(h.hubPub, time.Minute, time.Minute),
		Paste:       h.pasteSvc, RSS: h.rssSvc, Log: nil, Bus: h.bus,
		Upstream: u,
		Build:    BuildInfo{Version: "test"},
		Started:  time.Now(),
	})
}

func signedGET(t *testing.T, h *harness, path, nonce string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	signHubRequest(t, req, h.hubPriv, nil, nonce)
	return h.do(t, req)
}

// 未签名请求必须是 401 JSON，而不是落到 SPA 首页。
// 首页是 200 text/html——宿主的能力探测会把「404 或 HTML」判成端点不存在。
func TestUpstreamRoutesRequireSignature(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/api/v1/fetch?url=https%3A%2F%2Fexample.com%2Ffeed.xml",
		"/api/v1/avatar/00000000000000000000000000000000",
	} {
		w := h.do(t, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s: content type = %q, want JSON", path, ct)
		}
	}
}

// 宿主探测 /fetch 时不带 url，期望 400；404 会被判成端点不存在。
func TestFetchProxyMissingURLIsBadRequest(t *testing.T) {
	h := newHarness(t)
	w := signedGET(t, h, "/api/v1/fetch", "fetch-missing-url", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content type = %q, want JSON", ct)
	}
	var body ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "invalid_request" || body.Error.RequestID == "" {
		t.Errorf("error = %+v", body.Error)
	}
}

func TestFetchProxyReturnsUpstreamBytes(t *testing.T) {
	h := newHarness(t)
	fake := &fakeUpstream{feed: upstream.FeedResponse{
		Status:      http.StatusOK,
		ContentType: "application/rss+xml",
		Body:        []byte(`<rss version="2.0"></rss>`),
	}}
	h.useUpstream(fake)

	target := "https://example.com/feed.xml"
	path := "/api/v1/fetch?" + url.Values{"url": {target}}.Encode()
	w := signedGET(t, h, path, "fetch-ok", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `<rss version="2.0"></rss>` {
		t.Errorf("body = %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/rss+xml" {
		t.Errorf("content type = %q", ct)
	}
	if fake.gotFeedURL != target {
		t.Errorf("upstream got %q", fake.gotFeedURL)
	}
}

func TestFetchProxyPassesUpstreamStatus(t *testing.T) {
	h := newHarness(t)
	h.useUpstream(&fakeUpstream{feed: upstream.FeedResponse{
		Status:     http.StatusTooManyRequests,
		RetryAfter: "30",
	}})

	w := signedGET(t, h, "/api/v1/fetch?url=https%3A%2F%2Fexample.com%2Ffeed.xml",
		"fetch-429", nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q", got)
	}
	var body ErrorBody
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != "upstream_error" || body.Error.RequestID == "" {
		t.Errorf("error = %+v", body.Error)
	}
}

func TestFetchProxyRejectsBlockedHost(t *testing.T) {
	h := newHarness(t)
	h.useUpstream(&fakeUpstream{feedErr: upstream.ErrBlockedHost})
	w := signedGET(t, h, "/api/v1/fetch?url=http%3A%2F%2F127.0.0.1%2Ffeed",
		"fetch-blocked", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// 拨号期（名字解析到内网）拒绝也是 400，不是 502。
// 包了一层之后哨兵丢失的回归会在这里暴露。
func TestFetchProxyMapsDialTimeBlockTo400(t *testing.T) {
	h := newHarness(t)
	h.useUpstream(&fakeUpstream{feedErr: fmt.Errorf("%w: feed.test resolves to 127.0.0.1", upstream.ErrBlockedHost)})
	w := signedGET(t, h, "/api/v1/fetch?url=http%3A%2F%2Ffeed.test%2Ffeed.xml",
		"fetch-dial-blocked", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// 不跟随重定向，也不能把没有 Location 的 3xx 回给宿主。
func TestFetchProxyMapsUpstreamRedirectTo502(t *testing.T) {
	h := newHarness(t)
	h.useUpstream(&fakeUpstream{feed: upstream.FeedResponse{Status: http.StatusFound}})
	w := signedGET(t, h, "/api/v1/fetch?url=https%3A%2F%2Fexample.com%2Ffeed.xml",
		"fetch-redirect", nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, must not be forwarded", loc)
	}
}

func TestAvatarProxyPassesThrough304(t *testing.T) {
	h := newHarness(t)
	fake := &fakeUpstream{avatar: upstream.AvatarResponse{
		Status:       http.StatusNotModified,
		ETag:         `"v2"`,
		LastModified: "Wed, 21 Oct 2025 07:28:00 GMT",
		CacheControl: "public, max-age=86400",
	}}
	h.useUpstream(fake)

	hash := strings.Repeat("a", 32)
	path := "/api/v1/avatar/" + hash + "?s=64&d=mp&r=g"
	w := signedGET(t, h, path, "avatar-304", map[string]string{
		"If-None-Match":     `"v1"`,
		"If-Modified-Since": "Tue, 20 Oct 2025 07:28:00 GMT",
	})
	if w.Code != http.StatusNotModified {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("304 must not carry a body, got %q", w.Body.String())
	}
	if got := w.Header().Get("ETag"); got != `"v2"` {
		t.Errorf("ETag = %q", got)
	}
	if got := w.Header().Get("Last-Modified"); got == "" {
		t.Error("Last-Modified not passed through")
	}
	if fake.gotAvatar.ETag != `"v1"` || fake.gotAvatar.LastModified == "" {
		t.Errorf("conditional headers not forwarded: %+v", fake.gotAvatar)
	}
	if fake.gotAvatar.Size != 64 || fake.gotAvatar.Default != "mp" || fake.gotAvatar.Rating != "g" {
		t.Errorf("query params not forwarded: %+v", fake.gotAvatar)
	}
}

func TestAvatarProxyRejectsInvalidHash(t *testing.T) {
	h := newHarness(t)
	fake := &fakeUpstream{}
	h.useUpstream(fake)

	for _, hash := range []string{
		strings.Repeat("A", 32),
		strings.Repeat("a", 31),
	} {
		w := signedGET(t, h, "/api/v1/avatar/"+hash, "avatar-bad-"+hash, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", hash, w.Code)
		}
	}
	if fake.gotAvatar.Hash != "" {
		t.Errorf("invalid hash reached the upstream client: %+v", fake.gotAvatar)
	}
}

func TestAvatarProxyMapsUpstreamFailureTo502(t *testing.T) {
	h := newHarness(t)
	h.useUpstream(&fakeUpstream{avatar: upstream.AvatarResponse{
		Status: http.StatusInternalServerError,
		ETag:   `"stale"`,
	}})

	hash := strings.Repeat("b", 32)
	w := signedGET(t, h, "/api/v1/avatar/"+hash, "avatar-500", nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	var body ErrorBody
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != "upstream_error" || body.Error.RequestID == "" {
		t.Errorf("error = %+v", body.Error)
	}
	// 错误体不能带上游的验证器：宿主会把它当成一次有效的缓存描述。
	if got := w.Header().Get("ETag"); got != "" {
		t.Errorf("error response carried ETag %q", got)
	}
}

func TestAvatarProxyNotFoundCarriesNoValidators(t *testing.T) {
	h := newHarness(t)
	h.useUpstream(&fakeUpstream{avatar: upstream.AvatarResponse{
		Status:       http.StatusNotFound,
		ETag:         `"v1"`,
		LastModified: "Wed, 21 Oct 2025 07:28:00 GMT",
		CacheControl: "public, max-age=86400",
	}})

	hash := strings.Repeat("c", 32)
	w := signedGET(t, h, "/api/v1/avatar/"+hash, "avatar-404", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	for _, hdr := range []string{"ETag", "Last-Modified", "Cache-Control"} {
		if got := w.Header().Get(hdr); got != "" {
			t.Errorf("404 carried %s = %q", hdr, got)
		}
	}
}

// 代理名额满是本端暂时过载，必须是 503 + Retry-After，而不是 502：
// 宿主会把 502 记成该源抓取失败并累计失败次数。
func TestFetchProxyMapsBusyTo503(t *testing.T) {
	h := newHarness(t)
	h.useUpstream(&fakeUpstream{feedErr: fmt.Errorf("%w: waited 10s", upstream.ErrBusy)})
	w := signedGET(t, h, "/api/v1/fetch?url=https%3A%2F%2Fexample.com%2Ffeed.xml",
		"fetch-busy", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("503 应带 Retry-After")
	}
}
