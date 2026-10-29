package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateSetsVersion(t *testing.T) {
	db := openTest(t)
	v, err := db.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Errorf("schema version = %d, want 1", v)
	}
	// 重复打开不应重复执行迁移。
	if err := migrate(context.Background(), db.rw); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestPasteCRUD(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	p := &store.Paste{
		ID: "01JC0000000000000000000001", Slug: "abc123", Title: "hi",
		Body: "hello", Language: "text", Visibility: "unlisted",
		SizeBytes: 5, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Paste().Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.Paste().GetBySlug(ctx, "abc123")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.Body != "hello" {
		t.Errorf("body = %q", got.Body)
	}

	// slug 冲突必须是 ErrConflict，而不是裸的驱动错误。
	dup := *p
	dup.ID = "01JC0000000000000000000002"
	if err := db.Paste().Create(ctx, &dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate slug error = %v, want ErrConflict", err)
	}

	if err := db.Paste().SoftDelete(ctx, "abc123"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	after, err := db.Paste().GetBySlug(ctx, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if after.DeletedAt == nil {
		t.Error("deleted_at should be set")
	}
	// 已删除的再删一次应是 ErrNotFound，而不是静默成功。
	if err := db.Paste().SoftDelete(ctx, "abc123"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}

func TestPasteListCursor(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	// ULID 的时间前缀让字典序等于时间序，这里用递增 ID 模拟。
	ids := []string{
		"01JC0000000000000000000001",
		"01JC0000000000000000000002",
		"01JC0000000000000000000003",
	}
	for i, id := range ids {
		if err := db.Paste().Create(ctx, &store.Paste{
			ID: id, Slug: string(rune('a' + i)), Body: "x",
			Language: "text", Visibility: "unlisted",
			SizeBytes: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	page1, next, err := db.Paste().List(ctx, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || next == "" {
		t.Fatalf("page1 len=%d next=%q", len(page1), next)
	}
	// 最新优先：第一条应是 id 最大的。
	if page1[0].ID != ids[2] {
		t.Errorf("first item = %s, want %s", page1[0].ID, ids[2])
	}

	page2, next2, err := db.Paste().List(ctx, 2, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || next2 != "" {
		t.Fatalf("page2 len=%d next=%q", len(page2), next2)
	}
}

func TestPastePurgeExpired(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()
	past := now - 100

	if err := db.Paste().Create(ctx, &store.Paste{
		ID: "01JC0000000000000000000010", Slug: "old", Body: "x",
		Language: "text", Visibility: "unlisted", ExpiresAt: &past,
		SizeBytes: 1, CreatedAt: past, UpdatedAt: past,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Paste().Create(ctx, &store.Paste{
		ID: "01JC0000000000000000000011", Slug: "live", Body: "x",
		Language: "text", Visibility: "unlisted",
		SizeBytes: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := db.Paste().PurgeExpired(ctx, now, now-3600)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	if _, err := db.Paste().GetBySlug(ctx, "live"); err != nil {
		t.Errorf("live paste should survive: %v", err)
	}
}

func TestFeedUpsertPreservesLearnedFields(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	f := &store.Feed{
		URL: "https://example.com/feed.xml", Title: "", SiteURL: "",
		Source: "hub", Enabled: true, FetchInterval: 1800, CreatedAt: now,
	}
	saved, created, err := db.Feed().Upsert(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first upsert should create")
	}

	// 模拟抓取学到的字段。
	saved.Title = "Example Blog"
	saved.SiteURL = "https://example.com/"
	saved.Etag = `"abc"`
	if err := db.Feed().Update(ctx, &saved); err != nil {
		t.Fatal(err)
	}

	// Homehub 再推一次，只带 URL 与新间隔。
	again, created2, err := db.Feed().Upsert(ctx, &store.Feed{
		URL: "https://example.com/feed.xml", Source: "hub",
		Enabled: true, FetchInterval: 3600, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created2 {
		t.Error("second upsert should update, not create")
	}
	if again.FetchInterval != 3600 {
		t.Errorf("interval = %d, want 3600", again.FetchInterval)
	}
	// 空值视为"没说"，不能把已学到的标题清掉。
	if again.Title != "Example Blog" {
		t.Errorf("title was cleared: %q", again.Title)
	}
	if again.SiteURL != "https://example.com/" {
		t.Errorf("site_url was cleared: %q", again.SiteURL)
	}
}

func TestEntryDedupAndRefCount(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	feed, _, err := db.Feed().Upsert(ctx, &store.Feed{
		URL: "https://example.com/f", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	e := &store.Entry{FeedID: feed.ID, GUID: "g1", Title: "t", FetchedAt: now}
	existed, err := db.Entry().InsertIfNew(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Fatal("first insert should be new")
	}

	dup := &store.Entry{FeedID: feed.ID, GUID: "g1", Title: "t2", FetchedAt: now}
	existed, err = db.Entry().InsertIfNew(ctx, dup)
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Fatal("same guid should be reported as existing")
	}

	// blob 引用计数
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := db.Blob().Upsert(ctx, &store.Blob{
		SHA256: sha, Size: 100, MIME: "image/png", Ext: "png",
		Kind: "image", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Blob().Link(ctx, e.ID, sha); err != nil {
		t.Fatal(err)
	}
	// 同一个 link 重复调用必须幂等，否则引用计数会虚高。
	if err := db.Blob().Link(ctx, e.ID, sha); err != nil {
		t.Fatal(err)
	}

	b, err := db.Blob().Get(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	if b.RefCount != 1 {
		t.Errorf("ref_count = %d, want 1 (duplicate link must be idempotent)", b.RefCount)
	}

	// 删除条目后引用必须归零。CASCADE 不会触发触发器，
	// 所以 Feed.Delete 里显式删了 entry_blob。
	if err := db.Feed().Delete(ctx, feed.ID); err != nil {
		t.Fatal(err)
	}
	b, err = db.Blob().Get(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	if b.RefCount != 0 {
		t.Errorf("ref_count after delete = %d, want 0", b.RefCount)
	}

	// 归零后应能被 GC 找到。
	unref, err := db.Blob().Unreferenced(ctx, time.Now().Unix(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unref) != 1 || unref[0].SHA256 != sha {
		t.Errorf("unreferenced = %+v", unref)
	}
}

func TestFeedDeleteRemovesMediaFetch(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	feed, _, _ := db.Feed().Upsert(ctx, &store.Feed{
		URL: "https://example.com/f2", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	e := &store.Entry{FeedID: feed.ID, GUID: "g", FetchedAt: now}
	if _, err := db.Entry().InsertIfNew(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := db.Media().Record(ctx, &store.MediaRecord{
		EntryID: e.ID, SourceURL: "https://cdn/x.png",
		Status: "failed", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.Feed().Delete(ctx, feed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Media().ByEntry(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	recs, _ := db.Media().ByEntry(ctx, e.ID)
	if len(recs) != 0 {
		t.Errorf("media_fetch should be removed with the feed, got %d rows", len(recs))
	}
}

func TestSessionLifecycle(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	if err := db.Auth().CreateClient(ctx, &store.Client{
		ID: "c1", Name: "test", PublicKey: make([]byte, 32),
		Algorithm: "ed25519", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	hash := []byte("hash-hash-hash-hash-hash-hash-01")
	if err := db.Auth().CreateSession(ctx, &store.Session{
		TokenHash: hash, ClientID: "c1", CreatedAt: now,
		ExpiresAt: now + 3600, LastUsedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	sess, err := db.Auth().GetSession(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if !sess.Live(now) {
		t.Error("session should be live")
	}

	// 撤销设备必须同时撤销它的会话。
	if err := db.Auth().RevokeClient(ctx, "c1", now); err != nil {
		t.Fatal(err)
	}
	sess, err = db.Auth().GetSession(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Live(now) {
		t.Error("session should be revoked after client revoke")
	}
}

func TestChallengeConsumeOnce(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	if err := db.Auth().CreateClient(ctx, &store.Client{
		ID: "c2", Name: "test", PublicKey: make([]byte, 32),
		Algorithm: "ed25519", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Auth().CreateChallenge(ctx, &store.Challenge{
		Nonce: "n1", ClientID: "c2", CreatedAt: now, ExpiresAt: now + 120,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Auth().ConsumeChallenge(ctx, "n1", now); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	// 第二次必须是 ErrNotFound：nonce 一次性是防重放的核心。
	if _, err := db.Auth().ConsumeChallenge(ctx, "n1", now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second consume = %v, want ErrNotFound", err)
	}
}

// TestMediaRecordIdempotent 覆盖 media_fetch 的唯一键。
// 重复推送同一个源会重复走本地化路径，没有幂等写入就会直接报约束冲突。
func TestMediaRecordIdempotent(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	feed, _, _ := db.Feed().Upsert(ctx, &store.Feed{
		URL: "https://example.com/f3", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	e := &store.Entry{FeedID: feed.ID, GUID: "g", FetchedAt: now}
	if _, err := db.Entry().InsertIfNew(ctx, e); err != nil {
		t.Fatal(err)
	}

	rec := &store.MediaRecord{
		EntryID: e.ID, SourceURL: "https://cdn/a.png",
		Status: "failed", Error: "timeout", UpdatedAt: now,
	}
	if err := db.Media().Record(ctx, rec); err != nil {
		t.Fatalf("first record: %v", err)
	}
	// 第二次必须更新而不是报 UNIQUE 冲突。
	rec2 := &store.MediaRecord{
		EntryID: e.ID, SourceURL: "https://cdn/a.png",
		Status: "ok", SHA256: "abc", UpdatedAt: now + 1,
	}
	if err := db.Media().Record(ctx, rec2); err != nil {
		t.Fatalf("second record should upsert, got: %v", err)
	}

	recs, err := db.Media().ByEntry(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("rows = %d, want 1", len(recs))
	}
	if recs[0].Status != "ok" {
		t.Errorf("status = %q, want ok", recs[0].Status)
	}
}

func TestMediaRetryableFiltersByAttempts(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	feed, _, _ := db.Feed().Upsert(ctx, &store.Feed{
		URL: "https://example.com/f4", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	e := &store.Entry{FeedID: feed.ID, GUID: "g", FetchedAt: now}
	db.Entry().InsertIfNew(ctx, e)

	db.Media().Record(ctx, &store.MediaRecord{
		EntryID: e.ID, SourceURL: "https://cdn/a.png",
		Status: "failed", Attempts: 0, UpdatedAt: now,
	})
	db.Media().Record(ctx, &store.MediaRecord{
		EntryID: e.ID, SourceURL: "https://cdn/b.png",
		Status: "failed", Attempts: 5, UpdatedAt: now,
	})

	// maxAttempts=3 时，attempts=5 的那条不该再被选中。
	got, err := db.Media().Retryable(ctx, 3, now-3600, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SourceURL != "https://cdn/a.png" {
		t.Errorf("retryable = %+v", got)
	}
}

// BackupTo 必须用参数绑定，不是字符串拼接。
//
// VACUUM INTO 曾拼过 SQL 字符串（规范里的阻塞项）。参数绑定可用，
// 这条测试用一个带单引号的路径验证它——拼接版本要么报错，
// 要么需要一个容易被写错的转义函数。
func TestBackupToHandlesQuoteInPath(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	dir := t.TempDir()
	dest := filepath.Join(dir, "it's a backup.db")

	if err := db.BackupTo(ctx, dest); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}

	// 备份必须可用：能通过完整性检查。
	backup, err := Open(ctx, dest, Options{})
	if err != nil {
		t.Fatalf("打开备份失败: %v", err)
	}
	defer backup.Close()

	if err := backup.Ping(ctx); err != nil {
		t.Errorf("备份不可读: %v", err)
	}
}
