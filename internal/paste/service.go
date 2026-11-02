// Package paste 实现 pastebin 领域逻辑：slug 生成、过期、一次性读取、口令校验。
package paste

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/oklog/ulid/v2"
	"golang.org/x/crypto/argon2"
)

var (
	ErrExpired     = errors.New("paste expired")
	ErrBadPassword = errors.New("wrong password")
	ErrTooLarge    = errors.New("paste body too large")
	ErrBadExpiry   = errors.New("expiry out of range")
	ErrForbidden   = errors.New("public pastes are disabled")
	ErrLang        = errors.New("unsupported language")
)

const (
	VisPublic   = "public"
	VisUnlisted = "unlisted"
	VisPrivate  = "private"
)

// Config 是 pastebin 的限额与语言白名单。
type Config struct {
	MaxBodyBytes  int64
	DefaultExpiry time.Duration
	MaxExpiry     time.Duration
	SlugLength    int
	AllowPublic   bool
	Languages     map[string]bool
}

// 归属哈希的算法在 store 包：它是 paste.owner_hash 这一列的
// 持久化格式，迁移代码也要用同一套计算，放在领域包里会让存储层
// 反过来依赖领域包。这里保留薄封装只是为了让调用点读起来短一些。

// Service 实现片段的生命周期、一次性读取与口令校验。
type Service struct {
	st  store.Store
	cfg Config
	now func() time.Time
}

// NewService 构造片段服务。SlugLength 过小时回落到 6。
func NewService(st store.Store, cfg Config) *Service {
	if cfg.SlugLength < 4 {
		cfg.SlugLength = 6
	}
	return &Service{st: st, cfg: cfg, now: time.Now}
}

// CreateRequest 是创建片段的输入。ExpiresIn 为 nil 表示用默认值，
// 显式传 0 表示永久。
type CreateRequest struct {
	Title         string
	Body          string
	Language      string
	Visibility    string
	Password      string
	ExpiresIn     *time.Duration
	BurnAfterRead bool
	// OwnerClientID 由 handler 注入：已登录时是设备 id，匿名时为空。
	OwnerClientID string
	// OwnerID 优先于 OwnerClientID：非空时表示这次调用属于一个跨
	// client 稳定的身份（访问密钥），归属按它算而不是按设备算。
	// 设备配对产生的 client 没有 owner_id，仍然走按设备隔离。
	OwnerID string
	// OwnerToken 是匿名调用方提交的已有凭证。
	// 首次创建时为空，service 会生成一个；之后同一客户端应复用它，
	// 否则每次创建都产生新归属，列表里永远只有最近一条。
	OwnerToken string
}

// resolveOwner 决定这次创建的归属，并返回需要回显给调用方的匿名 token。
//
// 优先级：稳定身份 > 设备 id > 匿名 token。
//
// 稳定身份排第一是为了让轮换密钥不丢数据：同一主人的两把密钥算出
// 同一个 owner_hash，旧密钥名下的片段新密钥照样看得到。
//
// 匿名客户端首次创建时带着空 token 来，这里生成一个；之后它应该在
// 每次创建时复用同一个 token，否则每条片段都是独立归属，列表里只会
// 出现最近创建的那一条。
func resolveOwner(req CreateRequest) (string, string, error) {
	switch {
	case req.OwnerID != "":
		return store.OwnerHashForOwner(req.OwnerID), "", nil
	case req.OwnerClientID != "":
		return store.OwnerHashForClient(req.OwnerClientID), "", nil
	}
	token := req.OwnerToken
	if token == "" {
		generated, err := randomToken(32)
		if err != nil {
			return "", "", err
		}
		token = generated
	}
	return store.OwnerHashForToken(token), token, nil
}

// Create 生成 slug 并落库。slug 冲突时重试，5 次仍失败则报错。
func (s *Service) Create(ctx context.Context, req CreateRequest) (*store.Paste, string, error) {
	p, ownerToken, err := s.buildPaste(req)
	if err != nil {
		return nil, "", err
	}

	// slug 冲突几乎不可能（62^6 ≈ 5.7e10），但一旦发生就重试。
	for attempt := 0; attempt < 5; attempt++ {
		slug, err := randomSlug(s.cfg.SlugLength)
		if err != nil {
			return nil, "", err
		}
		p.Slug = slug
		err = s.st.Paste().Create(ctx, p)
		if err == nil {
			return p, ownerToken, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not allocate a unique slug")
}

// buildPaste 校验输入并构造值对象，不涉及任何 IO。
//
// 独立出来是为了让 Create 只承担"生成 slug 并处理冲突"这一件事：
// 参数校验、默认值填充、过期时间解析与口令哈希都属于纯计算，
// 混在一起会让主流程读不出骨架。
func (s *Service) buildPaste(req CreateRequest) (*store.Paste, string, error) {
	if int64(len(req.Body)) > s.cfg.MaxBodyBytes {
		return nil, "", fmt.Errorf("%w: limit is %d bytes", ErrTooLarge, s.cfg.MaxBodyBytes)
	}
	if strings.TrimSpace(req.Body) == "" {
		return nil, "", errors.New("body must not be empty")
	}

	vis := req.Visibility
	if vis == "" {
		vis = VisUnlisted
	}
	if vis == VisPublic && !s.cfg.AllowPublic {
		return nil, "", ErrForbidden
	}

	// 未知语言回落到 text 而不是报错：语言标记只影响展示，
	// 为了一个展示属性拒绝保存内容不值得。
	lang := req.Language
	if lang == "" {
		lang = "text"
	}
	if len(s.cfg.Languages) > 0 && !s.cfg.Languages[lang] {
		lang = "text"
	}

	now := s.now()
	expiresAt, err := s.resolveExpiry(req.ExpiresIn, now)
	if err != nil {
		return nil, "", err
	}

	hash := ""
	if req.Password != "" {
		h, err := HashPassword(req.Password)
		if err != nil {
			return nil, "", err
		}
		hash = h
	}

	ownerHash, ownerToken, err := resolveOwner(req)
	if err != nil {
		return nil, "", err
	}

	return &store.Paste{
		ID:            ulid.Make().String(),
		Title:         truncate(req.Title, 200),
		Body:          req.Body,
		Language:      lang,
		Visibility:    vis,
		PasswordHash:  hash,
		ExpiresAt:     expiresAt,
		BurnAfterRead: req.BurnAfterRead,
		SizeBytes:     int64(len(req.Body)),
		CreatedAt:     now.Unix(),
		UpdatedAt:     now.Unix(),
		OwnerHash:     ownerHash,
	}, ownerToken, nil
}

// resolveExpiry 把请求里的相对时长换算成绝对时刻。
// nil 返回 nil 表示永久，由调用方判断而非用 0 代替。
func (s *Service) resolveExpiry(in *time.Duration, now time.Time) (*int64, error) {
	switch {
	case in == nil:
		// 未指定：用默认值。默认值本身为 0 时表示永久。
		if s.cfg.DefaultExpiry <= 0 {
			return nil, nil
		}
		v := now.Add(s.cfg.DefaultExpiry).Unix()
		return &v, nil

	case *in == 0:
		// 显式传 0 表示永久。尊重用户意图，不再额外限制。
		return nil, nil

	case *in < 0:
		return nil, ErrBadExpiry

	case s.cfg.MaxExpiry > 0 && *in > s.cfg.MaxExpiry:
		return nil, fmt.Errorf("%w: max is %s", ErrBadExpiry, s.cfg.MaxExpiry)

	default:
		v := now.Add(*in).Unix()
		return &v, nil
	}
}

// Read 返回片段。带密码的片段必须提供正确口令。
func (s *Service) Read(ctx context.Context, slug, password, ownerHash string) (*store.Paste, error) {
	p, err := s.st.Paste().GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if !p.Live(now.Unix()) {
		return nil, ErrExpired
	}
	// private 的语义是"只有创建者能读"。归属不匹配时返回 ErrNotFound
	// 而不是 ErrForbidden：告诉对方"存在但你没权限"本身就是信息泄露。
	if p.Visibility == VisPrivate && p.OwnerHash != ownerHash {
		return nil, store.ErrNotFound
	}
	if p.PasswordHash != "" {
		if !VerifyPassword(p.PasswordHash, password) {
			return nil, ErrBadPassword
		}
	}
	if p.BurnAfterRead {
		won, err := s.st.Paste().ClaimBurn(ctx, slug, now.Unix())
		if err != nil {
			return nil, err
		}
		if !won {
			// 另一个并发请求已经读走了。语义上等价于"已过期"。
			return nil, ErrExpired
		}
	}
	return p, nil
}

// Meta 只返回元信息，不消耗一次性读取机会。
// 公开页需要先知道标题与语言才能渲染，而 burn_after_read 的片段
// 不应因为页面预取就被烧掉。
func (s *Service) Meta(ctx context.Context, slug, ownerHash string) (*store.Paste, error) {
	p, err := s.st.Paste().GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !p.Live(s.now().Unix()) {
		return nil, ErrExpired
	}
	// 元信息也会泄露标题，private 片段同样要挡住。
	if p.Visibility == VisPrivate && p.OwnerHash != ownerHash {
		return nil, store.ErrNotFound
	}
	p.Body = ""
	return p, nil
}

// UpdateRequest 的 nil 字段表示不修改。
type UpdateRequest struct {
	Title      *string
	Body       *string
	Language   *string
	Visibility *string
	Password   *string
	ExpiresIn  *time.Duration
}

// Update 覆盖可变字段。已过期或已删除的片段返回 ErrExpired。
func (s *Service) Update(ctx context.Context, slug string, req UpdateRequest, ownerHash string) (*store.Paste, error) {
	p, err := s.st.Paste().GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if !p.Live(now.Unix()) {
		return nil, ErrExpired
	}
	// 只有创建者能改。匿名创建者也一样——凭 owner token 证明身份。
	// 返回 ErrNotFound 而非 ErrForbidden，理由同 Read。
	if p.OwnerHash != "" && p.OwnerHash != ownerHash {
		return nil, store.ErrNotFound
	}

	if req.Title != nil {
		p.Title = truncate(*req.Title, 200)
	}
	if req.Body != nil {
		if int64(len(*req.Body)) > s.cfg.MaxBodyBytes {
			return nil, fmt.Errorf("%w: limit is %d bytes", ErrTooLarge, s.cfg.MaxBodyBytes)
		}
		p.Body = *req.Body
		p.SizeBytes = int64(len(*req.Body))
	}
	if req.Language != nil {
		if len(s.cfg.Languages) > 0 && !s.cfg.Languages[*req.Language] {
			p.Language = "text"
		} else {
			p.Language = *req.Language
		}
	}
	if req.Visibility != nil {
		if *req.Visibility == VisPublic && !s.cfg.AllowPublic {
			return nil, ErrForbidden
		}
		p.Visibility = *req.Visibility
	}
	if req.Password != nil {
		if *req.Password == "" {
			p.PasswordHash = ""
		} else {
			h, err := HashPassword(*req.Password)
			if err != nil {
				return nil, err
			}
			p.PasswordHash = h
		}
	}
	if req.ExpiresIn != nil {
		switch {
		case *req.ExpiresIn == 0:
			p.ExpiresAt = nil
		case s.cfg.MaxExpiry > 0 && *req.ExpiresIn > s.cfg.MaxExpiry:
			return nil, fmt.Errorf("%w: max is %s", ErrBadExpiry, s.cfg.MaxExpiry)
		default:
			v := now.Add(*req.ExpiresIn).Unix()
			p.ExpiresAt = &v
		}
	}
	p.UpdatedAt = now.Unix()

	if err := s.st.Paste().Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Delete 软删除片段。物理删除由 paste-expired 任务负责。
func (s *Service) Delete(ctx context.Context, slug, ownerHash string) error {
	p, err := s.st.Paste().GetBySlug(ctx, slug)
	if err != nil {
		return err
	}
	if !p.Live(s.now().Unix()) {
		return ErrExpired
	}
	if p.OwnerHash != "" && p.OwnerHash != ownerHash {
		return store.ErrNotFound
	}
	return s.st.Paste().SoftDelete(ctx, slug)
}

// List 分页返回未删除且未过期的片段，不含正文。
func (s *Service) List(ctx context.Context, ownerHash string, limit int, cursor string) ([]store.Paste, string, error) {
	items, next, err := s.st.Paste().List(ctx, ownerHash, limit, cursor)
	if err != nil {
		return nil, "", err
	}
	now := s.now().Unix()
	out := items[:0]
	for _, p := range items {
		if p.Live(now) {
			p.Body = "" // 列表不带正文
			out = append(out, p)
		}
	}
	return out, next, nil
}

// ---- slug ----

const base62 = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// randomToken 生成 URL 安全的随机凭证。
//
// 用 base64url 而不是 base62：它编码 32 字节只需 43 个字符，
// 且可以被客户端原样放进 header 或 localStorage，不需要转义。
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomSlug(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = base62[int(b)%len(base62)]
	}
	return string(out), nil
}

// ---- 口令 ----

// 参数取自 OWASP 对交互式登录的建议值。
// 片段口令校验是低频操作，16MB / 3 轮的代价在 1C2G 上可以接受。
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// 校验时可接受的 argon2 参数区间。
//
// 这些数字必须存在：VerifyPassword 从数据库里的 PHC 字符串解析
// m= / t= / p= 后直接交给 argon2.IDKey，而 IDKey 会按 m 分配内存
// （单位 KiB）。没有上限时，一个被篡改或导入的 hash 写成
// m=4294967295 就是 4 TiB 分配——在 MemoryMax=512M 的容器里
// 直接 OOM kill，而触发它只需要一次带口令的读请求。
//
// 区间取宽是为了以后调参不失效：当前 64 MiB / 3 轮落在中间。
var (
	argonMinMemory  uint32 = 8 * 1024    // 8 MiB
	argonMaxMemory  uint32 = 1024 * 1024 // 1 GiB
	argonMaxTime    uint32 = 10
	argonMaxThreads uint8  = 16
)

const (
	argonMaxSaltLen = 64
	argonMaxKeyLen  = 64
)

// HashPassword 产出 PHC 格式字符串，自描述参数，将来调参不会让旧口令失效。
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 用常量时间比较，避免通过响应时间猜测口令。
func VerifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false
	}
	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	// 参数必须落在合理区间。argon2.IDKey 按 m 分配内存（KiB），
	// 不校验就是一个可以从 hash 字符串触发的 OOM：
	// m=4294967295 相当于 4 TiB。这类 hash 一律当作验证失败，
	// 不报错——攻击者不应从响应差异里得知我们拒绝了什么。
	if memory < argonMinMemory || memory > argonMaxMemory ||
		timeCost == 0 || timeCost > argonMaxTime ||
		threads == 0 || threads > argonMaxThreads {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 || len(salt) > argonMaxSaltLen {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > argonMaxKeyLen {
		return false
	}
	// len(want) 已在上面限定在 [16, argonMaxKeyLen]，
	// 转 uint32 不会截断。
	got := argon2.IDKey([]byte(pw), salt, timeCost, memory, threads, uint32(len(want))) // #nosec G115 -- 上方已限定长度
	return subtle.ConstantTimeCompare(got, want) == 1
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
