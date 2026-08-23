package middleware

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// GatewayConversationTracker 在 API Key 鉴权之后登记每条在途网关请求的
// 取消函数。cyber 处置禁用用户后调用 CancelAllForUser 即可立即终止该
// 用户全部在途 SSE/WebSocket/长请求，消除认证缓存 TTL 尾巴。
//
// 必须挂在 apiKeyAuth 之后、业务 handler 之前；仅覆盖 /v1 网关路由。
func GatewayConversationTracker() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}
		subject, ok := GetAuthSubjectFromContext(c)
		if !ok || subject.UserID <= 0 {
			c.Next()
			return
		}
		requestID := conversationTrackerRequestID(c)
		ctx, cancel := context.WithCancel(c.Request.Context())
		c.Request = c.Request.WithContext(ctx)
		registry := service.DefaultGatewayKillSwitchRegistry()
		registry.Register(subject.UserID, requestID, cancel)
		defer registry.Unregister(subject.UserID, requestID)
		c.Next()
	}
}

func conversationTrackerRequestID(c *gin.Context) string {
	if v, ok := c.Request.Context().Value(ctxkey.ClientRequestID).(string); ok && v != "" {
		return v
	}
	if v, ok := c.Request.Context().Value(ctxkey.RequestID).(string); ok && v != "" {
		return v
	}
	// clientRequestID 中间件保证网关路由总有 ID；此分支仅防御性兜底，
	// 用随机值避免同用户并发请求共享键导致误删登记。
	return "tracker:" + uuid.New().String()
}
