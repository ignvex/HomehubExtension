package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/blob"
	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/gin-gonic/gin"
)

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// handleMedia 提供本地化后的媒体文件。
//
// 内容寻址意味着一个路径永远对应同一份内容，可以放心 immutable 长缓存；
// 文件名变化即内容变化，浏览器不需要回源验证。
func (s *server) handleMedia(c *gin.Context) {
	name := c.Param("name")
	sha := name
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		sha, ext = name[:i], name[i+1:]
	}
	if len(sha) != 64 || !isHex(sha) {
		failWith(c, http.StatusNotFound, "not_found", "资源不存在")
		return
	}

	rec, err := s.Store.Blob().Get(c.Request.Context(), sha)
	if err != nil {
		failWith(c, http.StatusNotFound, "not_found", "资源不存在")
		return
	}
	// 扩展名以数据库为准：地址里的 ext 只影响缓存键，不影响内容类型。
	if rec.Ext != "" {
		ext = rec.Ext
	}

	f, err := s.Blobs.Open(sha, ext)
	if err != nil {
		// 数据库有记录但文件缺失：内容损坏，但不该 500 吓到读者。
		failWith(c, http.StatusNotFound, "not_found", "资源文件已丢失")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		failWith(c, http.StatusInternalServerError, "internal", "读取资源失败")
		return
	}

	h := c.Writer.Header()
	h.Set("Content-Type", rec.MIME)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+sha+`"`)
	if rec.Kind == "image" && rec.MIME == "image/svg+xml" {
		// 直接打开 SVG 地址时会以顶级文档执行脚本，必须限制它的能力。
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	}
	if c.Query("download") == "1" {
		h.Set("Content-Disposition", "attachment")
	}

	// ServeContent 负责 Range、If-None-Match、If-Modified-Since。
	http.ServeContent(c.Writer, c.Request, sha+"."+ext, info.ModTime(), f)
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

type feedResponse struct {
	ID            int64  `json:"id"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	SiteURL       string `json:"site_url"`
	Enabled       bool   `json:"enabled"`
	FetchInterval string `json:"fetch_interval"`
	NextFetchAt   *int64 `json:"next_fetch_at"`
	LastFetchAt   *int64 `json:"last_fetch_at"`
	LastStatus    int    `json:"last_status"`
	LastError     string `json:"last_error"`
	ErrorCount    int    `json:"error_count"`
	UnreadCount   int64  `json:"unread_count"`
}

func (s *server) handleFeedList(c *gin.Context) {
	feeds, err := s.Store.Feed().List(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]feedResponse, 0, len(feeds))
	for i := range feeds {
		f := &feeds[i]
		out = append(out, feedResponse{
			ID: f.ID, URL: f.URL, Title: f.Title, SiteURL: f.SiteURL,
			Enabled:       f.Enabled,
			FetchInterval: (time.Duration(f.FetchInterval) * time.Second).String(),
			NextFetchAt:   optTS(f.NextFetchAt),
			LastFetchAt:   optTS(f.LastFetchAt),
			LastStatus:    f.LastStatus,
			LastError:     f.LastError,
			ErrorCount:    f.ErrorCount,
			UnreadCount:   f.UnreadCount,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (s *server) handleFeedGet(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "id 必须是整数")
		return
	}
	f, err := s.Store.Feed().Get(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	unread, _ := s.Store.Entry().UnreadCount(c.Request.Context(), id)
	c.JSON(http.StatusOK, feedResponse{
		ID: f.ID, URL: f.URL, Title: f.Title, SiteURL: f.SiteURL,
		Enabled:       f.Enabled,
		FetchInterval: (time.Duration(f.FetchInterval) * time.Second).String(),
		NextFetchAt:   optTS(f.NextFetchAt),
		LastFetchAt:   optTS(f.LastFetchAt),
		LastStatus:    f.LastStatus,
		LastError:     f.LastError,
		ErrorCount:    f.ErrorCount,
		UnreadCount:   unread,
	})
}

func (s *server) handleFeedRefresh(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "id 必须是整数")
		return
	}
	ctx, cancel := contextWithTimeout(2 * time.Minute)
	defer cancel()
	res, err := s.RSS.Fetch(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *server) handleFeedDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "id 必须是整数")
		return
	}
	if err := s.Store.Feed().Delete(c.Request.Context(), id); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type entrySummary struct {
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
}

func toEntrySummary(e *store.Entry) entrySummary {
	return entrySummary{
		ID: e.ID, FeedID: e.FeedID, Title: e.Title, URL: e.URL, Author: e.Author,
		Summary:     e.Summary,
		PublishedAt: optTS(e.PublishedAt),
		FetchedAt:   ts(e.FetchedAt),
		ReadAt:      optTS(e.ReadAt),
		StarredAt:   optTS(e.StarredAt),
	}
}

func (s *server) handleEntryList(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	feedID, _ := strconv.ParseInt(c.Query("feed_id"), 10, 64)

	items, next, err := s.Store.Entry().List(c.Request.Context(), store.EntryQuery{
		FeedID:  feedID,
		Unread:  c.Query("unread") == "true",
		Starred: c.Query("starred") == "true",
		Search:  c.Query("q"),
		Limit:   limit,
		Cursor:  c.Query("cursor"),
	})
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]entrySummary, 0, len(items))
	for i := range items {
		out = append(out, toEntrySummary(&items[i]))
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "next_cursor": cursorOrNil(next)})
}

func (s *server) handleEntryGet(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "id 必须是整数")
		return
	}
	e, err := s.Store.Entry().Get(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	// 打开即已读。写失败不影响阅读，只记日志。
	if e.ReadAt == nil {
		if err := s.Store.Entry().SetRead(c.Request.Context(), id, true); err != nil {
			logFrom(c).Warn("mark read failed", "entry", id, "err", err)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"id": e.ID, "feed_id": e.FeedID, "title": e.Title, "url": e.URL,
		"author": e.Author, "summary": e.Summary,
		"published_at": optTS(e.PublishedAt),
		"fetched_at":   ts(e.FetchedAt),
		"read_at":      optTS(e.ReadAt),
		"starred_at":   optTS(e.StarredAt),
		"content_html": e.ContentHTML,
		"content_text": e.ContentText,
	})
}

type entryPatchRequest struct {
	Read    *bool `json:"read"`
	Starred *bool `json:"starred"`
}

func (s *server) handleEntryPatch(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "id 必须是整数")
		return
	}
	var req entryPatchRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Read != nil {
		if err := s.Store.Entry().SetRead(c.Request.Context(), id, *req.Read); err != nil {
			fail(c, err)
			return
		}
	}
	if req.Starred != nil {
		if err := s.Store.Entry().SetStarred(c.Request.Context(), id, *req.Starred); err != nil {
			fail(c, err)
			return
		}
	}
	c.Status(http.StatusNoContent)
}

type bulkRequest struct {
	IDs  []int64 `json:"ids" binding:"required"`
	Read *bool   `json:"read"`
}

func (s *server) handleEntryBulk(c *gin.Context) {
	var req bulkRequest
	if !bindJSON(c, &req) {
		return
	}
	if len(req.IDs) > 200 {
		failWith(c, http.StatusBadRequest, "invalid_request", "单次最多 200 条")
		return
	}
	read := true
	if req.Read != nil {
		read = *req.Read
	}
	if err := s.Store.Entry().BulkSetRead(c.Request.Context(), req.IDs, read); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleEntryOriginal 返回清洗前的原文，只用于排查。
// 响应体可能包含任何东西，所以强制下载并禁止渲染。
func (s *server) handleEntryOriginal(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "id 必须是整数")
		return
	}
	e, err := s.Store.Entry().Get(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename=entry-"+strconv.FormatInt(id, 10)+".txt")
	c.Header("Content-Security-Policy", "default-src 'none'")
	c.String(http.StatusOK, e.ContentHTML)
}

var _ = blob.ErrNotFound
