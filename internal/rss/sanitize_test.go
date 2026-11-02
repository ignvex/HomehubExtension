package rss

import (
	"context"
	"encoding/base64"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ignvex/HomehubExtension/internal/blob"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func sanitize(t *testing.T, html, base string) *Document {
	t.Helper()
	doc, err := Sanitize(html, SanitizeOptions{Base: mustURL(t, base)})
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	return doc
}

func render(t *testing.T, doc *Document) string {
	t.Helper()
	out, err := doc.Render()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDropsExecutableContent(t *testing.T) {
	doc := sanitize(t, `<p>keep</p><script>alert(1)</script><style>body{background:url(http://evil/x)}</style><noscript><img src="http://t/pixel"></noscript>`, "https://example.com/")
	out := render(t, doc)

	for _, bad := range []string{"<script", "alert", "<style", "url(http", "<noscript", "evil"} {
		if strings.Contains(out, bad) {
			t.Errorf("output still contains %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "keep") {
		t.Errorf("legitimate text was dropped:\n%s", out)
	}
}

func TestIframeBecomesLink(t *testing.T) {
	doc := sanitize(t, `<iframe src="https://www.youtube.com/embed/abc123?rel=0"></iframe>`, "https://example.com/")
	out := render(t, doc)

	if strings.Contains(out, "<iframe") {
		t.Errorf("iframe survived:\n%s", out)
	}
	// 只保留链接，浏览器不会自动请求。
	if !strings.Contains(out, "https://www.youtube.com/watch?v=abc123") {
		t.Errorf("expected a plain link to the video:\n%s", out)
	}
}

func TestUnknownIframeDropped(t *testing.T) {
	doc := sanitize(t, `<iframe src="https://ads.example.com/frame"></iframe>`, "https://example.com/")
	out := render(t, doc)
	if strings.Contains(out, "ads.example.com") {
		t.Errorf("unknown iframe should be dropped entirely:\n%s", out)
	}
}

func TestRejectsJavascriptURL(t *testing.T) {
	cases := []string{
		`<a href="javascript:alert(1)">x</a>`,
		"<a href=\"java\tscript:alert(1)\">x</a>",
		`<img src="javascript:alert(1)">`,
		`<img src="vbscript:msgbox(1)">`,
		`<img src="file:///etc/passwd">`,
	}
	for _, c := range cases {
		out := render(t, sanitize(t, c, "https://example.com/"))
		if strings.Contains(out, "javascript") || strings.Contains(out, "vbscript") || strings.Contains(out, "file://") {
			t.Errorf("dangerous URL survived in %q:\n%s", c, out)
		}
	}
}

func TestStripsStyleAndClass(t *testing.T) {
	doc := sanitize(t, `<p class="post-body" style="background:url(http://evil/x.png)">text</p>`,
		"https://example.com/")
	out := render(t, doc)

	if strings.Contains(out, "style=") {
		t.Errorf("style attribute survived (CSP relies on its removal):\n%s", out)
	}
	if strings.Contains(out, "class=") {
		t.Errorf("class attribute survived:\n%s", out)
	}
	if strings.Contains(out, "evil") {
		t.Errorf("background URL survived:\n%s", out)
	}
}

func TestAddsRelOnLinks(t *testing.T) {
	doc := sanitize(t, `<a href="https://other.example/post">link</a>`, "https://example.com/")
	out := render(t, doc)

	for _, want := range []string{"rel=", "noopener", "noreferrer", "nofollow", `target="_blank"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestResolvesRelativeURLs(t *testing.T) {
	doc := sanitize(t, `<img src="/images/a.png">`, "https://example.com/blog/post")
	out := render(t, doc)

	if !strings.Contains(out, "https://example.com/images/a.png") {
		t.Errorf("relative URL not resolved:\n%s", out)
	}
	if len(doc.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(doc.Targets))
	}
}

func TestSrcsetWithCommasInQuery(t *testing.T) {
	// CDN 常见 ?w=100,200 这类含逗号的查询串。
	// 用 strings.Split(",") 会把它切成两条错乱的 URL。
	input := `<img srcset="https://cdn/x.png?w=100,200 1x, https://cdn/x.png?w=400 2x">`
	doc := sanitize(t, input, "https://example.com/")

	if len(doc.Targets) != 2 {
		t.Fatalf("targets = %d, want 2: %+v", len(doc.Targets), doc.Targets)
	}
	if doc.Targets[0].Resolved != "https://cdn/x.png?w=100,200" {
		t.Errorf("first candidate mangled: %q", doc.Targets[0].Resolved)
	}
	if doc.Targets[1].Resolved != "https://cdn/x.png?w=400" {
		t.Errorf("second candidate mangled: %q", doc.Targets[1].Resolved)
	}
}

func TestCollectsMediaTargets(t *testing.T) {
	input := `<img src="/a.png"><picture><source srcset="/b.png 1x"></picture><video src="/c.mp4" poster="/d.jpg"></video><audio src="/e.mp3"></audio>`
	doc := sanitize(t, input, "https://example.com/")

	got := map[string]bool{}
	for _, tgt := range doc.Targets {
		got[tgt.Resolved] = true
	}
	for _, want := range []string{
		"https://example.com/a.png",
		"https://example.com/b.png",
		"https://example.com/c.mp4",
		"https://example.com/d.jpg",
		"https://example.com/e.mp3",
	} {
		if !got[want] {
			t.Errorf("missing target %q, got %v", want, got)
		}
	}
}

func TestKsPrefixInAttributesIsDropped(t *testing.T) {
	doc := sanitize(t, `<img src="/a.png" onerror="alert(1)" data-src="/orig.png">`, "https://example.com/")
	out := render(t, doc)

	if strings.Contains(out, "onerror") || strings.Contains(out, "data-src") {
		t.Errorf("event handler or data attribute survived:\n%s", out)
	}
}

func TestDataURILimitedToImages(t *testing.T) {
	ok := `<img src="data:image/png;base64,iVBORw0KGgo=">`
	out := render(t, sanitize(t, ok, "https://example.com/"))
	if !strings.Contains(out, "data:image/png") {
		t.Errorf("inline image dropped:\n%s", out)
	}

	bad := `<img src="data:text/html;base64,PHNjcmlwdD4=">`
	out = render(t, sanitize(t, bad, "https://example.com/"))
	if strings.Contains(out, "data:text/html") {
		t.Errorf("non-image data URI survived:\n%s", out)
	}
}

func TestUnwrapsUnknownTags(t *testing.T) {
	doc := sanitize(t, `<custom-element><p>inner</p></custom-element>`, "https://example.com/")
	out := render(t, doc)

	if strings.Contains(out, "custom-element") {
		t.Errorf("unknown tag survived:\n%s", out)
	}
	if !strings.Contains(out, "inner") {
		t.Errorf("content inside unknown tag was lost:\n%s", out)
	}
}

func TestCommentsDropped(t *testing.T) {
	doc := sanitize(t, `<p>a</p><!--[if IE]><script>x</script><![endif]-->`, "https://example.com/")
	out := render(t, doc)
	if strings.Contains(out, "script") || strings.Contains(out, "IE") {
		t.Errorf("comment survived:\n%s", out)
	}
}

func TestTextExtraction(t *testing.T) {
	doc := sanitize(t, `<h1>Title</h1><p>First para.</p><p>Second.</p>`, "https://example.com/")
	text := doc.Text()

	for _, want := range []string{"Title", "First para.", "Second."} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
}

// ---- URL 与 IP 判定 ----

func TestAllowedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", // 云元数据
		"::1", "fe80::1", "fc00::1", "100.64.0.1",
		"0.0.0.0", "255.255.255.255",
	}
	for _, s := range blocked {
		ip := mustAddr(t, s)
		if allowedIP(ip, false) {
			t.Errorf("%s should be blocked", s)
		}
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		ip := mustAddr(t, s)
		if !allowedIP(ip, false) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestAllowedIPUnmapsIPv4InIPv6(t *testing.T) {
	// ::ffff:127.0.0.1 是一个现成的绕过，必须 Unmap 后再判断。
	ip := mustAddr(t, "::ffff:127.0.0.1")
	if allowedIP(ip, false) {
		t.Error("IPv4-mapped loopback should be blocked")
	}
}

func TestCheckHostRejectsBadURLs(t *testing.T) {
	cases := []string{
		"ftp://example.com/x",
		"http://user:pass@example.com/x",
		"http://127.0.0.1/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://example.com:22/x",
	}
	for _, c := range cases {
		u := mustURL(t, c)
		if err := checkHost(u, false); err == nil {
			t.Errorf("%s should be rejected", c)
		}
	}

	ok := []string{"https://example.com/x", "http://example.com:8080/x"}
	for _, c := range ok {
		u := mustURL(t, c)
		if err := checkHost(u, false); err != nil {
			t.Errorf("%s should be accepted: %v", c, err)
		}
	}
}

func TestAllowPrivateHostsOptIn(t *testing.T) {
	for _, raw := range []string{
		"http://192.168.1.10/feed.xml",
		"http://127.0.0.1:19000/feed.xml",
		"http://[::1]:19000/feed.xml",
	} {
		u := mustURL(t, raw)
		if err := checkHost(u, true); err != nil {
			t.Errorf("%s should be allowed when opted in: %v", raw, err)
		}
	}

	// 链路本地（云元数据）即使开了开关也不能放开。
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/x",
	} {
		if allowedIP(mustAddr(t, mustHost(t, raw)), true) {
			t.Errorf("%s must stay blocked even with allow_private_hosts", raw)
		}
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

// ---- 重写 ----

func TestRewriteReplacesSrcAndDropsSrcsetOnFailure(t *testing.T) {
	doc := sanitize(t, `<img src="/a.png" srcset="/a.png 1x, /b.png 2x">`, "https://example.com/")
	sha := strings.Repeat("a", 64)

	results := map[string]MediaResult{
		"https://example.com/a.png": {URL: "https://example.com/a.png", Status: "ok", SHA256: sha, Ext: "png"},
		"https://example.com/b.png": {URL: "https://example.com/b.png", Status: "failed"},
	}
	doc.Rewrite(results)
	out := render(t, doc)

	if !strings.Contains(out, "/media/"+sha+".png") {
		t.Errorf("src not rewritten:\n%s", out)
	}
	if strings.Contains(out, "b.png") {
		t.Errorf("failed srcset candidate should be removed:\n%s", out)
	}
	// 原始外链一个都不能留。
	if strings.Contains(out, "https://example.com/a.png") {
		t.Errorf("original URL leaked:\n%s", out)
	}
}

func TestRewriteRemovesVideoOnFailure(t *testing.T) {
	doc := sanitize(t, `<video src="/v.mp4"></video>`, "https://example.com/")
	doc.Rewrite(map[string]MediaResult{
		"https://example.com/v.mp4": {URL: "https://example.com/v.mp4", Status: "failed"},
	})
	out := render(t, doc)

	if strings.Contains(out, "<video") {
		t.Errorf("failed video element should be removed, not placeholder-ed:\n%s", out)
	}
}

func TestRewriteImagePlaceholderHasNoOriginalURL(t *testing.T) {
	doc := sanitize(t, `<img src="/secret.png" alt="描述">`, "https://example.com/")
	doc.Rewrite(map[string]MediaResult{
		"https://example.com/secret.png": {URL: "https://example.com/secret.png", Status: "blocked"},
	})
	out := render(t, doc)

	if strings.Contains(out, "secret.png") {
		t.Errorf("original URL leaked into placeholder:\n%s", out)
	}
	if !strings.Contains(out, "描述") {
		t.Errorf("alt text should be preserved for screen readers:\n%s", out)
	}
	if !strings.Contains(out, "data:image/svg+xml") {
		t.Errorf("expected inline placeholder:\n%s", out)
	}
}

func TestNoExternalURLsRemainAfterRewrite(t *testing.T) {
	// 端到端断言：清洗 + 重写之后，除 <a href> 外不应有任何绝对外链。
	input := `<p><img src="https://cdn.example.com/a.png"></p>
	          <video src="https://cdn.example.com/v.mp4" poster="https://cdn.example.com/p.jpg"></video>
	          <a href="https://other.example/post">外部链接</a>`
	doc := sanitize(t, input, "https://example.com/")

	sha := strings.Repeat("b", 64)
	results := map[string]MediaResult{}
	for _, tgt := range doc.Targets {
		results[tgt.Resolved] = MediaResult{URL: tgt.Resolved, Status: "ok", SHA256: sha, Ext: "png"}
	}
	doc.Rewrite(results)
	out := render(t, doc)

	// 允许出现的绝对地址只有 a[href] 那一个。
	if strings.Count(out, "https://") != 1 {
		t.Errorf("expected exactly one external URL (the anchor), got %d:\n%s",
			strings.Count(out, "https://"), out)
	}
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return a
}

// 追踪像素必须被识别并整棵丢弃。
// 这是 docs/rss.md 4.6 承诺的隐私行为：一张 1×1 的图会让原站
// 知道「谁在什么时候读了这篇」，而这正是本地化要消除的东西。
func TestTrackingPixelsRemoved(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"1x1 尺寸", `<img src="/a.gif" width="1" height="1">`},
		{"0x0 尺寸", `<img src="/b.gif" width="0" height="0">`},
		{"已知追踪域名", `<img src="https://www.google-analytics.com/collect?v=1">`},
		{"子域命中", `<img src="https://pixel.facebook.com/tr?id=1">`},
		{"像素域名", `<img src="https://stats.wp.com/g.gif">`},
		{"无扩展名路径", `<img src="https://example.com/pixel">`},
		{"beacon 路径", `<img src="https://example.com/beacon">`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := sanitize(t, `<p>正文</p>`+c.input, "https://example.com/post/1")
			out := render(t, doc)

			if strings.Contains(out, "<img") {
				t.Errorf("追踪像素未被移除:\n%s", out)
			}
			// 同一批里的正文不能受影响。
			if !strings.Contains(out, "正文") {
				t.Errorf("正文被误删:\n%s", out)
			}
			// 也不该进入下载队列——下载它等于替追踪器完成一次上报。
			if len(doc.Targets) != 0 {
				t.Errorf("追踪像素不应进入下载队列: %+v", doc.Targets)
			}
		})
	}
}

// 反向用例：正常的配图不能被误判。
// 误删内容比多下载一个文件更糟，所以判定必须保守。
func TestNormalImagesNotTreatedAsPixels(t *testing.T) {
	cases := []string{
		`<img src="/photo.jpg" width="800" height="600" alt="配图">`,
		// 有扩展名说明是真实文件，路径里出现 track 不算证据。
		`<img src="https://example.com/track.png" alt="轨道图">`,
		// 只有一边是 1 不算——很多图标的 width 写成 1em 之类。
		`<img src="/icon.png" width="1" height="32">`,
		`<img src="/pic.png" width="2" height="2" alt="小图">`,
	}

	for _, input := range cases {
		doc := sanitize(t, input, "https://example.com/post/1")
		out := render(t, doc)

		if !strings.Contains(out, "<img") {
			t.Errorf("正常图片被误删: %s\n输出: %s", input, out)
		}
		if len(doc.Targets) == 0 {
			t.Errorf("正常图片应进入下载队列: %s", input)
		}
	}
}

// 内联（data:）图片必须被本地化，而不是被当成失败替换成占位图。
//
// 这曾经是一个真实 bug：sanitize 允许 data:image/* 通过，
// 但 DownloadAll 跳过了 data URI，rewrite 于是找不到下载结果，
// 把所有内联图片都换成了灰色占位块。
func TestDataURIIsLocalizedNotDiscarded(t *testing.T) {
	dir := t.TempDir()
	blobs, err := openTestBlobs(t, dir)
	if err != nil {
		t.Fatal(err)
	}

	// 1×1 PNG，真实的 PNG 字节。
	pngB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	input := `<p>图文</p><img src="data:image/png;base64,` + pngB64 + `" alt="内联图">`

	doc := sanitize(t, input, "https://example.com/post/1")
	if len(doc.Targets) != 1 {
		t.Fatalf("targets = %d, want 1: %+v", len(doc.Targets), doc.Targets)
	}

	dl := NewDownloader(blobs, DownloadConfig{MaxBytes: 1 << 20})
	res := dl.Download(context.Background(), doc.Targets[0].Resolved, "", 0)
	if res.Status != "ok" {
		t.Fatalf("内联 PNG 应写入 blob，得到 status=%s err=%s", res.Status, res.Error)
	}
	if res.MIME != "image/png" {
		t.Errorf("MIME = %q, want image/png（以嗅探结果为准）", res.MIME)
	}
	if res.SHA256 == "" || res.Ext != "png" {
		t.Errorf("sha=%q ext=%q", res.SHA256, res.Ext)
	}

	doc.Rewrite(map[string]MediaResult{doc.Targets[0].Resolved: res})
	out := render(t, doc)

	if strings.Contains(out, "data:image/png") {
		t.Errorf("内联数据应被替换成本地地址:\n%s", out)
	}
	if !strings.Contains(out, "/media/"+res.SHA256+".png") {
		t.Errorf("未改写成 /media 地址:\n%s", out)
	}
	if strings.Contains(out, "media-missing") {
		t.Errorf("不应降级为占位图:\n%s", out)
	}
}

// 声明类型与实际内容不符时，以嗅探结果为准。
func TestInlineMIMESpoofingRejected(t *testing.T) {
	dir := t.TempDir()
	blobs, err := openTestBlobs(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	dl := NewDownloader(blobs, DownloadConfig{MaxBytes: 1 << 20})

	// 声称是 PNG，实际是 HTML。若按声明类型保存，
	// 浏览器打开 /media/xxx.png 会拿到 text/html。
	spoofed := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("<html><script>alert(1)</script></html>"))
	res := dl.Download(context.Background(), spoofed, "", 0)

	if res.Status == "ok" {
		t.Errorf("伪装的内联数据不应被接受，得到 MIME=%q", res.MIME)
	}
}

func openTestBlobs(t *testing.T, dir string) (*blob.Store, error) {
	t.Helper()
	return blob.Open(blob.Options{
		Root:    filepath.Join(dir, "blobs"),
		TmpDir:  filepath.Join(dir, "blobs", "tmp"),
		MaxSize: 1 << 20,
	})
}

// GO-2026-6866：html.Render 在渲染 raw text 元素时，必须检查所有祖先
// 的命名空间转换。修复前，SVG/MathML 里的 raw text 可能被当成 HTML
// raw text 输出，从而绕过清洗「读到就是安全 HTML」的契约。
//
// 本项目的清洗器直接丢弃 svg/math 整棵子树，所以这条路径本就走不通；
// 这个测试固定住那个前提——如果将来有人把 svg 从 dropElements 里挪走
// （比如为了支持内联图标），这里会立刻红。
func TestNamespaceConfusionDoesNotLeakExecutableContent(t *testing.T) {
	inputs := []string{
		// SVG 里的 script（合法 SVG 内容，但在我们的输出里不该存在）
		`<svg><script>alert(1)</script></svg>`,
		// foreignObject 是 SVG 到 HTML 的命名空间转换点
		`<svg><foreignObject><script>alert(1)</script></foreignObject></svg>`,
		`<svg><foreignObject><p>keep-text</p></foreignObject></svg>`,
		// MathML 的 annotation-xml 也是转换点
		`<math><annotation-xml encoding="text/html"><script>alert(1)</script></annotation-xml></math>`,
		// 嵌套多层模拟解析器的命名空间栈
		`<svg><g><svg><script>alert(1)</script></svg></g></svg>`,
		// 大小写与属性伪装
		`<SVG><SCRIPT>alert(1)</SCRIPT></SVG>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><script xmlns="http://www.w3.org/2000/svg">alert(1)</script></svg>`,
		// raw text 元素：内容不该被当成标记解释
		`<xmp><script>alert(1)</script></xmp>`,
		`<noembed><script>alert(1)</script></noembed>`,
		`<noframes><script>alert(1)</script></noframes>`,
		`<plaintext><script>alert(1)</script>`,
		// template 内容在 HTML 解析里是惰性的，容易被清洗器漏掉
		`<template><script>alert(1)</script></template>`,
		// 注释里藏标记
		`<!--<script>alert(1)</script>-->`,
		`<!--[if IE]><script>alert(1)</script><![endif]-->`,
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			out := render(t, sanitize(t, in, "https://example.com/"))

			// 输出的任何位置都不能出现可执行内容的痕迹。
			// 容忍 `alert(1)` 作为纯文本出现（会被转义成 &lt;script&gt;），
			// 但绝不能有真的标签。
			for _, bad := range []string{"<script", "<SCRIPT", "<foreignObject", "<annotation-xml", "<template", "<!--"} {
				if strings.Contains(out, bad) {
					t.Errorf("输出仍含 %q:\n  in:  %s\n  out: %s", bad, in, out)
				}
			}
		})
	}
}

// 脱壳保留下来的文本必须被转义：`<xmp><b>x</b></xmp>` 的内容
// 要么整个消失，要么以纯文本形式出现，不能变成真的 <b> 标签。
func TestRawTextContentStaysText(t *testing.T) {
	out := render(t, sanitize(t, `<xmp><img src=x onerror=alert(1)></xmp>`, "https://example.com/"))

	// 关键：没有真的 img 标签。
	if strings.Contains(out, "<img") {
		t.Errorf("raw text 里的标记被当成标签重新构造了:\n%s", out)
	}
	// 内容若被保留，必须是转义后的纯文本。`onerror` 作为文字出现
	// 无害；有害的是它作为属性出现。用「标签是否成形」判断，
	// 而不是搜关键词——搜 `onerror` 会把安全的转义输出误报成泄漏。
	if strings.Contains(out, "onerror") && !strings.Contains(out, "&lt;img") {
		t.Errorf("事件处理器以未转义形式泄漏到输出:\n%s", out)
	}
}

// 属性位置上的 on* 必须被删干净——与上面「文本里的 on*」区分开。
func TestEventAttributesDroppedFromRealElements(t *testing.T) {
	out := render(t, sanitize(t,
		`<img src="https://example.com/a.png" onerror="alert(1)" onload="alert(2)">`+
			`<p onclick="alert(3)">text</p><a href="https://example.com" onmouseover="alert(4)">l</a>`,
		"https://example.com/"))

	for _, bad := range []string{"onerror", "onload", "onclick", "onmouseover", "alert("} {
		if strings.Contains(out, bad) {
			t.Errorf("事件处理器残留 %q:\n%s", bad, out)
		}
	}
	// 正常属性不能被连带删掉。
	if !strings.Contains(out, `src="https://example.com/a.png"`) {
		t.Errorf("合法 src 被误删:\n%s", out)
	}
}
