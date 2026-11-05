package paste

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"

	"golang.org/x/crypto/argon2"
)

// legacyHash 用指定参数造一个 PHC hash，模拟旧版本写进库里的数据。
func legacyHash(pw string, m, t uint32, p uint8) string {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(pw), salt, t, m, p, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, m, t, p,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func TestHashPasswordUsesOWASPParams(t *testing.T) {
	hash, err := HashPassword(context.Background(), "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hash, "$m=19456,t=2,p=1$") {
		t.Errorf("hash 参数不是 OWASP m=19456,t=2,p=1: %s", hash)
	}
}

func TestVerifyPasswordAcceptsLegacy64MiBHash(t *testing.T) {
	// 旧版本参数 m=65536,t=3,p=2 正好在上限上，必须继续可用。
	hash := legacyHash("old", 64*1024, 3, 2)
	ok, err := VerifyPassword(context.Background(), hash, "old")
	if err != nil || !ok {
		t.Fatalf("旧 hash 验证失败: ok=%v err=%v", ok, err)
	}
}

func TestVerifyPasswordRejectsMemoryAbove64MiB(t *testing.T) {
	salt := base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef"))
	key := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	for _, params := range []string{"m=65537,t=2,p=1", "m=19456,t=11,p=1"} {
		hash := "$argon2id$v=19$" + params + "$" + salt + "$" + key
		ok, err := VerifyPassword(context.Background(), hash, "x")
		if ok || err != nil {
			t.Errorf("%s: ok=%v err=%v，应为 false/nil", params, ok, err)
		}
	}
}

func TestArgonSemaphoreHonoursContext(t *testing.T) {
	// 占满全部名额，再发起一次计算：必须在 ctx 超时后返回，而不是一直阻塞。
	for i := 0; i < cap(argonSem); i++ {
		argonSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(argonSem); i++ {
			<-argonSem
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := HashPassword(ctx, "pw"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HashPassword err = %v, want DeadlineExceeded", err)
	}
	hash := legacyHash("pw", 19456, 2, 1)
	if _, err := VerifyPassword(ctx, hash, "pw"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("VerifyPassword err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("等待名额时没有尊重 ctx")
	}
}

func TestArgonSemaphoreCapacityIsTwo(t *testing.T) {
	if cap(argonSem) != 2 {
		t.Errorf("argonSem 容量 = %d, want 2", cap(argonSem))
	}
}

func TestReadWithCancelledContextIsNotBadPassword(t *testing.T) {
	s := newService(t)
	p, _, err := s.Create(context.Background(), CreateRequest{Body: "x", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cap(argonSem); i++ {
		argonSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(argonSem); i++ {
			<-argonSem
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Read(ctx, p.Slug, "pw", "")
	if err == nil || errors.Is(err, ErrBadPassword) {
		t.Fatalf("err = %v，ctx 取消不应被当成口令错误", err)
	}
}

func TestAnonymousCreateRejectsPermanentExpiry(t *testing.T) {
	s := newService(t)
	zero := time.Duration(0)
	_, _, err := s.Create(context.Background(), CreateRequest{Body: "x", ExpiresIn: &zero})
	if !errors.Is(err, ErrBadExpiry) {
		t.Fatalf("err = %v, want ErrBadExpiry（映射到 400）", err)
	}
	// 带 owner token 仍然是匿名。
	_, _, err = s.Create(context.Background(), CreateRequest{Body: "x", ExpiresIn: &zero, OwnerToken: "tok"})
	if !errors.Is(err, ErrBadExpiry) {
		t.Fatalf("owner token 不是登录身份: err = %v", err)
	}
}

func TestAuthenticatedCreateAllowsPermanentExpiry(t *testing.T) {
	s := newService(t)
	zero := time.Duration(0)
	for _, req := range []CreateRequest{
		{Body: "x", ExpiresIn: &zero, OwnerClientID: "dev"},
		{Body: "x", ExpiresIn: &zero, OwnerID: "owner"},
	} {
		p, _, err := s.Create(context.Background(), req)
		if err != nil {
			t.Fatalf("登录身份创建永久片段失败: %v", err)
		}
		if p.ExpiresAt != nil {
			t.Error("expires_at 应为 nil")
		}
	}
}

func TestAnonymousUpdateRejectsPermanentExpiry(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	p, token, err := s.Create(ctx, CreateRequest{Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	zero := time.Duration(0)
	_, err = s.Update(ctx, p.Slug, UpdateRequest{ExpiresIn: &zero}, store.OwnerHashForToken(token))
	if !errors.Is(err, ErrBadExpiry) {
		t.Fatalf("err = %v, want ErrBadExpiry", err)
	}
}

func TestUpdateRejectsUnknownVisibility(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	p, token, err := s.Create(ctx, CreateRequest{Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	bad := "everyone"
	_, err = s.Update(ctx, p.Slug, UpdateRequest{Visibility: &bad}, store.OwnerHashForToken(token))
	if !errors.Is(err, ErrBadVisibility) {
		t.Fatalf("err = %v, want ErrBadVisibility", err)
	}
	got, _ := s.st.Paste().GetBySlug(ctx, p.Slug)
	if got.Visibility != VisUnlisted {
		t.Errorf("非法值被写入: %q", got.Visibility)
	}
}

func TestCreateRejectsUnknownVisibility(t *testing.T) {
	s := newService(t)
	_, _, err := s.Create(context.Background(), CreateRequest{Body: "x", Visibility: "secret"})
	if !errors.Is(err, ErrBadVisibility) {
		t.Fatalf("err = %v, want ErrBadVisibility", err)
	}
}

func TestListWithEmptyOwnerReturnsNothing(t *testing.T) {
	s := newService(t)
	legacyPaste(t, s, VisUnlisted) // owner_hash = ''
	items, next, err := s.List(context.Background(), "", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 || next != "" {
		t.Fatalf("空归属列出了 %d 条历史片段", len(items))
	}
}

func TestCountByOwnerEmptyIsZero(t *testing.T) {
	s := newService(t)
	legacyPaste(t, s, VisUnlisted)
	n, err := s.st.Paste().CountByOwner(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("CountByOwner(\"\") = %d, want 0", n)
	}
}

func TestFixedWindowLimitsAndResets(t *testing.T) {
	now := time.Unix(1000, 0)
	w := newFixedWindow(3, time.Minute, 100, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if ok, _ := w.take("a"); !ok {
			t.Fatalf("第 %d 次被拒", i+1)
		}
	}
	ok, wait := w.take("a")
	if ok || wait <= 0 || wait > time.Minute {
		t.Fatalf("超限应拒绝并给出等待时间: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := w.take("b"); !ok {
		t.Fatal("其他 key 不应受影响")
	}
	now = now.Add(time.Minute)
	if ok, _ := w.take("a"); !ok {
		t.Fatal("窗口过后应恢复")
	}
}

func TestFixedWindowMemoryBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	w := newFixedWindow(1, time.Minute, 5, func() time.Time { return now })
	for i := 0; i < 50; i++ {
		w.take(fmt.Sprintf("k%d", i))
	}
	if w.size() > 5 {
		t.Fatalf("size = %d, 超过上限 5", w.size())
	}
	// 窗口过后清扫，新来源可以进入。
	now = now.Add(2 * time.Minute)
	if ok, _ := w.take("fresh"); !ok {
		t.Fatal("过期后新来源应被接纳")
	}
	if w.size() != 1 {
		t.Errorf("清扫后 size = %d, want 1", w.size())
	}
}

func TestPasswordAttemptsLimitedPerSourceAndSlug(t *testing.T) {
	s := newService(t)
	for i := 0; i < PasswordAttemptLimit; i++ {
		if ok, _ := s.PasswordAttemptAllowed("1.2.3.4", "abc"); !ok {
			t.Fatalf("第 %d 次被提前拦截", i+1)
		}
		s.RecordPasswordResult("1.2.3.4", "abc", false)
	}
	if ok, wait := s.PasswordAttemptAllowed("1.2.3.4", "abc"); ok || wait <= 0 {
		t.Fatalf("超过 %d 次后应被拦截: ok=%v wait=%v", PasswordAttemptLimit, ok, wait)
	}
	if ok, _ := s.PasswordAttemptAllowed("1.2.3.4", "other"); !ok {
		t.Error("不同 slug 不应受影响")
	}
	if ok, _ := s.PasswordAttemptAllowed("5.6.7.8", "abc"); !ok {
		t.Error("不同来源不应受影响")
	}
}
