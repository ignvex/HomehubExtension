package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// 内嵌时区数据：精简系统与容器通常没装 tzdata，
	// 缺了它 cleanup.timezone = "Asia/Shanghai" 会静默回落到 Local。
	_ "time/tzdata"

	"github.com/ignvex/HomehubExtension/internal/app"
	"github.com/ignvex/HomehubExtension/internal/config"
	"github.com/ignvex/HomehubExtension/internal/httpapi"
)

// 由 -ldflags 注入。
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

const usage = `HomehubExtension — Homehub 的扩展服务

用法:
  homehub-extension [serve] -config <path>        启动服务（默认子命令）
  homehub-extension enroll -config <path> -name <n>  生成一次性配对码
  homehub-extension migrate -config <path>        只执行迁移后退出
  homehub-extension backup -config <path> -out <dir>  导出一份数据库副本
  homehub-extension version                       打印版本信息

配置查找顺序:
  -config > $HOMEHUB_CONFIG > ./config.json > /etc/homehub-extension/config.json
  显式指定的路径不存在时直接失败，不会回落到下一项。
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "version":
		fmt.Printf("homehub-extension %s (commit %s, built %s)\n", version, commit, buildTime)
		return nil
	case "serve", "enroll", "migrate", "backup":
		return runWithConfig(cmd, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func runWithConfig(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	configPath := fs.String("config", "", "config.json 的路径")
	name := fs.String("name", "", "设备名（仅 enroll 使用）")
	outDir := fs.String("out", "", "备份输出目录（仅 backup 使用）")
	devMode := fs.Bool("dev", false, "开发模式：前端由 Vite 提供")
	if err := fs.Parse(args); err != nil {
		return err
	}

	explicit := *configPath
	if explicit == "" {
		explicit = os.Getenv("HOMEHUB_CONFIG")
	}

	cfg, path, err := config.Load(explicit)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return fmt.Errorf("找不到配置文件：用 -config 指定，或把 config.json 放在当前目录\n" +
				"参考 config.example.json")
		}
		return err
	}

	logger := newLogger(cfg.Log.Level, cfg.Log.Format)
	logger.Info("config loaded", "path", path)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "enroll":
		return runEnroll(cfg, logger, *name)
	case "backup":
		return runBackup(ctx, cfg, logger, *outDir)
	case "migrate":
		return fmt.Errorf("migrate 子命令尚未实现（serve 启动时会自动执行迁移）")
	}

	return app.Run(ctx, app.Options{
		Config:  cfg,
		Log:     logger,
		Build:   httpapi.BuildInfo{Version: version, Commit: commit, BuildTime: buildTime},
		WebFS:   webAssets(),
		DevMode: *devMode,
	})
}

// newLogger 按配置构造 slog。JSON 给 journald，text 给本地开发。
func newLogger(level, format string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// runEnroll 生成一次性配对码。
//
// 输出只走 stdout，错误走 stderr，方便 `ssh host homehub-extension enroll`
// 之后直接复制。配对码不落盘，服务端只存哈希。
func runEnroll(cfg config.Config, log *slog.Logger, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 只打开数据库，不启动 HTTP 与调度器。
	db, svc, err := openAuthOnly(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	code, expires, err := svc.GenerateEnrollment(ctx, name)
	if err != nil {
		return err
	}
	log.Info("enrollment created", "name", name, "expires_at", expires.Format(time.RFC3339))

	fmt.Println("配对码（15 分钟内有效，仅显示一次）：")
	fmt.Println()
	fmt.Println("   ", code)
	fmt.Println()
	fmt.Println("在浏览器打开", strings.TrimRight(cfg.Server.PublicURL, "/")+"/pair", "输入上面的码。")
	return nil
}
