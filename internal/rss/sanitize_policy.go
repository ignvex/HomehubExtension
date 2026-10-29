package rss

// HTML 清洗策略表。
//
// 与执行逻辑分开：这里是「允许什么」，sanitize.go 是「怎么执行」。
// 日常维护（加标签、加属性、加追踪域名）只改本文件，不需要碰遍历代码，
// 也让审查者能一屏看清整个信任边界。

// 允许保留的元素。不在这个集合里、也不在 dropElements 里的标签会被"脱壳"：
// 去掉标签本身，保留子节点。这样 <custom-tag>正文</custom-tag> 不会丢内容。
var allowedElements = map[string]bool{
	"p": true, "div": true, "span": true, "br": true, "hr": true,
	"a": true, "img": true, "picture": true, "source": true,
	"figure": true, "figcaption": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"ul": true, "ol": true, "li": true, "dl": true, "dt": true, "dd": true,
	"blockquote": true, "q": true, "cite": true,
	"pre": true, "code": true, "kbd": true, "samp": true, "var": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true,
	"th": true, "td": true, "caption": true, "colgroup": true, "col": true,
	"strong": true, "em": true, "b": true, "i": true, "u": true,
	"s": true, "del": true, "ins": true, "mark": true, "small": true,
	"sub": true, "sup": true, "abbr": true,
	"details": true, "summary": true,
	"video": true, "audio": true, "track": true,
}

// 整棵子树丢弃。这些元素要么执行代码，要么必然发起第三方请求。
var dropElements = map[string]bool{
	"script": true, "noscript": true, "style": true, "link": true,
	"iframe": true, "frame": true, "frameset": true,
	"object": true, "embed": true, "applet": true,
	"form": true, "input": true, "button": true, "select": true,
	"textarea": true, "label": true, "option": true,
	"meta": true, "base": true, "title": true, "head": true,
	"svg": true, "math": true, "template": true,
	"canvas": true, "map": true, "area": true,
}

// 属性白名单。全局属性单独处理。
var allowedAttrs = map[string]map[string]bool{
	"a": {"href": true, "title": true},
	"img": {
		"src": true, "srcset": true, "alt": true,
		"width": true, "height": true, "loading": true, "decoding": true,
	},
	"source": {"src": true, "srcset": true, "type": true, "media": true},
	"video": {
		"src": true, "poster": true, "controls": true,
		"width": true, "height": true, "preload": true, "muted": true, "loop": true,
	},
	"audio":   {"src": true, "controls": true, "preload": true, "loop": true},
	"track":   {"src": true, "kind": true, "srclang": true, "label": true, "default": true},
	"td":      {"colspan": true, "rowspan": true},
	"th":      {"colspan": true, "rowspan": true, "scope": true},
	"ol":      {"start": true, "reversed": true},
	"details": {"open": true},
}

var globalAttrs = map[string]bool{"id": true, "lang": true, "dir": true}

// 已知追踪域名。命中即视为像素并移除。
var trackerHosts = []string{
	"google-analytics.com",
	"doubleclick.net",
	"googletagmanager.com",
	"facebook.com",
	"pixel.wp.com",
	"stats.wp.com",
	"scorecardresearch.com",
	"quantserve.com",
}
