package rss

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Target 是一个待下载并重写的资源引用。
type Target struct {
	Node     *html.Node
	Attr     string
	Index    int // srcset 中的候选下标；-1 表示普通属性
	URL      string
	Resolved string // 相对地址解析后的绝对值
	Kind     string // image / video / audio / attachment
	// Descriptor 是 srcset 候选的描述符（"2x"、"640w"），普通属性为空。
	//
	// 必须在收集时随候选一起带走：之前重写阶段按 Index 回到清洗后的
	// srcset 里取描述符，而清洗会丢掉 javascript: 之类的候选，
	// 原始下标与清洗后的位置就错开了，2x 的图会被标成 1x。
	Descriptor string
}

// Document 是清洗后的文档与资源引用的集合。
type Document struct {
	// Root 是承载清洗结果的容器节点，用 Render 输出 HTML。
	Root    *html.Node
	Targets []Target
}

// SanitizeOptions 控制清洗行为。
type SanitizeOptions struct {
	// Base 用于把相对地址解析成绝对地址。必须在清洗前确定，否则
	// src="/a.png" 会被误判成站内资源。
	Base *url.URL
}

// Render 输出最终的 HTML 片段。
func (d *Document) Render() (string, error) {
	var sb strings.Builder
	for c := d.Root.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&sb, c); err != nil {
			return "", err
		}
	}
	return sb.String(), nil
}

// Text 提取纯文本，供检索与摘要。
func (d *Document) Text() string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if isBlock(n.Data) {
			sb.WriteByte(' ')
		}
	}
	for c := d.Root.FirstChild; c != nil; c = c.NextSibling {
		walk(c)
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

func isBlock(tag string) bool {
	switch tag {
	case "p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre":
		return true
	}
	return false
}

// Sanitize 按白名单重建 HTML，同时收集需要本地化的资源。
//
// 输出是全新构建的树，而不是"在原树上删节点"：后者很容易漏掉
// 注释、条件注释、命名空间属性这类边角，而重建的树只包含明确允许的东西。
func Sanitize(input string, opts SanitizeOptions) (*Document, error) {
	context := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(input), context)
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}

	root := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	doc := &Document{Root: root}
	for _, n := range nodes {
		doc.emit(n, root, opts)
	}
	return doc, nil
}

// emit 把源节点（及其子节点）转换成允许的形式追加到 dst。
func (d *Document) emit(src, dst *html.Node, opts SanitizeOptions) {
	switch src.Type {
	case html.TextNode:
		dst.AppendChild(&html.Node{Type: html.TextNode, Data: src.Data})
		return
	case html.CommentNode, html.DoctypeNode, html.ErrorNode:
		// 注释可能藏条件注释与 SSI 指令，一律丢弃。
		return
	case html.DocumentNode:
		for c := src.FirstChild; c != nil; c = c.NextSibling {
			d.emit(c, dst, opts)
		}
		return
	case html.ElementNode:
	default:
		return
	}

	tag := strings.ToLower(src.Data)

	if dropElements[tag] {
		// iframe 有唯一的例外：已知视频源换成纯链接，用户点击才离开本站。
		if tag == "iframe" {
			if link := embedFallback(src); link != nil {
				dst.AppendChild(link)
			}
		}
		return
	}

	if !allowedElements[tag] {
		// 未知标签：脱壳保留内容，避免误删正文。
		for c := src.FirstChild; c != nil; c = c.NextSibling {
			d.emit(c, dst, opts)
		}
		return
	}

	// 追踪像素在复制属性之前判定：需要读原始的 src 与 width/height。
	// 判定为像素就整棵子树丢弃，不下载、不占位、不渲染。
	// 必须在 copyAttrs 之前——它会把 src 解析成绝对地址并丢掉
	// class/data-* 等属性，之后就不好判断了。
	if tag == "img" && d.isPixel(src, opts) {
		return
	}

	el := &html.Node{Type: html.ElementNode, Data: tag, DataAtom: src.DataAtom}
	copyAttrs(el, src, tag, opts, d)
	dst.AppendChild(el)
	for c := src.FirstChild; c != nil; c = c.NextSibling {
		d.emit(c, el, opts)
	}
}

func copyAttrs(dst, src *html.Node, tag string, opts SanitizeOptions, doc *Document) {
	perTag := allowedAttrs[tag]

	for _, a := range src.Attr {
		key := strings.ToLower(a.Key)
		if strings.HasPrefix(key, "on") {
			continue // 事件处理器
		}
		if key == "style" || key == "class" || strings.HasPrefix(key, "data-") {
			// style 会带来 background:url() 与绝对定位；class 与原站 CSS 耦合。
			// 两者都删，配合 CSP 的 style-src 'self' 保证零外部请求。
			continue
		}
		if !globalAttrs[key] && (perTag == nil || !perTag[key]) {
			continue
		}

		switch key {
		case "href":
			v, ok := sanitizeURL(a.Val, opts.Base, false)
			if !ok {
				continue
			}
			dst.Attr = append(dst.Attr, html.Attribute{Key: "href", Val: v})
		case "src", "poster":
			v, ok := sanitizeURL(a.Val, opts.Base, true)
			if !ok {
				continue
			}
			dst.Attr = append(dst.Attr, html.Attribute{Key: key, Val: v})
			doc.collect(dst, key, -1, v, kindFor(tag))
		case "srcset":
			entries := parseSrcset(a.Val)
			if len(entries) == 0 {
				continue
			}
			rebuilt := make([]string, 0, len(entries))
			for i, e := range entries {
				v, ok := sanitizeURL(e.URL, opts.Base, true)
				if !ok {
					continue
				}
				rebuilt = append(rebuilt, strings.TrimSpace(v+" "+e.Descriptor))
				doc.collectCandidate(dst, i, v, e.Descriptor, kindFor(tag))
			}
			if len(rebuilt) > 0 {
				dst.Attr = append(dst.Attr, html.Attribute{Key: "srcset", Val: strings.Join(rebuilt, ", ")})
			}
		default:
			dst.Attr = append(dst.Attr, html.Attribute{Key: key, Val: a.Val})
		}
	}

	if tag == "a" {
		// 外链一律加 rel/target：不泄露来源，也不让目标页拿到 opener。
		setAttr(dst, "rel", "noopener noreferrer nofollow")
		setAttr(dst, "target", "_blank")
	}
	if tag == "img" {
		setAttr(dst, "loading", "lazy")
		setAttr(dst, "decoding", "async")
		// 没有 src 的 img 渲染出来是破图，直接标记以便上层移除。
		if getAttr(dst, "src") == "" && getAttr(dst, "srcset") == "" {
			dst.Attr = append(dst.Attr, html.Attribute{Key: "data-empty", Val: "1"})
		}
	}
}

func (d *Document) collect(node *html.Node, attr string, index int, resolved, kind string) {
	if resolved == "" {
		return
	}
	d.Targets = append(d.Targets, Target{
		Node: node, Attr: attr, Index: index, URL: resolved, Resolved: resolved, Kind: kind,
	})
}

// collectCandidate 收集一个 srcset 候选，连同它自己的描述符。
func (d *Document) collectCandidate(node *html.Node, index int, resolved, descriptor, kind string) {
	if resolved == "" {
		return
	}
	d.Targets = append(d.Targets, Target{
		Node: node, Attr: "srcset", Index: index, URL: resolved, Resolved: resolved,
		Kind: kind, Descriptor: descriptor,
	})
}

func kindFor(tag string) string {
	switch tag {
	case "img", "source":
		return "image"
	case "video":
		return "video"
	case "audio", "track":
		return "audio"
	default:
		return "image"
	}
}

// sanitizeURL 校验协议并解析相对地址。allowData 只对媒体地址开放。
func sanitizeURL(raw string, base *url.URL, allowData bool) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	// 去掉所有控制字符后再判断：java\tscript: 是经典绕过手法。
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, raw)
	lower := strings.ToLower(cleaned)

	if strings.HasPrefix(lower, "data:") {
		if !allowData || !strings.HasPrefix(lower, "data:image/") {
			return "", false
		}
		if len(cleaned) > 64<<10 {
			return "", false
		}
		return cleaned, true
	}

	u, err := url.Parse(cleaned)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.String(), true
	case "mailto", "tel":
		if allowData {
			return "", false
		}
		return u.String(), true
	case "":
		if base == nil {
			return "", false
		}
		return base.ResolveReference(u).String(), true
	default:
		// javascript:, vbscript:, file:, blob:, filesystem: 全部拒绝
		return "", false
	}
}

type srcsetEntry struct{ URL, Descriptor string }

// parseSrcset 按 HTML 规范解析候选列表。
//
// 不能用 strings.Split(s, ",")：CDN 常见 ?w=100,200 这类含逗号的查询串，
// 粗暴切分会把一条拆成两条错乱的 URL。
func parseSrcset(s string) []srcsetEntry {
	var out []srcsetEntry
	i := 0
	n := len(s)
	for i < n {
		for i < n && (isSpace(s[i]) || s[i] == ',') {
			i++
		}
		if i >= n {
			break
		}
		start := i
		for i < n && !isSpace(s[i]) {
			i++
		}
		u := s[start:i]
		desc := ""
		if strings.HasSuffix(u, ",") {
			u = strings.TrimRight(u, ",")
		} else {
			// 读描述符直到逗号
			dstart := i
			for i < n && s[i] != ',' {
				i++
			}
			desc = strings.TrimSpace(s[dstart:i])
			if i < n {
				i++ // 跳过逗号
			}
		}
		if u != "" {
			out = append(out, srcsetEntry{URL: u, Descriptor: desc})
		}
	}
	return out
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

// embedFallback 把已知视频 iframe 换成纯链接。
// 只放链接：浏览器不会自动请求，用户点击才会离开本站。
func embedFallback(n *html.Node) *html.Node {
	src := getAttr(n, "src")
	if src == "" {
		return nil
	}
	target := ""
	switch {
	case strings.Contains(src, "youtube.com/embed/"),
		strings.Contains(src, "youtube-nocookie.com/embed/"):
		id := src[strings.LastIndex(src, "/")+1:]
		if i := strings.IndexAny(id, "?&"); i >= 0 {
			id = id[:i]
		}
		if id != "" {
			target = "https://www.youtube.com/watch?v=" + id
		}
	case strings.Contains(src, "player.vimeo.com/video/"):
		id := src[strings.LastIndex(src, "/")+1:]
		if i := strings.IndexAny(id, "?&"); i >= 0 {
			id = id[:i]
		}
		if id != "" {
			target = "https://vimeo.com/" + id
		}
	case strings.Contains(src, "player.bilibili.com/player.html"):
		if u, err := url.Parse(src); err == nil {
			if bv := u.Query().Get("bvid"); bv != "" {
				target = "https://www.bilibili.com/video/" + bv
			}
		}
	}
	if target == "" {
		return nil
	}

	p := &html.Node{Type: html.ElementNode, Data: "p", DataAtom: atom.P}
	p.Attr = append(p.Attr, html.Attribute{Key: "class", Val: "embed-fallback"})
	a := &html.Node{Type: html.ElementNode, Data: "a", DataAtom: atom.A}
	a.Attr = append(a.Attr,
		html.Attribute{Key: "href", Val: target},
		html.Attribute{Key: "rel", Val: "noopener noreferrer nofollow"},
		html.Attribute{Key: "target", Val: "_blank"})
	a.AppendChild(&html.Node{Type: html.TextNode, Data: "▶ 在原站观看"})
	p.AppendChild(a)
	return p
}

// isPixel 用尺寸与域名启发式识别追踪像素。
//
// 三种信号任一命中即判定为像素：
//  1. width 与 height 都是 0 或 1——人眼看不见的图，只可能是像素；
//  2. 域名命中已知追踪器列表；
//  3. 路径是 /pixel、/beacon、/track、/1x1 结尾且没有扩展名。
//
// 判断不出来的一律按普通图片处理：误删正文配图比多下载一个文件更糟。
// 这是有意的保守取舍——清洗的目的是去掉「不该显示的东西」，
// 而不是变成一个不可靠的广告拦截器。
func (d *Document) isPixel(n *html.Node, opts SanitizeOptions) bool {
	w, h := getAttr(n, "width"), getAttr(n, "height")
	if (w == "0" || w == "1") && (h == "0" || h == "1") {
		return true
	}

	raw := getAttr(n, "src")
	if raw == "" {
		return false
	}
	// 相对地址要先解析，否则 /pixel.gif 拿不到 host 也就判断不了。
	resolved := raw
	if u, err := url.Parse(raw); err == nil && u.Scheme == "" && opts.Base != nil {
		resolved = opts.Base.ResolveReference(u).String()
	}

	u, err := url.Parse(resolved)
	if err != nil {
		return false
	}

	host := strings.ToLower(u.Hostname())
	for _, tracker := range trackerHosts {
		if host == tracker || strings.HasSuffix(host, "."+tracker) {
			return true
		}
	}

	// 路径特征：/pixel、/beacon、/track、/1x1 结尾且无扩展名。
	// 有扩展名说明是一个真实的图片文件（/track.png 也可能是图），
	// 不因为路径里出现 track 就误删。
	path := strings.ToLower(strings.TrimSuffix(u.Path, "/"))
	if path != "" && !strings.Contains(pathBase(path), ".") {
		for _, suffix := range []string{"/pixel", "/beacon", "/track", "/1x1"} {
			if strings.HasSuffix(path, suffix) {
				return true
			}
		}
	}
	return false
}

// pathBase 取路径最后一段。
func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i, a := range n.Attr {
		if a.Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func removeAttr(n *html.Node, key string) {
	out := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Key != key {
			out = append(out, a)
		}
	}
	n.Attr = out
}

// decodeDataURI 把 data:image/...;base64,... 解成字节。
// 上线前用于把内联图片也纳入统一的内容寻址存储。
func decodeDataURI(s string) ([]byte, string, bool) {
	if !strings.HasPrefix(strings.ToLower(s), "data:") {
		return nil, "", false
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, "", false
	}
	meta, payload := s[5:comma], s[comma+1:]
	if !strings.Contains(meta, "base64") {
		return nil, "", false
	}
	mime := strings.SplitN(meta, ";", 2)[0]
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", false
	}
	return raw, mime, true
}
