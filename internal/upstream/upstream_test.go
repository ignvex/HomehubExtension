package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testTransport 把任意域名拨到 httptest 服务器。
//
// /fetch 会先过 SSRF 校验，测试不能用 127.0.0.1 字面量作为目标，
// 所以这里用 feed.test 这种合法域名，再在拨号层改道。
func testTransport(srv *httptest.Server) http.RoundTripper {
	tr := srv.Client().Transport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: time.Second}
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, srv.Listener.Addr().String())
	}
	return tr
}

func TestFetchFeedReturnsUpstreamBytes(t *testing.T) {
	const want = `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title></channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != "test-agent" {
			t.Errorf("User-Agent = %q", ua)
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(want))
	}))
	defer srv.Close()

	c := newClient(Config{UserAgent: "test-agent", MaxFeedBytes: 1024, FeedTimeout: time.Second},
		testTransport(srv), http.DefaultTransport)
	res, err := c.FetchFeed(context.Background(), "http://feed.test/feed.xml")
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
	if res.ContentType != "application/rss+xml" {
		t.Errorf("content type = %q", res.ContentType)
	}
	if string(res.Body) != want {
		t.Errorf("body changed:\n%s", res.Body)
	}
}

func TestFetchFeedPassesUpstreamErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer srv.Close()

	c := newClient(Config{FeedTimeout: time.Second}, testTransport(srv), http.DefaultTransport)
	res, err := c.FetchFeed(context.Background(), "http://feed.test/feed.xml")
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if res.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", res.Status)
	}
	if res.RetryAfter != "30" {
		t.Errorf("Retry-After = %q", res.RetryAfter)
	}
	if len(res.Body) != 0 {
		t.Errorf("non-2xx body should not be forwarded, got %q", res.Body)
	}
}

func TestFetchFeedDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://feed.test/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	c := newClient(Config{FeedTimeout: time.Second}, testTransport(srv), http.DefaultTransport)
	res, err := c.FetchFeed(context.Background(), "http://feed.test/feed.xml")
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if res.Status != http.StatusFound {
		t.Errorf("status = %d, want the un-followed 302", res.Status)
	}
}

func TestFetchFeedRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()

	c := newClient(Config{FeedTimeout: time.Second, MaxFeedBytes: 10},
		testTransport(srv), http.DefaultTransport)
	_, err := c.FetchFeed(context.Background(), "http://feed.test/feed.xml")
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
}

func TestFetchFeedBlocksSSRFTargets(t *testing.T) {
	c := New(Config{})
	for _, raw := range []string{
		"http://127.0.0.1/feed",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/feed",
		"file:///etc/passwd",
		"http://user:pass@example.com/feed",
		"http://example.com:6379/feed",
	} {
		if _, err := c.FetchFeed(context.Background(), raw); !errors.Is(err, ErrBlockedHost) {
			t.Errorf("%s: err = %v, want ErrBlockedHost", raw, err)
		}
	}
}

// 字面 IP 在预检就拦住了。名字形式的地址（这里是 localhost）会通过预检，
// 真正的拦截发生在拨号期——这条路径正是 extension.md 警告的绕过方式，
// 必须实际跑一遍真实 Transport，而不是只测 allowedIP。
func TestFetchFeedBlocksLoopbackResolvedAtDialTime(t *testing.T) {
	c := New(Config{FeedTimeout: 3 * time.Second})
	_, err := c.FetchFeed(context.Background(), "http://localhost/feed")
	if !errors.Is(err, ErrBlockedHost) {
		t.Fatalf("err = %v, want ErrBlockedHost from the dialer", err)
	}
	if errors.Is(err, ErrUpstream) {
		t.Fatalf("SSRF rejection must not be flattened into ErrUpstream: %v", err)
	}
}

// 拨号期拒绝必须保留哨兵，否则 handler 只能回 502，
// 把一个「地址不允许」的可修问题报成上游故障。
type blockedTransport struct{}

func (blockedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%w: feed.test resolves to 127.0.0.1", ErrBlockedHost)
}

func TestFetchFeedKeepsBlockedHostFromTransport(t *testing.T) {
	c := newClient(Config{FeedTimeout: time.Second}, blockedTransport{}, http.DefaultTransport)
	_, err := c.FetchFeed(context.Background(), "http://feed.test/feed.xml")
	if !errors.Is(err, ErrBlockedHost) {
		t.Fatalf("err = %v, want ErrBlockedHost", err)
	}
}

func TestParseAvatarSize(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", defaultAvatarSize},
		{"64", 64},
		{"1", minAvatarSize},
		{"9999", maxAvatarSize},
		{"-5", minAvatarSize},
	}
	for _, tc := range cases {
		got, err := ParseAvatarSize(tc.raw)
		if err != nil {
			t.Errorf("ParseAvatarSize(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAvatarSize(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
	if _, err := ParseAvatarSize("abc"); !errors.Is(err, ErrInvalidSize) {
		t.Errorf("non-numeric size = %v, want ErrInvalidSize", err)
	}
}

func TestFetchAvatarRejectsInvalidHash(t *testing.T) {
	c := New(Config{})
	for _, hash := range []string{
		"",
		"0123456789abcdef0123456789abcde",   // 31
		"0123456789abcdef0123456789abcdef0", // 33
		"0123456789ABCDEF0123456789ABCDEF",  // 大写
		"../../etc/passwd",
		"0123456789abcdef0123456789abcdeg",
	} {
		if _, err := c.FetchAvatar(context.Background(), AvatarRequest{Hash: hash}); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("%q: err = %v, want ErrInvalidHash", hash, err)
		}
	}
}

func TestFetchAvatarPassesThroughValidators(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef"
	var gotPath, gotSize, gotStyle, gotETag, gotIMS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSize = r.URL.Query().Get("s")
		gotStyle = r.URL.Query().Get("d")
		gotETag = r.Header.Get("If-None-Match")
		gotIMS = r.Header.Get("If-Modified-Since")
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2025 07:28:00 GMT")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	c := newClient(Config{AvatarTimeout: time.Second, avatarBaseURL: srv.URL + "/avatar"},
		http.DefaultTransport, srv.Client().Transport)
	res, err := c.FetchAvatar(context.Background(), AvatarRequest{
		Hash:         hash,
		Size:         9999,
		ETag:         `"v1"`,
		LastModified: "Tue, 20 Oct 2025 07:28:00 GMT",
	})
	if err != nil {
		t.Fatalf("FetchAvatar: %v", err)
	}
	if res.Status != http.StatusNotModified {
		t.Errorf("status = %d, want 304", res.Status)
	}
	if res.ETag != `"v2"` || res.LastModified == "" {
		t.Errorf("validators not passed through: etag=%q lastmod=%q", res.ETag, res.LastModified)
	}
	if gotPath != "/avatar/"+hash {
		t.Errorf("path = %q", gotPath)
	}
	if gotSize != "512" {
		t.Errorf("s = %q, want 512 (clamped)", gotSize)
	}
	if gotStyle != "mp" {
		t.Errorf("d = %q, want mp", gotStyle)
	}
	if gotETag != `"v1"` || gotIMS == "" {
		t.Errorf("conditional headers not forwarded: etag=%q ims=%q", gotETag, gotIMS)
	}
}

func TestFetchAvatarReturnsImageAndStatus(t *testing.T) {
	const hash = "fedcba9876543210fedcba9876543210"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("d") {
		case "404":
			w.WriteHeader(http.StatusNotFound)
			return
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte("PNGDATA"))
	}))
	defer srv.Close()

	c := newClient(Config{AvatarTimeout: time.Second, avatarBaseURL: srv.URL + "/avatar"},
		http.DefaultTransport, srv.Client().Transport)

	res, err := c.FetchAvatar(context.Background(), AvatarRequest{Hash: hash, Size: 64, Default: "identicon"})
	if err != nil {
		t.Fatalf("FetchAvatar: %v", err)
	}
	if res.Status != http.StatusOK || string(res.Body) != "PNGDATA" {
		t.Errorf("200 path: status=%d body=%q", res.Status, res.Body)
	}
	if res.ContentType != "image/png" || res.ETag != `"abc"` {
		t.Errorf("headers not passed through: %+v", res)
	}

	res, err = c.FetchAvatar(context.Background(), AvatarRequest{Hash: hash, Size: 64, Default: "404"})
	if err != nil || res.Status != http.StatusNotFound {
		t.Errorf("404 path: status=%d err=%v", res.Status, err)
	}

	res, err = c.FetchAvatar(context.Background(), AvatarRequest{Hash: hash, Size: 64, Default: "500"})
	if err != nil || res.Status != http.StatusInternalServerError {
		t.Errorf("500 path: status=%d err=%v", res.Status, err)
	}
}
