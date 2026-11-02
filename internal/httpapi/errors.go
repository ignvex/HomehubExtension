package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ignvex/HomehubExtension/internal/auth"
	"github.com/ignvex/HomehubExtension/internal/paste"
	"github.com/ignvex/HomehubExtension/internal/store"

	"github.com/gin-gonic/gin"
)

// ErrorBody 是统一错误响应。客户端只需要认 code。
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 是统一错误响应的主体。RequestID 可用来查服务端日志。
type ErrorDetail struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	Details   []FieldError `json:"details,omitempty"`
	RequestID string       `json:"request_id,omitempty"`
}

// FieldError 指出具体哪个字段有问题，供前端高亮。
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// codeFor 把内部错误映射成对外 code 与状态码。
//
// 关键原则：5xx 不回显内部错误文本。数据库路径、SQL 片段、文件系统细节
// 都不该出现在响应里——需要排查时用 X-Request-ID 去查日志。
func codeFor(err error) (int, string, string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not_found", "资源不存在"
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict, "conflict", "资源已存在"
	case errors.Is(err, store.ErrGone):
		return http.StatusGone, "gone", "资源已失效"
	case errors.Is(err, paste.ErrExpired):
		return http.StatusGone, "gone", "该片段已过期或已被读取"
	case errors.Is(err, paste.ErrBadPassword):
		return http.StatusForbidden, "wrong_password", "口令不正确"
	case errors.Is(err, paste.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "payload_too_large", err.Error()
	case errors.Is(err, paste.ErrBadExpiry):
		return http.StatusBadRequest, "invalid_request", err.Error()
	case errors.Is(err, paste.ErrForbidden):
		return http.StatusForbidden, "forbidden", "公开片段已被禁用"
	case errors.Is(err, auth.ErrBadCredentials):
		return http.StatusUnauthorized, "unauthenticated", "凭证无效"
	case errors.Is(err, auth.ErrEnrollmentUsed):
		return http.StatusUnauthorized, "unauthenticated", "配对码已使用或已过期"
	case errors.Is(err, auth.ErrClockSkew):
		return http.StatusUnauthorized, "unauthenticated", "请求时间戳超出允许范围"
	case errors.Is(err, auth.ErrReplayed):
		return http.StatusUnauthorized, "unauthenticated", "请求已被处理过"
	case errors.Is(err, auth.ErrBadSig), errors.Is(err, auth.ErrBadKey):
		return http.StatusUnauthorized, "unauthenticated", "签名校验失败"
	case errors.Is(err, auth.ErrNoHubKey):
		// 这是部署问题（漏配公钥），不是请求方的问题，所以用 503 而不是 4xx。
		return http.StatusServiceUnavailable, "unavailable", "Homehub 签名通道未配置"
	}
	return http.StatusInternalServerError, "internal", "服务器内部错误"
}

func fail(c *gin.Context, err error) {
	status, code, msg := codeFor(err)
	if status >= 500 {
		// 详细原因只进日志。
		logFrom(c).Error("request failed", "err", err, "path", c.FullPath())
	}
	c.AbortWithStatusJSON(status, ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   msg,
		RequestID: requestID(c),
	}})
}

func failWith(c *gin.Context, status int, code, msg string, details ...FieldError) {
	c.AbortWithStatusJSON(status, ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   msg,
		Details:   details,
		RequestID: requestID(c),
	}})
}

// bindJSON 统一处理绑定错误。
func bindJSON(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		// 超限要报 413 而不是 400：前者是「太大了」，后者是「格式错了」，
		// 客户端据此决定「改小重发」还是「修格式」。
		// MaxBytesReader 只在 chunked 或声明长度不为 0 时才会真正生效，
		// 但两种路径的错误都包成 *http.MaxBytesError。
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			failWith(c, http.StatusRequestEntityTooLarge, "payload_too_large", "请求体过大")
			return false
		}
		failWith(c, http.StatusBadRequest, "invalid_request", "请求体格式不正确",
			FieldError{Field: "body", Reason: cleanBindError(err)})
		return false
	}
	return true
}

// cleanBindError 去掉 gin 绑定的冗长类型名，只留可操作的部分。
func cleanBindError(err error) string {
	msg := err.Error()
	msg = strings.ReplaceAll(msg, "json: cannot unmarshal ", "字段类型错误: ")
	msg = strings.ReplaceAll(msg, " into Go struct field ", " -> ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
