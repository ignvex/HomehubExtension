package rss

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/store"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

// retryHarness 造一套「一条 entry + 一条 failed 的 media_fetch」现场，
// 让 RetryMedia 能真的走到下载与配额累加。
type retryHarness struct {
	svc     *Service
	db      *sqlite.DB
	feed    int64
	entryID int64
	url     string
}

func newRetryHarness(t *testing.T, quota int64) *retryHarness {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()

	db, err := sqlite.Open(ctx, filepath.Join(dir, "f.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	body := pngBytes('r')

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	blobs, err := blob.Open(blob.Options{
		Root:   filepath.Join(dir, "blobs"),
		TmpDir: filepath.Join(dir, "blobs", "tmp"),
		Quota:  quota,
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := NewService(db, blobs, Config{
		UserAgent: "test/1", RequestTimeout: 5 * time.Second,
		FetchTimeout: 20 * time.Second, MaxFeedBytes: 1 << 20,
		MaxEntryBytes: 1 << 20, MaxBlobBytes: 1 << 20,
		DefaultInterval:   30 * time.Minute,
		MinInterval:       10 * time.Minute,
		MaxInterval:       6 * time.Hour,
		AllowPrivateHosts: true,
		FetchConcurrency:  1,
	}, nil)

	now := time.Now().Unix()
	feed, _, err := db.Feed().Upsert(ctx, &store.Feed{
		URL: srv.URL + "/feed.xml", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 正文里放一个占位 img，data-media-ref 指向要让 RetryMedia 重试的 URL。
	mediaURL := srv.URL + "/a.png"
	content := `<p><img src="data:image/svg+xml;base64,PHN2Zy8+" alt="x" data-media-ref="` +
		MediaRef(mediaURL) + `" class="media-missing"></p>`

	entry := &store.Entry{
		FeedID: feed.ID, GUID: "g1", URL: srv.URL + "/p/1",
		Title: "t", FetchedAt: now, ContentHTML: content, ContentText: "t",
	}
	if _, err := db.Entry().InsertIfNew(ctx, entry); err != nil {
		t.Fatal(err)
	}

	listed, _, err := db.Entry().List(ctx, store.EntryQuery{FeedID: feed.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("entry 数 = %d, want 1", len(listed))
	}
	insertedID := listed[0].ID

	if err := db.Media().Record(ctx, &store.MediaRecord{
		EntryID: insertedID, SourceURL: mediaURL,
		Status: "failed", Error: "network", Attempts: 0, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	return &retryHarness{svc: svc, db: db, feed: feed.ID, url: mediaURL, entryID: insertedID}
}

// 重试同一份内容第二次时不能再把用量累加一遍。
//
// 场景：配额刚好放得下一份。第一次重试下载成功、用量 = 1 份。
// 把记录的 attempts 重置后（模拟下一轮重试任务再次看到它）第二次
// 重试命中去重，没有新占磁盘，用量必须仍是 1 份。
// 无差别累加会变成 2 份，直接超配额，图片就再也补不回来了。
func TestRetryMediaDoesNotDoubleCountDedup(t *testing.T) {
	const size = 608 // pngBytes 的实际长度
	h := newRetryHarness(t, size+size/2)

	ctx := context.Background()

	res, err := h.svc.RetryMedia(ctx, 3, 72*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 1 {
		t.Fatalf("首次重试 OK = %d, want 1（err 状态见下）", res.OK)
	}
	used, err := h.db.Blob().TotalSize(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟下一轮重试任务再次看到同一条记录。attempts 归零以通过
	// Retryable 的 attempts < maxAttempts 过滤。
	recs, err := h.db.Media().ByEntry(ctx, h.entryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("media 记录数 = %d, want 1", len(recs))
	}
	r := recs[0]
	r.Status = "failed"
	r.Attempts = 0
	r.UpdatedAt = time.Now().Unix()
	if err := h.db.Media().Update(ctx, &r); err != nil {
		t.Fatal(err)
	}

	res2, err := h.svc.RetryMedia(ctx, 3, 72*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res2.OK != 1 {
		t.Errorf("第二次重试 OK = %d, want 1（去重后应仍算成功）: failed=%d", res2.OK, res2.Failed)
	}

	after, err := h.db.Blob().TotalSize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != used {
		t.Errorf("去重后 blob 总量变了：%d → %d（同一内容不该占两份）", used, after)
	}
}

// 重试成功的资源要按真实类型记 kind，不能一律写成 image。
func TestRetryMediaRecordsRealKind(t *testing.T) {
	h := newRetryHarness(t, 1<<20)

	if _, err := h.svc.RetryMedia(context.Background(), 3, 72*time.Hour, 100); err != nil {
		t.Fatal(err)
	}

	recs, err := h.db.Media().ByEntry(context.Background(), h.entryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("media 记录数 = %d, want 1", len(recs))
	}
	if recs[0].Status != "ok" {
		t.Fatalf("status = %q (%s)", recs[0].Status, recs[0].Error)
	}

	bl, err := h.db.Blob().Get(context.Background(), recs[0].SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if bl.Kind != "image" {
		t.Errorf("kind = %q, want image", bl.Kind)
	}
	if !bytes.HasPrefix([]byte(bl.MIME), []byte("image/png")) {
		t.Errorf("mime = %q", bl.MIME)
	}
}
