package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

type nopLogger struct{}

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

func newPollerFixture(t *testing.T) (*feedPoller, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "p.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	var calls atomic.Int32
	p := newFeedPoller(db, func(ctx context.Context, id int64) error {
		calls.Add(1)
		return nil
	}, 2, nopLogger{})
	return p, db
}

func addFeed(t *testing.T, db store.Store, url string, nextFetch int64) store.Feed {
	t.Helper()
	past := nextFetch
	f, _, err := db.Feed().Upsert(context.Background(), &store.Feed{
		URL: url, Source: "hub", Enabled: true,
		FetchInterval: 1800, NextFetchAt: &past,
		CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// 这是 RSS 的核心循环：到期就抓。
// 在此之前 Due() 从未被调用，订阅只在首推和手动刷新时才更新。
func TestPollerFetchesDueFeeds(t *testing.T) {
	p, db := newPollerFixture(t)

	var fetched sync.Map
	p.fetch = func(ctx context.Context, id int64) error {
		fetched.Store(id, true)
		return nil
	}

	now := time.Now().Unix()
	due := addFeed(t, db, "https://example.com/a.xml", now-60)
	future := addFeed(t, db, "https://example.com/b.xml", now+3600)

	p.scan(context.Background())

	if _, ok := fetched.Load(due.ID); !ok {
		t.Error("到期的订阅应该被抓取")
	}
	if _, ok := fetched.Load(future.ID); ok {
		t.Error("未到期的订阅不该被抓取")
	}
}

func TestPollerSkipsDisabledFeeds(t *testing.T) {
	p, db := newPollerFixture(t)

	var fetched sync.Map
	p.fetch = func(ctx context.Context, id int64) error {
		fetched.Store(id, true)
		return nil
	}

	past := time.Now().Add(-time.Hour).Unix()
	f, _, _ := db.Feed().Upsert(context.Background(), &store.Feed{
		URL: "https://example.com/off.xml", Source: "hub",
		Enabled: false, FetchInterval: 1800,
		NextFetchAt: &past, CreatedAt: time.Now().Unix(),
	})

	p.scan(context.Background())
	if _, ok := fetched.Load(f.ID); ok {
		t.Error("已禁用的订阅不该被抓取")
	}
}

// 并发上限必须生效：一次扫描里有 10 个到期项，
// 但同一时刻在跑的抓取不能超过配置值。
func TestPollerRespectsConcurrency(t *testing.T) {
	p, db := newPollerFixture(t)

	var current, peak atomic.Int32
	p.fetch = func(ctx context.Context, id int64) error {
		n := current.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		current.Add(-1)
		return nil
	}

	past := time.Now().Add(-time.Hour).Unix()
	for i := 0; i < 10; i++ {
		addFeed(t, db, "https://example.com/"+string(rune('a'+i))+".xml", past)
	}

	p.scan(context.Background())

	if got := peak.Load(); got > 2 {
		t.Errorf("峰值并发 = %d，超过配置的 2", got)
	}
	if got := peak.Load(); got < 2 {
		t.Errorf("峰值并发 = %d，并发没有真正并行起来", got)
	}
}

// 抓取失败不该中断整轮：其他订阅继续抓。
func TestPollerContinuesAfterFailure(t *testing.T) {
	p, db := newPollerFixture(t)

	var ok int32
	p.fetch = func(ctx context.Context, id int64) error {
		if id%2 == 0 {
			return errors.New("boom")
		}
		atomic.AddInt32(&ok, 1)
		return nil
	}

	past := time.Now().Add(-time.Hour).Unix()
	for i := 0; i < 4; i++ {
		addFeed(t, db, "https://example.com/f"+string(rune('0'+i))+".xml", past)
	}

	p.scan(context.Background())
	if ok == 0 {
		t.Error("部分订阅失败后，其余订阅仍应被抓取")
	}
}
