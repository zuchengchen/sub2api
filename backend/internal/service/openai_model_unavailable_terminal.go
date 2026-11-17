package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openAIModelErrorResponses   = "responses"
	openAIModelErrorChat        = "chat"
	openAIModelErrorMessages    = "messages"
	openAIModelErrorWrittenTurn = "openai_model_error_written_turn"
)

// Only explicit upstream usage is returned. This helper does not estimate
// tokens, change replay eligibility, or replace an accumulated stream total.
func openAIModelUnavailableUsage(actualStatus int, body []byte) *OpenAIUsage {
	if !isOpenAIModelUnavailableError(actualStatus, body) {
		return nil
	}
	usage, ok := extractOpenAIUsageFromJSONBytes(body)
	if !ok || !openAIUsageHasTokens(&usage) {
		return nil
	}
	return &usage
}

func openAIModelUnavailablePartialResult(c *gin.Context, resp *http.Response, body []byte, model string) *OpenAIForwardResult {
	if resp == nil {
		return nil
	}
	usage := openAIModelUnavailableUsage(resp.StatusCode, body)
	if usage == nil {
		return nil
	}
	if observer := upstreamResponseModelObserverFromContext(c); observer != nil {
		observer.ObserveOpenAI(body, gjson.GetBytes(body, "type").String())
	}
	responseID := gjson.GetBytes(body, "response.id").String()
	if responseID == "" {
		responseID = gjson.GetBytes(body, "id").String()
	}
	return &OpenAIForwardResult{
		RequestID: firstNonEmpty(resp.Header.Get("x-request-id"), responseID), ResponseID: responseID,
		Usage: *usage, Model: model, BillingModel: model, UpstreamModel: model,
		UpstreamResponseModel: observedUpstreamResponseModel(c), UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
		UpstreamResponseServiceTier: observedUpstreamResponseServiceTier(c),
		UpstreamTerminalEvent:       "response.failed", ResponseHeaders: cloneHeader(resp.Header), UpstreamHeaders: cloneHeader(resp.Header),
	}
}

func stampOpenAIModelErrorPartialResult(c *gin.Context, result *OpenAIForwardResult, outboundBody []byte, originalModel, billingModel, upstreamModel string, stream bool, start time.Time) {
	if result == nil {
		return
	}
	result.Model, result.BillingModel, result.UpstreamModel = originalModel, billingModel, upstreamModel
	result.Stream, result.Duration = stream, time.Since(start)
	result.ServiceTier = resolvedOpenAIUpstreamServiceTier(c, extractOpenAIServiceTierFromBody(outboundBody))
	result.ReasoningEffort = ApplyThinkingEnabledFallback(extractOpenAIReasoningEffortFromBody(outboundBody, upstreamModel, billingModel, originalModel), outboundBody, upstreamModel)
	stampOpenAIResponsesUpstreamEndpoint(c, result)
}

func openAIModelUnavailableErrorObject() gin.H {
	return gin.H{"type": "invalid_request_error", "code": "model_not_found", "param": "model", "message": OpenAIModelUnavailableClientMessage}
}

func openAIModelErrorProtocol(c *gin.Context) string {
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path := strings.TrimSuffix(c.Request.URL.Path, "/")
		if strings.HasSuffix(path, "/messages") {
			return openAIModelErrorMessages
		}
		if strings.HasSuffix(path, "/chat/completions") {
			return openAIModelErrorChat
		}
	}
	return openAIModelErrorResponses
}

// Terminal-only writer: callers have already decided not to replay. A
// committed header is not an emitted error, so it must not suppress this first
// terminal. Its own per-turn marker prevents duplicate writes by this helper.
func writeOpenAIModelUnavailableTerminal(c *gin.Context, actualStatus int, protocol string) {
	if c == nil || c.Writer == nil {
		return
	}
	turnValue, _ := c.Get(OpsStreamTurnKey)
	turn, _ := turnValue.(int)
	if previous, written := c.Get(openAIModelErrorWrittenTurn); written && previous == turn {
		return
	}
	c.Set(openAIModelErrorWrittenTurn, turn)
	setOpsUpstreamError(c, actualStatus, OpenAIModelUnavailableClientMessage, "")
	keepalive := StopOpenAICompactSSEKeepaliveCommitted(c)
	MarkResponseCommitted(c)
	errorObject := openAIModelUnavailableErrorObject()
	if !c.Writer.Written() && !keepalive {
		envelope := gin.H{"error": errorObject}
		if protocol == openAIModelErrorMessages {
			envelope["type"] = "error"
		}
		c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusBadRequest, envelope)
		return
	}
	if protocol == openAIModelErrorResponses {
		writeOpenAIResponsesSSEFailure(c, http.StatusBadRequest, errorObject)
	} else {
		envelope := gin.H{"error": errorObject}
		prefix := "data: "
		if protocol == openAIModelErrorMessages {
			envelope["type"] = "error"
			prefix = "event: error\ndata: "
		}
		payload, _ := json.Marshal(envelope)
		if _, err := fmt.Fprintf(c.Writer, "%s%s\n\n", prefix, payload); err != nil {
			_ = c.Error(err)
		}
		c.Writer.Flush()
	}
	MarkOpsStreamFailure(c, "invalid_request_error", "model_not_found", OpenAIModelUnavailableClientMessage, http.StatusBadRequest)
}

func isOpenAIModelUnavailableTerminalEvent(payload []byte) bool {
	kind := gjson.GetBytes(payload, "type").String()
	if kind != "error" && kind != "response.failed" {
		return false
	}
	return isOpenAIModelUnavailableError(http.StatusBadRequest, payload)
}

// The client copy gets a fixed error object. Upstream payloads, usage, output,
// account side effects and retry classification continue to use the original.
func normalizeOpenAIModelUnavailableClientEvent(payload []byte) ([]byte, bool) {
	if !isOpenAIModelUnavailableTerminalEvent(payload) {
		return payload, false
	}
	path := "error"
	if gjson.GetBytes(payload, "response.error").IsObject() {
		path = "response.error"
	}
	errorObject := gjson.GetBytes(OpenAIModelUnavailableClientErrorBody(), "error").Raw
	updated, err := sjson.SetRawBytes(payload, path, []byte(errorObject))
	if err != nil {
		return payload, false
	}
	if gjson.GetBytes(payload, "type").String() == "error" {
		updated, err = sjson.SetBytes(updated, "status", http.StatusBadRequest)
		if err != nil {
			return payload, false
		}
		for key, value := range map[string]any{"status_code": http.StatusBadRequest, "message": OpenAIModelUnavailableClientMessage, "code": "model_not_found", "param": "model"} {
			if gjson.GetBytes(payload, key).Exists() {
				updated, err = sjson.SetBytes(updated, key, value)
				if err != nil {
					return payload, false
				}
			}
		}
	}
	return updated, true
}
