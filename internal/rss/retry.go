package rss

import (
	"context"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// RetryResult 汇报一轮媒体重试的结果。
type RetryResult struct {
	Tried   int
	OK      int
	Failed  int
	Entries int
}

// RetryMedia 重试失败的资源，并就地修补受影响条目的正文。
//
// 关键点：重试成功后不改动整篇内容，只替换那个占位 <img>。
// 靠清洗阶段写入的 data-media-ref 定位——这个标记是我们自己加的，
// 值来自原始 URL 的哈希，所以和 media_fetch 记录能一一对应。
func (s *Service) RetryMedia(
	ctx context.Context,
	maxAttempts int,
	within time.Duration,
	limit int,
) (RetryResult, error) {
	var out RetryResult
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if within <= 0 {
		within = 72 * time.Hour
	}
	after := time.Now().Add(-within).Unix()

	records, err := s.st.Media().Retryable(ctx, maxAttempts, after, limit)
	if err != nil {
		return out, err
	}
	if len(records) == 0 {
		return out, nil
	}

	// 按条目分组：一篇文可能有多张图失败，逐个条目重建正文比逐条重写便宜。
	byEntry := map[int64][]store.MediaRecord{}
	for _, r := range records {
		byEntry[r.EntryID] = append(byEntry[r.EntryID], r)
	}

	used, _ := s.st.Blob().TotalSize(ctx)

	for entryID, items := range byEntry {
		if ctx.Err() != nil {
			break
		}
		entry, err := s.st.Entry().Get(ctx, entryID)
		if err != nil {
			continue
		}
		out.Entries++

		ok, failed := s.retryEntry(ctx, entry, items, &used)
		out.Tried += ok + failed
		out.OK += ok
		out.Failed += failed
	}
	return out, nil
}

// retryEntry 重试一条条目下所有失败的资源，并就地修补正文。
//
// 返回成功与失败的资源数。补丁只在有资源成功时才写回：
// 全失败时正文没有变化，多一次 UPDATE 是无意义的写放大。
func (s *Service) retryEntry(
	ctx context.Context, entry *store.Entry, items []store.MediaRecord, used *int64,
) (ok, failed int) {
	// ref → 下载结果，用于在 HTML 里定位占位图。
	fixed := map[string]MediaResult{}

	for i := range items {
		rec := &items[i]
		res := s.dl.Download(ctx, rec.SourceURL, entry.URL, *used)

		rec.Attempts++
		rec.UpdatedAt = time.Now().Unix()
		rec.Status = res.Status
		rec.Error = res.Error
		rec.SHA256 = res.SHA256

		if res.Status == "ok" {
			ok++
			*used += res.Size
			s.storeRetried(ctx, entry.ID, res)
			fixed[MediaRef(rec.SourceURL)] = res
		} else {
			failed++
		}

		if err := s.st.Media().Update(ctx, rec); err != nil {
			s.log.Warn("update media record", "id", rec.ID, "err", err)
		}
	}

	if len(fixed) == 0 {
		return ok, failed
	}
	patched, changed := patchPlaceholders(entry.ContentHTML, fixed)
	if !changed {
		return ok, failed
	}
	if err := s.st.Media().SetEntryHTML(ctx, entry.ID, patched, entry.ContentText); err != nil {
		s.log.Warn("write back entry html", "entry", entry.ID, "err", err)
	}
	return ok, failed
}

// storeRetried 记录重试成功后的 blob 元数据与引用。
func (s *Service) storeRetried(ctx context.Context, entryID int64, res MediaResult) {
	now := time.Now().Unix()
	if err := s.st.Blob().Upsert(ctx, &store.Blob{
		SHA256: res.SHA256, Size: res.Size, MIME: res.MIME,
		Ext: res.Ext, Kind: "image",
		CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		s.log.Warn("upsert blob on retry", "sha", res.SHA256, "err", err)
	}
	if err := s.st.Blob().Link(ctx, entryID, res.SHA256); err != nil {
		s.log.Warn("link blob on retry", "entry", entryID, "err", err)
	}
}

// patchPlaceholders 在解析后的树上替换占位图片的 src。
// 不做字符串替换：HTML 里的属性顺序、引号风格都不可控，
// 正则替换很容易漏掉或改错。
func patchPlaceholders(source string, fixed map[string]MediaResult) (string, bool) {
	contextNode := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(source), contextNode)
	if err != nil {
		return source, false
	}
	changed := false

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			if ref := getAttr(n, "data-media-ref"); ref != "" {
				if res, ok := fixed[ref]; ok {
					setAttr(n, "src", MediaURL(res.SHA256, res.Ext))
					removeAttr(n, "data-media-ref")
					removeAttr(n, "class")
					changed = true
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	if !changed {
		return source, false
	}

	var sb strings.Builder
	for _, n := range nodes {
		if err := html.Render(&sb, n); err != nil {
			return source, false
		}
	}
	return sb.String(), true
}
