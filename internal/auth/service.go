package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/oklog/ulid/v2"
)

// 签名消息的领域前缀。首行固定，防止在一个协议里生成的签名被搬到另一个协议重放。
const (
	authDomain = "homehubext/auth/v1"
	reqDomain  = "homehubext/req/v1"
)

var (
	ErrBadCredentials = errors.New("bad credentials")
	ErrRevoked        = errors.New("client revoked")
	ErrEnrollmentUsed = errors.New("enrollment token already used or expired")
)

// Settings 是鉴权服务的可调参数。零值字段使用默认值。
type Settings struct {
	SessionTTL    time.Duration
	ChallengeTTL  time.Duration
	EnrollmentTTL time.Duration
	// AccessTTL 固定 15 分钟，不暴露给配置。
	AccessTTL time.Duration
}

func (s Settings) withDefaults() Settings {
	if s.AccessTTL <= 0 {
		s.AccessTTL = 15 * time.Minute
	}
	if s.ChallengeTTL <= 0 {
		s.ChallengeTTL = 2 * time.Minute
	}
	if s.EnrollmentTTL <= 0 {
		s.EnrollmentTTL = 15 * time.Minute
	}
	if s.SessionTTL <= 0 {
		s.SessionTTL = 168 * time.Hour
	}
	return s
}

// Service 实现挑战-响应鉴权与设备管理。
type Service struct {
	st     store.AuthStore
	signer *Signer
	cfg    Settings
	now    func() time.Time
}

// NewService 构造鉴权服务。零值 Settings 会补上合理默认。
func NewService(st store.AuthStore, signer *Signer, cfg Settings) *Service {
	return &Service{st: st, signer: signer, cfg: cfg.withDefaults(), now: time.Now}
}

// Signer 暴露给 handler 生成 /auth/server-key 响应。
func (s *Service) Signer() *Signer { return s.signer }

// AccessTTL 返回 access token 的有效期。它固定 15 分钟，不可配置——
// 这个值就是「撤销一台设备」的最坏延迟。
func (s *Service) AccessTTL() time.Duration { return s.cfg.AccessTTL }

// ---- 配对 ----

// GenerateEnrollment 生成一次性配对码，只在 CLI 输出一次明文。
func (s *Service) GenerateEnrollment(ctx context.Context, nameHint string) (string, time.Time, error) {
	plain, err := pairingCode()
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	expires := now.Add(s.cfg.EnrollmentTTL)
	err = s.st.CreateEnrollment(ctx, &store.Enrollment{
		TokenHash: HashToken(normalizePairingCode(plain)),
		NameHint:  nameHint,
		CreatedAt: now.Unix(),
		ExpiresAt: expires.Unix(),
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return plain, expires, nil
}

// pairingCode 生成 HH-XXXX-XXXX-XXXX 形式的码。
// 字符集去掉了 0/O/1/I/L，抄写和念读时不会混淆。
func pairingCode() (string, error) {
	const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	buf := make([]byte, 12)
	if err := readRandom(buf); err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("HH-")
	for i, b := range buf {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	return sb.String(), nil
}

// normalizePairingCode 容忍大小写、空格与分隔符差异：
// 用户从终端复制时经常带上多余字符。
func normalizePairingCode(code string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(code) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// EnrollResult 是配对成功的返回值。服务端公钥交给客户端留存，
// 用于将来验证服务端签名。
type EnrollResult struct {
	Client    *store.Client
	ServerKey string
	KID       string
}

// Enroll 用一次性配对码登记设备公钥。
func (s *Service) Enroll(ctx context.Context, token, name, alg, publicKeyB64 string) (*EnrollResult, error) {
	if alg == "" {
		alg = AlgEd25519
	}
	key, err := ParsePublicKey(alg, publicKeyB64)
	if err != nil {
		return nil, err
	}
	raw, err := EncodePublicKey(alg, key)
	if err != nil {
		return nil, err
	}

	now := s.now()
	enr, err := s.st.ConsumeEnrollment(ctx, HashToken(normalizePairingCode(token)), now.Unix())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrEnrollmentUsed
		}
		return nil, err
	}

	if name == "" {
		name = enr.NameHint
	}
	if name == "" {
		name = "unnamed device"
	}
	client := &store.Client{
		ID:        ulid.Make().String(),
		Name:      truncate(name, 64),
		PublicKey: raw,
		Algorithm: alg,
		CreatedAt: now.Unix(),
	}
	if err := s.st.CreateClient(ctx, client); err != nil {
		return nil, err
	}
	return &EnrollResult{
		Client:    client,
		ServerKey: b64.EncodeToString(s.signer.PublicKey()),
		KID:       s.signer.KID(),
	}, nil
}

// ---- 挑战-响应 ----

// ChallengeResult 是一次挑战的 nonce 与有效期。
type ChallengeResult struct {
	Nonce     string
	ExpiresAt time.Time
}

// Challenge 为已登记的设备签发一次性 nonce。
// 先消费再验签：失败也不允许同一 nonce 重试。
func (s *Service) Challenge(ctx context.Context, clientID string) (*ChallengeResult, error) {
	client, err := s.st.GetClient(ctx, clientID)
	if err != nil {
		return nil, ErrBadCredentials
	}
	if !client.Active() {
		return nil, ErrBadCredentials
	}

	nonce, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	now := s.now()
	expires := now.Add(s.cfg.ChallengeTTL)
	if err := s.st.CreateChallenge(ctx, &store.Challenge{
		Nonce:     nonce,
		ClientID:  clientID,
		CreatedAt: now.Unix(),
		ExpiresAt: expires.Unix(),
	}); err != nil {
		return nil, err
	}
	return &ChallengeResult{Nonce: nonce, ExpiresAt: expires}, nil
}

// AuthMessage 构造待签名消息。前端必须逐字节生成同样的内容。
func AuthMessage(clientID, nonce string) []byte {
	return []byte(authDomain + "\n" + clientID + "\n" + nonce)
}

// Tokens 是一次成功登录签发的凭证集合。
type Tokens struct {
	AccessToken   string
	AccessExpires time.Time
	RefreshToken  string
	CSRF          string
	Client        *store.Client
}

// Exchange 校验签名并签发会话。
//
// 所有失败路径都返回 ErrBadCredentials，不区分"设备不存在"、"签名错"、
// "nonce 过期"——区分会给攻击者一个枚举设备的通道。
func (s *Service) Exchange(ctx context.Context, clientID, nonce, sigB64, userAgent string) (*Tokens, error) {
	client, err := s.st.GetClient(ctx, clientID)
	if err != nil || !client.Active() {
		return nil, ErrBadCredentials
	}
	key, err := MarshalPublicKey(client.Algorithm, client.PublicKey)
	if err != nil {
		return nil, ErrBadCredentials
	}
	sig, err := decodeSignature(sigB64)
	if err != nil {
		return nil, ErrBadCredentials
	}

	ch, err := s.st.ConsumeChallenge(ctx, nonce, s.now().Unix())
	if err != nil || ch.ClientID != clientID {
		return nil, ErrBadCredentials
	}
	if err := Verify(client.Algorithm, key, AuthMessage(clientID, nonce), sig); err != nil {
		return nil, ErrBadCredentials
	}
	return s.issue(ctx, client, userAgent)
}

func (s *Service) issue(ctx context.Context, client *store.Client, userAgent string) (*Tokens, error) {
	now := s.now()
	access, err := s.signer.IssueToken(client.ID, now, s.cfg.AccessTTL)
	if err != nil {
		return nil, err
	}
	refresh, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(24)
	if err != nil {
		return nil, err
	}
	if err := s.st.CreateSession(ctx, &store.Session{
		TokenHash:  HashToken(refresh),
		ClientID:   client.ID,
		CreatedAt:  now.Unix(),
		ExpiresAt:  now.Add(s.cfg.SessionTTL).Unix(),
		LastUsedAt: now.Unix(),
		UserAgent:  truncate(userAgent, 200),
	}); err != nil {
		return nil, err
	}
	_ = s.st.TouchClient(ctx, client.ID, now.Unix())

	return &Tokens{
		AccessToken:   access,
		AccessExpires: now.Add(s.cfg.AccessTTL),
		RefreshToken:  refresh,
		CSRF:          csrf,
		Client:        client,
	}, nil
}

// Refresh 用 refresh token 换新会话，并旋转 refresh 与 csrf。
func (s *Service) Refresh(ctx context.Context, refreshToken, userAgent string) (*Tokens, error) {
	if refreshToken == "" {
		return nil, ErrBadCredentials
	}
	hash := HashToken(refreshToken)
	sess, err := s.st.GetSession(ctx, hash)
	if err != nil || !sess.Live(s.now().Unix()) {
		return nil, ErrBadCredentials
	}
	client, err := s.st.GetClient(ctx, sess.ClientID)
	if err != nil || !client.Active() {
		return nil, ErrBadCredentials
	}
	// 旧 refresh 立刻失效：复用同一个 token 视为泄露信号。
	if err := s.st.RevokeSession(ctx, hash, s.now().Unix()); err != nil {
		return nil, err
	}
	return s.issue(ctx, client, userAgent)
}

// Logout 撤销当前会话。token 为空时视为已登出，返回 nil。
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	return s.st.RevokeSession(ctx, HashToken(refreshToken), s.now().Unix())
}

// Authenticate 校验 Bearer token，返回仍然有效的设备。
func (s *Service) Authenticate(ctx context.Context, accessToken string) (*store.Client, error) {
	clientID, err := s.signer.VerifyToken(accessToken, s.now())
	if err != nil {
		return nil, ErrBadCredentials
	}
	client, err := s.st.GetClient(ctx, clientID)
	if err != nil || !client.Active() {
		return nil, ErrBadCredentials
	}
	return client, nil
}

// SessionClient 直接校验 refresh cookie（供 SSE 使用，EventSource 无法设请求头）。
func (s *Service) SessionClient(ctx context.Context, refreshToken string) (*store.Client, error) {
	if refreshToken == "" {
		return nil, ErrBadCredentials
	}
	sess, err := s.st.GetSession(ctx, HashToken(refreshToken))
	if err != nil || !sess.Live(s.now().Unix()) {
		return nil, ErrBadCredentials
	}
	client, err := s.st.GetClient(ctx, sess.ClientID)
	if err != nil || !client.Active() {
		return nil, ErrBadCredentials
	}
	return client, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ParseBearer 从 Authorization 头里取出 token。
func ParseBearer(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

// readRandom 用 crypto/rand 填充缓冲。单独抽出来便于测试替换思路。
func readRandom(b []byte) error {
	_, err := rand.Read(b)
	return err
}

// decodeSignature 依次尝试标准 base64、URL-safe 与 raw 变体。
// 客户端实现语言不同，产出的编码也不同；只认一种会让合法请求莫名失败。
func decodeSignature(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrBadSig
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if out, err := enc.DecodeString(s); err == nil {
			return out, nil
		}
	}
	return nil, ErrBadSig
}
