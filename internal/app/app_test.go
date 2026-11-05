package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ignvex/HomehubExtension/internal/config"
	"github.com/ignvex/HomehubExtension/internal/httpapi"
	"github.com/ignvex/HomehubExtension/internal/scheduler"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

// TestSdNotifyReadiness 锁定 systemd 的就绪契约。
//
// Type=notify 的语义是「收到 READY=1 时服务已经能接流量」。这条顺序如果
// 反过来——先 READY 再 bind——`systemctl start` 会返回一个连不上的服务，
// 而表面上一切正常。这里在收到 READY 的同一刻发请求，验证它确实可用。
func TestSdNotifyReadiness(t *testing.T) {
	dir := t.TempDir()

	// systemd 侧的告示 socket。
	notifyPath := filepath.Join(dir, "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifyPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen notify socket: %v", err)
	}
	defer conn.Close()
	t.Setenv("NOTIFY_SOCKET", notifyPath)

	// 先借一个空闲端口再还给系统：http.Server 需要配置里的固定地址。
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe port: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()
	baseURL := "http://" + addr

	cfg := loadTestConfig(t, dir, addr)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: cfg,
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Build:  httpapi.BuildInfo{Version: "test"},
			WebFS: fstest.MapFS{
				"index.html": &fstest.MapFile{
					Data: []byte(`<!doctype html><html data-theme="__THEME__"><body></body></html>`),
				},
			},
		})
	}()

	messages := make(chan string, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				close(messages)
				return
			}
			messages <- string(buf[:n])
		}
	}()

	waitForMessage(t, messages, "READY=1", 15*time.Second)

	// READY 已发出，此刻服务必须真的能响应。
	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("READY=1 之后健康检查失败：%v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("READY=1 之后 /healthz = %d，期望 200", resp.StatusCode)
	}

	cancel()
	waitForMessage(t, messages, "STOPPING=1", 15*time.Second)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 正常关闭应返回 nil，得到 %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run 在 ctx 取消后 30 秒仍未返回")
	}
}

// TestListenConflictFails 验证端口被占时启动失败会返回错误，而不是继续跑。
//
// 它锁的是契约（启动失败必须让 Run 返回错误），不是实现顺序：变异验证显示，
// 把 net.Listen 换回 ListenAndServe 后这个用例仍然通过——旧实现会把 bind 错误
// 经 srvErr 送回 Run，只是晚一步、且日志里已经先打了一行假的 "listening"。
// 真正锁住「READY 不得早于 bind」的是 TestSdNotifyReadiness。
func TestListenConflictFails(t *testing.T) {
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	defer holder.Close()
	dir := t.TempDir()
	cfg := loadTestConfig(t, dir, holder.Addr().String())

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			Config: cfg,
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("端口被占时 Run 应返回错误")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("端口被占时 Run 没有返回——bind 失败被吞到后台 goroutine 里了")
	}
}

// TestNoReadyWhenBindFails 锁住「READY 不得早于 bind」这个顺序。
//
// 这是 Type=notify 唯一的失效模式中最危险的一种：进程在退出前先宣告就绪，
// systemd 于是在一个没有监听的端口上认为服务可用，反代开始送流量。
// TestSdNotifyReadiness 只能证明「就绪后可用」，抓不到方向反过来的回归——
// 因为那次 bind 本来就会成功。
func TestNoReadyWhenBindFails(t *testing.T) {
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	defer holder.Close()

	dir := t.TempDir()
	notifyPath := filepath.Join(dir, "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifyPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen notify socket: %v", err)
	}
	defer conn.Close()
	t.Setenv("NOTIFY_SOCKET", notifyPath)

	cfg := loadTestConfig(t, dir, holder.Addr().String())
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			Config: cfg,
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("端口被占时 Run 应返回错误")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("端口被占时 Run 没有返回——bind 失败被吞到后台 goroutine 里了")
	}

	// 给残余的异步通知一点时间落到 socket 上。
	if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err == nil {
		t.Fatalf("bind 失败时不应发通知，却收到 %q", buf[:n])
	}
}

func waitForMessage(t *testing.T, messages <-chan string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-messages:
			if !ok {
				t.Fatalf("通知 socket 已关闭，未等到 %q", want)
			}
			if msg == want {
				return
			}
		case <-deadline:
			t.Fatalf("%v 内未收到 %q", timeout, want)
		}
	}
}

func loadTestConfig(t *testing.T, dir, addr string) config.Config {
	t.Helper()
	body := fmt.Sprintf(`{
  "version": 1,
  "server": {"addr": %q, "public_url": "http://%s"},
  "database": {"path": %q},
  "storage": {"blob_dir": %q}
}`, addr, addr, filepath.Join(dir, "hh.db"), filepath.Join(dir, "blobs"))

	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// TestShutdownDrainsBackgroundBeforeDBClose 锁定关停顺序：轮询器 / 补跑
// （workers）、请求触发的后台任务（bg）与调度任务都要先被取消并返回，
// 数据库才关闭。后台任务收到取消后再写一次库，模拟「取消后的收尾写入」：
// 若关库早于它返回，写入会撞上已关闭的连接。
//
// 每个子测试只放一种来源：shutdown 的几步等待是串行的，混在一起时
// 省掉其中一步也可能被另一步更长的等待掩盖。
func TestShutdownDrainsBackgroundBeforeDBClose(t *testing.T) {
	cases := map[string]func(a *App, task func(context.Context)){
		"workers": func(a *App, task func(context.Context)) {
			a.goWorker(func() { task(a.rootCtx) })
		},
		"request-background": func(a *App, task func(context.Context)) {
			a.bg.Go(task)
		},
		"scheduler": func(a *App, task func(context.Context)) {
			a.sched.Register(scheduler.Task{Name: "rss-prune", Timeout: time.Hour,
				Run: func(ctx context.Context, _ map[string]any) (string, error) {
					task(ctx)
					return "", ctx.Err()
				}})
			go func() { _, _ = a.sched.RunByName(context.Background(), "rss-prune") }()
		},
	}
	for name, start := range cases {
		t.Run(name, func(t *testing.T) {
			db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"), sqlite.Options{})
			if err != nil {
				t.Fatal(err)
			}
			a := &App{log: slog.New(slog.NewTextHandler(io.Discard, nil)), db: db}
			a.rootCtx, a.rootCancel = context.WithCancel(context.Background())
			a.bg = httpapi.NewBackground(a.rootCtx)
			a.sched = scheduler.New(db.Job(), time.UTC, a.log)

			ready := make(chan struct{})
			result := make(chan error, 1)
			start(a, func(ctx context.Context) {
				close(ready)
				<-ctx.Done()
				time.Sleep(200 * time.Millisecond)
				_, err := db.Job().Start(context.Background(), "late-write", time.Now().Unix())
				result <- err
			})
			<-ready

			begin := time.Now()
			a.shutdown()
			if took := time.Since(begin); took > 5*time.Second {
				t.Fatalf("shutdown took %v; background work was not cancelled", took)
			}
			if a.db != nil {
				t.Error("shutdown should clear a.db so closeInfra does not close twice")
			}
			select {
			case err := <-result:
				if err != nil {
					t.Errorf("write after cancel failed (db closed too early?): %v", err)
				}
			default:
				t.Fatal("background task had not returned when the database was closed")
			}
		})
	}
}
