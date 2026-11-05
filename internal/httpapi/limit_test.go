package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/auth"

	"github.com/gin-gonic/gin"
)

// 同一 IP 的 50 个并发失败请求：真正到达处理器并以 401 结束的次数
// 不能超过 limit，其余必须是 429。先查后计的实现在这里会放行远多于 limit 的请求。
func TestAttemptLimitConcurrentFailures(t *testing.T) {
	const limit, workers = 3, 50
	h := newHarness(t)
	h.setLimiter(auth.NewAttemptLimiter(limit, time.Minute))

	badToken := map[string]any{
		"token": "HH-0000-0000-0000", "name": "x",
		"algorithm":  "ed25519",
		"public_key": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}

	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		unauthorized int
		limited      int
		other        []int
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := h.doJSON(t, http.MethodPost, "/api/v1/auth/enroll", badToken, nil)
			mu.Lock()
			defer mu.Unlock()
			switch w.Code {
			case http.StatusUnauthorized:
				unauthorized++
			case http.StatusTooManyRequests:
				limited++
			default:
				other = append(other, w.Code)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected statuses: %v", other)
	}
	if unauthorized > limit {
		t.Fatalf("放行的失败次数 = %d, 超过上限 %d", unauthorized, limit)
	}
	if unauthorized+limited != workers {
		t.Fatalf("401=%d + 429=%d != %d", unauthorized, limited, workers)
	}
}

// limitEngine 构造一个只挂限流中间件的最小路由，状态码由查询参数决定。
func limitEngine(mw gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/x", mw, func(c *gin.Context) {
		code, _ := strconv.Atoi(c.Query("code"))
		c.Status(code)
	})
	return r
}

func hit(r *gin.Engine, code int) int {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x?code="+strconv.Itoa(code), nil)
	r.ServeHTTP(w, req)
	return w.Code
}

func TestAttemptLimitStatusAccounting(t *testing.T) {
	const ip = "192.0.2.1" // httptest.NewRequest 的默认 RemoteAddr

	t.Run("non-failure releases reservation", func(t *testing.T) {
		l := auth.NewAttemptLimiter(2, time.Minute)
		r := limitEngine(RequireAttemptLimit(l, nil))
		// 400、404、500 都不算失败：多少次都不该被拦。
		for i := 0; i < 10; i++ {
			for _, code := range []int{400, 404, 500} {
				if got := hit(r, code); got != code {
					t.Fatalf("status = %d, want %d", got, code)
				}
			}
		}
		if rem, _ := l.Remaining(ip); rem != 2 {
			t.Errorf("remaining = %d, want 2", rem)
		}
	})

	t.Run("401 and 403 are kept", func(t *testing.T) {
		l := auth.NewAttemptLimiter(2, time.Minute)
		r := limitEngine(RequireAttemptLimit(l, nil))
		hit(r, 401)
		hit(r, 403)
		if got := hit(r, 401); got != http.StatusTooManyRequests {
			t.Fatalf("第 3 次 = %d, want 429", got)
		}
	})

	t.Run("non-resetting success does not clear failures", func(t *testing.T) {
		// 模拟 /auth/challenge：成功只是发了 nonce，不能把失败计数刷回零。
		l := auth.NewAttemptLimiter(2, time.Minute)
		r := limitEngine(RequireAttemptLimit(l, nil))
		hit(r, 401)
		if got := hit(r, 200); got != 200 {
			t.Fatalf("success = %d", got)
		}
		if rem, _ := l.Remaining(ip); rem != 1 {
			t.Fatalf("remaining = %d, want 1（成功不该清零也不该计数）", rem)
		}
		hit(r, 401)
		if got := hit(r, 200); got != http.StatusTooManyRequests {
			t.Fatalf("达到上限后连成功路径也该被拦，got %d", got)
		}
	})

	t.Run("resetting success clears failures", func(t *testing.T) {
		l := auth.NewAttemptLimiter(2, time.Minute)
		r := limitEngine(RequireAttemptLimitResetting(l, nil))
		hit(r, 401)
		if got := hit(r, 201); got != 201 {
			t.Fatalf("success = %d", got)
		}
		if l.Size() != 0 {
			t.Fatalf("凭证验证成功后应清空计数，size=%d", l.Size())
		}
	})
}

// 路由层面：challenge 的成功不能清空 enroll 失败留下的计数。
func TestChallengeDoesNotResetLimiter(t *testing.T) {
	h := newHarness(t)
	l := auth.NewAttemptLimiter(3, time.Minute)
	h.setLimiter(l)

	// 先建一个合法设备（直接走 service，不经过限流路由）。
	token, _ := h.login(t)
	if token == "" {
		t.Fatal("login failed")
	}
	// login 内部的 enroll/token 成功会清零；从这里开始计数。
	if l.Size() != 0 {
		t.Fatalf("登录成功后计数应为 0，size=%d", l.Size())
	}

	badToken := map[string]any{
		"token": "HH-0000-0000-0000", "name": "x",
		"algorithm":  "ed25519",
		"public_key": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}
	for i := 0; i < 2; i++ {
		if w := h.doJSON(t, http.MethodPost, "/api/v1/auth/enroll", badToken, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("enroll = %d", w.Code)
		}
	}

	clients, err := h.db.Auth().ListClients(t.Context())
	if err != nil || len(clients) == 0 {
		t.Fatalf("list clients: %v (%d)", err, len(clients))
	}
	w := h.doJSON(t, http.MethodPost, "/api/v1/auth/challenge",
		map[string]any{"client_id": clients[0].ID}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("challenge = %d: %s", w.Code, w.Body.String())
	}
	if rem, _ := l.Remaining("192.0.2.1"); rem != 1 {
		t.Fatalf("challenge 成功后 remaining = %d, want 1", rem)
	}
}
