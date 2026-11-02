package httpapi

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OPML 是阅读器迁移的入口。导入要能处理分组嵌套——多数阅读器
// 导出时按文件夹套两层，只读顶层会得到「成功导入 0 条」。
func TestOPMLImportNestedGroups(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	doc := `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>from another reader</title></head>
  <body>
    <outline text="技术">
      <outline type="rss" text="A" title="博客 A" xmlUrl="https://a.example.com/feed.xml" htmlUrl="https://a.example.com/"/>
      <outline text="子分类">
        <outline type="rss" text="B" xmlUrl="https://b.example.com/rss"/>
      </outline>
    </outline>
    <outline type="rss" text="C" xmlUrl="https://c.example.com/atom.xml"/>
  </body>
</opml>`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/import", strings.NewReader(doc))
	req.Header.Set("Content-Type", "text/x-opml")
	req.Header.Set("Authorization", "Bearer "+token)
	w := h.do(t, req)

	if w.Code != http.StatusOK {
		t.Fatalf("import = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Created int `json:"created"`
		Updated int `json:"updated"`
		Failed  []struct {
			URL    string `json:"url"`
			Reason string `json:"reason"`
		} `json:"failed"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if resp.Created != 3 {
		t.Errorf("created = %d, want 3（分组里的订阅也要算）: %s", resp.Created, w.Body.String())
	}
	if resp.Total != 3 {
		t.Errorf("total = %d, want 3", resp.Total)
	}
	if len(resp.Failed) != 0 {
		t.Errorf("failed = %v, want 空", resp.Failed)
	}

	feeds, err := h.db.Feed().List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byURL := map[string]string{}
	for _, f := range feeds {
		byURL[f.URL] = f.Title
	}
	if byURL["https://a.example.com/feed.xml"] != "博客 A" {
		t.Errorf("title = %q, want 博客 A（title 属性优先于 text）", byURL["https://a.example.com/feed.xml"])
	}
	if byURL["https://b.example.com/rss"] != "B" {
		t.Errorf("title = %q, want B（无 title 时用 text）", byURL["https://b.example.com/rss"])
	}
	if _, ok := byURL["https://c.example.com/atom.xml"]; !ok {
		t.Error("顶层订阅未导入")
	}
}

// 单条失败不能拖垮整批，也不能让非法地址入库。
func TestOPMLImportPartialFailure(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	doc := `<?xml version="1.0"?>
<opml version="2.0"><body>
  <outline type="rss" text="ok" xmlUrl="https://good.example.com/feed.xml"/>
  <outline type="rss" text="meta" xmlUrl="http://169.254.169.254/latest/meta-data/"/>
  <outline type="rss" text="loopback" xmlUrl="http://127.0.0.1/feed.xml"/>
  <outline type="rss" text="file" xmlUrl="file:///etc/passwd"/>
</body></opml>`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/import", strings.NewReader(doc))
	req.Header.Set("Content-Type", "text/x-opml")
	req.Header.Set("Authorization", "Bearer "+token)
	w := h.do(t, req)

	if w.Code != http.StatusOK {
		t.Fatalf("import = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Created int `json:"created"`
		Failed  []struct {
			URL string `json:"url"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Created != 1 {
		t.Errorf("created = %d, want 1", resp.Created)
	}
	if len(resp.Failed) != 3 {
		t.Errorf("failed = %d 条, want 3", len(resp.Failed))
	}

	feeds, err := h.db.Feed().List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 {
		t.Fatalf("入库 %d 条, want 1", len(feeds))
	}
	if feeds[0].URL != "https://good.example.com/feed.xml" {
		t.Errorf("入库了 %q，非法地址没有被挡住", feeds[0].URL)
	}
}

// 导出要能被自己导入回去，且字段不丢。
func TestOPMLRoundTrip(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	importDoc := `<?xml version="1.0"?>
<opml version="2.0"><body>
  <outline type="rss" text="往返测试 &amp; 转义" xmlUrl="https://rt.example.com/feed.xml" htmlUrl="https://rt.example.com/"/>
</body></opml>`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/import", strings.NewReader(importDoc))
	req.Header.Set("Authorization", "Bearer "+token)
	if w := h.do(t, req); w.Code != http.StatusOK {
		t.Fatalf("import = %d: %s", w.Code, w.Body.String())
	}

	// 导出
	expReq := httptest.NewRequest(http.MethodGet, "/api/v1/feeds/export", nil)
	expReq.Header.Set("Authorization", "Bearer "+token)
	exp := h.do(t, expReq)
	if exp.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", exp.Code, exp.Body.String())
	}
	if ct := exp.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-opml") {
		t.Errorf("content-type = %q", ct)
	}
	if cd := exp.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("content-disposition = %q，应当作为附件下载", cd)
	}

	body := exp.Body.String()
	var parsed opmlDocument
	if err := xml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("导出内容无法解析: %v\n%s", err, body)
	}
	if parsed.Version != "2.0" {
		t.Errorf("version = %q, want 2.0", parsed.Version)
	}
	if len(parsed.Body.Outlines) != 1 {
		t.Fatalf("outline = %d 条, want 1", len(parsed.Body.Outlines))
	}
	o := parsed.Body.Outlines[0]
	if o.XMLURL != "https://rt.example.com/feed.xml" {
		t.Errorf("xmlUrl = %q", o.XMLURL)
	}
	if o.HTMLURL != "https://rt.example.com/" {
		t.Errorf("htmlUrl = %q", o.HTMLURL)
	}
	// XML 转义必须正确：& 不能原样出现在属性里。
	if o.Title != "往返测试 & 转义" {
		t.Errorf("title = %q", o.Title)
	}
}

// 非法文档要明确拒绝，而不是当成空 OPML 静默成功。
func TestOPMLImportRejectsGarbage(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	cases := []struct {
		name string
		body string
	}{
		{"不是 XML", "this is not xml"},
		{"不是 OPML", `<?xml version="1.0"?><rss><channel/></rss>`},
		{"空 body", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/import", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			w := h.do(t, req)
			if w.Code < 400 {
				t.Fatalf("status = %d, want >= 400: %s", w.Code, w.Body.String())
			}
		})
	}
}

// 未登录不能导入：批量创建订阅会主动访问外部地址。
func TestOPMLRequiresAuth(t *testing.T) {
	h := newHarness(t)

	doc := `<opml version="2.0"><body><outline type="rss" xmlUrl="https://x.example.com/f"/></body></opml>`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/feeds/import", strings.NewReader(doc))
	w := h.do(t, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}
