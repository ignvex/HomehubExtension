package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/gin-gonic/gin"
)

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
