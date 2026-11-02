package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// countingReader 记录被读走的字节数。
// 只断言状态码不够：paste 层自己也会在读完 body 后返回 413，
// 两种实现的响应完全一样，区别在「内存里装了多少」。
type countingReader struct {
	r io.Reader
	n *int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// chunkedReader 让 httptest 请求表现为「长度未知」的流。
type chunkedReader struct{ r io.Reader }

func (c chunkedReader) Read(p []byte) (int, error) { return c.r.Read(p) }

// 只检查 Content-Length 是不足的：chunked 编码下它是 -1，
// 判断整个被绕过。handler 会把 body 全部读进内存。
//
// 用一个「合法但超大」的 JSON 验证：内容形状正确，所以解析器不会
// 提前失败，会一直读到流的末尾——正是内存耗尽所需的条件。
//
// 断言的是「实际读取量」，而不是状态码：paste 层自己也会在读完
// 之后返回 413，两种实现的响应一模一样，区别在于进程的峰值内存。
func TestChunkedBodyIsLimited(t *testing.T) {
	h := newHarness(t)
	limit := h.cfg.Server.MaxBodyBytes
	if limit <= 0 {
		t.Skip("未配置请求体上限")
	}

	// 构造一个结构合法、体积为上限 20 倍的 JSON。
	oversized := int(limit * 20)
	var buf bytes.Buffer
	buf.WriteString(`{"body":"`)
	buf.WriteString(strings.Repeat("a", oversized))
	buf.WriteString(`"}`)

	var consumed int64
	body := countingReader{r: chunkedReader{bytes.NewReader(buf.Bytes())}, n: &consumed}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/pastes", body)
	req.ContentLength = -1 // 关键：模拟 chunked
	req.Header.Set("Content-Type", "application/json")

	w := h.do(t, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413: %s", w.Code, strings.TrimSpace(w.Body.String()))
	}

	// 核心断言：最多被读走 limit 加上一两个读块的余量。
	// MaxBytesReader 会在越过上限时让下一次 Read 返回错误，
	// 多读的量不超过一个缓冲区（默认 512 字节，实际可能更大）。
	slack := int64(64 << 10)
	if consumed > limit+slack {
		t.Errorf("body 被读了 %d 字节，上限 %d（多读 %d，超过 %d 容忍量）——读取层没有封顶",
			consumed, limit, consumed-limit, slack)
	}
}

// 声明了 Content-Length 的超大请求走快速拒绝路径。
func TestDeclaredLengthBodyIsRejected(t *testing.T) {
	h := newHarness(t)
	limit := h.cfg.Server.MaxBodyBytes

	req := httptest.NewRequest(http.MethodPost, "/api/v1/pastes",
		strings.NewReader(strings.Repeat("a", int(limit*2))))
	req.Header.Set("Content-Type", "application/json")

	w := h.do(t, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

// 正常大小的请求不能被误伤。
func TestNormalBodyStillAccepted(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/pastes",
		strings.NewReader(`{"body":"hello","visibility":"unlisted"}`))
	req.Header.Set("Content-Type", "application/json")

	w := h.do(t, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
}
