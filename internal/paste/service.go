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
}

// Create 生成 slug 并落库。slug 冲突时重试，5 次仍失败则报错。
func (s *Service) Create(ctx context.Context, req CreateRequest) (*store.Paste, error) {
	p, err := s.buildPaste(req)
	if err != nil {
		return nil, err
	}

	// slug 冲突几乎不可能（62^6 ≈ 5.7e10），但一旦发生就重试。
	for attempt := 0; attempt < 5; attempt++ {
		slug, err := randomSlug(s.cfg.SlugLength)
		if err != nil {
			return nil, err
		}
		p.Slug = slug
		err = s.st.Paste().Create(ctx, p)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return nil, err
		}
	}
	return nil, errors.New("could not allocate a unique slug")
}

// buildPaste 校验输入并构造值对象，不涉及任何 IO。
//
// 独立出来是为了让 Create 只承担"生成 slug 并处理冲突"这一件事：
// 参数校验、默认值填充、过期时间解析与口令哈希都属于纯计算，
// 混在一起会让主流程读不出骨架。
func (s *Service) buildPaste(req CreateRequest) (*store.Paste, error) {
	if int64(len(req.Body)) > s.cfg.MaxBodyBytes {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrTooLarge, s.cfg.MaxBodyBytes)
	}
	if strings.TrimSpace(req.Body) == "" {
		return nil, errors.New("body must not be empty")
	}

	vis := req.Visibility
	if vis == "" {
		vis = VisUnlisted
	}
	if vis == VisPublic && !s.cfg.AllowPublic {
		return nil, ErrForbidden
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
		return nil, err
	}

	hash := ""
	if req.Password != "" {
		h, err := HashPassword(req.Password)
		if err != nil {
			return nil, err
		}
		hash = h
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
	}, nil
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
func (s *Service) Read(ctx context.Context, slug, password string) (*store.Paste, error) {
	p, err := s.st.Paste().GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if !p.Live(now.Unix()) {
		return nil, ErrExpired
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
func (s *Service) Meta(ctx context.Context, slug string) (*store.Paste, error) {
	p, err := s.st.Paste().GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !p.Live(s.now().Unix()) {
		return nil, ErrExpired
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
func (s *Service) Update(ctx context.Context, slug string, req UpdateRequest) (*store.Paste, error) {
	p, err := s.st.Paste().GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if !p.Live(now.Unix()) {
		return nil, ErrExpired
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
func (s *Service) Delete(ctx context.Context, slug string) error {
	return s.st.Paste().SoftDelete(ctx, slug)
}

// List 分页返回未删除且未过期的片段，不含正文。
func (s *Service) List(ctx context.Context, limit int, cursor string) ([]store.Paste, string, error) {
	items, next, err := s.st.Paste().List(ctx, limit, cursor)
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
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
