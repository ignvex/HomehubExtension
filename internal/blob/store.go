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
	"math"
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

	// mu 串行化「最终配额判定 + 落位」。没有它，多个并发写入会各自
	// 用同一份过期快照通过检查，然后一起 rename，合计超出配额。
	mu sync.Mutex
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
	// Quota 让一批并发写入共享同一份累加视图。为 nil 时只用 Used，
	// 并发写入之间的窗口由 Store.mu 串行化收窄，但各自仍看到同一份快照。
	Quota *QuotaTracker
}

// QuotaTracker 让一组并发写入共享同一份配额视图。
//
// 调用方在批次开始时查一次数据库得到基线，之后每个成功落盘的写入把
// 实际字节数累加进来。没有它时，N 个并发下载都拿同一份快照去检查
// 配额，各自都能通过，合计可以写入 N 倍配额。
type QuotaTracker struct {
	mu    sync.Mutex
	total int64
}

// NewQuotaTracker 用当前已占用字节数开一个批次视图。
func NewQuotaTracker(used int64) *QuotaTracker {
	return &QuotaTracker{total: used}
}

// Used 返回当前用量：基线 + 本批次已落盘字节。
// nil 接收者返回 0，方便调用方不写分支。
func (q *QuotaTracker) Used() int64 {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.total
}

// lockQuota 取得配额判定所需的锁。
// 有 tracker 时用它的锁，让同一批次共享累加值；否则退回 Store.mu，
// 至少保证「检查」与「落位」不交叠。
func (s *Store) lockQuota(q *QuotaTracker) func() {
	if q != nil {
		q.mu.Lock()
		return q.mu.Unlock
	}
	s.mu.Lock()
	return s.mu.Unlock
}

// Result 是一次写入的结果。Existed 表示内容已存在，本次没有落盘。
type Result struct {
	SHA256 string
	Size   int64
	Path   string
	// Existed 表示内容已存在，本次没有写入数据（调用方不必再建引用以外的处理）。
	Existed bool
}

// place 在配额锁内做最终判定并把临时文件安置到内容寻址位置。
//
// 单独拆出来有三个原因：Put 的主体只负责流式写入；
// 「检查 — stat — rename」必须是连续的一段；
// rename 失败时临时文件的清理只写一次。
func (s *Store) place(tmpName, sum string, ext string, written int64, opt PutOptions) (*Result, error) {
	unlock := s.lockQuota(opt.Quota)
	defer unlock()

	// 去重检查必须排在配额检查之前。
	//
	// 内容已在库里意味着这次写入不占任何新空间（数据库的 SUM(size)
	// 也只算一份）。若先查配额，接近上限时同一张被多篇文章引用的
	// 图片会被判成 quota_exceeded —— 一个纯粹由检查顺序制造的失败，
	// 而用户在界面上看到的是「图片加载失败」。
	dest := s.Path(sum, ext)
	if _, err := os.Stat(dest); err == nil {
		os.Remove(tmpName)
		return &Result{SHA256: sum, Size: written, Path: dest, Existed: true}, nil
	}

	// 这里读 opt.Quota.total 而不是调 Quota.Used()：
	// 上面的 lockQuota 已经持有 q.mu，而 sync.Mutex 不可重入，
	// 再进 Used() 会直接死锁（测试表现为整个包超时）。
	used := opt.Used
	if opt.Quota != nil {
		used = opt.Quota.total
	}

	// 到这里才确定要新增 written 字节。
	if s.quota > 0 && used+written > s.quota {
		os.Remove(tmpName)
		return nil, ErrQuotaExceeded
	}
	if err := s.checkSpace(used, written); err != nil {
		os.Remove(tmpName)
		return nil, err
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
	// 只有新增内容才计入用量：内容寻址去重命中时没有多占空间。
	if opt.Quota != nil {
		opt.Quota.total += written
	}
	return &Result{SHA256: sum, Size: written, Path: dest}, nil
}

// Put 把 r 流式写入临时文件并移动到内容寻址位置。
func (s *Store) Put(ctx context.Context, r io.Reader, opt PutOptions) (*Result, error) {
	// 先做一次便宜的检查，避免为注定被拒的下载浪费带宽。
	// 这一步用旧快照无妨：并发窗口里可能多下载一次，但下面落位前的
	// 检查才是有效约束，超配额的内容不会真的落盘。
	preUsed := opt.Used
	if opt.Quota != nil {
		preUsed = opt.Quota.Used()
	}
	if err := s.checkSpace(preUsed, 0); err != nil {
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

	// 最终检查必须与落位在同一把锁内：否则两个并发写入可以各自
	// 通过检查，然后一起 rename，合计超出配额。
	//
	// 锁只覆盖一次 stat 与一次 rename，不是整个流式写入，
	// 所以不会把并发下载串行化。
	return s.place(tmpName, hex.EncodeToString(h.Sum(nil)), normalizeExt(opt.Ext), written, opt)
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

// fsBytes 把 statfs 的「块数 × 块大小」安全地转成 int64。
//
// 直接用 int64(blocks) * int64(bsize) 在极端值下会溢出成负数：
// 结果是 checkSpace 误判「空间不足」而拒绝写入——方向上是安全的
// （不会写爆磁盘），但会把一个大容量的卷变成不可写，且日志只有
// 一句 not enough free disk space，完全看不出是溢出。
//
// 这里做饱和处理：超过 MaxInt64 一律取 MaxInt64，负数取 0。
// 真实文件系统到不了这个量级，但防御性代码的代价只有两行。
func fsBytes(blocks uint64, bsize int64) int64 {
	if blocks == 0 || bsize <= 0 {
		return 0
	}
	// 先检查乘法是否会溢出，再转换，避免先溢出再判断。
	// bsize > 0 已在上面保证，所以除法结果非负，转 uint64 安全。
	// nosec G115：这里的 int64→uint64 是「已验证为非负」，不是任意转换。
	limit := uint64(math.MaxInt64) / uint64(bsize) // #nosec G115 -- bsize > 0 已保证
	if blocks > limit {
		return math.MaxInt64
	}
	// nosec G115：blocks <= MaxInt64/bsize，乘积必然落在 int64 内。
	return int64(blocks) * bsize // #nosec G115 -- 上方已做溢出检查
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
	free := fsBytes(st.Bavail, int64(st.Bsize))
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
		Total: fsBytes(st.Blocks, int64(st.Bsize)),
		Free:  fsBytes(st.Bavail, int64(st.Bsize)),
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
