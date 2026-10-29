package rss

// 订阅源的纯函数工具：退避计算、GUID 归一化、字段截断。
//
// 这些都不依赖 Service 状态，单独放一个文件便于测试，
// 也让 fetcher.go 只保留抓取编排本身。

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

// nextInterval 实现指数退避：失败次数越多间隔越长，上限 max_interval。
func nextInterval(baseSeconds int64, errorCount int, cfg Config) time.Duration {
	base := time.Duration(baseSeconds) * time.Second
	if base <= 0 {
		base = cfg.DefaultInterval
	}
	min := cfg.MinInterval
	max := cfg.MaxInterval
	if min <= 0 {
		min = 10 * time.Minute
	}
	if max <= 0 {
		max = 6 * time.Hour
	}
	if base < min {
		base = min
	}
	if errorCount <= 0 {
		return base
	}
	d := base
	for i := 1; i < errorCount && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

func itemGUID(item *gofeed.Item) string {
	if g := strings.TrimSpace(item.GUID); g != "" {
		return g
	}
	if item.Link != "" {
		return normalizeLink(item.Link)
	}
	sum := sha256.Sum256([]byte(item.Title + "\n" + item.Content + "\n" + item.Description))
	return hex.EncodeToString(sum[:16])
}

// normalizeLink 去掉 fragment 与常见跟踪参数，避免同一文章因 utm 参数被当成两条。
func normalizeLink(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	u.Fragment = ""
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || lk == "fbclid" || lk == "gclid" || lk == "ref" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func authorOf(item *gofeed.Item) string {
	if len(item.Authors) > 0 && item.Authors[0] != nil {
		return truncateString(item.Authors[0].Name, 200)
	}
	if item.Author != nil {
		return truncateString(item.Author.Name, 200)
	}
	return ""
}

func truncateString(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func ptr[T any](v T) *T { return &v }
