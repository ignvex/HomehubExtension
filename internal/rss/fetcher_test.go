package rss

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/store"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

// 抓取管线要验证的是「回调真的被触发」。
// 回调是 SSE 的唯一事件来源；它一旦断了，前端就永远收不到实时更新，
// 而所有单元测试仍然会通过。
const sampleFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel>
  <title>测试源</title>
  <link>%BASE%/</link>
  <item>
    <title>文章一</title>
    <link>%BASE%/post/1</link>
    <guid>post-1</guid>
    <pubDate>Wed, 29 Oct 2025 10:00:00 GMT</pubDate>
    <description>第一篇的内容</description>
  </item>
  <item>
    <title>文章二</title>
    <link>%BASE%/post/2</link>
    <guid>post-2</guid>
    <description>第二篇的内容</description>
  </item>
</channel>
</rss>`

func TestFetchFiresCallbacks(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	blobs, err := blob.Open(blob.Options{
		Root:   filepath.Join(dir, "blobs"),
		TmpDir: filepath.Join(dir, "blobs", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var created []string
	var fetched []int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		// 用请求的 Host 回填，避免依赖固定端口。
		body := strings.ReplaceAll(sampleFeed, "%BASE%", "http://"+r.Host)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	svc := NewService(db, blobs, Config{
		UserAgent:       "test/1",
		RequestTimeout:  5 * time.Second,
		FetchTimeout:    20 * time.Second,
		MaxFeedBytes:    1 << 20,
		MaxEntryBytes:   1 << 20,
		DefaultInterval: 30 * time.Minute,
		MinInterval:     10 * time.Minute,
		MaxInterval:     6 * time.Hour,
		// httptest 监听 127.0.0.1，必须显式放行内网。
		AllowPrivateHosts: true,
		OnEntryCreated: func(entryID, feedID int64, title string) {
			mu.Lock()
			created = append(created, title)
			mu.Unlock()
		},
		OnFeedFetched: func(feedID int64, status, newEntries int, errMsg string) {
			mu.Lock()
			fetched = append(fetched, status)
			mu.Unlock()
		},
	}, nil)

	now := time.Now().Unix()
	feed, _, err := db.Feed().Upsert(context.Background(), &store.Feed{
		URL: srv.URL + "/feed.xml", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Fetch(context.Background(), feed.ID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.NewEntries != 2 {
		t.Errorf("new entries = %d, want 2", res.NewEntries)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(created) != 2 {
		t.Errorf("OnEntryCreated 触发 %d 次, want 2: %v", len(created), created)
	}
	if len(fetched) != 1 || fetched[0] != http.StatusOK {
		t.Errorf("OnFeedFetched = %v, want [200]", fetched)
	}
}

func TestFetchFiresCallbackOnFailure(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	blobs, err := blob.Open(blob.Options{
		Root:   filepath.Join(dir, "blobs"),
		TmpDir: filepath.Join(dir, "blobs", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}

	called := false
	var gotErr string

	// 指向一个关闭的端口，必然失败。
	srv := httptest.NewServer(http.NotFoundHandler())
	deadURL := srv.URL
	srv.Close()

	svc := NewService(db, blobs, Config{
		UserAgent:         "test/1",
		RequestTimeout:    2 * time.Second,
		FetchTimeout:      5 * time.Second,
		MaxFeedBytes:      1 << 20,
		DefaultInterval:   30 * time.Minute,
		MinInterval:       10 * time.Minute,
		MaxInterval:       6 * time.Hour,
		AllowPrivateHosts: true,
		OnFeedFetched: func(feedID int64, status, newEntries int, errMsg string) {
			called = true
			gotErr = errMsg
		},
	}, nil)

	now := time.Now().Unix()
	feed, _, err := db.Feed().Upsert(context.Background(), &store.Feed{
		URL: deadURL + "/feed.xml", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Fetch(context.Background(), feed.ID); err == nil {
		t.Fatal("对已关闭的端口抓取应当失败")
	}
	// 失败也要通知：前端要能看到订阅源变成错误状态。
	if !called {
		t.Error("失败路径也必须触发 OnFeedFetched")
	}
	if gotErr == "" {
		t.Error("失败时应带出错误信息")
	}
}

func TestFetchDeduplicatesOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	blobs, err := blob.Open(blob.Options{
		Root:   filepath.Join(dir, "blobs"),
		TmpDir: filepath.Join(dir, "blobs", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := strings.ReplaceAll(sampleFeed, "%BASE%", "http://"+r.Host)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	svc := NewService(db, blobs, Config{
		UserAgent: "test/1", RequestTimeout: 5 * time.Second,
		FetchTimeout: 20 * time.Second, MaxFeedBytes: 1 << 20,
		MaxEntryBytes: 1 << 20, DefaultInterval: 30 * time.Minute,
		MinInterval: 10 * time.Minute, MaxInterval: 6 * time.Hour,
		AllowPrivateHosts: true,
	}, nil)

	now := time.Now().Unix()
	feed, _, _ := db.Feed().Upsert(context.Background(), &store.Feed{
		URL: srv.URL + "/feed.xml", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})

	first, err := svc.Fetch(context.Background(), feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.NewEntries != 2 {
		t.Fatalf("首次 = %d 条新条目, want 2", first.NewEntries)
	}

	// 再抓一次：guid 相同，不该产生新条目。
	second, err := svc.Fetch(context.Background(), feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.NewEntries != 0 {
		t.Errorf("第二次 = %d 条新条目, want 0（guid 去重失效）", second.NewEntries)
	}

	unread, _ := db.Entry().UnreadCount(context.Background(), feed.ID)
	if unread != 2 {
		t.Errorf("未读数 = %d, want 2（重复抓取不该翻倍）", unread)
	}
}

// 压缩协商必须交给 net/http。手动写 Accept-Encoding 会让 Transport 认为
// 压缩是调用方自己要求的，从而跳过解压——gofeed 收到的是 gzip 二进制。
// 若再在头里写上 br/zstd，CDN 还会选一个根本没有解码器的编码（dmhy 现象）。
func TestFetchDecodesCompressedFeed(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	blobs, err := blob.Open(blob.Options{
		Root:   filepath.Join(dir, "blobs"),
		TmpDir: filepath.Join(dir, "blobs", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var acceptEncoding string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		acceptEncoding = r.Header.Get("Accept-Encoding")
		mu.Unlock()

		body := strings.ReplaceAll(sampleFeed, "%BASE%", "http://"+r.Host)
		var compressed bytes.Buffer
		zw := gzip.NewWriter(&compressed)
		if _, err := zw.Write([]byte(body)); err != nil {
			t.Errorf("gzip write: %v", err)
			return
		}
		if err := zw.Close(); err != nil {
			t.Errorf("gzip close: %v", err)
			return
		}

		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer srv.Close()

	svc := NewService(db, blobs, Config{
		UserAgent: "test/1", RequestTimeout: 5 * time.Second,
		FetchTimeout: 20 * time.Second, MaxFeedBytes: 1 << 20,
		MaxEntryBytes: 1 << 20, DefaultInterval: 30 * time.Minute,
		MinInterval: 10 * time.Minute, MaxInterval: 6 * time.Hour,
		AllowPrivateHosts: true,
	}, nil)

	now := time.Now().Unix()
	feed, _, err := db.Feed().Upsert(context.Background(), &store.Feed{
		URL: srv.URL + "/feed.xml", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Fetch(context.Background(), feed.ID)
	if err != nil {
		t.Fatalf("Fetch: %v（响应体未解压？）", err)
	}
	if res.NewEntries != 2 {
		t.Errorf("new entries = %d, want 2", res.NewEntries)
	}

	mu.Lock()
	defer mu.Unlock()
	if acceptEncoding != "gzip" {
		t.Errorf("Accept-Encoding = %q, want gzip（标准库协商，且不含 br/zstd）", acceptEncoding)
	}
}

// 服务端无视协商、发了没请求的编码时，错误必须指向编码本身。
// 否则 gofeed 只会报 "failed to detect feed type"，把排查引向解析器，
// 而真正的原因在传输层（dmhy 的 CDN 就是这样）。
func TestFetchRejectsUnhandledContentEncoding(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	blobs, err := blob.Open(blob.Options{
		Root:   filepath.Join(dir, "blobs"),
		TmpDir: filepath.Join(dir, "blobs", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write([]byte("\x1b\x02\x00\xa4not-really-brotli"))
	}))
	defer srv.Close()

	svc := NewService(db, blobs, Config{
		UserAgent: "test/1", RequestTimeout: 5 * time.Second,
		FetchTimeout: 20 * time.Second, MaxFeedBytes: 1 << 20,
		MaxEntryBytes: 1 << 20, DefaultInterval: 30 * time.Minute,
		MinInterval: 10 * time.Minute, MaxInterval: 6 * time.Hour,
		AllowPrivateHosts: true,
	}, nil)

	now := time.Now().Unix()
	feed, _, err := db.Feed().Upsert(context.Background(), &store.Feed{
		URL: srv.URL + "/feed.xml", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Fetch(context.Background(), feed.ID)
	if err == nil {
		t.Fatal("未请求的 Content-Encoding 应当导致失败")
	}
	if !strings.Contains(err.Error(), "content-encoding") {
		t.Errorf("错误应指明编码问题，got: %v", err)
	}
	if !strings.Contains(err.Error(), "br") {
		t.Errorf("错误应带出具体编码名，got: %v", err)
	}
	if strings.Contains(err.Error(), "failed to detect feed type") {
		t.Errorf("错误指向了解析器而不是传输层，got: %v", err)
	}
}
