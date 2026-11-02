package auth

import (
	"net/http"
	"strings"
	"testing"
)

// 宿主侧用 strings.ToUpper(method) 参与签名。Extension 侧不规范化的话，
// 换一个不自动大写的客户端库就会算出不同的待签消息，表现为 401。
func TestRequestMessageNormalizesMethodCase(t *testing.T) {
	build := func(method string) *http.Request {
		req, err := http.NewRequest(method, "https://example.com/api/v1/fetch?url=x", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-HH-Timestamp", "1793251200")
		req.Header.Set("X-HH-Nonce", "test-nonce")
		return req
	}

	lower := build("get")
	if lower.Method != "get" {
		t.Fatalf("test setup: http.NewRequest uppercased the method to %q", lower.Method)
	}
	upperMsg := RequestMessage(build("GET"), nil)
	lowerMsg := RequestMessage(lower, nil)
	if lowerMsg != upperMsg {
		t.Fatalf("method case changed the signed message:\nlower=%q\nupper=%q", lowerMsg, upperMsg)
	}
	if !strings.Contains(lowerMsg, "\nGET\n") {
		t.Errorf("signed message should contain the uppercase method: %q", lowerMsg)
	}
}
