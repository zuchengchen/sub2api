package service

import "github.com/gin-gonic/gin"

// CanRetain bounds in-flight WebSocket input across account retries.
func (o *ResponsesCacheDiagnosticObserver) CanRetain(raw []byte) bool {
	if o == nil || len(raw) == 0 || len(raw) > userCacheDiagnosticMaxBytes {
		return false
	}
	_, active := o.window.admit(false)
	return active
}

// RecordWSClientTurn identifies the accepted frame on this client socket.
// A new connection has its own numbering even with the same session header.
func (d *ResponsesCacheDiagnostic) RecordWSClientTurn(turn int) {
	if d == nil || turn < 1 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.finished {
		d.event["ws_client_turn"] = turn
	}
}

// The HTTP-to-WS forwarder publishes its own checkpoint decision. Other
// transports do not run this algorithm; their samples omit this field.
func (d *ResponsesCacheDiagnostic) ObserveHTTPIncremental(c *gin.Context, result *OpenAIForwardResult) {
	if d == nil || c == nil || result == nil || !result.OpenAIWSMode {
		return
	}
	reason := c.GetString("openai_ws_auto_incremental_reason")
	switch reason {
	case "":
		return
	case "disabled", "prewarm", "incremental", "no_checkpoint", "scope_changed", "context_window_changed",
		"checkpoint_expired", "no_chain", "baseline_too_large", "account_mismatch", "no_previous_response_id",
		"request_property_changed", "input_shortened", "input_mismatch", "incomplete_tool_delta",
		"compaction_boundary", "non_self_contained_input":
	default:
		reason = "other"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.finished {
		d.event["http_ws_incremental"] = map[string]any{
			"reason": reason, "succeeded": c.GetBool("openai_ws_auto_incremental_succeeded"),
			"attempted_during_request": c.GetBool("openai_ws_auto_incremental_attempted"),
		}
	}
}
