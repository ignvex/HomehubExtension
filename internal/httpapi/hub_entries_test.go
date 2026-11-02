package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"
)

// seedEntry 直接写库造一条带正文的条目，绕过抓取。
func seedEntry(t *testing.T, h *harness, title, contentHTML string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()

	feed, _, err := h.db.Feed().Upsert(ctx, &store.Feed{
		URL:    "https://seed.example.com/" + title + ".xml",
		Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Entry().InsertIfNew(ctx, &store.Entry{
		FeedID: feed.ID, GUID: "g-" + title, URL: "https://seed.example.com/p/" + title,
		Title: title, FetchedAt: now, ContentHTML: contentHTML, ContentText: title,
	}); err != nil {
		t.Fatal(err)
	}

	items, _, err := h.db.Entry().List(ctx, store.EntryQuery{FeedID: feed.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	return feed.ID, items[0].ID
}

func hubGET(t *testing.T, h *harness, path, nonce string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	signHubRequest(t, req, h.hubPriv, nil, nonce)
	return h.do(t, req)
}

// 宿主需要能读到条目正文，否则自己渲染阅读界面就无从下手。
func TestHubEntriesReturnsContent(t *testing.T) {
	h := newHarness(t)
	seedEntry(t, h, "hello", `<p><img src="/media/abc123.png" alt="x"></p>`)

	w := hubGET(t, h, "/api/v1/hub/entries", "nonce-he-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Items []struct {
			ID          int64  `json:"id"`
			Title       string `json:"title"`
			ContentHTML string `json:"content_html"`
		} `json:"items"`
		MediaBase string `json:"media_base"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].Title != "hello" {
		t.Errorf("title = %q", resp.Items[0].Title)
	}
	if resp.MediaBase == "" {
		t.Error("media_base 不应为空")
	}

	// 关键：媒体地址必须是绝对 URL。宿主不在我们的源上，
	// 相对路径会指向宿主自己的域名，图片全 404。
	body := resp.Items[0].ContentHTML
	if !strings.Contains(body, `src="`+resp.MediaBase+`/media/abc123.png"`) {
		t.Errorf("媒体地址未绝对化: %s", body)
	}
	if strings.Contains(body, `src="/media/`) {
		t.Errorf("仍存在相对路径: %s", body)
	}
}

// 宿主拉列表不应把内容标成已读：那只代表「取了一次数据」，
// 不代表用户读过。
func TestHubEntriesDoesNotMarkRead(t *testing.T) {
	h := newHarness(t)
	feedID, _ := seedEntry(t, h, "unread-one", "<p>x</p>")

	if w := hubGET(t, h, "/api/v1/hub/entries", "nonce-he-2"); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	unread, err := h.db.Entry().UnreadCount(context.Background(), feedID)
	if err != nil {
		t.Fatal(err)
	}
	if unread != 1 {
		t.Errorf("unread = %d, want 1（列表接口不该标记已读）", unread)
	}
}

// 过滤条件必须与网页端一致，否则宿主看到的内容和浏览器不一样。
func TestHubEntriesFilters(t *testing.T) {
	h := newHarness(t)
	feedID, entryID := seedEntry(t, h, "filter-me", "<p>x</p>")
	seedEntry(t, h, "other", "<p>y</p>")

	if err := h.db.Entry().SetRead(context.Background(), entryID, true); err != nil {
		t.Fatal(err)
	}

	w := hubGET(t, h, "/api/v1/hub/entries?unread=true", "nonce-he-3")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("unread 过滤后 %d 条, want 1", len(resp.Items))
	}
	if resp.Items[0].ID == entryID {
		t.Error("已读条目不该出现在 unread=true 的结果里")
	}

	// 按订阅源过滤
	w = hubGET(t, h, "/api/v1/hub/entries?feed_id="+jsonNumber(feedID), "nonce-he-4")
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 1 {
		t.Errorf("feed_id 过滤后 %d 条, want 1", len(resp.Items))
	}
}

func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// 批量推送必须与单条推送语义一致，且能一次处理很多条。
func TestHubBatchUpsert(t *testing.T) {
	h := newHarness(t)

	body := []byte(`{"items":[
	  {"url":"https://b1.example.com/f.xml","title":"B1"},
	  {"url":"https://b2.example.com/f.xml","title":"B2","interval":"2h"},
	  {"url":"http://127.0.0.1/blocked.xml"},
	  {"url":"https://b3.example.com/f.xml","interval":"bogus"}
	]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	signHubRequest(t, req, h.hubPriv, body, "nonce-batch-1")

	w := h.do(t, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Created int `json:"created"`
		Failed  []struct {
			URL string `json:"url"`
		} `json:"failed"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 4 {
		t.Errorf("total = %d, want 4", resp.Total)
	}
	if resp.Created != 2 {
		t.Errorf("created = %d, want 2（内网地址与坏 interval 应被拒）", resp.Created)
	}
	if len(resp.Failed) != 2 {
		t.Errorf("failed = %d 条, want 2", len(resp.Failed))
	}

	// 再推一次同样的两条：应当是更新而不是新建。
	body2 := []byte(`{"items":[
	  {"url":"https://b1.example.com/f.xml","title":"B1 renaming"},
	  {"url":"https://b2.example.com/f.xml"}
	]}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions/batch", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	signHubRequest(t, req2, h.hubPriv, body2, "nonce-batch-2")
	w = h.do(t, req2)

	var resp2 struct {
		Created   int `json:"created"`
		Updated   int `json:"updated"`
		Unchanged int `json:"unchanged"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp2)
	if resp2.Created != 0 {
		t.Errorf("created = %d, want 0（重复推送是更新）", resp2.Created)
	}
	if resp2.Updated != 1 {
		t.Errorf("updated = %d, want 1（只改了 B1 的标题）", resp2.Updated)
	}
	if resp2.Unchanged != 1 {
		t.Errorf("unchanged = %d, want 1（B2 什么都没改）", resp2.Unchanged)
	}
}

// 宿主仪表盘需要聚合数字，而不是自己拉全量订阅再算。
func TestHubStats(t *testing.T) {
	h := newHarness(t)
	seedEntry(t, h, "stat-a", "<p>a</p>")
	seedEntry(t, h, "stat-b", "<p>b</p>")

	w := hubGET(t, h, "/api/v1/hub/stats", "nonce-stats-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Feeds   int64 `json:"feeds"`
		Entries int64 `json:"entries"`
		Unread  int64 `json:"unread"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Entries != 2 {
		t.Errorf("entries = %d, want 2", resp.Entries)
	}
	if resp.Unread != 2 {
		t.Errorf("unread = %d, want 2", resp.Unread)
	}
	if resp.Feeds < 2 {
		t.Errorf("feeds = %d, want >= 2", resp.Feeds)
	}
}

// 新增的宿主端点同样必须要求签名。
func TestHubNewEndpointsRequireSignature(t *testing.T) {
	h := newHarness(t)

	paths := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/hub/entries"},
		{http.MethodGet, "/api/v1/hub/stats"},
		{http.MethodPost, "/api/v1/hub/subscriptions/batch"},
	}
	for _, p := range paths {
		t.Run(p.method+" "+p.path, func(t *testing.T) {
			req := httptest.NewRequest(p.method, p.path, strings.NewReader(`{"items":[]}`))
			req.Header.Set("Content-Type", "application/json")
			w := h.do(t, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401（缺签名）", w.Code)
			}
		})
	}
}
