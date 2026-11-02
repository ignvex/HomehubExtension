package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/ignvex/HomehubExtension/internal/upstream"

	"github.com/gin-gonic/gin"
)

// handleFetch 代理取回一个订阅源的原始字节。
//
// 只取字节，不解析、不落库：入库与去重留在宿主。走错到
// rss.Service.FetchFeed 会让同一条内容在两个库各存一份，而且本服务的
// entries 里会混进它从未订阅过的源。
func (s *server) handleFetch(c *gin.Context) {
	rawURL := c.Query("url")
	if rawURL == "" {
		failWith(c, http.StatusBadRequest, "invalid_request", "缺少 url 参数")
		return
	}

	res, err := s.upstream.FetchFeed(c.Request.Context(), rawURL)
	if err != nil {
		switch {
		case errors.Is(err, upstream.ErrInvalidURL), errors.Is(err, upstream.ErrBlockedHost):
			// 原因是调用方可修的（协议、凭据、内网地址），回显有助定位。
			failWith(c, http.StatusBadRequest, "invalid_request", err.Error())
		case errors.Is(err, upstream.ErrResponseTooLarge):
			logFrom(c).Warn("fetch proxy response too large", "url", rawURL, "err", err)
			failWith(c, http.StatusBadGateway, "upstream_error", "上游响应超过大小上限")
		default:
			logFrom(c).Warn("fetch proxy failed", "url", rawURL, "err", err)
			failWith(c, http.StatusBadGateway, "upstream_error", "上游抓取失败")
		}
		return
	}

	if res.Status < 200 || res.Status >= 300 {
		// 上游状态码原样带回，宿主会写进 error_msg。429 的 Retry-After
		// 必须一起透传，否则宿主只能把限流看成「慢」。
		if res.RetryAfter != "" {
			c.Header("Retry-After", res.RetryAfter)
		}
		if res.Status == http.StatusNotModified {
			c.Status(http.StatusNotModified)
			return
		}
		if res.Status >= 300 && res.Status < 400 {
			// 本端不信重定向，但也不能把一个没有 Location 的 3xx 回给宿主：
			// 那既不是可跟随的跳转，也不是明确的失败。按上游故障处理。
			failWith(c, http.StatusBadGateway, "upstream_error",
				fmt.Sprintf("上游返回重定向 %d（不跟随）", res.Status))
			return
		}
		failWith(c, res.Status, "upstream_error",
			fmt.Sprintf("上游返回 HTTP %d", res.Status))
		return
	}

	contentType := res.ContentType
	if contentType == "" {
		contentType = "application/xml; charset=utf-8"
	}
	c.Data(res.Status, contentType, res.Body)
}

// handleAvatar 代理 Gravatar 头像。
//
// 304 与 ETag/Last-Modified 必须原样透传：宿主靠它们做增量刷新，
// 吞掉 304 自己回 200 会让每次刷新都重新下载整张图。
func (s *server) handleAvatar(c *gin.Context) {
	hash := c.Param("hash")
	if !upstream.ValidHash(hash) {
		failWith(c, http.StatusBadRequest, "invalid_request",
			"hash 必须是 32 位小写十六进制")
		return
	}
	size, err := upstream.ParseAvatarSize(c.Query("s"))
	if err != nil {
		failWith(c, http.StatusBadRequest, "invalid_request",
			"s 必须是整数（16–512，超出会夹紧）")
		return
	}

	res, err := s.upstream.FetchAvatar(c.Request.Context(), upstream.AvatarRequest{
		Hash:         hash,
		Size:         size,
		Default:      c.Query("d"),
		Rating:       c.Query("r"),
		ETag:         c.GetHeader("If-None-Match"),
		LastModified: c.GetHeader("If-Modified-Since"),
		Accept:       c.GetHeader("Accept"),
	})
	if err != nil {
		switch {
		case errors.Is(err, upstream.ErrInvalidHash), errors.Is(err, upstream.ErrInvalidSize):
			failWith(c, http.StatusBadRequest, "invalid_request", err.Error())
		case errors.Is(err, upstream.ErrResponseTooLarge):
			// 不记 hash：它是邮箱的 MD5，单独出现也能被字典反查。
			logFrom(c).Warn("avatar proxy response too large", "err", err)
			failWith(c, http.StatusBadGateway, "upstream_error", "头像响应超过大小上限")
		default:
			logFrom(c).Warn("avatar proxy failed", "err", err)
			failWith(c, http.StatusBadGateway, "upstream_error", "头像上游请求失败")
		}
		return
	}

	switch res.Status {
	case http.StatusNotModified:
		// 304 必须带验证器：宿主靠它们判断「还是同一张图」。
		copyAvatarValidators(c, res)
		c.Status(http.StatusNotModified)
	case http.StatusNotFound:
		failWith(c, http.StatusNotFound, "not_found", "上游没有该头像")
	case http.StatusOK:
		// 验证器与 Cache-Control 只跟随成功响应。404/502 的错误体
		// 若带着上游的 ETag，宿主可能把它当成一次有效缓存命中。
		copyAvatarValidators(c, res)
		if res.CacheControl != "" {
			c.Header("Cache-Control", res.CacheControl)
		}
		contentType := res.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		c.Data(http.StatusOK, contentType, res.Body)
	default:
		// 除 304/404 外的非 2xx 一律归类为上游故障，宿主据此记失败
		// 并写负缓存，不需要知道上游到底回了什么。
		failWith(c, http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("头像上游返回 HTTP %d", res.Status))
	}
}

func copyAvatarValidators(c *gin.Context, res upstream.AvatarResponse) {
	if res.ETag != "" {
		c.Header("ETag", res.ETag)
	}
	if res.LastModified != "" {
		c.Header("Last-Modified", res.LastModified)
	}
}
