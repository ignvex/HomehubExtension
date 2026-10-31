package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 时间字段的形态是 API 契约的一部分，必须锁住。
//
// 这个测试的存在理由：项目早期时间字段是混用的——job_run 是裸 Unix 秒，
// 其余是 RFC3339 字符串，前端为此写了 `value < 1e12 ? value*1000 : value`
// 的启发式去猜单位。那种状态没有测试保护，谁改一处都不会被发现。
func TestTimeFieldsAreUnixSeconds(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	now := time.Now().Unix()
	// 允许几秒的执行偏差。
	lo, hi := now-30, now+30

	isUnixSec := func(t *testing.T, label string, v any) int64 {
		t.Helper()
		num, ok := v.(float64)
		if !ok {
			t.Fatalf("%s 应为数字（Unix 秒），实际是 %T: %v", label, v, v)
		}
		got := int64(num)
		if got < lo || got > hi {
			t.Errorf("%s = %d，不在 [%d, %d] 之内——单位可能不是秒", label, got, lo, hi)
		}
		return got
	}

	// ---- paste ----
	w := h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"body": "time test", "visibility": "unlisted", "expires_in": "1h",
	}, bearer(token))
	if w.Code != http.StatusCreated {
		t.Fatalf("create paste = %d: %s", w.Code, w.Body.String())
	}
	var paste map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &paste); err != nil {
		t.Fatal(err)
	}

	created := isUnixSec(t, "paste.created_at", paste["created_at"])

	// expires_at 是未来时刻，不能拿「接近现在」断言，
	// 要用与 created_at 的相对关系验证单位一致。
	expRaw, ok := paste["expires_at"].(float64)
	if !ok {
		t.Fatalf("paste.expires_at 应为数字，实际是 %T", paste["expires_at"])
	}
	if delta := int64(expRaw) - created; delta < 3595 || delta > 3605 {
		t.Errorf("expires_at - created_at = %d，期望约 3600（单位可能不一致）", delta)
	}

	// 永久片段的 expires_at 必须是 null，不能是 0
	// （0 在 Unix 时间里是 1970-01-01，是个真实时刻）。
	w = h.doJSON(t, http.MethodPost, "/api/v1/pastes", map[string]any{
		"body": "permanent", "visibility": "unlisted", "expires_in": "0",
	}, bearer(token))
	var perm map[string]any
	json.Unmarshal(w.Body.Bytes(), &perm)
	if v, ok := perm["expires_at"]; !ok || v != nil {
		t.Errorf("永久片段 expires_at = %v，应为 null", v)
	}

	// ---- 系统任务（这条本来就是对的形式，防回归） ----
	w = h.do(t, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/system/jobs", nil), token))
	if w.Code != http.StatusOK {
		t.Fatalf("jobs = %d", w.Code)
	}
	var jobs struct {
		Jobs []map[string]any `json:"jobs"`
	}
	json.Unmarshal(w.Body.Bytes(), &jobs)
	for _, job := range jobs.Jobs {
		isUnixSec(t, "job.started_at", job["started_at"])
		if job["finished_at"] != nil {
			isUnixSec(t, "job.finished_at", job["finished_at"])
		}
	}
}

// TestDurationFieldsStayStrings 区分「时刻」与「时长」。
//
// 时刻是绝对时间点，用 Unix 秒；时长是人要读的量，用 "30m"。
// 两者混在一起的后果是客户端不知道该不该乘以 1000。
func TestDurationFieldsStayStrings(t *testing.T) {
	h := newHarness(t)

	body := []byte(`{"url":"https://example.com/f.xml","interval":"45m"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	signHubRequest(t, req, h.hubPriv, body, "dur-nonce")

	w := h.do(t, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("push = %d: %s", w.Code, w.Body.String())
	}

	var sub map[string]any
	json.Unmarshal(w.Body.Bytes(), &sub)

	// 时长：字符串
	if v, ok := sub["fetch_interval"].(string); !ok || v != "45m" {
		t.Errorf("fetch_interval = %v (%T)，期望字符串 \"45m\"", sub["fetch_interval"], sub["fetch_interval"])
	}
	// 时刻：数字（首推会立刻排期，所以 next_fetch_at 一定有值）
	if _, ok := sub["next_fetch_at"].(float64); !ok {
		t.Errorf("next_fetch_at = %v (%T)，期望 Unix 秒数字", sub["next_fetch_at"], sub["next_fetch_at"])
	}
}

// TestZeroTimeSerializesAsNull 单独锁住 0 的处理。
//
// Go 里 int64 零值容易被当「没有」直接输出成 0，
// 而 0 在 Unix 时间里是 1970-01-01——客户端会把它显示成一个真实时刻。
func TestZeroTimeSerializesAsNull(t *testing.T) {
	if got := ts(0); got != nil {
		t.Errorf("ts(0) = %v，应为 nil", *got)
	}
	if got := ts(1793286035); got == nil || *got != 1793286035 {
		t.Errorf("ts(1793286035) = %v", got)
	}
	if got := optTS(nil); got != nil {
		t.Errorf("optTS(nil) = %v", *got)
	}
	zero := int64(0)
	if got := optTS(&zero); got != nil {
		t.Errorf("optTS(&0) = %v，应为 nil", *got)
	}
	v := int64(1793286035)
	if got := optTS(&v); got == nil || *got != v {
		t.Errorf("optTS(&v) = %v", got)
	}
}

func withBearer(r *http.Request, token string) *http.Request {
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// TestEmptyCollectionsAreArrays 锁住「空集合是 []，不是 null」。
//
// 这不是吹毛求疵：Go 的 nil slice 序列化成 JSON 的 null，
// 客户端拿到 null 后调 .map()/.length 会直接抛 TypeError。
// 这个 bug 在单元测试里不会暴露，因为测试数据总是非空；
// 只有真实跑一遍空库才会看到 "jobs": null。
func TestEmptyCollectionsAreArrays(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login(t)

	cases := []struct {
		path string
		key  string
	}{
		// 全部未创建任何数据，这些列表都应该是空数组。
		{"/api/v1/system/jobs", "jobs"},
		{"/api/v1/pastes", "items"},
		{"/api/v1/feeds", "items"},
		{"/api/v1/entries", "items"},
		{"/api/v1/apps", "apps"},
		{"/api/v1/clients", "items"},
	}

	for _, c := range cases {
		w := h.do(t, withBearer(httptest.NewRequest(http.MethodGet, c.path, nil), token))
		if w.Code != http.StatusOK {
			t.Errorf("%s = %d: %s", c.path, w.Code, w.Body.String())
			continue
		}

		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf("%s: %v", c.path, err)
			continue
		}
		raw, ok := body[c.key]
		if !ok {
			t.Errorf("%s 缺少字段 %q", c.path, c.key)
			continue
		}
		// 必须能从 null 里区分出来。
		if string(raw) == "null" {
			t.Errorf("%s 的 %q 是 null，应为 []", c.path, c.key)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			t.Errorf("%s 的 %q 不是数组: %s", c.path, c.key, raw)
		}
	}
}

// formatInterval 的输出去掉零分量，且必须能被 ParseDuration 原样读回。
//
// 用 Duration.String() 会得到 "45m0s"、"6h0m0s"。宿主把订阅间隔
// 回显给用户时看到的就是这种形状，而文档承诺的是 "45m"。
func TestFormatInterval(t *testing.T) {
	cases := []struct {
		seconds int64
		want    string
	}{
		{0, "0s"},
		{-1, "0s"},
		{30, "30s"},
		{600, "10m"},
		{2700, "45m"},
		{3600, "1h"},
		{5400, "1h30m"},
		{21600, "6h"},
		{604800, "168h"},
		// 非整分钟：秒分量必须保留，不能被静默截断。
		{3661, "1h1m1s"},
	}
	for _, c := range cases {
		got := formatInterval(c.seconds)
		if got != c.want {
			t.Errorf("formatInterval(%d) = %q, want %q", c.seconds, got, c.want)
		}
		if c.seconds > 0 {
			back, err := time.ParseDuration(got)
			if err != nil {
				t.Errorf("ParseDuration(%q) failed: %v", got, err)
				continue
			}
			if int64(back/time.Second) != c.seconds {
				t.Errorf("round-trip of %d gave %d", c.seconds, int64(back/time.Second))
			}
		}
	}
}
