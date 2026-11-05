package rss

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// 占位图是一个内联 SVG：灰色方块 + 一个破图符号。
// 刻意不含原始 URL —— 原地址只存在于 media_fetch 表里，不渲染到页面，
// 避免用户误点或某些前端脚本自动加载。
var placeholderDataURI = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(
	`<svg xmlns="http://www.w3.org/2000/svg" width="160" height="90" viewBox="0 0 160 90">`+
		`<rect width="160" height="90" fill="#e5e7eb"/>`+
		`<path d="M55 60l15-18 12 14 9-10 14 18z" fill="#9ca3af"/>`+
		`<circle cx="66" cy="34" r="6" fill="#9ca3af"/>`+
		`</svg>`))

// MediaURL 把内容寻址的 blob 映射成对外路径。集中在一处，
// 将来换路径前缀（比如加 CDN 前缀）只改这里。
func MediaURL(sha, ext string) string {
	if ext == "" {
		return "/media/" + sha
	}
	return "/media/" + sha + "." + ext
}

// Rewrite 用下载结果替换文档里的资源地址。
//
// 必须在写库之前调用：落库的 content_html 必须是最终形态，
// 否则读者打开时会看到原始外链，浏览器就会去访问原站。
func (d *Document) Rewrite(results map[string]MediaResult) {
	// 按节点聚合：一个 <img> 可能同时有 src 与多个 srcset 候选。
	grouped := make(map[*html.Node][]Target)
	for _, t := range d.Targets {
		grouped[t.Node] = append(grouped[t.Node], t)
	}

	for node, targets := range grouped {
		if node.Parent == nil {
			continue // 已被前面的决策移除
		}
		rewriteNode(node, targets, results)
	}

	// 清洗阶段标记的空 img（既无 src 也无 srcset）在这里移除。
	var toRemove []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" && getAttr(n, "data-empty") == "1" {
			toRemove = append(toRemove, n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(d.Root)
	for _, n := range toRemove {
		if n.Parent != nil {
			n.Parent.RemoveChild(n)
		}
	}
}

// rewriteNode 按下载结果重写一个元素的全部资源属性。
//
// 规则（本地化是硬约束，任何未本地化的外链属性都不能留下）：
//   - src 成功 → 换成本地地址；
//   - img 的 src 失败，但 srcset 有成功候选 → 保留元素，src 用第一个成功候选；
//   - srcset 单个候选失败只从候选集里剔除；全部失败时若 src 成功就只删 srcset；
//   - poster 失败只移除 poster，不删除整个 video；
//   - 音视频本体（src）失败 → 移除元素；图片无任何成功地址 → 换占位图。
//
// 最后统一 stripNonLocal：即便将来某条分支漏了，外链属性也不会被渲染出去。
func rewriteNode(node *html.Node, targets []Target, results map[string]MediaResult) {
	var srcsetTargets []Target
	var direct []Target
	for _, t := range targets {
		if t.Attr == "srcset" {
			srcsetTargets = append(srcsetTargets, t)
		} else {
			direct = append(direct, t)
		}
	}

	// 先算 srcset：src 失败时要用它的成功候选兜底。
	sort.Slice(srcsetTargets, func(i, j int) bool { return srcsetTargets[i].Index < srcsetTargets[j].Index })
	parts := make([]string, 0, len(srcsetTargets))
	firstLocal := ""
	for _, t := range srcsetTargets {
		r, ok := results[t.Resolved]
		if !ok || r.Status != "ok" {
			continue
		}
		u := MediaURL(r.SHA256, r.Ext)
		if firstLocal == "" {
			firstLocal = u
		}
		if t.Descriptor == "" {
			parts = append(parts, u)
		} else {
			parts = append(parts, u+" "+t.Descriptor)
		}
	}

	srcOK := false
	srcFailed := false
	for _, t := range direct {
		if r, ok := results[t.Resolved]; ok && r.Status == "ok" {
			setAttr(node, t.Attr, MediaURL(r.SHA256, r.Ext))
			if t.Attr == "src" {
				srcOK = true
			}
			continue
		}
		// 没有下载结果（未知 scheme 等）与失败一样按不可本地化处理。
		switch {
		case t.Attr == "poster":
			// 封面只是装饰，视频本体可能已经本地化了。
			removeAttr(node, "poster")
		case t.Attr == "src" && node.Data == "img" && firstLocal != "":
			// src 由下面的 srcset 兜底补上。
			removeAttr(node, "src")
			srcFailed = true
		default:
			removeOrPlaceholder(node, t.Kind, t.Resolved)
			return
		}
	}

	if len(srcsetTargets) > 0 {
		if len(parts) == 0 {
			removeAttr(node, "srcset")
			removeAttr(node, "sizes")
			if !srcOK {
				removeOrPlaceholder(node, "image", fallbackURL(srcsetTargets))
				return
			}
		} else {
			setAttr(node, "srcset", joinComma(parts))
			removeAttr(node, "sizes")
			// img 没有成功的 src（失败或本来就没有）时用第一个成功候选补上，
			// 不支持 srcset 的阅读器也能显示。
			if node.Data == "img" && (srcFailed || !srcOK) {
				setAttr(node, "src", firstLocal)
			}
		}
	}

	stripNonLocal(node)
}

// mediaAttrs 是可能承载资源地址的属性；重写之后它们只能指向本地。
var mediaAttrs = []string{"src", "srcset", "poster"}

// stripNonLocal 删除仍指向非本地地址的资源属性。
//
// 这是最后一道闸：rewriteNode 的每条分支本应已经处理干净，这里保证
// 即便出现没有覆盖到的组合，也只会少一张图，而不会让读者的浏览器
// 去请求第三方。
func stripNonLocal(node *html.Node) {
	for _, key := range mediaAttrs {
		v := getAttr(node, key)
		if v == "" {
			continue
		}
		if key == "srcset" {
			for _, e := range parseSrcset(v) {
				if !isLocalMedia(e.URL) {
					removeAttr(node, key)
					break
				}
			}
			continue
		}
		if !isLocalMedia(v) && v != placeholderDataURI {
			removeAttr(node, key)
		}
	}
}

func isLocalMedia(u string) bool {
	return strings.HasPrefix(u, "/media/") && !strings.HasPrefix(u, "/media//")
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// MediaRef 把一个原始 URL 映射成短的稳定标识。
// 失败资源在 HTML 里留下这个标记，重试成功后据此就地替换，
// 不必重抓整个 feed。
func MediaRef(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:8])
}

// removeOrPlaceholder 处理无法本地化的元素。
//
// 图片换成占位（保留 alt，读屏软件仍能读出描述）；
// 音视频直接移除——一个不能播放的播放器对读者没有任何意义。
func removeOrPlaceholder(node *html.Node, kind, rawURL string) {
	if kind == "image" && node.Data == "img" {
		alt := getAttr(node, "alt")
		// 清掉可能指向外站的属性，再替换成占位。
		node.Attr = nil
		setAttr(node, "src", placeholderDataURI)
		setAttr(node, "alt", alt)
		setAttr(node, "loading", "lazy")
		setAttr(node, "decoding", "async")
		setAttr(node, "class", "media-missing")
		if rawURL != "" {
			setAttr(node, "data-media-ref", MediaRef(rawURL))
		}
		removeAttr(node, "width")
		removeAttr(node, "height")
		removeAttr(node, "srcset")
		return
	}
	if node.Parent != nil {
		node.Parent.RemoveChild(node)
	}
}

// fallbackURL 在整组 srcset 候选都失败时，取第一个候选的地址作为标记。
func fallbackURL(targets []Target) string {
	if len(targets) == 0 {
		return ""
	}
	return targets[0].Resolved
}
