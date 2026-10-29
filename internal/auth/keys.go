// Package auth 实现双向公私钥鉴权。
//
// 方向一（浏览器 → 服务端）：Ed25519/P-256 挑战-响应，服务端只存公钥。
// 方向二（Homehub → 服务端）：每个入站请求带签名，见 signature.go。
package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	AlgEd25519 = "ed25519"
	AlgP256    = "p256"
)

var (
	ErrBadKey      = errors.New("invalid public key")
	ErrBadSig      = errors.New("invalid signature")
	ErrUnsupported = errors.New("unsupported algorithm")
)

// Signer 持有服务端密钥，负责签发与校验 access token。
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	kid  string
}

// LoadOrCreateSigner 读取 PKCS#8 私钥；不存在时生成并以 0600 落盘。
func LoadOrCreateSigner(path string) (*Signer, error) {
	if path == "" {
		return nil, errors.New("server key path is empty")
	}
	if raw, err := os.ReadFile(path); err == nil {
		return parsePrivateKey(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read server key: %w", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	// 先写临时文件再 rename，避免进程在写一半时被杀留下损坏的密钥。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, fmt.Errorf("write server key: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	return newSigner(pub, priv), nil
}

func parsePrivateKey(raw []byte) (*Signer, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("server key: not valid PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse server key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("server key: expected Ed25519, got %T", key)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("server key: public key type mismatch")
	}
	return newSigner(pub, priv), nil
}

func newSigner(pub ed25519.PublicKey, priv ed25519.PrivateKey) *Signer {
	sum := sha256.Sum256(pub)
	return &Signer{priv: priv, pub: pub, kid: base64.RawURLEncoding.EncodeToString(sum[:8])}
}

// KID 返回密钥标识，base64url(sha256(pub)[:8])。
func (s *Signer) KID() string { return s.kid }

// PublicKey 返回服务端公钥，供 /auth/server-key 公开。
func (s *Signer) PublicKey() ed25519.PublicKey { return s.pub }

// ---- 客户端公钥 ----

// ParsePublicKey 解析客户端注册时提交的公钥（base64 raw 字节）。
// 不接受 PEM：浏览器端没有理由提交 PEM，限制格式能减少解析面。
func ParsePublicKey(alg, b64 string) (any, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(b64); err != nil {
			return nil, fmt.Errorf("%w: not valid base64", ErrBadKey)
		}
	}
	switch alg {
	case AlgEd25519:
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: ed25519 key must be %d bytes, got %d", ErrBadKey, ed25519.PublicKeySize, len(raw))
		}
		return ed25519.PublicKey(raw), nil
	case AlgP256:
		x, y := elliptic.Unmarshal(elliptic.P256(), raw)
		if x == nil {
			return nil, fmt.Errorf("%w: p256 key must be uncompressed point (65 bytes), got %d", ErrBadKey, len(raw))
		}
		return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, alg)
	}
}

// MarshalPublicKey 反向序列化，用于把数据库里的 BLOB 还原成可验签的类型。
func MarshalPublicKey(alg string, raw []byte) (any, error) {
	switch alg {
	case AlgEd25519:
		if len(raw) != ed25519.PublicKeySize {
			return nil, ErrBadKey
		}
		return ed25519.PublicKey(raw), nil
	case AlgP256:
		x, y := elliptic.Unmarshal(elliptic.P256(), raw)
		if x == nil {
			return nil, ErrBadKey
		}
		return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
	default:
		return nil, ErrUnsupported
	}
}

// EncodePublicKey 把公钥压成定长字节，便于存进 BLOB 列。
func EncodePublicKey(alg string, key any) ([]byte, error) {
	switch alg {
	case AlgEd25519:
		k, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, ErrBadKey
		}
		return []byte(k), nil
	case AlgP256:
		k, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return nil, ErrBadKey
		}
		return elliptic.Marshal(elliptic.P256(), k.X, k.Y), nil
	default:
		return nil, ErrUnsupported
	}
}

// Verify 校验客户端对 msg 的签名。
//
// 注意 P-256 的格式差异：WebCrypto 的 ECDSA 输出是 IEEE P1363（r||s 各 32 字节），
// 而 Go 的 ecdsa.VerifyASN1 要 ASN.1 DER。浏览器发来的一定是前者，
// 直接当 DER 解析会 100% 失败，且错误信息毫无提示性。
func Verify(alg string, key any, msg, sig []byte) error {
	switch alg {
	case AlgEd25519:
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return ErrBadKey
		}
		if len(sig) == ed25519.SignatureSize && ed25519.Verify(pub, msg, sig) {
			return nil
		}
		return ErrBadSig
	case AlgP256:
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return ErrBadKey
		}
		digest := sha256.Sum256(msg)
		if len(sig) == 64 {
			// P1363：拆成 r、s 两个 32 字节大端整数。
			r := new(big.Int).SetBytes(sig[:32])
			s := new(big.Int).SetBytes(sig[32:])
			if ecdsa.Verify(pub, digest[:], r, s) {
				return nil
			}
			return ErrBadSig
		}
		if ecdsa.VerifyASN1(pub, digest[:], sig) {
			return nil
		}
		return ErrBadSig
	default:
		return ErrUnsupported
	}
}

// ---- access token（紧凑 JWS，EdDSA） ----

var b64 = base64.RawURLEncoding

type tokenPayload struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

// IssueToken 签发 15 分钟有效的 access token。
// 有效期固定，它就是"撤销一台设备"的最坏延迟，不做成配置项。
func (s *Signer) IssueToken(clientID string, now time.Time, ttl time.Duration) (string, error) {
	jti, err := randomToken(12)
	if err != nil {
		return "", err
	}
	// 用 json.Marshal 而不是 fmt.Sprintf 拼字符串：clientID 现在是 ULID，
	// 但将来若换成用户输入的标识，拼接就会变成注入面。
	headerBytes, err := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}{"EdDSA", "JWT", s.kid})
	if err != nil {
		return "", err
	}
	payloadBytes, err := json.Marshal(tokenPayload{
		Sub: clientID, Iat: now.Unix(), Exp: now.Add(ttl).Unix(), Jti: jti,
	})
	if err != nil {
		return "", err
	}

	signingInput := b64.EncodeToString(headerBytes) + "." + b64.EncodeToString(payloadBytes)
	sig := ed25519.Sign(s.priv, []byte(signingInput))
	return signingInput + "." + b64.EncodeToString(sig), nil
}

// VerifyToken 校验签名与过期时间，返回 clientID。
// 调用方仍需查库确认该设备未被撤销。
func (s *Signer) VerifyToken(token string, now time.Time) (string, error) {
	parts := splitToken(token)
	if len(parts) != 3 {
		return "", ErrBadSig
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return "", ErrBadSig
	}
	if !ed25519.Verify(s.pub, []byte(parts[0]+"."+parts[1]), sig) {
		return "", ErrBadSig
	}
	// 先验签再解析：解析未经验证的数据是常见的注入面。
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(b64Decode(parts[0]), &hdr); err != nil {
		return "", ErrBadSig
	}
	if hdr.Alg != "EdDSA" || hdr.Kid != s.kid {
		// kid 不匹配说明是旧密钥签的（例如换过密钥），必须拒绝而不是尝试兼容。
		return "", ErrBadSig
	}
	var p tokenPayload
	if err := json.Unmarshal(b64Decode(parts[1]), &p); err != nil {
		return "", ErrBadSig
	}
	if p.Exp <= now.Unix() {
		return "", ErrBadSig
	}
	if p.Sub == "" {
		return "", ErrBadSig
	}
	return p.Sub, nil
}

func splitToken(t string) []string {
	var out []string
	start := 0
	for i := 0; i < len(t); i++ {
		if t[i] == '.' {
			out = append(out, t[start:i])
			start = i + 1
		}
	}
	return append(out, t[start:])
}

func b64Decode(s string) []byte {
	b, _ := b64.DecodeString(s)
	return b
}

// randomToken 生成 n 字节随机数的 base64url 表示。
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return b64.EncodeToString(buf), nil
}

// HashToken 返回 token 的 sha256，数据库只存哈希。
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
