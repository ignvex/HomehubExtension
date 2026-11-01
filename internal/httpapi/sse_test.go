package httpapi

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/auth"
)

// SSE 的价值全在「事件真的从生产者流到消费者」。
// 只测 handler 返回 200 是不够的：曾经 Events 总线和 handler 都写好了，
// 但没有任何地方调用 Publish，整条管道只会发心跳。
// 这个测试用真实 HTTP 连接走完整链路。
func TestSSEDeliversPublishedEvents(t *testing.T) {
	h := newHarness(t)
	session := h.newSessionCookie(t)

	srv := httptest.NewServer(h.router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/v1/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	// EventSource 无法设 Authorization 头，真实浏览器走 cookie。
	req.Header.Set("Cookie", "hh_session="+session)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	// 少了它，Caddy/nginx 会把事件攒着不发，前端看起来像"实时更新坏了"。
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("缺 X-Accel-Buffering: no —— 反向代理会缓冲事件流")
	}

	lines := make(chan string, 32)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	// 首帧必须立刻到达，否则前端无法确认连接已建立。
	select {
	case line, ok := <-lines:
		if !ok {
			t.Fatal("连接立刻被关闭")
		}
		if !strings.HasPrefix(line, ":") {
			t.Errorf("首帧 = %q，期望注释行", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("首帧没在 3 秒内到达")
	}

	// 给订阅登记一点时间，避免 Publish 早于 Subscribe。
	time.Sleep(150 * time.Millisecond)
	h.bus.Publish("entry.created", map[string]any{"entry_id": 42, "title": "测试"})

	var sawEvent, sawData bool
	deadline := time.After(5 * time.Second)
	for !(sawEvent && sawData) {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("连接被提前关闭")
			}
			if strings.HasPrefix(line, "event: entry.created") {
				sawEvent = true
			}
			if strings.Contains(line, `"entry_id":42`) {
				sawData = true
			}
		case <-deadline:
			t.Fatalf("没收到事件（sawEvent=%v sawData=%v）", sawEvent, sawData)
		}
	}
}

// TestSSERequiresAuth 确认这条流不是匿名可读的。
// 它用 cookie 而非 Bearer 认证，很容易在中间件里配错豁免范围。
func TestSSERequiresAuth(t *testing.T) {
	h := newHarness(t)

	w := h.do(t, httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("匿名连接 = %d, want 401", w.Code)
	}
}

// newSessionCookie 走完 enroll → challenge → token，返回 hh_session 的值。
func (h *harness) newSessionCookie(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	code, _, err := h.svc.GenerateEnrollment(ctx, "sse")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	w := h.doJSON(t, http.MethodPost, "/api/v1/auth/enroll", map[string]any{
		"token": code, "name": "sse-device", "algorithm": "ed25519",
		"public_key": base64.StdEncoding.EncodeToString(pub),
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("enroll = %d: %s", w.Code, w.Body.String())
	}
	var enrollResp struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &enrollResp); err != nil {
		t.Fatal(err)
	}

	w = h.doJSON(t, http.MethodPost, "/api/v1/auth/challenge",
		map[string]any{"client_id": enrollResp.ClientID}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("challenge = %d", w.Code)
	}
	var chResp struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &chResp); err != nil {
		t.Fatal(err)
	}

	sig := ed25519.Sign(priv, auth.AuthMessage(enrollResp.ClientID, chResp.Nonce))
	w = h.doJSON(t, http.MethodPost, "/api/v1/auth/token", map[string]any{
		"client_id": enrollResp.ClientID, "nonce": chResp.Nonce,
		"signature": base64.StdEncoding.EncodeToString(sig),
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("token = %d: %s", w.Code, w.Body.String())
	}

	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == cookieSession {
			return cookie.Value
		}
	}
	t.Fatal("登录响应里没有 hh_session cookie")
	return ""
}

// 优雅关闭必须先断开 SSE：http.Server.Shutdown 不会取消在途请求的
// context，长连接会一直把 Shutdown 拖到 15 秒超时。
// app.shutdown 的做法是调用 Bus.Close()，这里验证那个机制真的能让
// handler 立刻返回，而不是等客户端自己断开。
func TestBusCloseEndsSSEStream(t *testing.T) {
	h := newHarness(t)
	session := h.newSessionCookie(t)

	srv := httptest.NewServer(h.router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/v1/events/stream", nil)
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
		t.Fatalf("status = %d, want 200（SSE 未建立，后续断言无意义）", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	// 首帧是 ": connected"，读到它才说明流真的建立了。
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读首帧: %v", err)
	}
	if !strings.Contains(line, "connected") {
		t.Fatalf("首帧 = %q, want 包含 connected", line)
	}

	// 这一步对应 app.shutdown 里的 a.bus.Close()。
	h.bus.Close()

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, reader)
		done <- err
	}()

	select {
	case <-done:
		// EOF：handler 收到关闭的通道并返回了。
	case <-time.After(2 * time.Second):
		t.Fatal("Bus.Close 后 SSE 流未结束，关闭会被拖到 Shutdown 超时")
	}
}
