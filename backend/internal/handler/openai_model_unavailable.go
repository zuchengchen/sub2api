package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// This consumes a classifier hint only at the terminal boundary. Account
// selection, retry budgets and progress/cancellation guards remain upstream.
func writeOpenAIModelUnavailableExhausted(c *gin.Context, failoverErr *service.UpstreamFailoverError, streamStarted, anthropic bool) bool {
	if !service.IsOpenAIModelUnavailableFailover(failoverErr) {
		return false
	}
	if c == nil || c.Writer == nil {
		return true
	}
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		streamStarted = true
	}
	imagePadding := service.StopOpenAIImagesJSONKeepaliveCommitted(c)
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	if service.IsResponseCommitted(c) {
		return true
	}
	if _, recorded := service.GetOpsStreamError(c); recorded && c.Writer.Written() {
		// A service has already emitted the authoritative error. Do not append
		// another terminal or replace its root cause with exhaustion metadata.
		return true
	}
	service.SetOpsUpstreamError(c, failoverErr.StatusCode, service.OpenAIModelUnavailableClientMessage, "")
	streamStarted = streamStarted || c.Writer.Written()
	body := service.OpenAIModelUnavailableClientErrorBody()
	errorObject := json.RawMessage(gjson.GetBytes(body, "error").Raw)
	if anthropic {
		body, _ = json.Marshal(gin.H{"type": "error", "error": errorObject})
	}
	service.MarkResponseCommitted(c)
	if imagePadding {
		service.MarkOpsStreamErrorValue(c, service.OpsStreamError{
			ErrType: "invalid_request_error", Code: "model_not_found", Message: service.OpenAIModelUnavailableClientMessage,
			IntendedStatus: http.StatusBadRequest, CountTowardsSLA: true, NonStream: true,
		})
		c.Data(c.Writer.Status(), "application/json; charset=utf-8", body)
		return true
	}
	if !streamStarted {
		c.Data(http.StatusBadRequest, "application/json; charset=utf-8", body)
		return true
	}

	service.MarkOpsStreamFailure(c, "invalid_request_error", "model_not_found", service.OpenAIModelUnavailableClientMessage, http.StatusBadRequest)
	if !c.Writer.Written() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
	}
	event := "error"
	if !anthropic && inboundIsResponses(c) {
		event = "response.failed"
		body, _ = json.Marshal(gin.H{
			"type": "response.failed", "sequence_number": 0,
			"response": gin.H{
				"id": synthesizeResponseID(c), "object": "response", "created_at": time.Now().Unix(),
				"model": requestModel(c), "status": "failed", "output": []any{}, "error": errorObject,
			},
		})
	}
	if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, body); err != nil {
		_ = c.Error(err)
		return true
	}
	c.Writer.Flush()
	return true
}

// The error frame is the useful protocol response; the policy close terminates
// the session without disguising deterministic model rejection as a retryable
// transport failure. Only the actual upstream status is retained in Ops.
func closeOpenAIWSModelUnavailable(c *gin.Context, conn *coderws.Conn, failoverErr *service.UpstreamFailoverError) bool {
	if !service.IsOpenAIModelUnavailableFailover(failoverErr) {
		return false
	}
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	if ctx.Err() != nil {
		closeOpenAIClientWS(conn, coderws.StatusPolicyViolation, "requested model unavailable")
		return true
	}
	alreadySent := c != nil && service.IsResponseCommitted(c)
	if c != nil {
		if previous, recorded := service.GetOpsStreamError(c); recorded {
			turnValue, _ := c.Get(service.OpsStreamTurnKey)
			turn, _ := turnValue.(int)
			alreadySent = alreadySent || previous.Turn == turn
		}
	}
	if !alreadySent {
		service.SetOpsUpstreamError(c, failoverErr.StatusCode, service.OpenAIModelUnavailableClientMessage, "")
		service.MarkOpsStreamFailure(c, "invalid_request_error", "model_not_found", service.OpenAIModelUnavailableClientMessage, http.StatusBadRequest)
		if c != nil {
			service.MarkResponseCommitted(c)
		}
		if conn != nil {
			errorObject := json.RawMessage(gjson.GetBytes(service.OpenAIModelUnavailableClientErrorBody(), "error").Raw)
			payload, _ := json.Marshal(gin.H{"type": "error", "status": http.StatusBadRequest, "error": errorObject})
			writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = conn.Write(writeCtx, coderws.MessageText, payload)
			cancel()
		}
	}
	closeOpenAIClientWS(conn, coderws.StatusPolicyViolation, "requested model unavailable")
	return true
}
