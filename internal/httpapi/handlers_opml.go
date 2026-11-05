package httpapi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/rss"
	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/gin-gonic/gin"
)

// OPML 导入导出（路线图 M10）。
//
// 迁移场景是阅读器的刚需：换一个客户端时，用户第一件事就是导 OPML。
// 没有这两个端点，"从别的阅读器迁过来"就只能一个个手敲订阅地址。

const (
	// maxOPMLBytes 是导入单次接受的文档上限。OPML 是纯文本索引，
	// 500 条订阅通常不到 100 KB；1 MB 足够容纳几千条，同时挡住
	// 拿大文件占内存的玩法。
	maxOPMLBytes = 1 << 20
	// maxOPMLItems 是单次导入的订阅条数上限。超过就整体拒绝而不是
	// 截断：静默丢弃一部分订阅，用户会以为导入成功但内容不全。
	maxOPMLItems = 500
)

// opmlDocument 只映射我们关心的部分。OPML 允许任意嵌套与自定义属性，
// 这里按 xml 包的特性忽略未知字段，不追求完整实现。
type opmlDocument struct {
	XMLName xml.Name `xml:"opml"`
	Version string   `xml:"version,attr"`
	Head    opmlHead `xml:"head"`
	Body    opmlBody `xml:"body"`
}

type opmlHead struct {
	Title       string `xml:"title,omitempty"`
	DateCreated string `xml:"dateCreated,omitempty"`
}

type opmlBody struct {
	Outlines []opmlOutline `xml:"outline"`
}

// opmlOutline 对应 OPML 的 <outline>。分组节点没有 xmlUrl，
// 订阅节点有；两者用同一个结构表示，靠 xmlUrl 是否为空区分。
type opmlOutline struct {
	Text     string        `xml:"text,attr,omitempty"`
	Title    string        `xml:"title,attr,omitempty"`
	Type     string        `xml:"type,attr,omitempty"`
	XMLURL   string        `xml:"xmlUrl,attr,omitempty"`
	HTMLURL  string        `xml:"htmlUrl,attr,omitempty"`
	Children []opmlOutline `xml:"outline"`
}

// collect 递归收集所有带 xmlUrl 的节点。
//
// 分组可以任意深：很多阅读器导出时按文件夹嵌套两层以上。
// 只取顶层会把分组里的订阅全丢掉，而导入方看到的是"成功导入 0 条"。
func (o opmlOutline) collect(dst *[]opmlOutline, seen map[string]struct{}) {
	if url := strings.TrimSpace(o.XMLURL); url != "" {
		// 同一次导入里重复出现只算一条：Upsert 本身幂等，
		// 但计数会翻倍，看起来像导入数量不对。
		if _, dup := seen[url]; !dup {
			seen[url] = struct{}{}
			item := o
			item.XMLURL = url
			*dst = append(*dst, item)
		}
	}
	for _, child := range o.Children {
		child.collect(dst, seen)
	}
}

// parseOPML 解析文档并返回所有订阅节点。
func parseOPML(data []byte) ([]opmlOutline, error) {
	var doc opmlDocument
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	// 不允许 DTD 与自定义实体：OPML 不需要它们，而外部实体是
	// XXE 的经典入口。encoding/xml 默认不解析外部实体，
	// 但明确关掉更不容易在以后换实现时被忘掉。
	dec.Strict = true
	dec.Entity = xml.HTMLEntity
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse opml: %w", err)
	}
	if doc.XMLName.Local != "opml" {
		return nil, errors.New("not an opml document")
	}

	var out []opmlOutline
	seen := make(map[string]struct{})
	for _, o := range doc.Body.Outlines {
		o.collect(&out, seen)
	}
	return out, nil
}

type opmlFailure struct {
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

// handleFeedExport 导出全部订阅为 OPML。
//
// 只导出与迁移相关的字段：标题、订阅地址、站点地址。
// 抓取状态、未读数这些是本实例的运行数据，换一个客户端没有意义。
func (s *server) handleFeedExport(c *gin.Context) {
	feeds, err := s.Store.Feed().List(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}

	doc := opmlDocument{
		Version: "2.0",
		Head: opmlHead{
			Title:       "HomehubExtension 订阅",
			DateCreated: time.Now().UTC().Format(time.RFC1123Z),
		},
	}
	for i := range feeds {
		f := &feeds[i]
		title := f.Title
		if strings.TrimSpace(title) == "" {
			title = f.URL
		}
		doc.Body.Outlines = append(doc.Body.Outlines, opmlOutline{
			Type:    "rss",
			Text:    title,
			Title:   title,
			XMLURL:  f.URL,
			HTMLURL: f.SiteURL,
		})
	}

	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		fail(c, err)
		return
	}

	c.Header("Content-Type", "text/x-opml; charset=utf-8")
	// attachment 让浏览器直接下载而不是渲染成一棵 XML 树。
	// 文件名带固定前缀，避免不同实例导出的文件互相覆盖。
	c.Header("Content-Disposition", `attachment; filename="homehub-subscriptions.opml"`)
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.String(http.StatusOK, xml.Header+string(body))
}

// handleFeedImport 从 OPML 批量创建订阅。
//
// readOPMLRequest 读取并解析导入请求体，把大小与条数限制集中在一处。
// 返回 false 表示已经写过错误响应。
func readOPMLRequest(c *gin.Context) ([]opmlOutline, bool) {
	// 不用 bindJSON：请求体是 XML，不是 JSON。
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, maxOPMLBytes+1))
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "无法读取请求体")
		return nil, false
	}
	if len(data) > maxOPMLBytes {
		failWith(c, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("OPML 超过 %d 字节", maxOPMLBytes))
		return nil, false
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		failWith(c, http.StatusBadRequest, "invalid_request", "请求体为空")
		return nil, false
	}

	items, err := parseOPML(data)
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request", "OPML 解析失败",
			FieldError{Field: "body", Reason: err.Error()})
		return nil, false
	}
	if len(items) > maxOPMLItems {
		failWith(c, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("一次最多导入 %d 条订阅，当前 %d 条", maxOPMLItems, len(items)))
		return nil, false
	}
	return items, true
}

// importOutlines 逐条写入订阅，返回新建/更新计数与失败项。
func (s *server) importOutlines(
	ctx context.Context, items []opmlOutline, interval time.Duration, now time.Time,
) (created, updated int, failed []opmlFailure) {
	failed = make([]opmlFailure, 0)

	for i := range items {
		item := &items[i]
		// 与其它入口同一套校验：导入的 OPML 同样可能含内网地址。
		if err := s.RSS.ValidateURL(item.XMLURL); err != nil {
			failed = append(failed, opmlFailure{URL: item.XMLURL, Reason: err.Error()})
			continue
		}

		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = strings.TrimSpace(item.Text)
		}

		// htmlUrl 会被渲染成站点链接：只收 http/https 绝对地址，
		// javascript: 之类的值存成空串。
		next := now.Unix()
		_, isNew, err := s.Store.Feed().Upsert(ctx, &store.Feed{
			URL:           item.XMLURL,
			SiteURL:       rss.SafeHTTPURL(item.HTMLURL),
			Title:         title,
			Source:        "manual",
			Enabled:       true,
			FetchInterval: int64(interval / time.Second),
			NextFetchAt:   &next,
			CreatedAt:     now.Unix(),
		})
		if err != nil {
			failed = append(failed, opmlFailure{URL: item.XMLURL, Reason: "写入失败"})
			continue
		}
		if isNew {
			created++
		} else {
			updated++
		}
	}
	return created, updated, failed
}

// importFeedUpsert 兼容两个调用点：OPML 导入与 hub 批量推送
// 需要同一套「新建/更新」判定。
func (s *server) handleFeedImport(c *gin.Context) {
	if s.RSS == nil {
		failWith(c, http.StatusNotFound, "not_found", "RSS 模块未启用")
		return
	}

	items, ok := readOPMLRequest(c)
	if !ok {
		return
	}

	interval := s.Cfg.RSS.DefaultInterval.Duration
	if interval < s.Cfg.RSS.MinInterval.Duration {
		interval = s.Cfg.RSS.MinInterval.Duration
	}
	if interval > s.Cfg.RSS.MaxInterval.Duration {
		interval = s.Cfg.RSS.MaxInterval.Duration
	}

	created, updated, failed := s.importOutlines(c.Request.Context(), items, interval, time.Now())

	c.JSON(http.StatusOK, gin.H{
		"created": created,
		"updated": updated,
		"failed":  failed,
		"total":   len(items),
	})
}
