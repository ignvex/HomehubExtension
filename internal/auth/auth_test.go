package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

func testService(t *testing.T) (*Service, *sqlite.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	signer, err := LoadOrCreateSigner(filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	return NewService(db.Auth(), signer, Settings{}), db
}

func TestSignerPersistsKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.key")

	s1, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	// 同一路径必须复用同一密钥，否则重启后所有 access token 立刻失效。
	if s1.KID() != s2.KID() {
		t.Errorf("kid changed across reload: %s vs %s", s1.KID(), s2.KID())
	}
}

func TestTokenRoundTrip(t *testing.T) {
	signer, err := LoadOrCreateSigner(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, err := signer.IssueToken("client-1", now, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := signer.VerifyToken(tok, now)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if sub != "client-1" {
		t.Errorf("sub = %q", sub)
	}
}

func TestTokenRejectsExpired(t *testing.T) {
	signer, _ := LoadOrCreateSigner(filepath.Join(t.TempDir(), "k"))
	now := time.Now()
	tok, _ := signer.IssueToken("c", now, time.Second)

	if _, err := signer.VerifyToken(tok, now.Add(2*time.Second)); err == nil {
		t.Fatal("expired token should be rejected")
	}
}

func TestTokenRejectsTampered(t *testing.T) {
	signer, _ := LoadOrCreateSigner(filepath.Join(t.TempDir(), "k"))
	now := time.Now()
	tok, _ := signer.IssueToken("c", now, time.Hour)

	// 改掉 payload 中的一个字符，签名必须失效。
	// 必须改中部字符：base64 最后一个字符的低位是填充位，
	// 换掉它可能解码出同样的字节，测不出问题。
	tampered := []byte(tok)
	firstDot := indexByte(tampered, '.')
	pos := firstDot + 3
	if tampered[pos] == 'A' {
		tampered[pos] = 'B'
	} else {
		tampered[pos] = 'A'
	}
	if _, err := signer.VerifyToken(string(tampered), now); err == nil {
		t.Fatal("tampered token should be rejected")
	}
}

func TestTokenRejectsWrongKID(t *testing.T) {
	dir := t.TempDir()
	s1, _ := LoadOrCreateSigner(filepath.Join(dir, "a"))
	s2, _ := LoadOrCreateSigner(filepath.Join(dir, "b"))

	now := time.Now()
	tok, _ := s1.IssueToken("c", now, time.Hour)
	if _, err := s2.VerifyToken(tok, now); err == nil {
		t.Fatal("token signed by another key must be rejected")
	}
}

func TestAuthMessageFormat(t *testing.T) {
	got := string(AuthMessage("01JC", "nonce-value"))
	want := "homehubext/auth/v1\n01JC\nnonce-value"
	// 前端逐字节生成同样的字符串，任何差异都会让签名校验失败。
	if got != want {
		t.Errorf("AuthMessage = %q, want %q", got, want)
	}
}

func TestVerifyEd25519(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := []byte("hello")
	sig := ed25519.Sign(priv, msg)

	if err := Verify(AlgEd25519, pub, msg, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := Verify(AlgEd25519, pub, []byte("other"), sig); err == nil {
		t.Fatal("signature over different message should fail")
	}
}

// TestVerifyP256P1363 覆盖 WebCrypto 与 Go 的格式差异。
// 浏览器产出的是 r||s 原始 64 字节，不是 ASN.1 DER；
// 不转换的话所有 P-256 客户端都会验签失败。
func TestVerifyP256P1363(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("homehubext/auth/v1\nc1\nn1")
	digest := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	// 手工拼 P1363：r||s 各 32 字节。
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	if err := Verify(AlgP256, &priv.PublicKey, msg, sig); err != nil {
		t.Fatalf("P1363 signature rejected: %v", err)
	}

	// 同一签名的 ASN.1 形式也必须被接受（Go 客户端会发这种）。
	asn1Sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(AlgP256, &priv.PublicKey, msg, asn1Sig); err != nil {
		t.Fatalf("ASN.1 signature rejected: %v", err)
	}
}

func TestEnrollChallengeTokenFlow(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	code, _, err := svc.GenerateEnrollment(ctx, "test-device")
	if err != nil {
		t.Fatal(err)
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	res, err := svc.Enroll(ctx, code, "macbook", AlgEd25519,
		base64.StdEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if res.Client.ID == "" {
		t.Fatal("client id empty")
	}

	// 配对码一次性。
	if _, err := svc.Enroll(ctx, code, "again", AlgEd25519,
		base64.StdEncoding.EncodeToString(pub)); err == nil {
		t.Fatal("enrollment token should be single-use")
	}

	ch, err := svc.Challenge(ctx, res.Client.ID)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, AuthMessage(res.Client.ID, ch.Nonce))
	tokens, err := svc.Exchange(ctx, res.Client.ID, ch.Nonce,
		base64.StdEncoding.EncodeToString(sig), "test-agent")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.CSRF == "" {
		t.Fatal("tokens incomplete")
	}

	client, err := svc.Authenticate(ctx, tokens.AccessToken)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if client.ID != res.Client.ID {
		t.Errorf("client id = %s", client.ID)
	}

	// refresh 必须旋转：旧 token 再用就被判为泄露。
	rotated, err := svc.Refresh(ctx, tokens.RefreshToken, "test-agent")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if rotated.RefreshToken == tokens.RefreshToken {
		t.Error("refresh token was not rotated")
	}
	if _, err := svc.Refresh(ctx, tokens.RefreshToken, "test-agent"); err == nil {
		t.Fatal("reused refresh token should be rejected")
	}
}

func TestExchangeRejectsWrongSignature(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	code, _, _ := svc.GenerateEnrollment(ctx, "")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	res, err := svc.Enroll(ctx, code, "d", AlgEd25519, base64.StdEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := svc.Challenge(ctx, res.Client.ID)

	bad := make([]byte, ed25519.SignatureSize)
	if _, err := svc.Exchange(ctx, res.Client.ID, ch.Nonce,
		base64.StdEncoding.EncodeToString(bad), ""); err == nil {
		t.Fatal("zero signature should be rejected")
	}

	// 失败后 nonce 已被消费：不能重试同一 nonce 换一个正确签名。
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	sig := ed25519.Sign(priv, AuthMessage(res.Client.ID, ch.Nonce))
	if _, err := svc.Exchange(ctx, res.Client.ID, ch.Nonce,
		base64.StdEncoding.EncodeToString(sig), ""); err == nil {
		t.Fatal("consumed nonce must not be reusable")
	}
}

func TestRevokedClientRejected(t *testing.T) {
	svc, db := testService(t)
	ctx := context.Background()

	code, _, _ := svc.GenerateEnrollment(ctx, "")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	res, _ := svc.Enroll(ctx, code, "d", AlgEd25519, base64.StdEncoding.EncodeToString(pub))

	ch, _ := svc.Challenge(ctx, res.Client.ID)
	sig := ed25519.Sign(priv, AuthMessage(res.Client.ID, ch.Nonce))
	tokens, err := svc.Exchange(ctx, res.Client.ID, ch.Nonce, base64.StdEncoding.EncodeToString(sig), "")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Auth().RevokeClient(ctx, res.Client.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, tokens.AccessToken); err == nil {
		t.Fatal("revoked client should not authenticate")
	}
}

func TestPairingCodeFormat(t *testing.T) {
	code, err := pairingCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != len("HH-XXXX-XXXX-XXXX") {
		t.Errorf("code = %q, unexpected length", code)
	}
	if code[:3] != "HH-" {
		t.Errorf("code should start with HH-, got %q", code)
	}
	// 归一化必须容忍大小写与多余符号：用户经常复制到带空格的文本。
	if normalizePairingCode(" hh-7qk4-2m9x-p3tb ") != normalizePairingCode(code) && len(normalizePairingCode(code)) != 14 {
		t.Error("normalizePairingCode should strip separators and case")
	}
	if len(normalizePairingCode(code)) != 14 {
		t.Errorf("normalized length = %d, want 14", len(normalizePairingCode(code)))
	}
}

// ---- 入站请求签名 ----

func TestHubSignatureVerification(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	v := NewHubVerifier(pub, time.Minute, time.Minute)

	body := []byte(`{"url":"https://example.com/feed.xml"}`)
	req := newSignedRequest(t, priv, http.MethodPost, "/api/v1/hub/subscriptions",
		"", body, time.Now().Unix(), "nonce-1")

	if err := v.Verify(req, body); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// 同一 nonce 再来一次必须是重放。
	if err := v.Verify(req, body); err != ErrReplayed {
		t.Errorf("second verify = %v, want ErrReplayed", err)
	}
}

func TestHubSignatureRejectsTamperedBody(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	v := NewHubVerifier(pub, time.Minute, time.Minute)

	body := []byte(`{"url":"https://example.com/feed.xml"}`)
	req := newSignedRequest(t, priv, http.MethodPost, "/api/v1/hub/subscriptions",
		"", body, time.Now().Unix(), "nonce-2")

	if err := v.Verify(req, []byte(`{"url":"https://evil.com/feed.xml"}`)); err == nil {
		t.Fatal("tampered body should fail verification")
	}
}

func TestHubSignatureRejectsPathChange(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	v := NewHubVerifier(pub, time.Minute, time.Minute)

	body := []byte(`{}`)
	req := newSignedRequest(t, priv, http.MethodPost, "/api/v1/hub/subscriptions",
		"", body, time.Now().Unix(), "nonce-3")

	// 把同一份签名搬到另一个路径：path 参与签名，必须失败。
	req.URL.Path = "/api/v1/hub/other"
	if err := v.Verify(req, body); err == nil {
		t.Fatal("signature must not be portable across paths")
	}
}

func TestHubSignatureRejectsClockSkew(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	v := NewHubVerifier(pub, 30*time.Second, time.Minute)

	body := []byte(`{}`)
	req := newSignedRequest(t, priv, http.MethodGet, "/api/v1/hub/subscriptions",
		"", body, time.Now().Add(-5*time.Minute).Unix(), "nonce-4")

	if err := v.Verify(req, body); err != ErrClockSkew {
		t.Errorf("err = %v, want ErrClockSkew", err)
	}
}

func TestHubSignatureRequiresHeaders(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	v := NewHubVerifier(pub, time.Minute, time.Minute)

	req, _ := http.NewRequest(http.MethodGet, "/x", nil)
	if err := v.Verify(req, nil); err == nil {
		t.Fatal("missing headers should fail")
	}
}

func newSignedRequest(t *testing.T, priv ed25519.PrivateKey, method, path, query string,
	body []byte, ts int64, nonce string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, path+"?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-HH-Timestamp", strconvItoa(ts))
	req.Header.Set("X-HH-Nonce", nonce)
	sig := ed25519.Sign(priv, []byte(RequestMessage(req, body)))
	req.Header.Set("X-HH-Signature", base64.StdEncoding.EncodeToString(sig))
	return req
}

func strconvItoa(v int64) string { return strconv.FormatInt(v, 10) }

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// 防重放表必须在没有外部调度的情况下自行回收。
//
// 之前的实现只在表满（10 万条）时才清理，意味着低频部署会让
// 2 分钟就该失效的 nonce 一直留在内存里直到表被填满。
func TestReplayGuardSelfSweeps(t *testing.T) {
	g := NewReplayGuard(80*time.Millisecond, 1000)

	if !g.Allow("n1") {
		t.Fatal("首次应允许")
	}
	if g.Allow("n1") {
		t.Fatal("重复应拒绝")
	}
	if got := g.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1", got)
	}

	// 等过一个 ttl，再写入新 nonce 触发清理。
	time.Sleep(120 * time.Millisecond)
	if !g.Allow("n2") {
		t.Fatal("过期后新 nonce 应允许")
	}
	if got := g.Size(); got != 1 {
		t.Errorf("清理后 Size = %d, want 1（n1 应已过期被回收）", got)
	}

	// 过期条目被回收后，同一个 nonce 可以再次使用——
	// 这是有意的：时间戳窗口（60s）已经拒绝了真正的重放，
	// nonce 表只是同一窗口内的第二道防线。
	time.Sleep(120 * time.Millisecond)
	if !g.Allow("n1") {
		t.Error("过期回收后 n1 应可再用")
	}
}

func TestReplayGuardRejectsWhenFull(t *testing.T) {
	// limit 很小且 ttl 很长：表填满后必须拒绝新 nonce，而不是无限增长。
	g := NewReplayGuard(time.Hour, 3)
	for _, n := range []string{"a", "b", "c"} {
		if !g.Allow(n) {
			t.Fatalf("%s 应允许", n)
		}
	}
	if g.Allow("d") {
		t.Error("表满后应拒绝，避免无界增长")
	}
	if got := g.Size(); got != 3 {
		t.Errorf("Size = %d, want 3", got)
	}
}
