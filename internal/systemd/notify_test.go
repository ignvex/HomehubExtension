package systemd

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listen 在临时目录建一个 unixgram socket 并返回其路径与接收通道。
func listen(t *testing.T) (string, <-chan string) {
	t.Helper()
	// 路径长度受 sun_path 限制，t.TempDir() 在长路径下可能超限，这里取短路径。
	path := filepath.Join(t.TempDir(), "n")
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	ch := make(chan string, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				close(ch)
				return
			}
			ch <- string(buf[:n])
		}
	}()
	return path, ch
}

func setSocket(t *testing.T, path string) {
	t.Helper()
	t.Setenv("NOTIFY_SOCKET", path)
}

func TestNotifyNoopWithoutSocket(t *testing.T) {
	// 明确清空：CI 里若父进程恰好是 systemd，这个变量可能已存在。
	t.Setenv("NOTIFY_SOCKET", "")
	if Enabled() {
		t.Fatal("Enabled() = true，未设置 NOTIFY_SOCKET 时应为 false")
	}
	if err := Notify("READY=1"); err != nil {
		t.Fatalf("无 socket 时应静默成功，得到 %v", err)
	}
}

func TestNotifyDeliversState(t *testing.T) {
	path, ch := listen(t)
	setSocket(t, path)

	if err := Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got := <-ch; got != "READY=1" {
		t.Fatalf("收到 %q，期望 READY=1", got)
	}

	if err := Stopping(); err != nil {
		t.Fatalf("Stopping: %v", err)
	}
	if got := <-ch; got != "STOPPING=1" {
		t.Fatalf("收到 %q，期望 STOPPING=1", got)
	}
}

func TestStatusFlattensNewlines(t *testing.T) {
	path, ch := listen(t)
	setSocket(t, path)

	// 换行会截断 KEY=VALUE 结构，必须被压平。
	if err := Status("serving\non %s", "127.0.0.1:8080"); err != nil {
		t.Fatalf("Status: %v", err)
	}
	got := <-ch
	if strings.Contains(got, "\n") {
		t.Fatalf("状态消息含换行：%q", got)
	}
	if !strings.HasPrefix(got, "STATUS=") {
		t.Fatalf("状态消息缺少前缀：%q", got)
	}
	if !strings.Contains(got, "127.0.0.1:8080") {
		t.Fatalf("状态消息丢失内容：%q", got)
	}
}

func TestAbstractSocket(t *testing.T) {
	// systemd 常用抽象命名空间（@ 前缀）。Go 用 \x00 前缀表达同一件事。
	name := "\x00hh-ext-test-" + strings.ReplaceAll(t.Name(), "/", "-")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Skipf("抽象 socket 不可用: %v", err)
	}
	defer conn.Close()

	setSocket(t, "@"+name[1:])
	if err := Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("抽象 socket 未收到消息: %v", err)
	}
	if string(buf[:n]) != "READY=1" {
		t.Fatalf("收到 %q，期望 READY=1", buf[:n])
	}
}

func TestWatchdogInterval(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	if got := WatchdogInterval(); got != 0 {
		t.Fatalf("未设置时应为 0，得到 %v", got)
	}

	t.Setenv("WATCHDOG_USEC", "60000000")
	if got := WatchdogInterval(); got != 60*time.Second {
		t.Fatalf("60000000us 应为 60s，得到 %v", got)
	}

	t.Setenv("WATCHDOG_USEC", "garbage")
	if got := WatchdogInterval(); got != 0 {
		t.Fatalf("非法值应为 0，得到 %v", got)
	}
}

func TestKeepWatchdogSendsPings(t *testing.T) {
	path, ch := listen(t)
	setSocket(t, path)
	// 20ms 周期，半周期即 10ms 一次心跳。
	t.Setenv("WATCHDOG_USEC", "20000")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		KeepWatchdog(ctx, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		close(done)
	}()

	select {
	case got := <-ch:
		if got != "WATCHDOG=1" {
			t.Fatalf("收到 %q，期望 WATCHDOG=1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("2 秒内未收到心跳")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 KeepWatchdog 未退出")
	}
}

func TestKeepWatchdogNoopWithoutWatchdog(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	// 没有心跳配置时必须立刻返回，否则会白起一个 goroutine。
	done := make(chan struct{})
	go func() {
		KeepWatchdog(context.Background(), slog.Default())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("未启用 watchdog 时 KeepWatchdog 应立即返回")
	}
}
