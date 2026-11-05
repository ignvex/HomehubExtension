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
	"time"
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

	// mu 串行化「最终配额判定 + 落位」与删除。所有写入——无论属于哪个
	// QuotaTracker、还是根本没带 tracker——都在这把锁下判定配额。
	//
	// 之前带 tracker 的写入只持 tracker 自己的锁：两轮并发抓取各开一个
	// tracker，彼此看不到对方刚落盘的字节，合计可以写到两倍配额。
	mu sync.Mutex
	// used 是磁盘上内容文件的总字节，受 mu 保护。
	//
	// Open 时遍历磁盘初始化，之后落位累加、删除回减，始终对应磁盘实际。
	// 判定时再与调用方的数据库基线（PutOptions.Used、tracker）取 max，
	// 但那个 max 只用于当次判定，绝不写回这里：调用方的快照可能已经
	// 过期（本轮开始后 GC 删过文件），写回会让用量只涨不跌，
	// 直到重启前都在拒绝本可以写入的下载。
	used int64
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
	st := &Store{
		root:    opts.Root,
		tmp:     opts.TmpDir,
		maxSize: opts.MaxSize,
		quota:   opts.Quota,
		minFree: opts.MinFreeBytes,
	}
	// 全局用量从磁盘实际内容起算：磁盘才是配额真正约束的对象，
	// 未入库的孤儿文件同样占空间。之后只随落位与删除增减。
	var used int64
	if err := st.WalkFiles(context.Background(), func(f FileInfo) error {
		used += f.Size
		return nil
	}); err != nil {
		return nil, fmt.Errorf("measure blob usage: %w", err)
	}
	st.used = used
	return st, nil
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

// Used 返回当前用量：基线 + 本批次已落盘字节（不含其他批次）。
// nil 接收者返回 0，方便调用方不写分支。
func (q *QuotaTracker) Used() int64 {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.total
}

func (q *QuotaTracker) add(n int64) {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.total += n
	q.mu.Unlock()
}

// baselineLocked 返回配额判定用的已用字节：Store 全局视图、调用方的
// 数据库快照、批次 tracker 三者取最大。调用方必须持有 s.mu。
//
// 取 max 而不是相加：三者描述的是同一批磁盘字节的不同视角，
// 相加会重复计数；取 max 保证任何一个视角看到的用量都不会被忽略。
func (s *Store) baselineLocked(opt PutOptions) int64 {
	used := s.used
	if opt.Used > used {
		used = opt.Used
	}
	if q := opt.Quota.Used(); q > used {
		used = q
	}
	return used
}

// UsedBytes 返回磁盘上内容文件的当前总字节（不含调用方基线）。
func (s *Store) UsedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
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
	// 始终持 Store 级锁：配额是全局的，判定也必须全局串行。
	s.mu.Lock()
	defer s.mu.Unlock()

	// 去重检查必须排在配额检查之前。
	//
	// 内容已在库里意味着这次写入不占任何新空间（数据库的 SUM(size)
	// 也只算一份）。若先查配额，接近上限时同一张被多篇文章引用的
	// 图片会被判成 quota_exceeded —— 一个纯粹由检查顺序制造的失败，
	// 而用户在界面上看到的是「图片加载失败」。
	dest := s.Path(sum, ext)
	if _, err := os.Stat(dest); err == nil {
		os.Remove(tmpName)
		// 刷新 mtime：这份文件可能是还没入库的孤儿，blob-gc 按 mtime
		// 判断宽限期。复用它的调用方马上就会写 blob 行，必须让 GC
		// 看到它「刚被用过」，否则 GC 可能在入库前把它删掉。
		// 与 RemoveIfOlder 同在 s.mu 下，两者不会交叠。
		now := time.Now()
		_ = os.Chtimes(dest, now, now)
		return &Result{SHA256: sum, Size: written, Path: dest, Existed: true}, nil
	}

	used := s.baselineLocked(opt)

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
	// 只加本次写入的字节，不把判定用的基线写回（见 used 字段注释）。
	s.used += written
	opt.Quota.add(written)
	return &Result{SHA256: sum, Size: written, Path: dest}, nil
}

// Put 把 r 流式写入临时文件并移动到内容寻址位置。
func (s *Store) Put(ctx context.Context, r io.Reader, opt PutOptions) (*Result, error) {
	// 先做一次便宜的检查，避免为注定被拒的下载浪费带宽。
	// 这一步用旧快照无妨：并发窗口里可能多下载一次，但下面落位前的
	// 检查才是有效约束，超配额的内容不会真的落盘。
	s.mu.Lock()
	preUsed := s.baselineLocked(opt)
	s.mu.Unlock()
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
	// 所以不会把并发下载串行化——串行化的只是判定本身。
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
//
// ext 在这里再过一次 normalizeExt，而不是信任调用方。Path 是导出的，
// 当前唯一能传外部值的调用点是 /media 路由，那里已经先做了长度与
// 十六进制校验；但只要有一个新调用点漏了校验，含 `/` 的 ext 就能
// 把路径拽出 root（filepath.Join 会把 `<sha>.../etc/passwd` 清理成
// 上一级目录）。代价是一次字符串送选，换的是不依赖调用方纪律。
func (s *Store) Path(sha, ext string) string {
	ext = normalizeExt(ext)
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
// 删除成功会从全局用量里回减文件大小。
func (s *Store) Delete(sha, ext string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.removeLocked(s.Path(sha, ext))
	return err
}

// RemoveIfOlder 仅当文件 mtime 早于 cutoff 时删除它，供孤儿 GC 使用。
//
// 判定与删除都在 s.mu 下完成，和 place 的「去重命中刷新 mtime」互斥：
// 要么 GC 先删（随后 place 看不到文件，会把临时文件重新落位），
// 要么 place 先刷新 mtime（GC 看到新 mtime 后跳过）。
// 不存在「GC 删掉了一个刚被复用、即将入库的文件」的窗口。
func (s *Store) RemoveIfOlder(sha, ext string, cutoff time.Time) (removed bool, size int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.Path(sha, ext)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if !info.ModTime().Before(cutoff) {
		return false, 0, nil
	}
	size, err = s.removeLocked(path)
	if err != nil {
		return false, 0, err
	}
	return true, size, nil
}

// removeLocked 删除文件并回减全局用量。调用方必须持有 s.mu。
func (s *Store) removeLocked(path string) (int64, error) {
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	s.used -= size
	if s.used < 0 {
		s.used = 0
	}
	return size, nil
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

// Walk 遍历磁盘上实际存在的文件。
func (s *Store) Walk(ctx context.Context, fn func(sha string, size int64) error) error {
	return s.WalkFiles(ctx, func(f FileInfo) error { return fn(f.SHA256, f.Size) })
}

// FileInfo 是磁盘上一个内容寻址文件的元信息，供 blob-gc 找出孤儿。
type FileInfo struct {
	SHA256  string
	Ext     string
	Size    int64
	ModTime time.Time
}

// WalkFiles 遍历磁盘上实际存在的文件，附带扩展名与 mtime。
//
// 文件名不是「64 位十六进制 sha + 可选扩展名」的一律跳过：
// GC 只该碰自己写出来的东西，形状不对的文件宁可留着。
func (s *Store) WalkFiles(ctx context.Context, fn func(FileInfo) error) error {
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
			if f.IsDir() {
				continue
			}
			ext := strings.TrimPrefix(filepath.Ext(f.Name()), ".")
			sha := strings.TrimSuffix(f.Name(), filepath.Ext(f.Name()))
			if !isSHA256Hex(sha) || !strings.HasPrefix(sha, e.Name()) || normalizeExt(ext) != ext {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue // 并发删除，跳过
			}
			if err := fn(FileInfo{SHA256: sha, Ext: ext, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
		}
	}
	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
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
