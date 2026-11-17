package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// handleOpenAIHTTP200JSONFailure prevents an actual structured error carried
// by a successful transport status from entering success/usage-estimate paths.
// JSON inside ordinary response text is never inspected as an error envelope.
// The bool is false for ordinary Responses JSON, including error:null.
func (s *OpenAIGatewayService) handleOpenAIHTTP200JSONFailure(
	c *gin.Context, resp *http.Response, account *Account, body []byte, passthrough bool, model string,
) (*OpenAIUsage, error, bool) {
	root := parseRawJSONView(body)
	if !root.IsObject() {
		return nil, nil, false
	}
	typ := root.Get("type").String()
	errorValue := root.Get("error")
	nestedError := root.Get("response.error")
	hasError := errorValue.Type != gjson.Null && errorValue.Exists()
	// Do not let error:null in a duplicate key hide a later error object.
	root.ForEach(func(key, value gjson.Result) bool {
		if key.Str == "error" && value.Type != gjson.Null {
			hasError = true
		}
		return !hasError
	})
	if !hasError &&
		(nestedError.Type == gjson.Null || !nestedError.Exists()) &&
		typ != "error" && typ != "response.failed" && !strings.HasSuffix(typ, "_error") &&
		root.Get("status").String() != "failed" {
		return nil, nil, false
	}
	upstreamError, normalized := normalizeOpenAIUpstreamError(body)
	status, message := http.StatusBadGateway, "Upstream returned an invalid JSON error response"
	errorBody := gin.H{"type": "upstream_error", "message": message}
	var usage *OpenAIUsage
	if normalized {
		if parsed, ok := extractOpenAIUsageFromJSONBytes(body); ok && openAIUsageHasTokens(&parsed) {
			usage = &parsed
		}
		markOpenAICyberPolicyEvent(c, body, resp.StatusCode, usage)
		var guard openAIResponseReplayGuard
		guard.Observe(body, "response.failed")
		// A valid JSON error can use the exact same targeted-repair signal as
		// a failed SSE terminal. Full original JSON supplies progress evidence;
		// only its canonical error is wrapped for the repair parser.
		if guard.CanReplay(c) && !openAIStreamClientOutputStarted(c, false) && !IsResponseCommitted(c) {
			failed := append([]byte(`{"type":"response.failed",`), upstreamError.ErrorBody[1:]...)
			if retry := newOpenAIRejectedFieldStreamRetrySignal(c, failed); retry != nil {
				return nil, retry, true
			}
		}
		status = openAIStreamFailureStatus(body, upstreamError.Message)
		if isOpenAIModelUnavailableError(http.StatusBadRequest, body) {
			writeOpenAIModelUnavailableTerminal(c, resp.StatusCode, openAIModelErrorResponses)
			return usage, fmt.Errorf("upstream model unavailable"), true
		}
		redacted := s.redactAgentIdentitySensitiveBody(c.Request.Context(), account, upstreamError.ErrorBody)
		if safe, ok := normalizeOpenAIUpstreamError(redacted); ok {
			message = sanitizeUpstreamErrorMessage(strings.TrimSpace(safe.Message))
			if message == "" {
				message = "Upstream rejected the request"
			}
			errorBody = gin.H{"type": "upstream_error", "message": message}
			if status == http.StatusBadRequest {
				errorBody["type"] = "invalid_request_error"
			}
			for key, value := range map[string]string{"type": safe.Type, "code": safe.Code, "param": safe.Param} {
				if value != "" {
					errorBody[key] = value
				}
			}
		}
	}
	// A contradictory/duplicate/oversized envelope is a protocol failure, not
	// permission to repair it or to manufacture billable usage from one view.
	diagnostic, _ := json.Marshal(gin.H{"error": errorBody, "status_code": status})
	s.recordOpenAIStreamUpstreamError(c, account, passthrough, resp.Header.Get("x-request-id"), "http_error", diagnostic, message)
	setOpsUpstreamError(c, status, message, "")
	keepaliveCommitted := StopOpenAICompactSSEKeepaliveCommitted(c)
	if c.Writer.Written() || keepaliveCommitted {
		writeOpenAIResponsesSSEFailure(c, status, errorBody)
	} else {
		writeOpenAIResponsesErrorJSON(c, status, gin.H{"error": errorBody})
	}
	if GetOpsCyberPolicy(c) != nil {
		usage = nil
	}
	return usage, fmt.Errorf("upstream JSON response failed: %s", message), true
}
