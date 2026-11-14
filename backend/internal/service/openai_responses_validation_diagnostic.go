package service

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// This summary deliberately contains only fixed categories and counts. Even
// unknown item types are counted as "other", never copied from the request.
// It is retained with Ops errors regardless of verbose body logging settings.
type openAIResponsesValidationSummary struct {
	RequestBytes          int    `json:"request_bytes"`
	ResponseBytes         int    `json:"response_bytes"`
	Attempt               int    `json:"attempt"`
	Transport             string `json:"transport"`
	WSHTTPBridge          bool   `json:"ws_http_bridge"`
	WSFallbackReason      string `json:"ws_fallback_reason,omitempty"`
	InputKind             string `json:"input_kind"`
	InputItems            int    `json:"input_items"`
	MessageItems          int    `json:"message_items"`
	ReasoningItems        int    `json:"reasoning_items"`
	ToolCallItems         int    `json:"tool_call_items"`
	ToolOutputItems       int    `json:"tool_output_items"`
	ReferenceItems        int    `json:"reference_items"`
	OtherItems            int    `json:"other_items"`
	HasPreviousResponseID bool   `json:"has_previous_response_id"`
	RejectedInputIndex    *int   `json:"rejected_input_index,omitempty"`
}

func appendOpenAIResponsesValidationDiagnostic(c *gin.Context, account *Account, requestBody, responseBody []byte, resp *http.Response, attempt int) {
	if c == nil || account == nil || !account.IsOpenAI() || resp == nil || resp.StatusCode != http.StatusBadRequest {
		return
	}
	index, _, rejectedMessageID := openAIResponsesRejectedMessageID(responseBody)
	if !rejectedMessageID && !strings.EqualFold(strings.TrimSpace(extractUpstreamErrorMessage(responseBody)), "Bad Request") {
		return
	}
	summary := openAIResponsesValidationSummary{
		RequestBytes: len(requestBody), ResponseBytes: len(responseBody), Attempt: attempt,
		Transport: "http", WSHTTPBridge: c.GetBool("openai_ws_http_bridge"),
	}
	if reason := c.GetString("openai_ws_session_fallback_reason"); reason != "" {
		summary.WSFallbackReason = "other"
		if reason == "payload_too_large" {
			summary.WSFallbackReason = reason
		}
	}
	reason := "unspecified_bad_request"
	if rejectedMessageID {
		reason = "message_id_prefix"
		summary.RejectedInputIndex = &index
	}
	root := parseRawJSONView(requestBody)
	summary.HasPreviousResponseID = root.Get("previous_response_id").Type == gjson.String && root.Get("previous_response_id").String() != ""
	input := root.Get("input")
	switch {
	case input.IsArray():
		summary.InputKind = "array"
		input.ForEach(func(_, item gjson.Result) bool {
			summary.InputItems++
			switch item.Get("type").String() {
			case "message":
				summary.MessageItems++
			case "":
				if item.IsObject() && item.Get("role").Type == gjson.String && item.Get("content").Exists() {
					summary.MessageItems++
				} else {
					summary.OtherItems++
				}
			case "reasoning":
				summary.ReasoningItems++
			case "function_call", "custom_tool_call", "tool_search_call", "local_shell_call":
				summary.ToolCallItems++
			case "function_call_output", "custom_tool_call_output", "tool_search_output", "local_shell_call_output":
				summary.ToolOutputItems++
			case "item_reference":
				summary.ReferenceItems++
			default:
				summary.OtherItems++
			}
			return true
		})
	case input.Type == gjson.String:
		summary.InputKind = "string"
	case !input.Exists():
		summary.InputKind = "absent"
	default:
		summary.InputKind = "other"
	}
	detail, err := json.Marshal(summary)
	if err != nil {
		return
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
		Kind: "request_validation", Stage: "forward", Reason: reason,
		Message: "Responses request validation failed", Detail: string(detail),
	})
}
