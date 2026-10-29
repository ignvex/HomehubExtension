package paste

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store/sqlite"
)

func newService(t *testing.T) *Service {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "p.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	return NewService(db, Config{
		MaxBodyBytes:  1024,
		DefaultExpiry: 24 * time.Hour,
		MaxExpiry:     365 * 24 * time.Hour,
		SlugLength:    6,
		AllowPublic:   true,
		Languages:     map[string]bool{"text": true, "go": true},
	})
}

func TestCreateAndRead(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	p, err := s.Create(ctx, CreateRequest{Body: "hello", Language: "go"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(p.Slug) != 6 {
		t.Errorf("slug = %q, want 6 chars", p.Slug)
	}
	if p.ExpiresAt == nil {
		t.Error("default expiry should be applied")
	}

	got, err := s.Read(ctx, p.Slug, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Body != "hello" {
		t.Errorf("body = %q", got.Body)
	}
}

func TestUnknownLanguageFallsBack(t *testing.T) {
	s := newService(t)
	p, err := s.Create(context.Background(), CreateRequest{
		Body: "x", Language: "brainfuck",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Language != "text" {
		t.Errorf("language = %q, want text", p.Language)
	}
}

func TestBodyTooLarge(t *testing.T) {
	s := newService(t)
	_, err := s.Create(context.Background(), CreateRequest{
		Body: strings.Repeat("a", 2048),
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestExpiryBeyondMax(t *testing.T) {
	s := newService(t)
	d := 400 * 24 * time.Hour
	_, err := s.Create(context.Background(), CreateRequest{Body: "x", ExpiresIn: &d})
	if !errors.Is(err, ErrBadExpiry) {
		t.Fatalf("err = %v, want ErrBadExpiry", err)
	}
}

func TestZeroExpiryMeansPermanent(t *testing.T) {
	s := newService(t)
	zero := time.Duration(0)
	p, err := s.Create(context.Background(), CreateRequest{Body: "x", ExpiresIn: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if p.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil (permanent)", *p.ExpiresAt)
	}
}

func TestPasswordProtection(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	p, err := s.Create(ctx, CreateRequest{Body: "secret", Password: "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if p.PasswordHash == "" {
		t.Fatal("password hash should be stored")
	}
	// 明文绝不能落库。
	if strings.Contains(p.PasswordHash, "hunter2") {
		t.Fatal("plaintext password leaked into hash field")
	}

	if _, err := s.Read(ctx, p.Slug, "wrong"); !errors.Is(err, ErrBadPassword) {
		t.Errorf("wrong password = %v, want ErrBadPassword", err)
	}
	if _, err := s.Read(ctx, p.Slug, "hunter2"); err != nil {
		t.Errorf("correct password rejected: %v", err)
	}
}

func TestBurnAfterReadIsSingleUse(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	p, err := s.Create(ctx, CreateRequest{Body: "once", BurnAfterRead: true})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Read(ctx, p.Slug, ""); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := s.Read(ctx, p.Slug, ""); !errors.Is(err, ErrExpired) {
		t.Errorf("second read = %v, want ErrExpired", err)
	}
}

func TestBurnAfterReadConcurrent(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	p, err := s.Create(ctx, CreateRequest{Body: "only once", BurnAfterRead: true})
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	results := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			_, err := s.Read(ctx, p.Slug, "")
			results <- err
		}()
	}
	close(start)

	success := 0
	for i := 0; i < n; i++ {
		if err := <-results; err == nil {
			success++
		}
	}
	// 条件更新保证只有一个请求能把 deleted_at 从 NULL 改掉。
	if success != 1 {
		t.Errorf("%d concurrent reads succeeded, want exactly 1", success)
	}
}

func TestMetaDoesNotBurn(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	p, _ := s.Create(ctx, CreateRequest{Body: "content", BurnAfterRead: true})
	if _, err := s.Meta(ctx, p.Slug); err != nil {
		t.Fatal(err)
	}
	// 页面预取不应烧掉一次性片段。
	if _, err := s.Read(ctx, p.Slug, ""); err != nil {
		t.Errorf("read after meta should succeed: %v", err)
	}
}

func TestDeleteThenRead(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	p, _ := s.Create(ctx, CreateRequest{Body: "x"})
	if err := s.Delete(ctx, p.Slug); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx, p.Slug, ""); !errors.Is(err, ErrExpired) {
		t.Errorf("read after delete = %v, want ErrExpired", err)
	}
}

func TestListOmitsBody(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := s.Create(ctx, CreateRequest{Body: "body-content"}); err != nil {
			t.Fatal(err)
		}
	}
	items, _, err := s.List(ctx, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d", len(items))
	}
	for _, p := range items {
		if p.Body != "" {
			t.Errorf("list should not include body, got %q", p.Body)
		}
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("unexpected hash format: %s", hash)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Error("correct password rejected")
	}
	if VerifyPassword(hash, "wrong") {
		t.Error("wrong password accepted")
	}
	// 同一口令两次哈希必须不同（盐随机）。
	hash2, _ := HashPassword("correct horse battery staple")
	if hash == hash2 {
		t.Error("salt does not appear to be random")
	}
}

func TestPublicDisabled(t *testing.T) {
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "p2.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := NewService(db, Config{MaxBodyBytes: 1024, AllowPublic: false})
	_, err = s.Create(context.Background(), CreateRequest{Body: "x", Visibility: VisPublic})
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}
