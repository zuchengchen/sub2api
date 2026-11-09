package service

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// chatDelivery observes the real Chat consumer and successful downstream writes,
// independently of the upstream diagnostic reader. It does not assert that a
// remote client read the bytes. Join its response ID and usage with the upstream
// terminal record before treating a post-terminal read error as settled.
func (a *userCacheDiagnosticAttempt) chatDelivery(c *gin.Context, delivery *NativeReasoningChatCapture, result *OpenAIForwardResult, forwardErr error, native *nativeReasoningChat) {
	if a == nil || a.diagnostic == nil || a.attempt < 1 || a.attempt > userCacheDiagnosticMaxAttempts || c == nil || c.Request == nil {
		return
	}
	d := a.diagnostic
	fields := map[string]any{
		"schema": 1, "forward_returned_success": forwardErr == nil && result != nil && GetOpsCyberPolicy(c) == nil,
		"context_canceled":  c.Request.Context().Err() != nil,
		"downstream_status": c.Writer.Status(), "downstream_delivered_complete": false,
		"native_mode": NativeReasoningReplayModeOff, "native_eligible": false, "restored_groups": 0,
	}
	if native != nil {
		fields["native_mode"], fields["native_eligible"], fields["restored_groups"] = native.mode, native.eligible, len(native.references)
	}
	if delivery != nil {
		// Use the capture's writer lock order; snapshots must not race writes.
		delivery.writeMu.Lock()
		delivery.mu.Lock()
		if delivery.ResponseWriter != nil && delivery.ResponseWriter.Status() == 200 {
			assistant, complete := delivery.messageLocked()
			fields["downstream_delivered_complete"] = complete
			if complete {
				responseID := delivery.chat.id
				if !delivery.stream {
					responseID = gjson.GetBytes(delivery.buffer, "id").String()
				}
				if responseID != "" {
					fields["consumer_response_id_sha256"] = diagnosticSHA256([]byte(responseID))
				}
				fields["delivered_assistant"] = summarizeUserCacheValue(assistant)
			}
		}
		delivery.mu.Unlock()
		delivery.writeMu.Unlock()
	}
	usageID := a.usageRequestIDHash
	if result != nil {
		fields["consumer_usage"] = result.Usage
		fields["stream"] = result.Stream
		if usageID == "" && strings.TrimSpace(result.RequestID) != "" {
			usageID = diagnosticSHA256([]byte(result.RequestID))
		}
	}
	d.write("chat_delivery", a.accountID, a.attempt, usageID, fields)
}
