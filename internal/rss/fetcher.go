package rss

import (
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
	DefaultInterval   time.Duration
	MinInterval       time.Duration
	MaxInterval       time.Duration

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
}

// NewService 构造抓取服务。log 为 nil 时用 slog.Default。
func NewService(st store.Store, blobs *blob.Store, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
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
	}
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

	parsed, err := s.fp.ParseString(string(doc.Body))
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
		feed.SiteURL = parsed.Link
	}

	base := baseURL(parsed, feed)
	for _, item := range parsed.Items {
		if ctx.Err() != nil {
			break
		}
		outcome, err := s.ingestItem(ctx, feed, item, base, now)
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

	res.Status = doc.Status
	s.markSuccess(ctx, feed, now, doc.Status, doc.ETag, doc.LastMod)
	if s.cfg.OnFeedFetched != nil {
		s.cfg.OnFeedFetched(feed.ID, doc.Status, res.NewEntries, "")
	}
	return res, nil
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

	client := &http.Client{
		Timeout:   s.cfg.RequestTimeout,
		Transport: newSafeTransport(s.cfg.AllowPrivateHosts, 2),
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return checkHost(r.URL, s.cfg.AllowPrivateHosts)
		},
	}

	resp, err := client.Do(req)
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
		if c == "" {
			continue
		}
		if u, err := url.Parse(c); err == nil && u.Scheme != "" {
			return u
		}
	}
	return nil
}

// ingestItem 处理单个条目：清洗 → 下载 → 重写 → 入库。
//
// 媒体下载在写库之前完成，落库的 content_html 就是最终形态。
// 读者打开时不会触发任何补抓，也不会看到"先原图后占位"的闪烁。
func (s *Service) ingestItem(
	ctx context.Context,
	feed *store.Feed,
	item *gofeed.Item,
	base *url.URL,
	now time.Time,
) (ingestOutcome, error) {
	guid := itemGUID(item)
	if guid == "" {
		return ingestOutcome{}, errors.New("empty guid")
	}

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

	// 条目自己的链接优先作为 base：它通常比站点首页更精确。
	itemBase := base
	if item.Link != "" {
		if u, err := url.Parse(item.Link); err == nil && u.Scheme != "" {
			itemBase = u
		}
	}

	doc, err := Sanitize(raw, SanitizeOptions{Base: itemBase})
	if err != nil {
		return ingestOutcome{}, err
	}

	referer := ""
	switch s.cfg.RefererMode {
	case "auto":
		referer = item.Link
	case "site":
		referer = feed.SiteURL
	}

	used, err := s.st.Blob().TotalSize(ctx)
	if err != nil {
		// 不能把查询失败当成 0：那等于关掉配额，让一次抓取无限写盘。
		// 保留 0 但留下日志，磁盘余量检查（min_free_bytes）仍在兜底。
		s.log.Warn("blob total size unavailable, quota check degraded",
			"feed_id", feed.ID, "err", err)
	}
	results := s.dl.DownloadAll(ctx, doc.Targets, referer, used)

	// 重写必须在 Render 之前：落库的 HTML 就是读者最终看到的形态。
	doc.Rewrite(results)

	rendered, err := doc.Render()
	if err != nil {
		return ingestOutcome{}, err
	}

	entry := buildEntry(feed.ID, guid, item, doc.Text(), rendered, now)
	exists, err := s.st.Entry().InsertIfNew(ctx, entry)
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
	mediaOK, mediaFail := s.persistMedia(ctx, entry.ID, doc.Targets, results)
	return ingestOutcome{Created: true, MediaOK: mediaOK, MediaFail: mediaFail}, nil
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
		URL:         item.Link,
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
func (s *Service) persistMedia(
	ctx context.Context,
	entryID int64,
	targets []Target,
	results map[string]MediaResult,
) (ok, fail int) {
	seen := map[string]struct{}{}
	nowSec := time.Now().Unix()

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
			if err := s.st.Blob().Upsert(ctx, &store.Blob{
				SHA256:     r.SHA256,
				Size:       r.Size,
				MIME:       r.MIME,
				Ext:        r.Ext,
				Kind:       t.Kind,
				CreatedAt:  nowSec,
				LastSeenAt: nowSec,
			}); err != nil {
				s.log.Warn("upsert blob failed", "sha", r.SHA256, "err", err)
			}
			if err := s.st.Blob().Link(ctx, entryID, r.SHA256); err != nil {
				s.log.Warn("link blob failed", "entry", entryID, "sha", r.SHA256, "err", err)
			}
		} else {
			fail++
		}

		if err := s.st.Media().Record(ctx, &store.MediaRecord{
			EntryID:   entryID,
			SourceURL: r.URL,
			SHA256:    r.SHA256,
			Status:    r.Status,
			Error:     r.Error,
			Attempts:  0,
			UpdatedAt: nowSec,
		}); err != nil {
			s.log.Warn("record media failed", "entry", entryID, "url", r.URL, "err", err)
		}
	}
	return ok, fail
}

func (s *Service) markSuccess(ctx context.Context, feed *store.Feed, now time.Time, status int, etag, lastMod string) {
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
