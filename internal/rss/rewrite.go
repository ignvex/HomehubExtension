package rss

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sort"

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

func rewriteNode(node *html.Node, targets []Target, results map[string]MediaResult) {
	// srcset 候选按原顺序重建，保证浏览器仍能按 DPR 选择。
	var srcsetTargets []Target
	var direct []Target
	for _, t := range targets {
		if t.Attr == "srcset" {
			srcsetTargets = append(srcsetTargets, t)
		} else {
			direct = append(direct, t)
		}
	}

	for _, t := range direct {
		r, ok := results[t.Resolved]
		if !ok {
			// 没有下载结果（data URI 等）——按不可本地化处理。
			removeOrPlaceholder(node, t.Kind, t.Resolved)
			return
		}
		switch r.Status {
		case "ok":
			setAttr(node, t.Attr, MediaURL(r.SHA256, r.Ext))
		case "blocked", "unsupported":
			// 直接移除：这类资源没有本地化价值，留占位只会误导。
			removeOrPlaceholder(node, t.Kind, t.Resolved)
			return
		default:
			// failed / too_large / quota_exceeded：保留位置，换成占位。
			removeOrPlaceholder(node, t.Kind, t.Resolved)
			return
		}
	}

	if len(srcsetTargets) > 0 {
		sort.Slice(srcsetTargets, func(i, j int) bool { return srcsetTargets[i].Index < srcsetTargets[j].Index })
		parts := make([]string, 0, len(srcsetTargets))
		firstLocal := ""
		for _, t := range srcsetTargets {
			r, ok := results[t.Resolved]
			if !ok || r.Status != "ok" {
				// 单个候选失败只是从候选集里剔除，不影响其他候选。
				continue
			}
			u := MediaURL(r.SHA256, r.Ext)
			if firstLocal == "" {
				firstLocal = u
			}
			desc := descriptorFor(t.Node, t.Index)
			if desc == "" {
				parts = append(parts, u)
			} else {
				parts = append(parts, u+" "+desc)
			}
		}
		if len(parts) == 0 {
			removeOrPlaceholder(node, "image", fallbackURL(srcsetTargets))
			return
		}
		setAttr(node, "srcset", joinComma(parts))
		// src 绝不能保留原始外链：如果它自己没下载成功，
		// 就用第一个成功的候选补上。
		if cur := getAttr(node, "src"); cur == "" || !isLocalMedia(cur) {
			if directTargetFailed(node, direct, results) && firstLocal != "" {
				setAttr(node, "src", firstLocal)
			}
		}
		removeAttr(node, "sizes")
	}
}

func directTargetFailed(node *html.Node, direct []Target, results map[string]MediaResult) bool {
	if len(direct) == 0 {
		return true
	}
	for _, t := range direct {
		if r, ok := results[t.Resolved]; ok && r.Status == "ok" {
			return false
		}
	}
	return true
}

func isLocalMedia(u string) bool {
	return len(u) > 7 && u[:7] == "/media/"
}

// descriptorFor 从节点当前的 srcset 里取回第 index 个候选的描述符。
func descriptorFor(node *html.Node, index int) string {
	entries := parseSrcset(getAttr(node, "srcset"))
	if index < 0 || index >= len(entries) {
		return ""
	}
	return entries[index].Descriptor
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
