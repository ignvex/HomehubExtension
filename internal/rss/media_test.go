package rss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
)

// 媒体下载显式声明 Accept-Encoding: identity，为的是不把压缩流
// 解成远超 Content-Length 的内容（解压炸弹）。但 CDN 可以无视它。
// 真遇到时必须在嗅探之前拦下：压缩字节喂给 DetectContentType 会
// 判成 unsupported，把一个传输层问题伪装成“类型不支持”。
func TestDownloadRejectsUnrequestedEncoding(t *testing.T) {
	dir := t.TempDir()
	blobs, err := blob.Open(blob.Options{
		Root:   filepath.Join(dir, "blobs"),
		TmpDir: filepath.Join(dir, "blobs", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", got)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte("\x1f\x8b\x08\x00compressed-not-a-png"))
	}))
	defer srv.Close()

	d := NewDownloader(blobs, DownloadConfig{
		MaxBytes:          1 << 20,
		UserAgent:         "test/1",
		RequestTimeout:    5 * time.Second,
		Concurrency:       1,
		AllowPrivateHosts: true,
	})

	res := d.Download(context.Background(), srv.URL+"/a.png", "", 0)
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed（err=%q）", res.Status, res.Error)
	}
	if !strings.Contains(res.Error, "content-encoding") {
		t.Errorf("错误应指明编码问题，got: %q", res.Error)
	}
	if !strings.Contains(res.Error, "gzip") {
		t.Errorf("错误应带出具体编码名，got: %q", res.Error)
	}
}
