// Package upstream 代理 Homehub 宿主需要的外部资源。
//
// /fetch 与 /avatar 是两条只读通路：前者原样返回订阅源字节，后者原样
// 返回 Gravatar 头像。两者都不解析、不入库——条目入库与去重留在宿主，
// 否则同一份内容会在两个数据库里各存一份，本服务的 entries 表里还会
// 混进它从未订阅过的源。
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/rss"
)

const (
	// FeedResponseLimit 是 /fetch 单次响应的上限。截断的 XML 会给出
	// 误导性的解析错误，所以超出时整份拒绝，不返回半截内容。
	FeedResponseLimit = 32 << 20
	// AvatarResponseLimit 是 /avatar 单次响应的上限。宿主的默认上限是
	// 2MB，这里留到 10MB，覆盖它允许配置的最大值。
	AvatarResponseLimit = 10 << 20

	gravatarBaseURL      = "https://www.gravatar.com/avatar/"
	defaultAvatarSize    = 80
	minAvatarSize        = 16
	maxAvatarSize        = 512
	defaultFeedTimeout   = 20 * time.Second
	defaultAvatarTimeout = 10 * time.Second

	// defaultFeedConcurrency 是 /fetch 同时在途的上游请求数上限。
	// 单次响应最多 32MB 全量读进内存，不设上限时宿主一轮批量刷新
	// 就能把 512M 的进程撑爆。
	defaultFeedConcurrency = 3
	// defaultFeedQueueWait 是名额满时最多排队多久，超出返回 ErrBusy（503）。
	defaultFeedQueueWait = 10 * time.Second
)

var (
	// ErrBlockedHost 与 RSS 媒体下载共用同一个哨兵：代理端点不能因为
	// 入口不同就放宽 SSRF 规则。
	ErrBlockedHost = rss.ErrBlockedHost
	// ErrInvalidURL 表示地址本身不合法（解析失败等）。
	ErrInvalidURL = errors.New("invalid upstream url")
	// ErrInvalidHash 表示头像 hash 不是 32 位小写十六进制。
	ErrInvalidHash = errors.New("invalid avatar hash")
	// ErrInvalidSize 表示 s 参数不是整数。
	ErrInvalidSize = errors.New("invalid avatar size")
	// ErrResponseTooLarge 表示上游响应超过上限。
	ErrResponseTooLarge = errors.New("upstream response too large")
	// ErrUpstream 表示网络错误、超时或其他上游故障。
	ErrUpstream = errors.New("upstream request failed")
	// ErrBusy 表示并发名额已满且排队超时（或调用方 ctx 先结束）。
	ErrBusy = errors.New("upstream proxy busy")
)

var avatarHashPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Config 是两条代理通路的可调项。
type Config struct {
	UserAgent     string
	FeedTimeout   time.Duration
	AvatarTimeout time.Duration
	MaxFeedBytes  int64
	// FeedConcurrency 为 0 时用默认值 3。
	FeedConcurrency int
	// FeedQueueWait 为 0 时用默认值 10s。
	FeedQueueWait time.Duration

	// avatarBaseURL 只在测试里改，生产固定官方 Gravatar。
	avatarBaseURL string
}

// FeedResponse 是 /fetch 需要透传的字段。
type FeedResponse struct {
	Status      int
	ContentType string
	// RetryAfter 透传上游 429 的退避建议，宿主会写进 error_msg。
	RetryAfter string
	Body       []byte
}

// AvatarRequest 是 /avatar 的一次取图请求。
type AvatarRequest struct {
	Hash string
	// Size 为 0 表示使用默认尺寸；调用方仍应先用 ParseAvatarSize 校验 s。
	Size int
	// Default 与 Rating 直接透传给 Gravatar。
	Default string
	Rating  string
	// ETag / LastModified 来自 If-None-Match / If-Modified-Since。
	ETag         string
	LastModified string
	Accept       string
}

// AvatarResponse 是 /avatar 需要透传的字段。
type AvatarResponse struct {
	Status       int
	ContentType  string
	ETag         string
	LastModified string
	CacheControl string
	Body         []byte
}

// Client 是代理端点的出站客户端。并发安全，进程内复用一个实例。
type Client struct {
	cfg        Config
	feedHTTP   *http.Client
	avatarHTTP *http.Client
	// feedSem 是 FetchFeed 的并发信号量。
	feedSem chan struct{}
}

// New 构造生产客户端。
//
// /fetch 走与媒体下载相同的 SSRF 防护：目标地址由调用方给出，拨号期
// 必须拒绝任何解析到内网的地址。
//
// /avatar 是例外，原因不是「它不重要」，而是目标主机由代码固定为
// https 的 Gravatar、hash 已严格校验、重定向不跟随——这三条叠起来，
// 拨号期白名单无法再多挡什么。保留环境代理是因为没有直连出口的部署
// （比如国内 VPS）不配代理时头像永远 502；而代理本身是运维配的，
// 属于可信出口。
func New(cfg Config) *Client {
	return newClient(cfg, rss.NewOutboundTransport(false, 8), avatarTransport())
}

func avatarTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
}

func newClient(cfg Config, feedTransport, avatarTransport http.RoundTripper) *Client {
	if cfg.UserAgent == "" {
		cfg.UserAgent = "HomehubExtension/1.0"
	}
	if cfg.FeedTimeout <= 0 {
		cfg.FeedTimeout = defaultFeedTimeout
	}
	if cfg.AvatarTimeout <= 0 {
		cfg.AvatarTimeout = defaultAvatarTimeout
	}
	if cfg.MaxFeedBytes <= 0 {
		cfg.MaxFeedBytes = FeedResponseLimit
	}
	if cfg.FeedConcurrency <= 0 {
		cfg.FeedConcurrency = defaultFeedConcurrency
	}
	if cfg.FeedQueueWait <= 0 {
		cfg.FeedQueueWait = defaultFeedQueueWait
	}
	if cfg.avatarBaseURL == "" {
		cfg.avatarBaseURL = gravatarBaseURL
	}
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		cfg:     cfg,
		feedSem: make(chan struct{}, cfg.FeedConcurrency),
		feedHTTP: &http.Client{
			Timeout:       cfg.FeedTimeout,
			Transport:     feedTransport,
			CheckRedirect: noRedirect,
		},
		avatarHTTP: &http.Client{
			Timeout:       cfg.AvatarTimeout,
			Transport:     avatarTransport,
			CheckRedirect: noRedirect,
		},
	}
}

// FetchFeed 取回订阅源的原始字节。
//
// 刻意不调用 rss.Service.FetchFeed：那条路径会解析、下载媒体、写
// entries 表。这里只做代理，内容原样交给宿主，由宿主决定怎么入库。
func (c *Client) FetchFeed(ctx context.Context, rawURL string) (FeedResponse, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return FeedResponse{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	// allowPrivate 固定为 false：这条端点本质上是把本服务变成跳板，
	// 谁拿到签名谁就能让它访问任意地址。宿主自己的 allow_private_hosts
	// 是宿主抓取策略，不能成为这里的放宽依据。
	if err := rss.CheckOutboundURL(u, false); err != nil {
		return FeedResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return FeedResponse{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}

	// 校验在前、排队在后：非法地址不该占用名额，也不该等。
	release, err := c.acquireFeed(ctx)
	if err != nil {
		return FeedResponse{}, err
	}
	defer release()

	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", strings.Join([]string{
		"application/rss+xml",
		"application/atom+xml",
		"application/feed+json",
		"application/xml;q=0.9",
		"*/*;q=0.5",
	}, ", "))

	resp, err := c.feedHTTP.Do(req)
	if err != nil {
		// 拨号期的 SSRF 拒绝也是 ErrBlockedHost，必须保持可识别。
		if errors.Is(err, rss.ErrBlockedHost) {
			return FeedResponse{}, err
		}
		return FeedResponse{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	out := FeedResponse{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		RetryAfter:  resp.Header.Get("Retry-After"),
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 非 2xx 不返回上游正文：宿主只需要状态码，正文只会挤占
		// error_msg 的预算。读一小段是为了让连接能被复用。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return out, nil
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		// 标准库只解 gzip 并会删掉这个头；还留着说明服务端无视了
		// Accept-Encoding。把压缩字节当 XML 交给宿主只会得到误导性的
		// 解析错误，这里直接按上游故障处理。
		return out, fmt.Errorf("%w: unexpected content-encoding %q", ErrUpstream, enc)
	}

	body, err := readLimited(resp.Body, c.cfg.MaxFeedBytes)
	if err != nil {
		return out, err
	}
	out.Body = body
	return out, nil
}

// acquireFeed 取一个 /fetch 名额。满时排队，直到拿到名额、ctx 结束
// 或等满 FeedQueueWait；后两种返回 ErrBusy。
func (c *Client) acquireFeed(ctx context.Context) (func(), error) {
	release := func() { <-c.feedSem }
	select {
	case c.feedSem <- struct{}{}:
		return release, nil
	default:
	}
	timer := time.NewTimer(c.cfg.FeedQueueWait)
	defer timer.Stop()
	select {
	case c.feedSem <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrBusy, ctx.Err())
	case <-timer.C:
		return nil, fmt.Errorf("%w: waited %s", ErrBusy, c.cfg.FeedQueueWait)
	}
}

// ValidHash 判断头像 hash 是否严格为 32 位小写十六进制。
//
// 它会拼进上游 URL，所以不能接受路径分隔符、大写或其他长度。
func ValidHash(hash string) bool { return avatarHashPattern.MatchString(hash) }

// ParseAvatarSize 解析 s 参数并夹紧到 16–512。空串使用默认值 80。
func ParseAvatarSize(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultAvatarSize, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidSize, raw)
	}
	return clampAvatarSize(n), nil
}

func clampAvatarSize(n int) int {
	if n < minAvatarSize {
		return minAvatarSize
	}
	if n > maxAvatarSize {
		return maxAvatarSize
	}
	return n
}

// FetchAvatar 取回一张 Gravatar 头像。
//
// 304 与 404 原样返回：前者让宿主复用缓存，后者让宿主写负缓存。
// 其余非 2xx 也原样返回状态码，由 handler 统一映射成 502——上游的
// 状态码对宿主没有意义，它只知道「头像源坏了」。
func (c *Client) FetchAvatar(ctx context.Context, in AvatarRequest) (AvatarResponse, error) {
	if !ValidHash(in.Hash) {
		return AvatarResponse{}, ErrInvalidHash
	}
	size := clampAvatarSize(in.Size)
	if in.Size <= 0 {
		size = defaultAvatarSize
	}

	q := url.Values{}
	q.Set("s", strconv.Itoa(size))
	// 未指定 d 时显式用 mp：未知 hash 也会得到默认头像而不是 404。
	// 宿主的能力探测正是用一个全零 hash、不带参数的请求，404 会被
	// 它判成「端点不存在」。
	style := in.Default
	if style == "" {
		style = "mp"
	}
	q.Set("d", style)
	if in.Rating != "" {
		q.Set("r", in.Rating)
	}

	target := strings.TrimRight(c.cfg.avatarBaseURL, "/") + "/" + in.Hash + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return AvatarResponse{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if in.Accept != "" {
		req.Header.Set("Accept", in.Accept)
	} else {
		req.Header.Set("Accept", "image/png, image/jpeg, image/gif, image/webp")
	}
	if in.ETag != "" {
		req.Header.Set("If-None-Match", in.ETag)
	}
	if in.LastModified != "" {
		req.Header.Set("If-Modified-Since", in.LastModified)
	}

	resp, err := c.avatarHTTP.Do(req)
	if err != nil {
		return AvatarResponse{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	out := AvatarResponse{
		Status:       resp.StatusCode,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		CacheControl: resp.Header.Get("Cache-Control"),
	}
	switch resp.StatusCode {
	case http.StatusNotModified, http.StatusNotFound:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return out, nil
	case http.StatusOK:
		if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
			return out, fmt.Errorf("%w: unexpected content-encoding %q", ErrUpstream, enc)
		}
		body, err := readLimited(resp.Body, AvatarResponseLimit)
		if err != nil {
			return out, err
		}
		out.Body = body
		return out, nil
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return out, nil
	}
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = FeedResponseLimit
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if int64(len(data)) > max {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}
