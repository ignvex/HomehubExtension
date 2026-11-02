package rss

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
)

var (
	ErrBlockedHost = errors.New("blocked host")
	ErrUnsupported = errors.New("unsupported media type")
)

// blockedPrefixes 覆盖 net/netip 的 IsPrivate 之外还需要单独拦的网段。
// 注意 IsPrivate 只覆盖 RFC1918 与 ULA，不能单独用它做判断。
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("255.255.255.255/32"),
}

// allowedIP 判断一个解析结果能否作为出站目标。
func allowedIP(ip netip.Addr, allowPrivate bool) bool {
	if !ip.IsValid() {
		return false
	}
	// 4in6（::ffff:127.0.0.1）必须先 Unmap，否则是一个现成的绕过。
	ip = ip.Unmap()

	// 这四类永远拒绝，与开关无关：
	//   - 未指定地址（0.0.0.0 / ::）没有合法的连接语义；
	//   - 组播不是主机目标；
	//   - 链路本地含云元数据 169.254.169.254，放开等于把凭据交给任意 feed。
	if ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return false
	}

	// 开关打开时，回环与 RFC1918/ULA 一并放开：
	// 这正是"订阅自家 NAS 上的 feed"的场景，只放开内网而不放开回环没意义。
	if allowPrivate {
		return true
	}

	if ip.IsLoopback() || ip.IsPrivate() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// DownloadConfig 控制下载行为与并发。
type DownloadConfig struct {
	MaxBytes          int64
	UserAgent         string
	RequestTimeout    time.Duration
	Concurrency       int
	AllowPrivateHosts bool
	RefererMode       string
}

// Downloader 负责把远程媒体抓回本地并落入内容寻址存储。
type Downloader struct {
	client *http.Client
	blobs  *blob.Store
	cfg    DownloadConfig
	sem    chan struct{}
}

// MediaResult 描述一次下载的结果。Status 取值与 media_fetch.status 一致。
type MediaResult struct {
	URL    string
	SHA256 string
	Ext    string
	MIME   string
	Kind   string
	Size   int64
	// Deduped 表示内容已在库里，本次没有新占磁盘。
	//
	// 配额累加必须看它：已存在的 blob 早在 SUM(size) 里算过一次，
	// 重试路径若不加区分地再累一次，用量会随重试次数单调上漂，
	// 最终把本还有余量的存储判成超配额。
	Deduped bool
	// Status 取值与 media_fetch.status 一致。
	Status string
	Error  string
}

// NewDownloader 构造下载器。并发上限同时作用于单次抓取与全局。
func NewDownloader(blobs *blob.Store, cfg DownloadConfig) *Downloader {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 3
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 20 * time.Second
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 50 << 20
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "HomehubExtension/1.0"
	}
	return &Downloader{
		client: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: newSafeTransport(cfg.AllowPrivateHosts, cfg.Concurrency),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				// 每一跳都重新检查 host：首跳合法、二跳指向内网是最常见的绕过。
				if err := checkHost(req.URL, cfg.AllowPrivateHosts); err != nil {
					return err
				}
				return nil
			},
		},
		blobs: blobs,
		cfg:   cfg,
		sem:   make(chan struct{}, cfg.Concurrency),
	}
}

func newSafeTransport(allowPrivate bool, conns int) *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if conns < 2 {
		conns = 2
	}
	return &http.Transport{
		Proxy:                 nil, // 不走环境代理：代理会让 SSRF 检查失去意义
		MaxIdleConns:          conns * 2,
		MaxIdleConnsPerHost:   conns,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
		// 在拨号时直接连已校验的 IP，从校验到连接之间不存在第二次 DNS 解析，
		// 这是防 DNS rebinding 的关键；SNI 与 Host 头仍由 Transport 按原 URL 设置。
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no address for %s", host)
			}
			var chosen netip.Addr
			for _, ip := range ips {
				a, ok := netip.AddrFromSlice(ip.IP)
				if !ok {
					continue
				}
				if !allowedIP(a, allowPrivate) {
					return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlockedHost, host, ip.IP)
				}
				if !chosen.IsValid() {
					chosen = a.Unmap()
				}
			}
			if !chosen.IsValid() {
				return nil, fmt.Errorf("%w: %s", ErrBlockedHost, host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(chosen.String(), port))
		},
	}
}

func checkHost(u *url.URL, allowPrivate bool) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q", ErrBlockedHost, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("%w: credentials in URL", ErrBlockedHost)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrBlockedHost)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !allowedIP(ip, allowPrivate) {
			return fmt.Errorf("%w: %s", ErrBlockedHost, host)
		}
	}
	// 端口白名单只在默认（防 SSRF）模式下生效。
	// 既然用户显式允许了内网地址，端口限制就没有意义了——
	// 自家 NAS 上的服务很少跑在 80/443。
	if !allowPrivate {
		if port := u.Port(); port != "" {
			switch port {
			case "80", "443", "8080", "8443":
			default:
				return fmt.Errorf("%w: port %s", ErrBlockedHost, port)
			}
		}
	}
	return nil
}

// Download 抓取一个资源并落入 blob 存储。
//
// 整个过程流式：先读 512 字节嗅探类型，再把这 512 字节与剩余流拼起来交给
// blob.Put，峰值内存只有几十 KB。
//
// 单次下载没有批次可共享，因此不传 QuotaTracker：并发安全由
// Store 内部的锁保证，但多次调用之间的用量不会互相累加。
// 需要批量下载时用 DownloadAll。
func (d *Downloader) Download(ctx context.Context, rawURL, referer string, used int64) MediaResult {
	return d.download(ctx, rawURL, referer, used, nil)
}

func (d *Downloader) download(
	ctx context.Context, rawURL, referer string, used int64, quota *blob.QuotaTracker,
) MediaResult {
	res := MediaResult{URL: rawURL}

	// data: URI 是内联内容，没有 host 可检查、也不需要网络。
	// 解码后走同一条 blob 写入路径，而不是原样留在 HTML 里：
	//   - base64 比原始字节大 33%，一篇图多的文章会把 content_html 撑到几 MB；
	//   - 转为内容寻址后，相同的内联图在多篇文章之间自动去重。
	if raw, declaredMIME, ok := decodeDataURI(rawURL); ok {
		return d.storeInline(ctx, res, raw, declaredMIME, used, quota)
	}

	if err := d.preflight(rawURL); err != nil {
		res.Status, res.Error = "blocked", err.Error()
		return res
	}

	// 全局并发闸门。放在请求之前而不是之后：带宽是稀缺资源，
	// 排队请求本身也会占用连接。
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-ctx.Done():
		res.Status, res.Error = "failed", "canceled"
		return res
	}

	resp, err := d.fetch(ctx, rawURL, referer)
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		return res
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Status, res.Error = "failed", fmt.Sprintf("http %d", resp.StatusCode)
		return res
	}
	if resp.ContentLength > d.cfg.MaxBytes {
		res.Status, res.Error = "too_large", fmt.Sprintf("content-length %d", resp.ContentLength)
		return res
	}

	// 请求时明确声明了 identity，但 CDN 仍可能无视它回压缩体。
	// 此时下面嗅探到的是压缩字节，必然误判类型；而解压又要承担
	// 解压炸弹风险（这正是 identity 的目的）。所以直接判失败，
	// 并把编码名写进错误，让排查不必从 "unsupported" 猜起。
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		res.Status = "failed"
		res.Error = fmt.Sprintf("unexpected content-encoding %q (requested identity)", enc)
		return res
	}

	return d.sniffAndStore(ctx, res, resp, used, quota)
}

// preflight 校验 URL 的 scheme、host 与端口。
func (d *Downloader) preflight(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("invalid url")
	}
	return checkHost(u, d.cfg.AllowPrivateHosts)
}

// fetch 发一次带完整头的 GET。
func (d *Downloader) fetch(ctx context.Context, rawURL, referer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", d.cfg.UserAgent)
	// identity 避免压缩炸弹：解压后的体积可能远超 Content-Length。
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Accept", "image/*,video/*,audio/*,*/*;q=0.5")
	if referer != "" && d.cfg.RefererMode != "none" {
		req.Header.Set("Referer", referer)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, errors.New(truncErr(err))
	}
	return resp, nil
}

// sniffAndStore 嗅探前 512 字节判断真实类型，再流式写入 blob。
//
// 以嗅探结果为准、响应头只作参考：服务端声称 image/png 而内容是 HTML
// 是常见的内容注入手法。
func (d *Downloader) sniffAndStore(
	ctx context.Context, res MediaResult, resp *http.Response, used int64, quota *blob.QuotaTracker,
) MediaResult {
	head := make([]byte, 512)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		res.Status, res.Error = "failed", err.Error()
		return res
	}
	head = head[:n]

	sniffed := http.DetectContentType(head)
	ext, kind, ok := extForMIME(sniffed)
	if !ok {
		res.Status, res.Error = "unsupported", sniffed
		return res
	}
	res.MIME = sniffed

	// 嗅探用的 512 字节要拼回数据流的开头，否则文件开头会缺一段。
	body := io.MultiReader(bytes.NewReader(head), resp.Body)
	put, err := d.blobs.Put(ctx, body, blob.PutOptions{Ext: ext, Used: used, Quota: quota})
	if err != nil {
		res.Status, res.Error = classifyPutError(err)
		return res
	}

	res.Status = "ok"
	res.SHA256 = put.SHA256
	res.Ext = ext
	res.Kind = kind
	res.Size = put.Size
	res.Deduped = put.Existed
	return res
}

// classifyPutError 把 blob 层的错误映射成 media_fetch.status。
func classifyPutError(err error) (status, msg string) {
	switch {
	case errors.Is(err, blob.ErrTooLarge):
		return "too_large", err.Error()
	case errors.Is(err, blob.ErrQuotaExceeded), errors.Is(err, blob.ErrNoSpace):
		return "quota_exceeded", err.Error()
	default:
		return "failed", truncErr(err)
	}
}

// storeInline 把内联（data:）内容写入内容寻址存储。
//
// 类型判定以嗅探结果为准，不信 data URI 里声明的 MIME——
// 那段文本是内容作者写的，可以随便撒谎，而我们随后会把它当作
// Content-Type 返回给浏览器。
func (d *Downloader) storeInline(
	ctx context.Context,
	res MediaResult,
	raw []byte,
	declaredMIME string,
	used int64,
	quota *blob.QuotaTracker,
) MediaResult {
	if len(raw) == 0 {
		res.Status, res.Error = "unsupported", "empty inline data"
		return res
	}
	if int64(len(raw)) > d.cfg.MaxBytes {
		res.Status = "too_large"
		res.Error = fmt.Sprintf("inline %d bytes", len(raw))
		return res
	}

	// 与 HTTP 路径一致：先嗅探，再查白名单。
	sniffed := detectInline(raw)
	ext, kind, ok := extForMIME(sniffed)
	if !ok {
		res.Status, res.Error = "unsupported", sniffed
		return res
	}

	put, err := d.blobs.Put(ctx, bytes.NewReader(raw), blob.PutOptions{Ext: ext, Used: used, Quota: quota})
	if err != nil {
		switch {
		case errors.Is(err, blob.ErrTooLarge):
			res.Status, res.Error = "too_large", err.Error()
		case errors.Is(err, blob.ErrQuotaExceeded), errors.Is(err, blob.ErrNoSpace):
			res.Status, res.Error = "quota_exceeded", err.Error()
		default:
			res.Status, res.Error = "failed", truncErr(err)
		}
		return res
	}

	res.Status = "ok"
	res.SHA256 = put.SHA256
	res.Ext = ext
	res.MIME = sniffed
	res.Kind = kind
	res.Size = put.Size
	res.Deduped = put.Existed
	return res
}

// detectInline 嗅探内联数据的类型。
// http.DetectContentType 对短于 512 字节的输入也能给结论，
// 而 data URI 里常见的就是几十字节的小图标。
func detectInline(raw []byte) string {
	head := raw
	if len(head) > 512 {
		head = head[:512]
	}
	return http.DetectContentType(head)
}

// extForMIME 是唯一的类型白名单。不在表里的一律拒绝——
// 拒绝比"尽力保存"安全：任何可执行的类型都不该进 /media。
func extForMIME(mime string) (ext, kind string, ok bool) {
	switch mime {
	case "image/jpeg":
		return "jpg", "image", true
	case "image/png":
		return "png", "image", true
	case "image/gif":
		return "gif", "image", true
	case "image/webp":
		return "webp", "image", true
	case "image/avif":
		return "avif", "image", true
	case "image/svg+xml":
		return "svg", "image", true
	case "audio/mpeg":
		return "mp3", "audio", true
	case "audio/mp4":
		return "m4a", "audio", true
	case "audio/ogg":
		return "ogg", "audio", true
	case "audio/wav", "audio/x-wav":
		return "wav", "audio", true
	case "video/mp4":
		return "mp4", "video", true
	case "video/webm":
		return "webm", "video", true
	case "video/ogg":
		return "ogv", "video", true
	case "font/woff2", "application/font-woff2":
		return "woff2", "font", true
	case "font/woff", "application/font-woff":
		return "woff", "font", true
	case "application/pdf":
		return "pdf", "attachment", true
	case "application/epub+zip":
		return "epub", "attachment", true
	}
	return "", "", false
}

func truncErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// DownloadAll 并发下载一组资源，返回 URL → 结果。
// 同一 URL 只下载一次。
//
// 所有并发写入共享一个 QuotaTracker：否则每个 goroutine 都拿调用方
// 传入的同一份旧快照去检查配额，各自都能通过，合计可写入 N 倍配额。
func (d *Downloader) DownloadAll(
	ctx context.Context,
	targets []Target,
	referer string,
	used int64,
) map[string]MediaResult {
	out := make(map[string]MediaResult)
	var mu sync.Mutex
	seen := make(map[string]struct{})
	var wg sync.WaitGroup

	quota := blob.NewQuotaTracker(used)

	for _, t := range targets {
		url := t.Resolved
		if url == "" {
			continue
		}
		mu.Lock()
		if _, dup := seen[url]; dup {
			mu.Unlock()
			continue
		}
		seen[url] = struct{}{}
		mu.Unlock()

		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			r := d.download(ctx, u, referer, used, quota)
			mu.Lock()
			out[u] = r
			mu.Unlock()
		}(url)
	}
	wg.Wait()
	return out
}

// ValidateURL 校验一个订阅地址是否可抓。
//
// 导出给 HTTP 层用：网页端新建订阅源时必须在入库前挡住内网地址，
// 否则服务就成了一个开放的 SSRF 跳板——匿名用户能让它去访问
// 家庭内网的任意地址并把响应内容读回来。
//
// 策略与媒体下载共用同一套判断（checkHost），避免两处规则漂移。
func (s *Service) ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: 无法解析", ErrBlockedHost)
	}
	return checkHost(u, s.cfg.AllowPrivateHosts)
}
