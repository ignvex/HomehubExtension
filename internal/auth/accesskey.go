package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/oklog/ulid/v2"
)

// AccessKeyPrefix 是密钥的可见前缀，用来和配对码（HH-）区分。
//
// 两者不能共用一个前缀：配对码消费后即失效，密钥长期可重复使用。
// 用户在同一个输入框里粘贴，服务端得先分清是哪一种，否则一次
// 误判就会把长期凭证当成一次性码处理。
const AccessKeyPrefix = "HHE"

// accessKeyLen 是密钥主体的字符数。
//
// 配对码是 12 位（约 59 bit），因为它活 15 分钟且只能用一次；
// 密钥要长期有效，熵必须更高：20 位 31 进制约 99 bit，
// 即使被离线穷举也不现实。
const accessKeyLen = 20

// accessKeyAlphabet 与配对码共用一张字符表，理由见 dialAlphabet。
const accessKeyAlphabet = dialAlphabet

// AccessKeyResult 是签发结果。Plain 是明文，只在这里出现一次。
type AccessKeyResult struct {
	Plain  string
	Key    *store.AccessKey
	Client *store.Client
}

// GenerateAccessKey 签发一把长期登录密钥。
//
// 关键在 client 的处理：这里生成一对 Ed25519 密钥、把公钥登记为一台
// 「无主客户端」，私钥当场丢弃（函数返回后没有任何地方持有它）。
// 于是这台 client 永远无法用挑战-响应登录——没人签得出那个消息——
// 只能靠这把密钥兑换。
//
// 这样做是为了不改 auth_client 的约束：algorithm 仍是 ed25519、
// public_key 仍是合法公钥，不需要在 SQLite 里重建表。
func (s *Service) GenerateAccessKey(ctx context.Context, name string, ttl time.Duration) (*AccessKeyResult, error) {
	if name == "" {
		name = "access key"
	}

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	now := s.now()
	client := &store.Client{
		ID:        ulid.Make().String(),
		Name:      truncate(name, 64),
		PublicKey: pub,
		Algorithm: AlgEd25519,
		// 填稳定身份：这是轮换密钥后数据仍然可见的全部原因。
		// 留空的话 owner_hash 会退回按 client id 推导，新旧密钥
		// 各自算出不同的哈希，旧私有片段变成无人认领的孤儿。
		OwnerID:   OwnerPrimary,
		CreatedAt: now.Unix(),
	}
	if err := s.st.CreateClient(ctx, client); err != nil {
		return nil, err
	}

	plain, err := accessKeyPlain()
	if err != nil {
		return nil, err
	}

	key := &store.AccessKey{
		ID:        ulid.Make().String(),
		Name:      truncate(name, 64),
		KeyHash:   HashToken(normalizeAccessKey(plain)),
		ClientID:  client.ID,
		CreatedAt: now.Unix(),
	}
	if ttl > 0 {
		exp := now.Add(ttl).Unix()
		key.ExpiresAt = &exp
	}
	if err := s.st.CreateAccessKey(ctx, key); err != nil {
		return nil, err
	}

	return &AccessKeyResult{Plain: plain, Key: key, Client: client}, nil
}

// RedeemAccessKey 用密钥换一组会话令牌。
//
// 成功即等同于一台设备登录成功：后续的 access token、refresh cookie、
// CSRF、owner_hash 全部走同一条链路，下游不需要知道这次登录用的是密钥
// 还是设备私钥。
func (s *Service) RedeemAccessKey(ctx context.Context, plain, userAgent string) (*Tokens, error) {
	hash := HashToken(normalizeAccessKey(plain))
	now := s.now()

	key, err := s.st.GetAccessKeyByHash(ctx, hash, now.Unix())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 与 Exchange 一致：不存在、已撤销、已过期都返回同一个错误，
			// 不给攻击者枚举"这把密钥曾经存在过"的通道。
			return nil, ErrBadCredentials
		}
		return nil, err
	}

	client, err := s.st.GetClient(ctx, key.ClientID)
	if err != nil || !client.Active() {
		return nil, ErrBadCredentials
	}

	_ = s.st.TouchAccessKey(ctx, key.ID, now.Unix())
	return s.issue(ctx, client, userAgent)
}

// accessKeyPlain 生成 HHE-XXXX-XXXX-XXXX-XXXX-XXXX。
func accessKeyPlain() (string, error) {
	buf := make([]byte, accessKeyLen)
	if err := readRandom(buf); err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString(AccessKeyPrefix)
	sb.WriteByte('-')
	for i, b := range buf {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(accessKeyAlphabet[int(b)%len(accessKeyAlphabet)])
	}
	return sb.String(), nil
}

// normalizeAccessKey 让哈希只取决于语义内容：容忍大小写、空格、
// 分隔符甚至省略 HHE- 前缀的差异。用户从聊天窗口复制时经常带来这些。
func normalizeAccessKey(raw string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(raw) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	compact := sb.String()

	// 只有「长度正好是前缀加主体」时才去前缀。
	//
	// 不能无条件 TrimPrefix：字母表含 H 和 E，20 位主体本身就可能以
	// "HHE" 开头。那种情况下无条件去前缀会切掉主体前三个字符，把一条
	// 合法密钥算成另一个哈希——用户手里的密钥会莫名其妙失效。
	if len(compact) == len(AccessKeyPrefix)+accessKeyLen &&
		strings.HasPrefix(compact, AccessKeyPrefix) {
		return compact[len(AccessKeyPrefix):]
	}
	return compact
}
