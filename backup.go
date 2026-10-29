package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/ignvex/HomehubExtension/internal/config"
	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

// runBackup 导出一份事务一致的数据库副本。
//
// 刻意做成子命令而不是让运维脚本去调 sqlite3 CLI：
// 整个项目承诺「单二进制部署」，备份却要求额外装一个工具说不通。
// 而且 VACUUM INTO 的语义由我们自己控制，不会被不同版本的 CLI 行为影响。
//
// 媒体文件不在这一步处理——它们是不可变的内容寻址文件，
// 用 rsync --link-dest 做增量比在这里复制高效得多，见 deploy/backup.sh。
func runBackup(ctx context.Context, cfg config.Config, log *slog.Logger, outDir string) error {
	if outDir == "" {
		return fmt.Errorf("缺少 -out：指定备份输出目录")
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return fmt.Errorf("创建备份目录: %w", err)
	}

	// 备份不该触发迁移：只读地打开现有库，版本不符时明确报错而不是悄悄升级。
	db, err := sqlite.Open(ctx, cfg.Database.Path, sqlite.Options{
		BusyTimeout: cfg.Database.BusyTimeout.Duration,
		ReadConns:   1,
	})
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}
	defer db.Close()

	// 文件名带日期，与 rsync 的目录命名保持一致，便于同一份备份对应。
	dest := filepath.Join(outDir, "homehub.db")
	if err := db.BackupTo(ctx, dest); err != nil {
		return err
	}

	info, err := os.Stat(dest)
	if err != nil {
		return err
	}
	log.Info("backup written", "path", dest, "bytes", info.Size(),
		"at", time.Now().Format(time.RFC3339))
	fmt.Printf("%s\t%d bytes\n", dest, info.Size())
	return nil
}
