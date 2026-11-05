package rss

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/mmcdole/gofeed"
)

// Config 是抓取管线的全部可调项，由 config.RSS 映射而来。
type Config struct {
	UserAgent         string
	RequestTimeout    time.Duration
	FetchTimeout      time.Duration
	MaxFeedBytes      int64
	MaxEntryBytes     int64
	MaxBlobBytes      int64
	AllowPrivateHosts bool
	RefererMode       string
	MediaConcurrency  int
	// FetchConcurrency 限制同时进行的 feed 抓取数，对所有触发路径生效：
	// 定时轮询、宿主推送后的首抓、以及手动刷新。
	FetchConcurrency int
	DefaultInterval  time.Duration
	MinInterval      time.Duration
	MaxInterval      time.Duration

	// OnEntryCreated 在新条目落库后触发，用于 SSE 实时推送。
	// 用回调而不是直接依赖 events 包：rss 是领域包，不该知道推送机制。
	OnEntryCreated func(entryID, feedID int64, title string)
	// OnFeedFetched 在一轮抓取结束后触发，无论成功失败。
	OnFeedFetched func(feedID int64, status int, newEntries int, errMsg string)
}

// Service 负责抓取、清洗、媒体本地化与入库。
type Service struct {
	st    store.Store
	blobs *blob.Store
	dl    *Downloader
	cfg   Config
	log   *slog.Logger
	fp    *gofeed.Parser

	// client 是整个进程共用的出站客户端。
	//
	// 之前每个 feed 抓取都 newSafeTransport() 新建一个 Transport，
	// 等于每次抓取都要重新做 TCP + TLS 握手，连接一条也复用不了。
	// 实测抓 10 次建 10 条连接。宿主批量补推订阅时，
	// 这就是 N 次完整握手，在 1 vCPU 上是真实开销。
	client *http.Client

	// fetchSem 限制同时在跑的 feed 抓取数。
	//
	// rss.fetch_concurrency 之前只传给 poller，HTTP 层触发的抓取
	// （宿主 upsert 的异步首抓、宿主/网页的手动刷新）完全不受约束。
	// 宿主对账补推 N 个订阅就能瞬间拉起 N 个 goroutine：实测 20 个
	// 订阅并发抓取时，对目标站就是 20 个并发连接。
	fetchSem chan struct{}
}

// NewService 构造抓取服务。log 为 nil 时用 slog.Default。
func NewService(st store.Store, blobs *blob.Store, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if cfg.FetchConcurrency <= 0 {
		cfg.FetchConcurrency = 2
	}
	return &Service{
		st:    st,
		blobs: blobs,
		dl: NewDownloader(blobs, DownloadConfig{
			MaxBytes:          cfg.MaxBlobBytes,
			UserAgent:         cfg.UserAgent,
			RequestTimeout:    cfg.RequestTimeout,
			Concurrency:       cfg.MediaConcurrency,
			AllowPrivateHosts: cfg.AllowPrivateHosts,
			RefererMode:       cfg.RefererMode,
		}),
		cfg: cfg,
		log: log,
		fp:  gofeed.NewParser(),
		client: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: newSafeTransport(cfg.AllowPrivateHosts, cfg.FetchConcurrency),
			CheckRedirect: func(r *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return checkHost(r.URL, cfg.AllowPrivateHosts)
			},
		},
		fetchSem: make(chan struct{}, cfg.FetchConcurrency),
	}
}

// stateWriteTimeout 是抓取结束后回写状态的独立时限。
//
// 回写不能用抓取的 ctx：FetchTimeout 到期后它已经取消，Update 必然失败，
// next_fetch_at 不前移、error_count 不累加——一个总是超时的源会被
// 调度器每轮重新捞起来，而且永远不会因连续失败被停用。
const stateWriteTimeout = 5 * time.Second

// writeContext 派生一个不随抓取 ctx 取消、但自带时限的写库 ctx。
func writeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
}

// SafeHTTPURL 只接受 http/https 绝对 URL，其他一律返回空串。
//
// 用于会被渲染成链接或当作 Referer 的外部字段（条目链接、站点首页、
// OPML 的 htmlUrl）：javascript:、data:、相对地址都不该进库。
func SafeHTTPURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return ""
	}
	if u.Hostname() == "" {
		return ""
	}
	return u.String()
}

// ingestOutcome 汇报单个条目的处理结果。
//
// 用 struct 而不是 (bool, int, int, error)：三个裸返回值里有两个都是 int，
// 调用方很容易把 mediaOK 与 mediaFail 传反，编译器不会报错。
// 具名字段让「这个 bool 是什么意思」不再需要翻函数体。
type ingestOutcome struct {
	// Created 为 true 表示新条目已入库；false 表示已存在（或未创建）。
	Created bool
	// MediaOK / MediaFail 分别是成功本地化与失败的资源数。
	MediaOK   int
	MediaFail int
}

// fetchedDoc 是一次条件请求的结果。
//
// 同样避免多个裸返回值：status/etag/lastMod 混在一起时，
// 顺序错误只能靠人眼发现。
type fetchedDoc struct {
	Body        []byte
	Status      int
	ETag        string
	LastMod     string
	NotModified bool
}

// Result 汇总一轮抓取的结果，供日志与 API 返回。
type Result struct {
	FeedID      int64  `json:"feed_id"`
	URL         string `json:"url"`
	NotModified bool   `json:"not_modified"`
	NewEntries  int    `json:"new_entries"`
	Skipped     int    `json:"skipped"`
	MediaOK     int    `json:"media_ok"`
	MediaFail   int    `json:"media_fail"`
	Status      int    `json:"status"`
	Error       string `json:"error,omitempty"`
}

// Fetch 抓取一个订阅源。
func (s *Service) Fetch(ctx context.Context, feedID int64) (*Result, error) {
	feed, err := s.st.Feed().Get(ctx, feedID)
	if err != nil {
		return nil, err
	}
	return s.FetchFeed(ctx, feed)
}

// FetchFeed 抓取一个订阅源并入库新条目。
// 单轮的媒体下载在写库之前完成，落库的 content_html 就是最终形态。
func (s *Service) FetchFeed(ctx context.Context, feed *store.Feed) (*Result, error) {
	// 全局抓取闸门。放在最外层而不是各调用点：定时轮询、宿主推送后
	// 的首抓、手动刷新最终都走到这里，一处限制就能覆盖三条路径。
	// 排队受 ctx 约束，调用方自己的超时仍然有效。
	select {
	case s.fetchSem <- struct{}{}:
		defer func() { <-s.fetchSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.FetchTimeout)
	defer cancel()

	res := &Result{FeedID: feed.ID, URL: feed.URL}

	doc, err := s.fetchDocument(ctx, feed)
	now := time.Now()

	if doc.NotModified {
		res.NotModified = true
		res.Status = http.StatusNotModified
		s.markSuccess(ctx, feed, now, doc.Status, doc.ETag, doc.LastMod)
		if s.cfg.OnFeedFetched != nil {
			s.cfg.OnFeedFetched(feed.ID, http.StatusNotModified, 0, "")
		}
		return res, nil
	}
	if err != nil {
		res.Status = doc.Status
		res.Error = err.Error()
		s.markFailure(ctx, feed, now, doc.Status, err)
		if s.cfg.OnFeedFetched != nil {
			s.cfg.OnFeedFetched(feed.ID, doc.Status, 0, err.Error())
		}
		return res, err
	}

	// 直接读字节：ParseString(string(body)) 会把最多 max_feed_bytes 的
	// 正文再复制一份，512MB 的机器上这份拷贝是纯浪费。
	parsed, err := s.fp.Parse(bytes.NewReader(doc.Body))
	if err != nil {
		res.Status = doc.Status
		res.Error = "parse: " + err.Error()
		s.markFailure(ctx, feed, now, doc.Status, err)
		return res, err
	}

	if feed.Title == "" && parsed.Title != "" {
		feed.Title = truncateString(parsed.Title, 200)
	}
	if feed.SiteURL == "" {
		feed.SiteURL = SafeHTTPURL(parsed.Link)
	}

	s.ingestItems(ctx, feed, parsed, now, res)

	res.Status = doc.Status
	s.markSuccess(ctx, feed, now, doc.Status, doc.ETag, doc.LastMod)
	if s.cfg.OnFeedFetched != nil {
		s.cfg.OnFeedFetched(feed.ID, doc.Status, res.NewEntries, "")
	}
	return res, nil
}

// ingestItems 逐条入库一轮解析结果，把计数累加到 res。
//
// 整轮只查一次 SUM(size)：之前每个条目查一次，一个 50 条的源就是
// 50 次聚合查询。本轮的新增用量由 tracker 累加，跨轮、跨 tracker 的
// 并发由 blob.Store 的全局视图兜底。
func (s *Service) ingestItems(ctx context.Context, feed *store.Feed, parsed *gofeed.Feed, now time.Time, res *Result) {
	base := baseURL(parsed, feed)

	used, err := s.st.Blob().TotalSize(ctx)
	if err != nil {
		// 不能把查询失败当成 0 而不留痕：blob.Store 的全局视图仍在兜底，
		// 磁盘余量检查（min_free_bytes）也还在。
		s.log.Warn("blob total size unavailable, quota check degraded",
			"feed_id", feed.ID, "err", err)
	}
	quota := blob.NewQuotaTracker(used)

	for _, item := range parsed.Items {
		if ctx.Err() != nil {
			break
		}
		outcome, err := s.ingestItem(ctx, feed, item, base, now, quota)
		if err != nil {
			s.log.Warn("ingest item failed", "feed_id", feed.ID, "err", err)
			continue
		}
		if outcome.Created {
			res.NewEntries++
		} else {
			res.Skipped++
		}
		res.MediaOK += outcome.MediaOK
		res.MediaFail += outcome.MediaFail
	}
}

// fetchDocument 发条件请求，返回正文与缓存验证器。
func (s *Service) fetchDocument(ctx context.Context, feed *store.Feed) (fetchedDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feed.URL, nil)
	if err != nil {
		return fetchedDoc{}, err
	}
	req.Header.Set("User-Agent", s.cfg.UserAgent)
	req.Header.Set("Accept", strings.Join([]string{
		"application/rss+xml",
		"application/atom+xml",
		"application/feed+json",
		"application/xml;q=0.9",
		"*/*;q=0.5",
	}, ", "))
	// 不要自己设置 Accept-Encoding。net/http 只在自己发起压缩协商时
	// 才透明解压（Transport 的 addedGzip 标记），手写这个头会让 resp.Body
	// 停在压缩字节上，gofeed 拿到的是二进制。交给 Transport 协商：它只
	// 请求 gzip，并且负责解压。
	if feed.Etag != "" {
		req.Header.Set("If-None-Match", feed.Etag)
	}
	if feed.LastModified != "" {
		req.Header.Set("If-Modified-Since", feed.LastModified)
	}

	// 复用 Service 级别的 client：每次抓取新建 Transport 会让
	// 每次请求都重新做 TCP + TLS 握手，连接池永远只有一条连接。
	resp, err := s.client.Do(req)
	if err != nil {
		return fetchedDoc{}, err
	}
	defer resp.Body.Close()

	doc := fetchedDoc{
		Status:  resp.StatusCode,
		ETag:    resp.Header.Get("ETag"),
		LastMod: resp.Header.Get("Last-Modified"),
	}

	if doc.Status == http.StatusNotModified {
		doc.NotModified = true
		return doc, nil
	}
	if doc.Status < 200 || doc.Status >= 300 {
		// 保留响应体前 512 字节便于排查：403 常带一段说明。
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return doc, fmt.Errorf("http %d: %s", doc.Status, sanitizePreview(preview))
	}

	// 标准库解完 gzip 会把 Content-Encoding 删掉。走到这里还留着它，
	// 说明服务端没按协商来（dmhy 的 CDN 会直接回 br）：此时 Body 是
	// 压缩字节，交给 gofeed 只会得到误导性的 "failed to detect feed type"。
	// 必须在读正文前拦住，否则报错指向解析器，而不是真正的原因。
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return doc, fmt.Errorf("unexpected content-encoding %q: server ignored Accept-Encoding negotiation", enc)
	}

	// 多读 1 字节用于判断是否超限。截断的 XML 会给出误导性错误，
	// 所以这里宁可整份拒绝。
	limited := io.LimitReader(resp.Body, s.cfg.MaxFeedBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return doc, err
	}
	if int64(len(data)) > s.cfg.MaxFeedBytes {
		return doc, fmt.Errorf("feed exceeds max_feed_bytes (%d)", s.cfg.MaxFeedBytes)
	}
	doc.Body = data
	return doc, nil
}

func sanitizePreview(b []byte) string {
	var sb strings.Builder
	for _, r := range string(b) {
		if r >= 0x20 && r != 0x7f {
			sb.WriteRune(r)
		} else if r == '\n' || r == '\t' {
			sb.WriteRune(' ')
		}
	}
	return truncateString(strings.TrimSpace(sb.String()), 300)
}

func baseURL(parsed *gofeed.Feed, feed *store.Feed) *url.URL {
	candidates := []string{parsed.Link, feed.SiteURL, feed.URL}
	for _, c := range candidates {
		// base 决定相对地址解析成什么：javascript: 之类的 base 会把
		// 相对 href 变成可执行链接，所以只接受 http/https。
		if c = SafeHTTPURL(c); c == "" {
			continue
		}
		if u, err := url.Parse(c); err == nil {
			return u
		}
	}
	return nil
}

// ingestItem 处理单个条目：查重 → 清洗 → 下载 → 重写 → 入库。
//
// 查重排在最前：已入库的条目不清洗、不下载。否则每轮轮询都会把
// 源里所有旧条目的媒体重新拉一遍（内容寻址只省磁盘，不省带宽）。
//
// 媒体下载在写库之前完成，落库的 content_html 就是最终形态。
// 读者打开时不会触发任何补抓，也不会看到"先原图后占位"的闪烁。
func (s *Service) ingestItem(
	ctx context.Context,
	feed *store.Feed,
	item *gofeed.Item,
	base *url.URL,
	now time.Time,
	quota *blob.QuotaTracker,
) (ingestOutcome, error) {
	guid := itemGUID(item)
	if guid == "" {
		return ingestOutcome{}, errors.New("empty guid")
	}

	exists, err := s.st.Entry().Exists(ctx, feed.ID, guid)
	if err != nil {
		return ingestOutcome{}, fmt.Errorf("check entry exists: %w", err)
	}
	if exists {
		return ingestOutcome{}, nil
	}

	link := SafeHTTPURL(item.Link)
	doc, err := Sanitize(s.itemBody(item), SanitizeOptions{Base: itemBase(base, link)})
	if err != nil {
		return ingestOutcome{}, err
	}

	results := s.dl.DownloadAll(ctx, doc.Targets, s.refererFor(feed, link), quota)

	// 重写必须在 Render 之前：落库的 HTML 就是读者最终看到的形态。
	doc.Rewrite(results)

	rendered, err := doc.Render()
	if err != nil {
		return ingestOutcome{}, err
	}

	entry := buildEntry(feed.ID, guid, item, doc.Text(), rendered, now)
	exists, err = s.st.Entry().InsertIfNew(ctx, entry)
	if err != nil {
		return ingestOutcome{}, err
	}
	if exists {
		return ingestOutcome{}, nil
	}

	if s.cfg.OnEntryCreated != nil {
		s.cfg.OnEntryCreated(entry.ID, entry.FeedID, entry.Title)
	}

	// blob 与 media_fetch 的写入必须在 entry 落库之后：
	// 两者都需要 entry.ID，而且 entry_blob 有外键约束。
	//
	// 用独立的写 ctx：条目已经落库，此时抓取超时也必须把 blob 行写完，
	// 否则文件在盘上却没有 blob 行，孤儿 GC 过了宽限期就会删掉它。
	wctx, cancel := writeContext(ctx)
	defer cancel()
	mediaOK, mediaFail := s.persistMedia(wctx, entry.ID, doc.Targets, results)
	return ingestOutcome{Created: true, MediaOK: mediaOK, MediaFail: mediaFail}, nil
}

// itemBody 取条目正文：content 优先，退回 description，按上限截断。
func (s *Service) itemBody(item *gofeed.Item) string {
	raw := item.Content
	if raw == "" {
		raw = item.Description
	}
	if int64(len(raw)) > s.cfg.MaxEntryBytes {
		raw = raw[:s.cfg.MaxEntryBytes]
	}
	if raw == "" {
		raw = "<p></p>"
	}
	return raw
}

// itemBase 决定相对地址的解析基准：条目自己的链接优先，
// 它通常比站点首页更精确。link 已经过 SafeHTTPURL。
func itemBase(base *url.URL, link string) *url.URL {
	if link != "" {
		if u, err := url.Parse(link); err == nil {
			return u
		}
	}
	return base
}

// refererFor 按 referer_mode 决定下载媒体时带的 Referer。
func (s *Service) refererFor(feed *store.Feed, link string) string {
	switch s.cfg.RefererMode {
	case "auto":
		return link
	case "site":
		return feed.SiteURL
	}
	return ""
}

// buildEntry 把抓取到的条目映射成待入库的值对象。
//
// 独立出来是为了让 ingestItem 的编排骨架保持在一屏内：
// 这里只做字段映射与时间归一，不涉及 IO。
func buildEntry(feedID int64, guid string, item *gofeed.Item, text, html string, now time.Time) *store.Entry {
	// 时间优先级：published > updated > 抓取时刻。
	// 有些源两者都没有，用抓取时刻兜底，至少保证排序稳定。
	published := now.Unix()
	if item.PublishedParsed != nil {
		published = item.PublishedParsed.Unix()
	} else if item.UpdatedParsed != nil {
		published = item.UpdatedParsed.Unix()
	}

	return &store.Entry{
		FeedID:      feedID,
		GUID:        guid,
		URL:         SafeHTTPURL(item.Link),
		Title:       truncateString(strings.TrimSpace(item.Title), 500),
		Author:      authorOf(item),
		PublishedAt: &published,
		FetchedAt:   now.Unix(),
		Summary:     truncateString(text, 280),
		ContentHTML: html,
		ContentText: text,
	}
}

// persistMedia 把下载结果写入 blob 表与 media_fetch。
//
// 失败记录也要落库：media-retry 任务靠它找到可重试的资源。
// 全部写入合并成一个事务：之前每个资源 3 次独立写（3 次提交），
// 一篇 30 张图的文章就是 90 次 fsync。
func (s *Service) persistMedia(
	ctx context.Context,
	entryID int64,
	targets []Target,
	results map[string]MediaResult,
) (ok, fail int) {
	seen := map[string]struct{}{}
	nowSec := time.Now().Unix()

	var blobs []store.Blob
	var records []store.MediaRecord
	for _, t := range targets {
		r, exists := results[t.Resolved]
		if !exists {
			continue
		}
		if _, dup := seen[r.URL]; dup {
			continue
		}
		seen[r.URL] = struct{}{}

		if r.Status == "ok" {
			ok++
			blobs = append(blobs, store.Blob{
				SHA256:     r.SHA256,
				Size:       r.Size,
				MIME:       r.MIME,
				Ext:        r.Ext,
				Kind:       t.Kind,
				CreatedAt:  nowSec,
				LastSeenAt: nowSec,
			})
		} else {
			fail++
		}
		records = append(records, store.MediaRecord{
			EntryID:   entryID,
			SourceURL: r.URL,
			SHA256:    r.SHA256,
			Status:    r.Status,
			Error:     r.Error,
			Attempts:  0,
			UpdatedAt: nowSec,
		})
	}

	if err := s.st.Blob().SaveEntryMedia(ctx, entryID, blobs, records); err != nil {
		s.log.Warn("save entry media failed", "entry", entryID, "err", err)
	}
	return ok, fail
}

func (s *Service) markSuccess(ctx context.Context, feed *store.Feed, now time.Time, status int, etag, lastMod string) {
	ctx, cancel := writeContext(ctx)
	defer cancel()
	feed.LastStatus = status
	feed.LastError = ""
	feed.ErrorCount = 0
	feed.LastFetchAt = ptr(now.Unix())
	feed.NextFetchAt = ptr(now.Add(nextInterval(feed.FetchInterval, 0, s.cfg)).Unix())
	feed.Etag = etag
	feed.LastModified = lastMod
	if err := s.st.Feed().Update(ctx, feed); err != nil {
		s.log.Error("update feed after success", "feed_id", feed.ID, "err", err)
	}
}

func (s *Service) markFailure(ctx context.Context, feed *store.Feed, now time.Time, status int, cause error) {
	ctx, cancel := writeContext(ctx)
	defer cancel()
	feed.LastStatus = status
	feed.LastError = truncateString(cause.Error(), 500)
	feed.ErrorCount++
	feed.LastFetchAt = ptr(now.Unix())
	feed.NextFetchAt = ptr(now.Add(nextInterval(feed.FetchInterval, feed.ErrorCount, s.cfg)).Unix())
	if feed.ErrorCount >= 10 {
		feed.Enabled = false
		feed.LastError = "停用：连续失败 10 次；" + feed.LastError
	}
	if err := s.st.Feed().Update(ctx, feed); err != nil {
		s.log.Error("update feed after failure", "feed_id", feed.ID, "err", err)
	}
}
