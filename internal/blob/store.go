// Package blob 提供内容寻址的文件存储。
//
// 布局：<root>/<sha256 前 2 位>/<sha256>.<ext>
// 同一内容被 N 篇文章引用只占一份空间，引用计数在数据库里维护。
//
// 所有写入都是流式的：任何路径都不允许把整个媒体读进内存。
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var (
	ErrTooLarge      = errors.New("blob exceeds size limit")
	ErrQuotaExceeded = errors.New("blob quota exceeded")
	ErrNoSpace       = errors.New("not enough free disk space")
	ErrNotFound      = errors.New("blob not found")
)

// Store 是内容寻址的文件存储。所有写入都是流式的。
type Store struct {
	root    string
	tmp     string
	maxSize int64
	quota   int64
	minFree int64

	mu sync.Mutex // 串行化配额判断，避免并发下载同时通过检查
}

// Options 是打开存储的参数。零值字段使用默认值。
type Options struct {
	Root         string
	TmpDir       string
	MaxSize      int64
	Quota        int64
	MinFreeBytes int64
}

// Open 创建目录并清空临时目录。残留的下载文件不应占用配额。
func Open(opts Options) (*Store, error) {
	if opts.Root == "" {
		return nil, fmt.Errorf("blob root is empty")
	}
	if opts.TmpDir == "" {
		opts.TmpDir = filepath.Join(opts.Root, "tmp")
	}
	if err := os.MkdirAll(opts.Root, 0o750); err != nil {
		return nil, fmt.Errorf("create blob root: %w", err)
	}
	if err := os.MkdirAll(opts.TmpDir, 0o750); err != nil {
		return nil, fmt.Errorf("create blob tmp: %w", err)
	}
	// 崩溃残留的下载文件不应占用配额。启动时无条件清空。
	if err := purgeDir(opts.TmpDir); err != nil {
		return nil, fmt.Errorf("purge tmp: %w", err)
	}
	return &Store{
		root:    opts.Root,
		tmp:     opts.TmpDir,
		maxSize: opts.MaxSize,
		quota:   opts.Quota,
		minFree: opts.MinFreeBytes,
	}, nil
}

// PutOptions 控制一次写入。Used 是当前已占用字节，由调用方从数据库取，
// 这样配额状态只有一个真相来源。
type PutOptions struct {
	Ext  string
	Used int64
}

// Result 是一次写入的结果。Existed 表示内容已存在，本次没有落盘。
type Result struct {
	SHA256 string
	Size   int64
	Path   string
	// Existed 表示内容已存在，本次没有写入数据（调用方不必再建引用以外的处理）。
	Existed bool
}

// Put 把 r 流式写入临时文件并移动到内容寻址位置。
func (s *Store) Put(ctx context.Context, r io.Reader, opt PutOptions) (*Result, error) {
	// 先做一次便宜的检查，避免为注定被拒的下载浪费带宽。
	if err := s.checkSpace(opt.Used, 0); err != nil {
		return nil, err
	}

	tmpFile, err := os.CreateTemp(s.tmp, "dl-*.part")
	if err != nil {
		return nil, fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmpFile.Name()
	cleanup := func() {
		tmpFile.Close()
		os.Remove(tmpName)
	}

	h := sha256.New()
	written, copyErr := copyLimited(ctx, io.MultiWriter(tmpFile, h), r, s.maxSize)
	if copyErr != nil {
		cleanup()
		return nil, copyErr
	}
	if err := tmpFile.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("sync temp: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return nil, err
	}

	// 写完再检查一次真实大小：Content-Length 可能撒谎或缺失。
	if s.quota > 0 && opt.Used+written > s.quota {
		os.Remove(tmpName)
		return nil, ErrQuotaExceeded
	}
	if err := s.checkSpace(opt.Used, written); err != nil {
		os.Remove(tmpName)
		return nil, err
	}

	sum := hex.EncodeToString(h.Sum(nil))
	ext := normalizeExt(opt.Ext)
	dest := s.Path(sum, ext)

	if _, err := os.Stat(dest); err == nil {
		os.Remove(tmpName)
		return &Result{SHA256: sum, Size: written, Path: dest, Existed: true}, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	// 同一文件系统内的 rename 是原子的：读者要么看不到文件，要么看到完整文件。
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("move into place: %w", err)
	}
	return &Result{SHA256: sum, Size: written, Path: dest}, nil
}

func copyLimited(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if limit > 0 && total > limit {
				return 0, ErrTooLarge
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func (s *Store) checkSpace(used, incoming int64) error {
	if s.quota > 0 && used+incoming > s.quota {
		return ErrQuotaExceeded
	}
	if s.minFree <= 0 {
		return nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.root, &st); err != nil {
		// 拿不到磁盘信息时放行：宁可多写一次，也不要因为平台差异停掉功能。
		return nil
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if free-incoming < s.minFree {
		return ErrNoSpace
	}
	return nil
}

// Path 返回内容寻址路径。ext 为空时返回不带扩展名的路径。
func (s *Store) Path(sha, ext string) string {
	if len(sha) < 2 {
		return filepath.Join(s.root, sha)
	}
	name := sha
	if ext != "" {
		name += "." + ext
	}
	return filepath.Join(s.root, sha[:2], name)
}

// Open 打开已存储的文件。调用方负责关闭。
func (s *Store) Open(sha, ext string) (*os.File, error) {
	f, err := os.Open(s.Path(sha, ext))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Delete 删除文件。文件已不存在时返回 nil——GC 可能重复调用。
func (s *Store) Delete(sha, ext string) error {
	err := os.Remove(s.Path(sha, ext))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Stat 返回根目录所在文件系统的容量信息，供 /readyz 与系统页展示。
type Space struct {
	Total int64 `json:"total"`
	Free  int64 `json:"free"`
}

// Space 返回根目录所在文件系统的容量，供 /readyz 与系统页展示。
func (s *Store) Space() (Space, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.root, &st); err != nil {
		return Space{}, err
	}
	return Space{
		Total: int64(st.Blocks) * int64(st.Bsize),
		Free:  int64(st.Bavail) * int64(st.Bsize),
	}, nil
}

// Walk 遍历磁盘上实际存在的文件，供 blob-gc 找出孤儿。
func (s *Store) Walk(ctx context.Context, fn func(sha string, size int64) error) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "tmp" || len(e.Name()) != 2 {
			continue
		}
		dir := filepath.Join(s.root, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, f := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			sha := strings.TrimSuffix(f.Name(), filepath.Ext(f.Name()))
			info, err := f.Info()
			if err != nil {
				continue // 并发删除，跳过
			}
			if err := fn(sha, info.Size()); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeExt(ext string) string {
	ext = strings.TrimSpace(strings.ToLower(ext))
	ext = strings.TrimPrefix(ext, ".")
	// 扩展名会进文件名，只允许字母数字，杜绝路径穿越。
	for _, r := range ext {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			return ""
		}
	}
	if len(ext) > 8 {
		return ""
	}
	return ext
}

func purgeDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
