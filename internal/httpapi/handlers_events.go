package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ignvex/HomehubExtension/internal/events"

	"github.com/gin-gonic/gin"
)

// handleEvents 是 SSE 长连接。
//
// 三个必须做对的细节：
//  1. 响应头 X-Accel-Buffering: no，配合 Caddy 的 flush_interval -1；
//  2. 每 20 秒发一次注释行，防止中间设备按空闲断开；
//  3. 客户端断开时立刻返回，不能在 select 里漏掉 ctx.Done。
func (s *server) handleEvents(c *gin.Context) {
	if s.Bus == nil {
		failWith(c, http.StatusServiceUnavailable, "unavailable", "事件总线未启动")
		return
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		failWith(c, http.StatusInternalServerError, "internal", "当前连接不支持流式响应")
		return
	}

	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	// 首帧注释让客户端立刻进入 open 状态，而不是等第一个事件。
	fmt.Fprint(c.Writer, ": connected\n\n")
	flusher.Flush()

	ch, cancel := s.Bus.Subscribe()
	defer cancel()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	ctx := c.Request.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			writeSSE(c, flusher, ev)
		case <-ping.C:
			fmt.Fprint(c.Writer, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(c *gin.Context, flusher http.Flusher, ev events.Event) {
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		return
	}
	fmt.Fprintf(c.Writer, "event: %s\nid: %d\ndata: %s\n\n", ev.Type, ev.ID, payload)
	flusher.Flush()
}
