package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 并发写入必须共享同一份配额视图。
//
// 修复前每个 goroutine 都拿调用方传入的同一个 Used 快照去检查，
// 各自都能通过，合计写入 N 倍配额。QuotaTracker 让每个成功落盘的
// 写入把字节数累加进去，后续写入看到的是最新用量。
func TestConcurrentPutRespectsQuota(t *testing.T) {
	const quota = 1000
	const payload = 100
	const writers = 40

	s := newStore(t, Options{Quota: quota})
	ctx := context.Background()

	// 内容各不相同，避免内容寻址去重让计数变得不可预测。
	makeBody := func(i int) []byte {
		body := make([]byte, payload)
		for j := range body {
			body[j] = byte('a' + (i+j)%26)
		}
		// 让每个分片唯一：同长度但内容不同。
		copy(body, fmt.Sprintf("%04d", i))
		return body
	}

	tracker := NewQuotaTracker(0)

	var mu sync.Mutex
	okCount := 0
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := s.Put(ctx, bytes.NewReader(makeBody(i)), PutOptions{Ext: "bin", Quota: tracker})
			if err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
				return
			}
			if !errors.Is(err, ErrQuotaExceeded) {
				t.Errorf("writer %d: err = %v, want nil 或 ErrQuotaExceeded", i, err)
			}
		}(i)
	}
	wg.Wait()

	// 成功写入的字节总量不得超过配额。
	if got := int64(okCount) * payload; got > quota {
		t.Errorf("落盘 %d 字节，超过配额 %d（成功 %d 次）", got, quota, okCount)
	}
	// 也不能因为过度保守而一个都不写：1000/100 = 10 个槽位。
	if okCount == 0 {
		t.Error("一个都没写成，配额判定过于激进")
	}
	if used := tracker.Used(); used != int64(okCount)*payload {
		t.Errorf("tracker.Used() = %d, want %d", used, int64(okCount)*payload)
	}
}

// 内容已存在（去重命中）不应计入新增用量：
// 文件本来就在盘上，数据库的 SUM(size) 也只算一份。
func TestQuotaTrackerSkipsDedupHit(t *testing.T) {
	s := newStore(t, Options{Quota: 1 << 20})
	ctx := context.Background()

	tracker := NewQuotaTracker(0)
	body := []byte("duplicate content")

	first, err := s.Put(ctx, bytes.NewReader(body), PutOptions{Ext: "bin", Quota: tracker})
	if err != nil {
		t.Fatal(err)
	}
	if first.Existed {
		t.Fatal("首次写入不应命中 Existed")
	}
	afterFirst := tracker.Used()
	if afterFirst != int64(len(body)) {
		t.Fatalf("首次写入后 used = %d, want %d", afterFirst, len(body))
	}

	second, err := s.Put(ctx, bytes.NewReader(body), PutOptions{Ext: "bin", Quota: tracker})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Existed {
		t.Fatal("相同内容应命中 Existed")
	}
	if got := tracker.Used(); got != afterFirst {
		t.Errorf("去重命中不该增加用量: used = %d, want %d", got, afterFirst)
	}
}

// 没有 tracker 时，Used 是调用方给的静态快照。
// 这个测试固定住它的语义：检查基于 Used，而不是从磁盘反推。
func TestPutUsesProvidedBaseline(t *testing.T) {
	s := newStore(t, Options{Quota: 100})
	ctx := context.Background()

	_, err := s.Put(ctx, bytes.NewReader([]byte("0123456789")), PutOptions{Ext: "bin", Used: 0})
	if err != nil {
		t.Fatalf("Used=0 时应允许写入: %v", err)
	}

	// 同样的内容换个基线：已用 95，再写 10 字节必须被拒。
	_, err = s.Put(ctx, bytes.NewReader([]byte("abcdefghij")), PutOptions{Ext: "bin", Used: 95})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
}

// nil tracker 的 Used 必须安全，否则调用方要到处写 nil 判断。
func TestQuotaTrackerNilSafe(t *testing.T) {
	var q *QuotaTracker
	if got := q.Used(); got != 0 {
		t.Errorf("nil tracker Used() = %d, want 0", got)
	}
}
