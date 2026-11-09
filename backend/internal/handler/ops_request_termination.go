package handler

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
)

const opsLocalRequestTerminationKey = "ops_local_request_termination"

// Record only the finite local context reason. A downstream cancellation can be
// caused by an intermediary; it does not prove that a user intentionally canceled.
func markOpsLocalRequestTermination(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	switch {
	case errors.Is(c.Request.Context().Err(), context.Canceled):
		c.Set(opsLocalRequestTerminationKey, "canceled")
	case errors.Is(c.Request.Context().Err(), context.DeadlineExceeded):
		c.Set(opsLocalRequestTerminationKey, "deadline_exceeded")
	}
}

func opsLocalRequestTerminationMessage(c *gin.Context, status int) string {
	if c == nil || status != statusClientClosedRequest {
		return ""
	}
	switch c.GetString(opsLocalRequestTerminationKey) {
	case "canceled":
		return "下游请求在响应发送前已取消 / Downstream request was canceled before a response was sent"
	case "deadline_exceeded":
		return "下游请求在响应发送前已超时 / Downstream request deadline expired before a response was sent"
	default:
		return ""
	}
}
