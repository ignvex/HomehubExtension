package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/auth"
	"github.com/ignvex/HomehubExtension/internal/scheduler"
)

func decodeMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return m
}

var opsFields = []string{"version", "commit", "build_time", "go_version", "schema_version", "uptime_seconds"}

func TestSystemInfoAnonymousHidesBuildFingerprint(t *testing.T) {
	h := newHarness(t)

	w := h.do(t, httptest.NewRequest(http.MethodGet, "/api/v1/system/info", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("info = %d", w.Code)
	}
	anon := decodeMap(t, w)
	for _, k := range opsFields {
		if _, ok := anon[k]; ok {
			t.Errorf("匿名 /system/info 不应包含 %q: %v", k, anon)
		}
	}
	if anon["public_url"] != "https://hh.example.com" {
		t.Errorf("public_url = %v", anon["public_url"])
	}

	token, _ := h.login(t)
	w = h.doJSON(t, http.MethodGet, "/api/v1/system/info", nil, bearer(token))
	authed := decodeMap(t, w)
	for _, k := range opsFields {
		if _, ok := authed[k]; !ok {
			t.Errorf("登录后 /system/info 应包含 %q: %v", k, authed)
		}
	}
	if authed["version"] != "test" {
		t.Errorf("version = %v", authed["version"])
	}
}

func TestSystemStatsAnonymousMinimal(t *testing.T) {
	h := newHarness(t)

	w := h.do(t, httptest.NewRequest(http.MethodGet, "/api/v1/system/stats", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("stats = %d", w.Code)
	}
	anon := decodeMap(t, w)
	// 首页概览（web/src/views/home.ts）用到的四个字段必须仍在。
	for _, k := range []string{"pastes", "unread", "feeds", "entries"} {
		if _, ok := anon[k]; !ok {
			t.Errorf("匿名 stats 缺少前端必需字段 %q", k)
		}
	}
	for _, k := range []string{"blobs", "blob_bytes", "pending_media", "disk_total", "disk_free"} {
		if _, ok := anon[k]; ok {
			t.Errorf("匿名 stats 不应包含运维字段 %q: %v", k, anon)
		}
	}

	token, _ := h.login(t)
	w = h.doJSON(t, http.MethodGet, "/api/v1/system/stats", nil, bearer(token))
	authed := decodeMap(t, w)
	for _, k := range []string{"pastes", "unread", "feeds", "entries", "blobs", "blob_bytes", "pending_media", "disk_total", "disk_free"} {
		if _, ok := authed[k]; !ok {
			t.Errorf("登录后 stats 应包含 %q: %v", k, authed)
		}
	}
}

func TestReadyzOmitsFreeBytes(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("readyz = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "free_bytes") {
		t.Errorf("/readyz 不应回显 free_bytes: %s", w.Body.String())
	}
	body := decodeMap(t, w)
	checks, _ := body["checks"].(map[string]any)
	if checks["storage"] != "ok" {
		t.Errorf("storage check = %v", checks["storage"])
	}
}

// withScheduler 用带调度器的 Deps 重建路由。
func (h *harness) withScheduler(s *scheduler.Scheduler) {
	h.router = NewRouter(Deps{
		Cfg: h.cfg, Store: h.db, Blobs: h.blobs, Auth: h.svc,
		HubVerifier: auth.NewHubVerifier(h.hubPub, time.Minute, time.Minute),
		Paste:       h.pasteSvc, RSS: h.rssSvc, Log: nil, Bus: h.bus,
		Scheduler: s,
		Build:     BuildInfo{Version: "test"},
		Started:   time.Now(),
	})
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) (code, msg string) {
	t.Helper()
	var body ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return body.Error.Code, body.Error.Message
}

func TestRunJobErrorsDoNotLeakDetails(t *testing.T) {
	h := newHarness(t)
	sched := scheduler.New(h.db.Job(), time.UTC, nil)
	const secret = "/var/lib/hhe/secret-path: database is locked"
	sched.Register(scheduler.Task{Name: "boom", Run: func(ctx context.Context, _ map[string]any) (string, error) {
		return "", errors.New(secret)
	}})
	release := make(chan struct{})
	started := make(chan struct{})
	sched.Register(scheduler.Task{Name: "slow", Run: func(ctx context.Context, _ map[string]any) (string, error) {
		close(started)
		<-release
		return "done", nil
	}})
	h.withScheduler(sched)
	token, _ := h.login(t)

	t.Run("unknown job is 404", func(t *testing.T) {
		w := h.doJSON(t, http.MethodPost, "/api/v1/system/jobs/nope/run", nil, bearer(token))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		if code, msg := errorCode(t, w); code != "not_found" || strings.Contains(msg, "nope") {
			t.Errorf("code=%q msg=%q", code, msg)
		}
	})

	t.Run("task failure is generic 500", func(t *testing.T) {
		w := h.doJSON(t, http.MethodPost, "/api/v1/system/jobs/boom/run", nil, bearer(token))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret-path") || strings.Contains(w.Body.String(), "locked") {
			t.Errorf("响应泄露了内部错误: %s", w.Body.String())
		}
		if code, _ := errorCode(t, w); code != "internal" {
			t.Errorf("code = %q", code)
		}
	})

	t.Run("already running is 409", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.doJSON(t, http.MethodPost, "/api/v1/system/jobs/slow/run", nil, bearer(token))
		}()
		<-started
		w := h.doJSON(t, http.MethodPost, "/api/v1/system/jobs/slow/run", nil, bearer(token))
		close(release)
		<-done
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		if code, _ := errorCode(t, w); code != "conflict" {
			t.Errorf("code = %q", code)
		}
	})
}
