package rss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/store"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

func newFixService(t *testing.T, fetchTimeout time.Duration) (*Service, *sqlite.DB) {
	t.Helper()
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
	svc := NewService(db, blobs, Config{
		UserAgent: "test/1", RequestTimeout: 5 * time.Second,
		FetchTimeout: fetchTimeout, MaxFeedBytes: 1 << 20,
		MaxEntryBytes: 1 << 20, MaxBlobBytes: 1 << 20,
		DefaultInterval: 30 * time.Minute, MinInterval: 10 * time.Minute,
		MaxInterval: 6 * time.Hour, AllowPrivateHosts: true,
	}, nil)
	return svc, db
}

// 抓取超时后状态仍必须落库：next_fetch_at 前移、error_count 累加。
// 修复前回写用的是已经超时取消的 ctx，Update 必然失败。
func TestFetchTimeoutStillPersistsFailure(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(release)

	svc, db := newFixService(t, 100*time.Millisecond)
	ctx := context.Background()
	before := time.Now().Unix()
	past := before - 3600
	feed, _, err := db.Feed().Upsert(ctx, &store.Feed{
		URL: srv.URL + "/slow.xml", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: before, NextFetchAt: &past,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Fetch(ctx, feed.ID); err == nil {
		t.Fatal("慢服务器应导致抓取失败")
	}

	got, err := db.Feed().Get(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ErrorCount != 1 {
		t.Errorf("error_count = %d, want 1（超时后回写失败）", got.ErrorCount)
	}
	if got.NextFetchAt == nil || *got.NextFetchAt <= before {
		t.Errorf("next_fetch_at = %v, 应前移到 %d 之后", got.NextFetchAt, before)
	}
	if got.LastError == "" {
		t.Error("last_error 未落库")
	}
}

const mediaFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>媒体源</title><link>%BASE%/</link>
<item><title>一</title><link>%BASE%/p/1</link><guid>m-1</guid>
<description><![CDATA[<p><img src="%BASE%/img/1.png"></p>]]></description></item>
<item><title>二</title><link>%BASE%/p/2</link><guid>m-2</guid>
<description><![CDATA[<p><img src="%BASE%/img/2.png" srcset="%BASE%/img/2.png 1x, %BASE%/img/3.png 2x"></p>]]></description></item>
</channel></rss>`

// 已入库的条目在第二轮抓取时不能再下载任何媒体。
func TestSecondFetchDownloadsNoMedia(t *testing.T) {
	var mediaHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/img/") {
			mediaHits.Add(1)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes(r.URL.Path[len(r.URL.Path)-5]))
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(strings.ReplaceAll(mediaFeed, "%BASE%", "http://"+r.Host)))
	}))
	defer srv.Close()

	svc, db := newFixService(t, 20*time.Second)
	ctx := context.Background()
	feed, _, _ := db.Feed().Upsert(ctx, &store.Feed{
		URL: srv.URL + "/feed.xml", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: time.Now().Unix(),
	})

	first, err := svc.Fetch(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.NewEntries != 2 || first.MediaOK != 3 {
		t.Fatalf("首轮 = %+v, want 2 条 3 个媒体", first)
	}
	if mediaHits.Load() != 3 {
		t.Fatalf("首轮媒体请求 %d 次, want 3", mediaHits.Load())
	}

	mediaHits.Store(0)
	second, err := svc.Fetch(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.NewEntries != 0 || second.Skipped != 2 {
		t.Errorf("第二轮 = %+v, want 0 新 2 跳过", second)
	}
	if n := mediaHits.Load(); n != 0 {
		t.Errorf("第二轮媒体请求 %d 次, want 0（应先查重再下载）", n)
	}

	// persistMedia 合并事务后 blob 引用仍然完整。
	n, _ := db.Blob().Count(ctx)
	if n != 3 {
		t.Errorf("blob 行 %d, want 3", n)
	}
}

// 含 javascript: 候选的 srcset：剩余候选的描述符不能错位。
func TestSrcsetDescriptorsSurviveDroppedCandidate(t *testing.T) {
	doc := sanitize(t, `<img srcset="javascript:alert(1) 1x, /b.png 2x, /c.png 3x">`, "https://example.com/")
	var descs []string
	for _, tg := range doc.Targets {
		descs = append(descs, tg.Descriptor)
	}
	if strings.Join(descs, ",") != "2x,3x" {
		t.Fatalf("targets descriptors = %v, want [2x 3x]", descs)
	}

	shaB, shaC := strings.Repeat("b", 64), strings.Repeat("c", 64)
	doc.Rewrite(map[string]MediaResult{
		"https://example.com/b.png": {Status: "ok", SHA256: shaB, Ext: "png"},
		"https://example.com/c.png": {Status: "ok", SHA256: shaC, Ext: "png"},
	})
	out := render(t, doc)
	want := "/media/" + shaB + ".png 2x, /media/" + shaC + ".png 3x"
	if !strings.Contains(out, want) {
		t.Errorf("srcset 描述符错位:\n%s\nwant %s", out, want)
	}
	if strings.Contains(out, "javascript") || strings.Contains(out, "example.com") {
		t.Errorf("外链或脚本残留:\n%s", out)
	}
}

// src 失败但 srcset 有成功候选：保留元素，src 换成成功候选。
func TestRewriteSrcFailsSrcsetSucceeds(t *testing.T) {
	doc := sanitize(t, `<img src="/a.png" srcset="/a.png 1x, /b.png 2x" alt="图">`, "https://example.com/")
	sha := strings.Repeat("e", 64)
	doc.Rewrite(map[string]MediaResult{
		"https://example.com/a.png": {Status: "failed"},
		"https://example.com/b.png": {Status: "ok", SHA256: sha, Ext: "png"},
	})
	out := render(t, doc)
	local := "/media/" + sha + ".png"
	if !strings.Contains(out, `src="`+local+`"`) {
		t.Errorf("src 应换成成功候选:\n%s", out)
	}
	if !strings.Contains(out, local+" 2x") {
		t.Errorf("srcset 应保留成功候选:\n%s", out)
	}
	if strings.Contains(out, "media-missing") || strings.Contains(out, "example.com") {
		t.Errorf("不应变成占位，也不能留外链:\n%s", out)
	}
}

// poster 失败只移除 poster，视频本体保留。
func TestRewritePosterFailureKeepsVideo(t *testing.T) {
	doc := sanitize(t, `<video src="/v.mp4" poster="/p.jpg" controls></video>`, "https://example.com/")
	sha := strings.Repeat("f", 64)
	doc.Rewrite(map[string]MediaResult{
		"https://example.com/v.mp4": {Status: "ok", SHA256: sha, Ext: "mp4"},
		"https://example.com/p.jpg": {Status: "failed"},
	})
	out := render(t, doc)
	if !strings.Contains(out, "<video") || !strings.Contains(out, "/media/"+sha+".mp4") {
		t.Errorf("视频应保留:\n%s", out)
	}
	if strings.Contains(out, "poster") || strings.Contains(out, "example.com") {
		t.Errorf("失败的 poster 必须移除:\n%s", out)
	}
}

// 本地化硬约束：各种部分失败组合下都不能留下第三方地址。
func TestRewriteNeverLeaksExternalURL(t *testing.T) {
	inputs := []string{
		`<img src="/a.png" srcset="/b.png 2x">`,
		`<img srcset="/a.png 1x, /b.png 2x">`,
		`<video src="/v.mp4" poster="/p.jpg"></video>`,
		`<picture><source srcset="/s.webp 1x"><img src="/a.png"></picture>`,
		`<audio src="/a.mp3"></audio>`,
	}
	sha := strings.Repeat("9", 64)
	for _, in := range inputs {
		for mask := 0; mask < 8; mask++ {
			doc := sanitize(t, in, "https://example.com/")
			res := map[string]MediaResult{}
			for i, tg := range doc.Targets {
				if mask&(1<<i) != 0 {
					res[tg.Resolved] = MediaResult{Status: "ok", SHA256: sha, Ext: "bin"}
				} else {
					res[tg.Resolved] = MediaResult{Status: "failed"}
				}
			}
			doc.Rewrite(res)
			if out := render(t, doc); strings.Contains(out, "example.com") {
				t.Errorf("input %q mask %b 泄露外链:\n%s", in, mask, out)
			}
		}
	}
}

// ::1 必须按回环处理，而不是被当成 IPv4-compatible 拆成 0.0.0.1。
func TestLoopbackV6CheckedBeforeEmbeddedV4(t *testing.T) {
	if allowedIP(mustAddr(t, "::1"), false) {
		t.Error("::1 默认应被拒绝")
	}
	if !allowedIP(mustAddr(t, "::1"), true) {
		t.Error("allowPrivate=true 时 ::1 应与 127.0.0.1 一样放行")
	}
	if allowedIP(mustAddr(t, "::"), true) {
		t.Error(":: 无论开关都必须拒绝")
	}
	// IPv4-compatible 的 0.0.0.1 不是回环，仍按 0.0.0.0/8 拒绝。
	if allowedIP(mustAddr(t, "::0.0.0.2"), false) {
		t.Error("::0.0.0.2 应被拒绝")
	}
}

func TestSafeHTTPURL(t *testing.T) {
	cases := map[string]string{
		"https://example.com/a":  "https://example.com/a",
		" http://example.com ":   "http://example.com",
		"HTTPS://Example.com/x":  "https://Example.com/x",
		"javascript:alert(1)":    "",
		"data:text/html,x":       "",
		"/relative/path":         "",
		"//example.com/a":        "",
		"ftp://example.com/a":    "",
		"http:///nohost":         "",
		"mailto:a@example.com":   "",
		"":                       "",
		"http://[::1":            "",
		"https:example.com/path": "",
	}
	for in, want := range cases {
		if got := SafeHTTPURL(in); got != want {
			t.Errorf("SafeHTTPURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// 条目链接与站点首页只接受 http/https：javascript: 不能进库。
func TestFetchDropsUnsafeLinks(t *testing.T) {
	const feedXML = `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title>
<link>javascript:alert(1)</link>
<item><title>t</title><link>javascript:alert(2)</link><guid>j-1</guid><description>d</description></item>
</channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(feedXML))
	}))
	defer srv.Close()

	svc, db := newFixService(t, 20*time.Second)
	ctx := context.Background()
	feed, _, _ := db.Feed().Upsert(ctx, &store.Feed{
		URL: srv.URL + "/feed.xml", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: time.Now().Unix(),
	})
	if _, err := svc.Fetch(ctx, feed.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := db.Feed().Get(ctx, feed.ID)
	if got.SiteURL != "" {
		t.Errorf("site_url = %q, want 空", got.SiteURL)
	}
	entries, _, _ := db.Entry().List(ctx, store.EntryQuery{FeedID: feed.ID})
	if len(entries) != 1 || entries[0].URL != "" {
		t.Errorf("entries = %+v, url 应为空", entries)
	}
}
