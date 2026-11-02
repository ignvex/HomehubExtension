package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/gin-gonic/gin"
)

// 宿主读内容的端点。
//
// 在此之前 /hub/* 只有 subscriptions 与 keys：Homehub 能推订阅、能看
// 抓取状态，但一条条目都读不到。宿主若想自己渲染阅读界面（而不是
// 甩一个链接跳到 HHE 的网页），没有可用的数据源。
//
// 这里刻意复用网页端 handleEntryList 的查询语义，只在两处不同：
//   - content_html 里的媒体地址被改写成绝对 URL（宿主不在我们的
//     源上，相对路径会 404）；
//   - 不自动标记已读。宿主可能只是预取列表，标记已读是用户行为。

// maxHubEntryLimit 比网页端小：宿主一次拉 500 条正文会让响应达到
// 几十 MB，而宿主真正需要的是「最新一屏」。
const maxHubEntryLimit = 50

type hubEntry struct {
	ID          int64  `json:"id"`
	FeedID      int64  `json:"feed_id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Author      string `json:"author"`
	Summary     string `json:"summary"`
	PublishedAt *int64 `json:"published_at"`
	FetchedAt   *int64 `json:"fetched_at"`
	ReadAt      *int64 `json:"read_at"`
	StarredAt   *int64 `json:"starred_at"`
	// ContentHTML 已把 /media/... 改写成绝对地址，宿主可直接渲染。
	ContentHTML string `json:"content_html"`
}

// absolutizeMedia 把正文里的站内媒体路径补成绝对 URL。
//
// 只替换我们自己生成的 `/media/<sha>.<ext>` 前缀：清洗阶段写入的
// 正文里，媒体地址只有这一种形态，其它 URL 在落库前就已被移除。
// 用前缀替换而不是解析 HTML，避免为了改几个属性再走一遍解析器。
func absolutizeMedia(html, base string) string {
	if base == "" || html == "" {
		return html
	}
	base = strings.TrimRight(base, "/")
	// 覆盖 src / srcset / poster 三种承载媒体的属性。
	// 双引号是 encoding/xml 与 html 渲染器统一输出的形态。
	for _, attr := range []string{`src="/media/`, `srcset="/media/`, `poster="/media/`} {
		html = strings.ReplaceAll(html, attr, strings.ReplaceAll(attr, `="/media/`, `="`+base+`/media/`))
	}
	return html
}

func (s *server) handleHubEntries(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = 20
	}
	if limit > maxHubEntryLimit {
		limit = maxHubEntryLimit
	}
	feedID, _ := strconv.ParseInt(c.Query("feed_id"), 10, 64)

	items, next, err := s.Store.Entry().List(c.Request.Context(), store.EntryQuery{
		FeedID:  feedID,
		Unread:  c.Query("unread") == "true",
		Starred: c.Query("starred") == "true",
		Search:  c.Query("q"),
		Limit:   limit,
		Cursor:  c.Query("cursor"),
		// 宿主就是为了拿正文而来的。
		WithContent: true,
	})
	if err != nil {
		fail(c, err)
		return
	}

	base := strings.TrimRight(s.Cfg.Server.PublicURL, "/")
	out := make([]hubEntry, 0, len(items))
	for i := range items {
		e := &items[i]
		out = append(out, hubEntry{
			ID: e.ID, FeedID: e.FeedID, Title: e.Title, URL: e.URL, Author: e.Author,
			Summary:     e.Summary,
			PublishedAt: optTS(e.PublishedAt),
			FetchedAt:   ts(e.FetchedAt),
			ReadAt:      optTS(e.ReadAt),
			StarredAt:   optTS(e.StarredAt),
			ContentHTML: absolutizeMedia(e.ContentHTML, base),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"items":       out,
		"next_cursor": next,
		// media_base 留着给需要自己处理缓存的宿主：正文里的绝对地址
		// 都由它拼出来，宿主可以据此做预热或改写。
		"media_base": base,
	})
}

type hubBatchRequest struct {
	Items []hubUpsertRequest `json:"items" binding:"required"`
}

type hubBatchFailure struct {
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

// handleHubBatch 在一次签名请求里推送多条订阅。
//
// 宿主按文档建议做启动对账时，逐个 POST 意味着 N 次签名、N 次读 body、
// N 个往返。200 个订阅就是 200 个 RT，而它们本可以是一条请求。
//
// 与单条推送共用同一套语义（省略字段保留原值、interval 夹紧、
// 地址校验），只是把逐条结果聚合成一个响应。
func (s *server) handleHubBatch(c *gin.Context) {
	var req hubBatchRequest
	if !bindJSON(c, &req) {
		return
	}
	if len(req.Items) == 0 {
		failWith(c, http.StatusBadRequest, "invalid_request", "items 不能为空")
		return
	}
	// 与 OPML 导入同一个量级：一次几百条是正常对账，几万条是异常。
	if len(req.Items) > maxOPMLItems {
		failWith(c, http.StatusRequestEntityTooLarge, "payload_too_large",
			"一次最多推送 "+strconv.Itoa(maxOPMLItems)+" 条订阅")
		return
	}

	ctx := c.Request.Context()
	var created, updated, unchanged int
	failed := make([]hubBatchFailure, 0)

	for i := range req.Items {
		item := &req.Items[i]
		if err := s.RSS.ValidateURL(item.URL); err != nil {
			failed = append(failed, hubBatchFailure{URL: item.URL, Reason: err.Error()})
			continue
		}

		existing, err := s.Store.Feed().GetByURL(ctx, item.URL)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			failed = append(failed, hubBatchFailure{URL: item.URL, Reason: "查询失败"})
			continue
		}

		interval := s.Cfg.RSS.DefaultInterval.Duration
		if existing != nil && existing.FetchInterval > 0 {
			interval = time.Duration(existing.FetchInterval) * time.Second
		}
		if item.Interval != "" {
			d, err := time.ParseDuration(item.Interval)
			if err != nil {
				failed = append(failed, hubBatchFailure{URL: item.URL, Reason: "interval 格式不正确"})
				continue
			}
			interval = d
		}
		// 与单条推送同一套夹紧规则，两个入口不能对同一个值给出不同答案。
		if interval < s.Cfg.RSS.MinInterval.Duration {
			interval = s.Cfg.RSS.MinInterval.Duration
		}
		if interval > s.Cfg.RSS.MaxInterval.Duration {
			interval = s.Cfg.RSS.MaxInterval.Duration
		}

		feed := planUpsert(*item, existing, interval, time.Now())
		_, isNew, err := s.Store.Feed().Upsert(ctx, feed)
		if err != nil {
			failed = append(failed, hubBatchFailure{URL: item.URL, Reason: "写入失败"})
			continue
		}
		switch {
		case isNew:
			created++
		case item.Enabled != nil || item.Title != "" || item.Interval != "":
			updated++
		default:
			unchanged++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"created":   created,
		"updated":   updated,
		"unchanged": unchanged,
		"failed":    failed,
		"total":     len(req.Items),
	})
}

// handleHubStats 一次给出宿主仪表盘需要的聚合数字。
//
// 没有它，宿主想显示「N 个订阅、M 条未读、X GB 存储」只能先拉全量
// subscriptions 再本地聚合——每个订阅还带一个未读计数的子查询。
//
// 不含 pastes：那是另一个模块的内容，与订阅通道无关，
// 宿主也不该通过这条通道看到片段数量。
func (s *server) handleHubStats(c *gin.Context) {
	stats, err := s.Store.Stats(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"feeds":         stats.Feeds,
		"entries":       stats.Entries,
		"unread":        stats.Unread,
		"blobs":         stats.Blobs,
		"blob_bytes":    stats.BlobBytes,
		"pending_media": stats.PendingMedia,
		"schema_version": func() int64 {
			v, err := s.Store.SchemaVersion(c.Request.Context())
			if err != nil {
				return 0
			}
			return int64(v)
		}(),
	})
}
