package middleware

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Logger 请求日志中间件
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 开始时间
		startTime := time.Now()

		// 请求路径
		path := c.Request.URL.Path

		// 处理请求
		c.Next()

		// 跳过健康检查等高频探针路径的日志
		if path == "/health" || path == "/setup/status" {
			return
		}

		endTime := time.Now()
		latency := endTime.Sub(startTime)

		method := c.Request.Method
		statusCode := c.Writer.Status()
		clientIP := ip.GetClientIP(c)
		protocol := c.Request.Proto
		accountID, hasAccountID := c.Request.Context().Value(ctxkey.AccountID).(int64)
		platform, _ := c.Request.Context().Value(ctxkey.Platform).(string)
		model, _ := c.Request.Context().Value(ctxkey.Model).(string)
		reason, rejected := GetIngressRejectReason(c)
		if rejected {
			recordIngressReject(c, reason)
			allowed, droppedSummary := globalIngressRejectAccessSampler.allow(endTime)
			if droppedSummary > 0 {
				logger.FromContext(c.Request.Context()).Info("ingress rejection access logs dropped",
					zap.String("component", "http.access"),
					zap.Uint64("dropped_count", droppedSummary),
					zap.Bool(logger.OpsSystemLogSkipField, true),
				)
			}
			if !allowed {
				return
			}
		}

		fields := []zap.Field{
			zap.String("component", "http.access"),
			zap.Int("status_code", statusCode),
			zap.Int64("latency_ms", latency.Milliseconds()),
			zap.String("client_ip", clientIP),
			zap.String("protocol", protocol),
			zap.String("method", method),
			zap.String("path", path),
		}
		if rejected {
			fields = append(fields,
				zap.String("ingress_reject_reason", string(reason)),
				zap.Bool(logger.OpsSystemLogSkipField, true),
			)
		}
		if hasAccountID && accountID > 0 {
			fields = append(fields, zap.Int64("account_id", accountID))
		}
		if platform != "" {
			fields = append(fields, zap.String("platform", platform))
		}
		if model != "" {
			fields = append(fields, zap.String("model", model))
		}

		fields = appendOpenAIWSIncrementalAccessFields(fields, c)

		l := logger.FromContext(c.Request.Context()).With(fields...)
		l.Info("http request completed", zap.Time("completed_at", endTime))

		if len(c.Errors) > 0 {
			l.Warn("http request contains gin errors", zap.String("errors", c.Errors.String()))
		}
	}
}

// Read only explicitly recorded lifecycle signals. Never infer an earlier
// attempt from final transport, HTTP success, or the current request payload.
func appendOpenAIWSIncrementalAccessFields(fields []zap.Field, c *gin.Context) []zap.Field {
	_, attemptedPresent := c.Get("openai_ws_auto_incremental_attempted")
	_, appliedPresent := c.Get("openai_ws_auto_incremental")
	if !attemptedPresent && !appliedPresent {
		return fields
	}
	attempted := c.GetBool("openai_ws_auto_incremental_attempted")
	applied := c.GetBool("openai_ws_auto_incremental")
	replay := c.GetBool("openai_ws_auto_incremental_replay")
	succeeded := attempted && applied && !replay && c.GetBool("openai_ws_auto_incremental_succeeded")
	fields = append(fields,
		zap.Bool("openai_ws_auto_incremental_attempted", attempted),
		zap.Bool("openai_ws_auto_incremental", applied),
		zap.Bool("openai_ws_auto_incremental_succeeded", succeeded),
		zap.Bool("openai_ws_auto_incremental_replay", replay),
		zap.String("openai_ws_auto_incremental_reason", openAIWSIncrementalAccessReason(c.GetString("openai_ws_auto_incremental_reason"))),
	)
	// The producer must supply an actual bounded item index. Absent metadata
	// stays absent; never derive it by inspecting/logging user history.
	if raw, exists := c.Get("openai_ws_auto_incremental_item_index"); exists {
		if index, valid := raw.(int); valid && index >= 0 && index < 2048 {
			fields = append(fields, zap.Int("openai_ws_auto_incremental_item_index", index))
			for _, key := range []string{"openai_ws_auto_incremental_item_type", "openai_ws_auto_incremental_previous_item_type"} {
				if _, exists := c.Get(key); exists {
					fields = append(fields, zap.String(key, openAIWSIncrementalAccessItemType(c.GetString(key))))
				}
			}
		}
	}
	return fields
}

func openAIWSIncrementalAccessReason(reason string) string {
	switch reason {
	case "disabled", "no_checkpoint", "no_chain", "scope_changed", "context_window_changed", "checkpoint_expired",
		"baseline_too_large", "account_mismatch", "no_previous_response_id", "request_property_changed", "input_shortened",
		"input_mismatch", "incomplete_tool_delta", "incomplete_tool_pairs", "compaction_boundary", "non_self_contained_input",
		"incremental", "prewarm", "http_fallback":
		return reason
	default:
		return "unknown"
	}
}

func openAIWSIncrementalAccessItemType(kind string) string {
	switch kind {
	case "message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output",
		"agent_message", "local_shell_call", "tool_search_call", "tool_search_output", "web_search_call", "image_generation_call",
		"compaction", "compaction_summary", "compaction_trigger", "context_compaction", "additional_tools", "configuration_update", "item_reference":
		return kind
	default:
		return "unknown"
	}
}
