package httpapi

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// app 给 http.Server 设了 ReadTimeout（读完整请求的上限）。这条用例锁定
// 它不会切断 SSE：无 body 的 GET 在 handler 开始前就进入后台读，
// net/http 在 connReader.startBackgroundRead 里把读 deadline 清零，
// 之后 ReadTimeout 不再作用于这个连接。若将来 net/http 改了这一行为，
// 后台读会在 deadline 到期时返回超时错误并取消请求 context——
// 表现为 SSE 每 ReadTimeout 秒断一次，这里会立刻失败。
func TestSSESurvivesServerReadTimeout(t *testing.T) {
	h := newHarness(t)
	session := h.newSessionCookie(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           h.router,
		ReadTimeout:       200 * time.Millisecond,
		ReadHeaderTimeout: 200 * time.Millisecond,
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+ln.Addr().String()+"/api/v1/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "hh_session="+session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	lines := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	// 首帧到达，然后保持 1 秒（5 倍 ReadTimeout）不应有任何断开。
	select {
	case line := <-lines:
		if !strings.HasPrefix(line, ":") {
			t.Fatalf("首帧 = %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("首帧没到")
	}
	hold := time.After(time.Second)
	for waiting := true; waiting; {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("SSE 在 ReadTimeout 之后被断开")
			}
			_ = line
		case <-hold:
			waiting = false
		}
	}

	// 连接还活着且 handler 的 context 未被取消：事件仍能送达。
	h.bus.Publish("entry.created", map[string]any{"entry_id": 7})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("SSE 在 ReadTimeout 之后被断开")
			}
			if strings.HasPrefix(line, "event: entry.created") {
				return
			}
		case <-deadline:
			t.Fatal("保持 1 秒后事件未送达：请求 context 可能已被 ReadTimeout 取消")
		}
	}
}
