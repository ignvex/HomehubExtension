package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/store"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

func newGCJobs(t *testing.T) (*Jobs, *sqlite.DB, *blob.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "gc.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobs, err := blob.Open(blob.Options{Root: filepath.Join(dir, "blobs")})
	if err != nil {
		t.Fatal(err)
	}
	return &Jobs{Store: db, Blobs: blobs, GCGrace: time.Hour}, db, blobs
}

func putFile(t *testing.T, s *blob.Store, content string, age time.Duration) *blob.Result {
	t.Helper()
	r, err := s.Put(context.Background(), strings.NewReader(content), blob.PutOptions{Ext: "png"})
	if err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		old := time.Now().Add(-age)
		if err := os.Chtimes(r.Path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// 磁盘上有、blob 表里没有的文件就是孤儿：过了宽限期删除，宽限期内保留。
func TestBlobGCRemovesOrphanFiles(t *testing.T) {
	j, db, blobs := newGCJobs(t)
	ctx := context.Background()

	oldOrphan := putFile(t, blobs, "old orphan", 2*time.Hour)
	freshOrphan := putFile(t, blobs, "fresh orphan", 0) // 刚落盘、尚未入库
	tracked := putFile(t, blobs, "tracked", 2*time.Hour)

	// tracked 有 blob 行且有引用，行级 GC 与孤儿 GC 都不能碰它。
	feed, _, _ := db.Feed().Upsert(ctx, &store.Feed{
		URL: "https://example.com/gc", Source: "hub", Enabled: true, FetchInterval: 1800, CreatedAt: 1,
	})
	e := &store.Entry{FeedID: feed.ID, GUID: "g", FetchedAt: 1}
	if _, err := db.Entry().InsertIfNew(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := db.Blob().SaveEntryMedia(ctx, e.ID, []store.Blob{{
		SHA256: tracked.SHA256, Size: tracked.Size, Ext: "png", Kind: "image", CreatedAt: 1, LastSeenAt: 1,
	}}, nil); err != nil {
		t.Fatal(err)
	}

	// dry_run 只计数，不删。
	msg, err := j.BlobGC(ctx, map[string]any{"dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "1 orphan files") {
		t.Errorf("dry_run 摘要 = %q, want 1 orphan", msg)
	}
	if !exists(oldOrphan.Path) {
		t.Fatal("dry_run 删除了文件")
	}

	msg, err = j.BlobGC(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "removed 1 orphan files") {
		t.Errorf("摘要 = %q, want removed 1 orphan files", msg)
	}
	if exists(oldOrphan.Path) {
		t.Error("过了宽限期的孤儿没被删除")
	}
	if !exists(freshOrphan.Path) {
		t.Error("宽限期内的文件被误删（可能是刚写入尚未入库）")
	}
	if !exists(tracked.Path) {
		t.Error("有 blob 行的文件被误删")
	}
}
