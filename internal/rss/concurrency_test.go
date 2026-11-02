package rss

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/store"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

// newProbeService 构造一个允许内网的抓取服务，用于并发/连接复用测试。
func newProbeService(t *testing.T) (*Service, store.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	blobs, err := blob.Open(blob.Options{
		Root:   filepath.Join(dir, "blobs"),
		TmpDir: filepath.Join(dir, "blobs", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := NewService(db, blobs, Config{
		UserAgent: "test/1", RequestTimeout: 10 * time.Second,
		FetchTimeout: 20 * time.Second, MaxFeedBytes: 1 << 20,
		MaxEntryBytes: 1 << 20, DefaultInterval: 30 * time.Minute,
		MinInterval: 10 * time.Minute, MaxInterval: 6 * time.Hour,
		AllowPrivateHosts: true,
		FetchConcurrency:  2,
	}, nil)
	return svc, db
}

// rss.fetch_concurrency 之前只传给 poller，HTTP 层触发的抓取
// （宿主 upsert 的异步首抓、手动刷新）完全不受约束。
// 宿主按文档建议做对账补推时，N 个订阅就是 N 个并发连接打向外部站点。
func TestFetchConcurrencyIsEnforced(t *testing.T) {
	svc, st := newProbeService(t)

	var active, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond) // 模拟慢站点，让并发窗口可见
		active.Add(-1)

		body := strings.ReplaceAll(sampleFeed, "%BASE%", "http://"+r.Host)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	const feeds = 20
	ids := make([]int64, 0, feeds)
	now := time.Now().Unix()
	for i := 0; i < feeds; i++ {
		f, _, err := st.Feed().Upsert(context.Background(), &store.Feed{
			URL:    fmt.Sprintf("%s/feed-%d.xml", srv.URL, i),
			Source: "hub", Enabled: true,
			FetchInterval: 1800, CreatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.ID)
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			_, _ = svc.Fetch(context.Background(), id)
		}(id)
	}
	wg.Wait()

	if got := peak.Load(); got > 2 {
		t.Errorf("%d 个订阅并发抓取，峰值并发 = %d, want <= 2（fetch_concurrency）", feeds, got)
	}
	// 也不能退化成串行：2 个槽位应该真的被用上。
	if got := peak.Load(); got < 2 {
		t.Errorf("峰值并发 = %d, want 2（并发额度没被用上，可能是过度串行）", got)
	}
}

// 每次抓取都 newSafeTransport 会让连接池只剩一条连接，
// 每次都要重做 TCP + TLS 握手。宿主批量补推时这是 N 次完整握手。
func TestFetchReusesConnections(t *testing.T) {
	svc, st := newProbeService(t)

	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := strings.ReplaceAll(sampleFeed, "%BASE%", "http://"+r.Host)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	feed, _, err := st.Feed().Upsert(context.Background(), &store.Feed{
		URL:    srv.URL + "/feed.xml",
		Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	const rounds = 10
	for i := 0; i < rounds; i++ {
		if _, err := svc.Fetch(context.Background(), feed.ID); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	time.Sleep(100 * time.Millisecond) // 等 connState 回调落定

	// 复用正常时应该是 1 条。给一点余量：不同机器上 keep-alive
	// 可能因空闲回收重建一次。
	if got := conns.Load(); got > 3 {
		t.Errorf("抓取 %d 次建了 %d 条 TCP 连接，连接没有被复用", rounds, got)
	}
}

// 排队必须受调用方 context 约束：槽位被慢站点占满时，
// 新请求要能在自己的超时到达时退出，而不是无限等待。
func TestFetchQueueRespectsContext(t *testing.T) {
	svc, st := newProbeService(t)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // 占住槽位直到测试放行
		body := strings.ReplaceAll(sampleFeed, "%BASE%", "http://"+r.Host)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	defer close(release)

	now := time.Now().Unix()
	var ids []int64
	for i := 0; i < 4; i++ {
		f, _, err := st.Feed().Upsert(context.Background(), &store.Feed{
			URL:    fmt.Sprintf("%s/slow-%d.xml", srv.URL, i),
			Source: "hub", Enabled: true,
			FetchInterval: 1800, CreatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.ID)
	}

	// 先把两个槽位占满。
	for i := 0; i < 2; i++ {
		go func(id int64) { _, _ = svc.Fetch(context.Background(), id) }(ids[i])
	}
	time.Sleep(100 * time.Millisecond)

	// 第三个请求必须在自己 300ms 的预算内放弃，而不是卡死。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := svc.Fetch(ctx, ids[2]); err == nil {
		t.Fatal("槽位占满且有 context 超时时，应当返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("排队耗时 %v，context 约束失效", elapsed)
	}
}
