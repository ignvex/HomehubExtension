package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T, opts Options) *Store {
	t.Helper()
	opts.Root = filepath.Join(t.TempDir(), "blobs")
	if opts.MaxSize == 0 {
		opts.MaxSize = 1 << 20
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestPutAndOpen(t *testing.T) {
	s := newStore(t, Options{})
	ctx := context.Background()
	content := []byte("hello world")

	res, err := s.Put(ctx, bytes.NewReader(content), PutOptions{Ext: "txt"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", res.Size, len(content))
	}
	if len(res.SHA256) != 64 {
		t.Errorf("sha256 = %q", res.SHA256)
	}
	if res.Existed {
		t.Error("first put should not report existing")
	}

	f, err := s.Open(res.SHA256, "txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, content) {
		t.Errorf("content = %q", got)
	}
}

func TestPutDeduplicates(t *testing.T) {
	s := newStore(t, Options{})
	ctx := context.Background()
	content := []byte("same content")

	first, err := s.Put(ctx, bytes.NewReader(content), PutOptions{Ext: "bin"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put(ctx, bytes.NewReader(content), PutOptions{Ext: "bin"})
	if err != nil {
		t.Fatal(err)
	}

	if first.SHA256 != second.SHA256 {
		t.Errorf("sha mismatch: %s vs %s", first.SHA256, second.SHA256)
	}
	if !second.Existed {
		t.Error("second put of identical content should report Existed")
	}

	// 同一内容只应有一个文件。
	dir := filepath.Dir(first.Path)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("files in shard = %d, want 1", len(entries))
	}
}

func TestPutRejectsOversize(t *testing.T) {
	s := newStore(t, Options{MaxSize: 10})
	ctx := context.Background()

	_, err := s.Put(ctx, strings.NewReader(strings.Repeat("x", 100)), PutOptions{Ext: "bin"})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}

	// 超限必须清理临时文件，不能留下垃圾。
	tmpEntries, _ := os.ReadDir(s.tmp)
	if len(tmpEntries) != 0 {
		t.Errorf("tmp dir not cleaned: %d entries", len(tmpEntries))
	}
}

func TestPutRejectsQuota(t *testing.T) {
	s := newStore(t, Options{Quota: 100})
	ctx := context.Background()

	// Used 传入接近配额，再写就会超。
	_, err := s.Put(ctx, strings.NewReader("0123456789"), PutOptions{Ext: "bin", Used: 95})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
}

func TestPutRejectsTraversalExt(t *testing.T) {
	s := newStore(t, Options{})
	ctx := context.Background()

	// 恶意扩展名不能穿透目录。
	res, err := s.Put(ctx, strings.NewReader("x"), PutOptions{Ext: "../../etc/passwd"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Path, "..") {
		t.Errorf("path traversal in %q", res.Path)
	}
	// 非法扩展名被归一化为空，路径不带扩展名。
	if filepath.Ext(res.Path) != "" {
		t.Errorf("unexpected ext in %q", res.Path)
	}
}

func TestTmpClearedOnOpen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃残留。
	if err := os.WriteFile(filepath.Join(tmp, "leftover.part"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(Options{Root: root, TmpDir: tmp, MaxSize: 1024}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(tmp)
	if len(entries) != 0 {
		t.Errorf("tmp not purged on open: %d entries", len(entries))
	}
}

func TestPathShardsByPrefix(t *testing.T) {
	s := newStore(t, Options{})
	path := s.Path("abcdef1234567890", "png")
	if !strings.Contains(path, string(filepath.Separator)+"ab"+string(filepath.Separator)) {
		t.Errorf("path not sharded by prefix: %s", path)
	}
	if !strings.HasSuffix(path, "abcdef1234567890.png") {
		t.Errorf("path = %s", path)
	}
}

func TestOpenMissing(t *testing.T) {
	s := newStore(t, Options{})
	_, err := s.Open(strings.Repeat("a", 64), "png")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	s := newStore(t, Options{})
	ctx := context.Background()
	res, _ := s.Put(ctx, strings.NewReader("x"), PutOptions{Ext: "bin"})

	if err := s.Delete(res.SHA256, "bin"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// 再删一次不应报错：GC 可能因为并发或重试重复调用。
	if err := s.Delete(res.SHA256, "bin"); err != nil {
		t.Errorf("second delete should be a no-op, got %v", err)
	}
}

func TestWalkFindsFiles(t *testing.T) {
	s := newStore(t, Options{})
	ctx := context.Background()

	for _, content := range []string{"one", "two", "three"} {
		if _, err := s.Put(ctx, strings.NewReader(content), PutOptions{Ext: "bin"}); err != nil {
			t.Fatal(err)
		}
	}

	var count int
	var total int64
	err := s.Walk(ctx, func(sha string, size int64) error {
		count++
		total += size
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("found %d files, want 3", count)
	}
	if total != int64(len("one")+len("two")+len("three")) {
		t.Errorf("total size = %d", total)
	}
}

func TestWalkSkipsTmp(t *testing.T) {
	s := newStore(t, Options{})
	if err := os.WriteFile(filepath.Join(s.tmp, "inflight.part"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var count int
	s.Walk(context.Background(), func(string, int64) error { count++; return nil })
	if count != 0 {
		t.Errorf("walk should skip tmp, found %d", count)
	}
}

func TestSpaceReports(t *testing.T) {
	s := newStore(t, Options{})
	space, err := s.Space()
	if err != nil {
		t.Fatalf("Space: %v", err)
	}
	if space.Total <= 0 || space.Free <= 0 {
		t.Errorf("space = %+v", space)
	}
	if space.Free > space.Total {
		t.Errorf("free > total: %+v", space)
	}
}

func TestNormalizeExt(t *testing.T) {
	cases := map[string]string{
		"png":         "png",
		".PNG":        "png",
		"jpeg ":       "jpeg",
		"":            "",
		"../../etc":   "",
		"a/b":         "",
		"verylongext": "",
	}
	for in, want := range cases {
		if got := normalizeExt(in); got != want {
			t.Errorf("normalizeExt(%q) = %q, want %q", in, got, want)
		}
	}
}
