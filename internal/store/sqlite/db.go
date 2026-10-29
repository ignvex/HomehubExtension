// Package sqlite 实现 store.Store。
//
// 并发模型：单写连接 + 只读连接池。SQLite 的写入本来就串行化，把写连接
// 钉死为 1 能彻底消除 SQLITE_BUSY，在单核机器上简单可靠。
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"

	_ "modernc.org/sqlite"
)

// Options 是打开数据库的参数。零值使用合理默认。
type Options struct {
	BusyTimeout time.Duration
	ReadConns   int
}

// DB 是 store.Store 的 SQLite 实现。写连接单条串行，读连接池并发。
type DB struct {
	rw   *sql.DB
	ro   *sql.DB
	path string
	opts Options
}

var _ store.Store = (*DB)(nil)

// Open 打开数据库、校验完整性、执行迁移。
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is empty")
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = 5 * time.Second
	}
	if opts.ReadConns <= 0 {
		opts.ReadConns = 3
	}

	rw, err := openPool(path, opts, false)
	if err != nil {
		return nil, err
	}
	db := &DB{rw: rw, path: path, opts: opts}

	// quick_check 比 integrity_check 快得多，能抓住绝大多数损坏。
	var check string
	if err := rw.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&check); err != nil {
		rw.Close()
		return nil, fmt.Errorf("quick_check: %w", err)
	}
	if check != "ok" {
		rw.Close()
		return nil, fmt.Errorf("database %s is corrupted (%s); restore from backup", path, check)
	}

	if err := migrate(ctx, rw); err != nil {
		rw.Close()
		return nil, err
	}

	ro, err := openPool(path, opts, true)
	if err != nil {
		rw.Close()
		return nil, err
	}
	db.ro = ro
	return db, nil
}

func openPool(path string, opts Options, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	pragmas := []string{
		"busy_timeout(" + fmt.Sprint(opts.BusyTimeout.Milliseconds()) + ")",
		"foreign_keys(1)",
		"temp_store(MEMORY)",
	}
	if readOnly {
		// 只读连接池不设置 journal_mode：那是持久化属性，由写连接负责。
		pragmas = append(pragmas, "query_only(1)")
		q.Set("mode", "ro")
	} else {
		pragmas = append(pragmas,
			"journal_mode(WAL)",
			"synchronous(NORMAL)",
			"cache_size(-16000)",
		)
	}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}

	dsn := "file:" + filepath.ToSlash(path) + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if readOnly {
		db.SetMaxOpenConns(opts.ReadConns)
		db.SetMaxIdleConns(opts.ReadConns)
	} else {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	return db, nil
}

func (d *DB) Ping(ctx context.Context) error {
	var one int
	return d.ro.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := d.ro.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

func (d *DB) Close() error {
	var errs []error
	if d.ro != nil {
		if err := d.ro.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if d.rw != nil {
		// 关库前强制 checkpoint，避免 -wal 文件残留未合并的事务。
		if _, err := d.rw.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			// 只读文件系统或已关闭时可能失败，不影响数据安全。
			_ = err
		}
		if err := d.rw.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return joinErrs(errs)
}

func joinErrs(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return fmt.Errorf("%s", strings.Join(parts, "; "))
}

// withTx 在写连接上跑一个事务：panic 与错误都回滚。
func (d *DB) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.rw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Vacuum 回收空闲页。
//
// full 模式会重写整个数据库，需要约 2 倍大小的临时空间且全程阻塞写入，
// 所以默认走 incremental。auto_vacuum=INCREMENTAL 在建库时已设好，
// 增量模式几乎瞬间完成。
func (d *DB) Vacuum(ctx context.Context, full bool) (int64, error) {
	before, err := d.fileSize()
	if err != nil {
		return 0, err
	}
	stmt := "PRAGMA incremental_vacuum"
	if full {
		stmt = "VACUUM"
	}
	if _, err := d.rw.ExecContext(ctx, stmt); err != nil {
		return 0, fmt.Errorf("vacuum: %w", err)
	}
	after, err := d.fileSize()
	if err != nil {
		return 0, nil
	}
	if before > after {
		return before - after, nil
	}
	return 0, nil
}

func (d *DB) fileSize() (int64, error) {
	info, err := os.Stat(d.path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// BackupTo 把数据库导出成一份完整、事务一致的副本。
//
// 用 VACUUM INTO 而不是拷文件：直接 cp .db 会漏掉 WAL 里尚未合并的事务，
// 恢复出来的数据是旧的，而且整个过程不会报任何错。
//
// 写到临时名再 rename：备份中途被中断，不会留下一个「看起来正常」的半个文件。
func (d *DB) BackupTo(ctx context.Context, dest string) error {
	tmp := dest + ".partial"
	_ = os.Remove(tmp)

	// 参数绑定可用（已实测）：modernc 驱动支持 VACUUM INTO ?。
	// 早期版本这里拼过字符串，属于规范里的阻塞项，已修正。
	if _, err := d.rw.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("vacuum into %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
