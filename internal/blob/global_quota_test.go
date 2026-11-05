package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 多个批次（各自一个 QuotaTracker）并发写入，合计也不能超过配额。
//
// 修复前带 tracker 的写入只持 tracker 自己的锁、只看 tracker 的累加值：
// 两轮并发抓取各开一个 tracker，彼此看不到对方刚落盘的字节。
func TestConcurrentTrackersShareGlobalQuota(t *testing.T) {
	const quota = 1000
	const payload = 100
	const trackers = 8
	const perTracker = 10

	s := newStore(t, Options{Quota: quota})
	ctx := context.Background()

	var okBytes atomic.Int64
	var wg sync.WaitGroup
	for ti := 0; ti < trackers; ti++ {
		tr := NewQuotaTracker(0) // 每个批次都拿到同一份过期的数据库快照
		for w := 0; w < perTracker; w++ {
			wg.Add(1)
			go func(ti, w int) {
				defer wg.Done()
				body := bytes.Repeat([]byte{byte('a' + w%26)}, payload)
				copy(body, fmt.Sprintf("%02d-%02d", ti, w))
				res, err := s.Put(ctx, bytes.NewReader(body), PutOptions{Ext: "bin", Quota: tr})
				if err == nil {
					if !res.Existed {
						okBytes.Add(res.Size)
					}
					return
				}
				if !errors.Is(err, ErrQuotaExceeded) {
					t.Errorf("err = %v", err)
				}
			}(ti, w)
		}
	}
	wg.Wait()

	if got := okBytes.Load(); got > quota {
		t.Fatalf("跨 tracker 合计落盘 %d 字节，超过配额 %d", got, quota)
	}
	if okBytes.Load() == 0 {
		t.Fatal("一个都没写成")
	}
	var disk int64
	_ = s.Walk(ctx, func(_ string, size int64) error { disk += size; return nil })
	if disk > quota {
		t.Fatalf("磁盘实际 %d 字节，超过配额 %d", disk, quota)
	}
	if s.UsedBytes() != disk {
		t.Errorf("全局视图 %d 与磁盘 %d 不一致", s.UsedBytes(), disk)
	}
}

// 调用方传入的数据库基线比磁盘视图大时，当次判定以基线为准（取 max），
// 但基线不写回全局用量。
func TestBaselineAppliesToCheckButIsNotPersisted(t *testing.T) {
	s := newStore(t, Options{Quota: 100})
	ctx := context.Background()
	if _, err := s.Put(ctx, strings.NewReader(strings.Repeat("x", 41)), PutOptions{Ext: "bin", Used: 60}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded（基线 60 + 41 > 100）", err)
	}
	if _, err := s.Put(ctx, strings.NewReader("0123456789"), PutOptions{Ext: "bin", Used: 50}); err != nil {
		t.Fatal(err)
	}
	if got := s.UsedBytes(); got != 10 {
		t.Fatalf("UsedBytes = %d, want 10（只记磁盘实际）", got)
	}
}

// 回归：本轮开始后 GC 删了文件，过期的 tracker 基线不能把全局用量
// 永久抬高——修复前 UsedBytes 会停在「已删字节 + 新写字节」。
func TestStaleTrackerDoesNotRatchetUsage(t *testing.T) {
	s := newStore(t, Options{Quota: 1000})
	ctx := context.Background()
	r, err := s.Put(ctx, strings.NewReader(strings.Repeat("a", 600)), PutOptions{Ext: "bin"})
	if err != nil {
		t.Fatal(err)
	}
	tr := NewQuotaTracker(600) // 本轮开始时的数据库快照
	if err := s.Delete(r.SHA256, "bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, strings.NewReader("b"), PutOptions{Ext: "bin", Quota: tr}); err != nil {
		t.Fatal(err)
	}
	if got := s.UsedBytes(); got != 1 {
		t.Fatalf("UsedBytes = %d, want 1（磁盘实际）", got)
	}
}

// 重新打开时全局用量从磁盘恢复，未入库的孤儿文件也计入。
func TestOpenMeasuresExistingFiles(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(Options{Root: dir, Quota: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Put(context.Background(), strings.NewReader(strings.Repeat("z", 30)), PutOptions{Ext: "bin"}); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(Options{Root: dir, Quota: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.UsedBytes(); got != 30 {
		t.Fatalf("重开后 UsedBytes = %d, want 30", got)
	}
}

func TestDeleteReleasesGlobalUsed(t *testing.T) {
	s := newStore(t, Options{Quota: 20})
	ctx := context.Background()
	r, err := s.Put(ctx, strings.NewReader(strings.Repeat("a", 15)), PutOptions{Ext: "bin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(r.SHA256, "bin"); err != nil {
		t.Fatal(err)
	}
	if s.UsedBytes() != 0 {
		t.Fatalf("删除后 UsedBytes = %d, want 0", s.UsedBytes())
	}
	if _, err := s.Put(ctx, strings.NewReader(strings.Repeat("b", 15)), PutOptions{Ext: "bin"}); err != nil {
		t.Fatalf("释放后应能再写: %v", err)
	}
}

func TestWalkFilesAndRemoveIfOlder(t *testing.T) {
	s := newStore(t, Options{})
	ctx := context.Background()
	r, err := s.Put(ctx, strings.NewReader("orphan"), PutOptions{Ext: "png"})
	if err != nil {
		t.Fatal(err)
	}
	// 形状不对的文件不能被 WalkFiles 报出来，GC 只碰自己写的东西。
	junkDir := filepath.Join(s.root, r.SHA256[:2])
	if err := os.WriteFile(filepath.Join(junkDir, "README"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var files []FileInfo
	if err := s.WalkFiles(ctx, func(f FileInfo) error { files = append(files, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].SHA256 != r.SHA256 || files[0].Ext != "png" {
		t.Fatalf("WalkFiles = %+v", files)
	}

	// mtime 晚于 cutoff：宽限期内不删。
	removed, _, err := s.RemoveIfOlder(r.SHA256, "png", time.Now().Add(-time.Hour))
	if err != nil || removed {
		t.Fatalf("宽限期内被删: removed=%v err=%v", removed, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(r.Path, old, old); err != nil {
		t.Fatal(err)
	}
	removed, size, err := s.RemoveIfOlder(r.SHA256, "png", time.Now().Add(-time.Hour))
	if err != nil || !removed || size != int64(len("orphan")) {
		t.Fatalf("应删除: removed=%v size=%d err=%v", removed, size, err)
	}
	if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
		t.Fatal("文件仍在")
	}
}

// 去重命中会刷新 mtime：复用一个旧孤儿的写入方即将入库，GC 不能删它。
func TestDedupHitRefreshesMtime(t *testing.T) {
	s := newStore(t, Options{})
	ctx := context.Background()
	r, err := s.Put(ctx, strings.NewReader("reuse"), PutOptions{Ext: "bin"})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(r.Path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, strings.NewReader("reuse"), PutOptions{Ext: "bin"}); err != nil {
		t.Fatal(err)
	}
	removed, _, err := s.RemoveIfOlder(r.SHA256, "bin", time.Now().Add(-time.Hour))
	if err != nil || removed {
		t.Fatalf("刚被复用的文件被 GC 删除: removed=%v err=%v", removed, err)
	}
}
