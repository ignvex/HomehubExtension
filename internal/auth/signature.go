package auth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HubVerifier 校验 Homehub 宿主发来的入站请求签名。
//
// 这是"双向"里的第二个方向。Extension 主动抓 RSS 时是出站请求，不需要签名；
// 而 Homehub 推送订阅、查询状态时是入站请求，必须证明它持有私钥。
type HubVerifier struct {
	pub   ed25519.PublicKey
	skew  time.Duration
	guard *ReplayGuard
}

var (
	ErrClockSkew = errors.New("timestamp outside allowed window")
	ErrReplayed  = errors.New("nonce already used")
	ErrNoHubKey  = errors.New("hub public key not configured")
)

// ParseHubPublicKey 解析内联在配置里的宿主公钥，接受 PEM 或裸 base64。
//
// 这个字段的语义是「公钥内容」，不是路径。之前这里会把解析失败的输入
// 当路径再读一次文件，结果是填错时错误信息指向错误的排查方向，也让
// 「路径」成了一种没有写进契约的隐式用法。现在明确拒绝。
func ParseHubPublicKey(raw []byte) (ed25519.PublicKey, error) {
	if block, _ := pem.Decode(raw); block != nil {
		if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
			if ed, ok := key.(ed25519.PublicKey); ok {
				return ed, nil
			}
			return nil, fmt.Errorf("hub public key: expected Ed25519, got %T", key)
		}
		if len(block.Bytes) == ed25519.PublicKeySize {
			return ed25519.PublicKey(block.Bytes), nil
		}
		return nil, errors.New("hub public key: unsupported PEM block")
	}

	// base64 经常被粘贴成多行，先去掉所有空白再解码。
	compact := strings.Join(strings.Fields(string(raw)), "")
	if compact != "" {
		for _, enc := range []*base64.Encoding{
			base64.StdEncoding, base64.RawStdEncoding,
			base64.URLEncoding, base64.RawURLEncoding,
		} {
			if decoded, err := enc.DecodeString(compact); err == nil && len(decoded) == ed25519.PublicKeySize {
				return ed25519.PublicKey(decoded), nil
			}
		}
	}
	return nil, errors.New("hub public key: expected PEM or base64; this field holds the key itself, not a path")
}

// HubKeyFingerprint 返回公钥的短指纹：SHA-256 前 8 字节的 hex。
// 与 Homehub 侧的 fingerprintKey 对齐；日志只打指纹，不打公钥内容。
func HubKeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// NewHubVerifier 构造入站签名校验器。skew 与 nonceTTL 为 0 时使用默认值。
func NewHubVerifier(pub ed25519.PublicKey, skew, nonceTTL time.Duration) *HubVerifier {
	if skew <= 0 {
		skew = 60 * time.Second
	}
	if nonceTTL <= 0 {
		nonceTTL = 2 * time.Minute
	}
	return &HubVerifier{pub: pub, skew: skew, guard: NewReplayGuard(nonceTTL, 100_000)}
}

// RequestMessage 构造待签名字符串。
//
// 把 method/path/query/body 摘要一起签进去，任何一个被改动签名都会失效。
// 只签 body 不签 path 的话，一条合法的写请求可以被重放到另一个端点。
func RequestMessage(r *http.Request, body []byte) string {
	sum := sha256.Sum256(body)
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	return strings.Join([]string{
		reqDomain,
		r.Method,
		path,
		r.URL.RawQuery,
		hex.EncodeToString(sum[:]),
		r.Header.Get("X-HH-Timestamp"),
		r.Header.Get("X-HH-Nonce"),
	}, "\n")
}

// Verify 校验签名头。body 必须是与请求体完全一致的原始字节。
func (v *HubVerifier) Verify(r *http.Request, body []byte) error {
	if v == nil || len(v.pub) == 0 {
		return ErrNoHubKey
	}
	tsStr := r.Header.Get("X-HH-Timestamp")
	nonce := r.Header.Get("X-HH-Nonce")
	sigStr := r.Header.Get("X-HH-Signature")
	if tsStr == "" || nonce == "" || sigStr == "" {
		return ErrBadSig
	}

	var ts int64
	if _, err := fmt.Sscanf(tsStr, "%d", &ts); err != nil {
		return ErrBadSig
	}
	now := time.Now()
	skew := now.Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > v.skew {
		return ErrClockSkew
	}

	sig, err := base64.StdEncoding.DecodeString(sigStr)
	if err != nil {
		if sig, err = base64.RawURLEncoding.DecodeString(sigStr); err != nil {
			return ErrBadSig
		}
	}
	if !ed25519.Verify(v.pub, []byte(RequestMessage(r, body)), sig) {
		return ErrBadSig
	}

	// 验签之后才登记 nonce：未通过签名的请求不该污染去重表，
	// 否则攻击者可以用垃圾请求把表撑满，把合法请求挤成"已重放"。
	if !v.guard.Allow(nonce) {
		return ErrReplayed
	}
	return nil
}

// ReplayGuard 是带 TTL 的 nonce 去重表。
// 单实例部署不需要跨节点共享；进程重启后计数清零可以接受——
// 时间戳窗口本身只有 60 秒。
type ReplayGuard struct {
	mu        sync.Mutex
	seen      map[string]time.Time
	ttl       time.Duration
	limit     int
	lastSweep time.Time
}

// NewReplayGuard 构造防重放表。ttl 是 nonce 的有效期，limit 是容量上限。
func NewReplayGuard(ttl time.Duration, limit int) *ReplayGuard {
	return &ReplayGuard{seen: make(map[string]time.Time), ttl: ttl, limit: limit}
}

// Allow 在 nonce 首次出现时返回 true；重复则返回 false。
//
// 过期项由这里按时间驱动清理，不依赖外部调度——
// 之前的实现只在表满（10 万条）时才清理，意味着低频部署会让
// 2 分钟就该失效的 nonce 一直留在内存里，直到某天表被填满。
// 那种"只在压力下才回收"的设计等于在小流量下持续泄漏。
func (g *ReplayGuard) Allow(nonce string) bool {
	if nonce == "" {
		return false
	}
	now := time.Now()

	g.mu.Lock()
	defer g.mu.Unlock()

	// 每 ttl/4 清理一次。选这个频率是因为：太频繁会浪费 CPU，
	// 太稀疏则内存会短暂高于"一个 ttl 窗口内应有的量"。
	if now.Sub(g.lastSweep) > g.ttl/4 {
		g.sweepLocked(now)
		g.lastSweep = now
	}

	if _, ok := g.seen[nonce]; ok {
		return false
	}
	// 清理后仍然满，说明短时间内请求量真的超过了容量上限。
	// 此时拒绝而不是无限增长：宁可让合法请求重试，也不要 OOM。
	if len(g.seen) >= g.limit {
		return false
	}
	g.seen[nonce] = now
	return true
}

func (g *ReplayGuard) sweepLocked(now time.Time) int {
	n := 0
	for k, t := range g.seen {
		if now.Sub(t) > g.ttl {
			delete(g.seen, k)
			n++
		}
	}
	return n
}

// Size 返回当前记录的 nonce 数量，供测试与诊断使用。
func (g *ReplayGuard) Size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen)
}
