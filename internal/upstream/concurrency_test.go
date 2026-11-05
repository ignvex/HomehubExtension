package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// /fetch 同时在途的上游请求不得超过 3 个；多出的排队而不是失败。
func TestFetchFeedConcurrencyLimited(t *testing.T) {
	var cur, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		cur.Add(-1)
		_, _ = w.Write([]byte("<rss/>"))
	}))
	defer srv.Close()

	c := newClient(Config{FeedTimeout: 5 * time.Second}, testTransport(srv), http.DefaultTransport)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.FetchFeed(context.Background(), "http://feed.test/x"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("FetchFeed: %v", err)
	}
	if p := peak.Load(); p > defaultFeedConcurrency {
		t.Fatalf("peak concurrency = %d, want <= %d", p, defaultFeedConcurrency)
	}
}

// 名额满时排队要尊重 ctx：调用方超时后立刻返回 ErrBusy，不再占着请求。
func TestFetchFeedBusyWhenQueueCtxExpires(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte("<rss/>"))
	}))
	defer srv.Close()
	defer close(release)

	c := newClient(Config{FeedTimeout: 5 * time.Second, FeedConcurrency: 1},
		testTransport(srv), http.DefaultTransport)

	go func() { _, _ = c.FetchFeed(context.Background(), "http://feed.test/hold") }()
	// 等第一个请求占住名额。
	deadline := time.Now().Add(2 * time.Second)
	for len(c.feedSem) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first request never acquired the slot")
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin := time.Now()
	_, err := c.FetchFeed(ctx, "http://feed.test/second")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("queued call ignored ctx, took %v", took)
	}
}

// 排队上限独立于调用方 ctx：没有截止时间的调用也不会无限挂起。
func TestFetchFeedBusyAfterQueueWait(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := newClient(Config{FeedTimeout: 5 * time.Second, FeedConcurrency: 1, FeedQueueWait: 50 * time.Millisecond},
		testTransport(srv), http.DefaultTransport)
	c.feedSem <- struct{}{} // 直接占满名额
	defer func() { <-c.feedSem }()

	if _, err := c.FetchFeed(context.Background(), "http://feed.test/x"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

// 非法地址不该排队：校验失败直接返回，即使名额已满。
func TestFetchFeedInvalidURLDoesNotQueue(t *testing.T) {
	c := newClient(Config{FeedConcurrency: 1, FeedQueueWait: time.Hour}, http.DefaultTransport, http.DefaultTransport)
	c.feedSem <- struct{}{}
	defer func() { <-c.feedSem }()
	if _, err := c.FetchFeed(context.Background(), "http://127.0.0.1/x"); !errors.Is(err, ErrBlockedHost) {
		t.Fatalf("err = %v, want ErrBlockedHost", err)
	}
}
