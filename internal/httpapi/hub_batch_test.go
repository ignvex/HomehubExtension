package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHubBatchUpsert(t *testing.T) {
	h := newHarness(t)

	body := []byte(`{"items":[
	  {"url":"https://b1.example.com/f.xml","title":"B1"},
	  {"url":"https://b2.example.com/f.xml","title":"B2","interval":"2h"},
	  {"url":"http://127.0.0.1/blocked.xml"},
	  {"url":"https://b3.example.com/f.xml","interval":"bogus"}
	]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	signHubRequest(t, req, h.hubPriv, body, "nonce-batch-1")

	w := h.do(t, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Created int `json:"created"`
		Failed  []struct {
			URL string `json:"url"`
		} `json:"failed"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 4 {
		t.Errorf("total = %d, want 4", resp.Total)
	}
	if resp.Created != 2 {
		t.Errorf("created = %d, want 2（内网地址与坏 interval 应被拒）", resp.Created)
	}
	if len(resp.Failed) != 2 {
		t.Errorf("failed = %d 条, want 2", len(resp.Failed))
	}

	// 再推一次同样的两条：应当是更新而不是新建。
	body2 := []byte(`{"items":[
	  {"url":"https://b1.example.com/f.xml","title":"B1 renaming"},
	  {"url":"https://b2.example.com/f.xml"}
	]}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions/batch", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	signHubRequest(t, req2, h.hubPriv, body2, "nonce-batch-2")
	w = h.do(t, req2)

	var resp2 struct {
		Created   int `json:"created"`
		Updated   int `json:"updated"`
		Unchanged int `json:"unchanged"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp2)
	if resp2.Created != 0 {
		t.Errorf("created = %d, want 0（重复推送是更新）", resp2.Created)
	}
	if resp2.Updated != 1 {
		t.Errorf("updated = %d, want 1（只改了 B1 的标题）", resp2.Updated)
	}
	if resp2.Unchanged != 1 {
		t.Errorf("unchanged = %d, want 1（B2 什么都没改）", resp2.Unchanged)
	}
}

// 签名通道上的端点一律要求签名。
func TestHubBatchRequiresSignature(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/subscriptions/batch",
		strings.NewReader(`{"items":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := h.do(t, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401（缺签名）", w.Code)
	}
}
