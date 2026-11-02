package rss

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
)

// PNG 头 + 足够长度，让嗅探稳定判成 image/png。
func pngBytes(tag byte) []byte {
	head := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	body := bytes.Repeat([]byte{tag}, 600)
	return append(head, body...)
}

func newTestDownloader(t *testing.T) *Downloader {
	t.Helper()
	dir := t.TempDir()
	blobs, err := blob.Open(blob.Options{
		Root:   filepath.Join(dir, "blobs"),
		TmpDir: filepath.Join(dir, "blobs", "tmp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewDownloader(blobs, DownloadConfig{
		MaxBytes: 1 << 20, UserAgent: "test/1",
		RequestTimeout: 5 * time.Second, Concurrency: 1,
		AllowPrivateHosts: true,
	})
}

// 同一份内容第二次下载不能再被当成新占空间。
// 重试任务每 30 分钟跑一次，无差别累加会让用量单调上漂。
func TestDownloadReportsDedup(t *testing.T) {
	d := newTestDownloader(t)
	body := pngBytes('a')

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	first := d.Download(context.Background(), srv.URL+"/a.png", "", 0)
	if first.Status != "ok" {
		t.Fatalf("first = %q (%s)", first.Status, first.Error)
	}
	if first.Deduped {
		t.Error("首次下载不应标记 Deduped")
	}
	if first.Kind != "image" {
		t.Errorf("Kind = %q, want image", first.Kind)
	}

	second := d.Download(context.Background(), srv.URL+"/a.png", "", 0)
	if second.Status != "ok" {
		t.Fatalf("second = %q (%s)", second.Status, second.Error)
	}
	if !second.Deduped {
		t.Error("相同内容第二次应标记 Deduped，否则重试会重复累加配额")
	}
	if second.SHA256 != first.SHA256 {
		t.Errorf("sha 不一致: %s vs %s", first.SHA256, second.SHA256)
	}
}

// kind 必须跟着真实 MIME 走，不能一律当成图片。
func TestDownloadReportsKind(t *testing.T) {
	d := newTestDownloader(t)

	cases := []struct {
		name    string
		ctype   string
		payload []byte
		want    string
	}{
		{
			name:    "png",
			ctype:   "image/png",
			payload: pngBytes('b'),
			want:    "image",
		},
		{
			name:  "pdf",
			ctype: "application/pdf",
			// %PDF- 魔数 + 足够长度延伸
			payload: append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{'x'}, 600)...),
			want:    "attachment",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				_, _ = w.Write(tc.payload)
			}))
			defer srv.Close()

			res := d.Download(context.Background(), srv.URL+"/f", "", 0)
			if res.Status != "ok" {
				t.Fatalf("status = %q (%s)", res.Status, res.Error)
			}
			if res.Kind != tc.want {
				t.Errorf("Kind = %q, want %q (MIME=%s)", res.Kind, tc.want, res.MIME)
			}
		})
	}
}
