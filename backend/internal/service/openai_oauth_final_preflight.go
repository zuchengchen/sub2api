package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

const openAIFinalProtocolDiagnosticKey = "openai_final_protocol_diagnostic"
const openAIClientToolsObservedKey = "openai_client_tools_observed"

// Only fixed categories survive request inspection. Tool names, schemas,
// arguments, history contents and arbitrary execution values are never retained.
type openAIFinalProtocolDiagnostic struct {
	FinalLite           bool     `json:"final_lite"`
	ToolSources         []string `json:"tool_sources"`
	ToolCarriers        []string `json:"tool_carriers"`
	ToolSearchExecution []string `json:"tool_search_execution"`
}

// Keep the existing API-key normalization contract. The new protocol preflight
// is OAuth-only, and the established GPT-5.5 compatibility step disables Lite.
func openAIResponsesLiteForNormalization(account *Account, model, header string, body []byte) bool {
	if account == nil || !account.IsOpenAI() {
		return false
	}
	if account.IsOpenAIOAuthLike() {
		return isOpenAIResponsesLiteFinalRequest(account, model, header, body)
	}
	return isOpenAIResponsesLiteHeader(header) || isOpenAIResponsesLiteWebSocketPayload(body)
}

func openAIRequestToolCategories(body []byte) (carriers, execution []string) {
	root := gjson.ParseBytes(body)
	carrierFlags := map[string]bool{}
	executionFlags := map[string]bool{}
	var lists []gjson.Result
	add := func(tools gjson.Result, carrier string) {
		if tools.IsArray() && len(tools.Array()) > 0 {
			carrierFlags[carrier] = true
			lists = append(lists, tools)
		}
	}
	add(root.Get("tools"), "top_level")
	if input := root.Get("input"); input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "additional_tools", "tool_search_output":
				add(item.Get("tools"), item.Get("type").String())
			}
			return true
		})
	}
	for len(lists) > 0 {
		tools := lists[len(lists)-1]
		lists = lists[:len(lists)-1]
		tools.ForEach(func(_, tool gjson.Result) bool {
			switch tool.Get("type").String() {
			case "namespace":
				add(tool.Get("tools"), "namespace")
				add(tool.Get("children"), "namespace")
			case "tool_search":
				value := tool.Get("execution")
				category := "invalid"
				switch {
				case !value.Exists():
					category = "missing"
				case value.Type == gjson.String && (value.String() == "client" || value.String() == "server"):
					category = value.String()
				}
				executionFlags[category] = true
			}
			return true
		})
	}
	for _, category := range []string{"top_level", "additional_tools", "tool_search_output", "namespace"} {
		if carrierFlags[category] {
			carriers = append(carriers, category)
		}
	}
	for _, category := range []string{"client", "server", "missing", "invalid"} {
		if executionFlags[category] {
			execution = append(execution, category)
		}
	}
	if len(carriers) == 0 {
		carriers = []string{"none"}
	}
	if len(execution) == 0 {
		execution = []string{"absent"}
	}
	return carriers, execution
}

func recordOpenAIFinalProtocolDiagnostic(c *gin.Context, account *Account, model, liteHeader string, body []byte) openAIFinalProtocolDiagnostic {
	carriers, execution := openAIRequestToolCategories(body)
	d := openAIFinalProtocolDiagnostic{
		FinalLite:    isOpenAIResponsesLiteFinalRequest(account, model, liteHeader, body),
		ToolCarriers: carriers, ToolSearchExecution: execution,
	}
	if c != nil {
		if c.GetBool(openAIClientToolsObservedKey) {
			d.ToolSources = append(d.ToolSources, "client")
		}
		value, _ := c.Get(openAIResponsesValidationSearchContextKey)
		sources, _ := value.(openAIResponsesValidationSearchSources)
		if sources.synthesis {
			d.ToolSources = append(d.ToolSources, "synthesis")
		}
		if sources.history {
			d.ToolSources = append(d.ToolSources, "history")
		}
	}
	if len(d.ToolSources) == 0 {
		source := "unknown"
		if len(carriers) == 1 && carriers[0] == "none" {
			source = "none"
		}
		d.ToolSources = []string{source}
	}
	if c != nil {
		c.Set(openAIFinalProtocolDiagnosticKey, d)
	}
	logger.L().Debug("openai final protocol preflight", zap.Bool("final_lite", d.FinalLite),
		zap.Strings("tool_sources", d.ToolSources), zap.Strings("tool_carriers", d.ToolCarriers),
		zap.Strings("tool_search_execution", d.ToolSearchExecution))
	return d
}

func validateOpenAIOAuthFinalRequest(c *gin.Context, account *Account, model, liteHeader string, body []byte) error {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return nil
	}
	d := recordOpenAIFinalProtocolDiagnostic(c, account, model, liteHeader, body)
	err := validateOpenAIResponsesLiteFinalRequest(account, model, liteHeader, body)
	if err == nil {
		err = validateOpenAIOAuthFinalReasoning(account, model, body)
	}
	if err != nil && c != nil {
		detail, _ := json.Marshal(d)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
			Kind: "request_validation", Stage: "preflight", Reason: "unsupported_protocol_parameter",
			Message: "Request parameter is incompatible with the final upstream protocol", Detail: string(detail),
		})
	}
	return err
}

func openAIProtocolValidationParam(err error) string {
	var contractErr *apicompat.RequestContractError
	if errors.As(err, &contractErr) {
		return contractErr.Param
	}
	var liteErr *openAIResponsesLiteValidationError
	if errors.As(err, &liteErr) {
		return liteErr.param
	}
	var effortErr *openAIReasoningEffortValidationError
	if errors.As(err, &effortErr) {
		return effortErr.param
	}
	return "tools"
}

func writeOpenAIProtocolValidationError(c *gin.Context, err error, anthropic bool) {
	setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
	errorBody := gin.H{"type": "invalid_request_error", "code": "unsupported_parameter", "param": openAIProtocolValidationParam(err), "message": err.Error()}
	if anthropic {
		MarkResponseCommitted(c)
		c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": errorBody})
		return
	}
	writeOpenAIResponsesErrorJSON(c, http.StatusBadRequest, gin.H{"error": errorBody})
}

func buildOpenAIProtocolValidationWSEvent(err error) []byte {
	data, _ := json.Marshal(gin.H{"type": "error", "status": http.StatusBadRequest,
		"error": gin.H{"type": "invalid_request_error", "code": "unsupported_parameter", "param": openAIProtocolValidationParam(err), "message": err.Error()}})
	return data
}

func (s *OpenAIGatewayService) writeOpenAIProtocolValidationWS(ctx context.Context, c *gin.Context, conn *coderws.Conn, hooks *OpenAIWSIngressHooks, err error) error {
	setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
	writeCtx, cancel := newOpenAIWSDownstreamWriteContext(ctx, hooks, s.openAIWSWriteTimeout())
	defer cancel()
	if writeErr := conn.Write(writeCtx, coderws.MessageText, buildOpenAIProtocolValidationWSEvent(err)); writeErr != nil {
		return writeErr
	}
	return NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "invalid request parameter", err)
}

// Model mapping and configured reasoning policy must precede this function.
// Preserve the caller's bytes on failure; an explicit declaration is never
// dropped to make a Lite attempt acceptable.
func prepareOpenAIFinalWSProtocol(c *gin.Context, account *Account, model string, body []byte) ([]byte, error) {
	if account != nil && account.IsOpenAIOAuthLike() && strings.TrimSpace(model) == "gpt-5.5" && isOpenAIResponsesLiteWebSocketPayload(body) {
		out, err := sjson.DeleteBytes(body, "client_metadata."+responsesLiteWSMetadataKey)
		if err != nil {
			return body, err
		}
		body = out
	}
	if err := validateOpenAIOAuthFinalRequest(c, account, model, "", body); err != nil {
		return body, err
	}
	if openAIResponsesLiteForNormalization(account, model, "", body) {
		out, _, err := normalizeOpenAIResponsesLitePayloadForAccount(body, account)
		if err != nil {
			return body, err
		}
		body = out
	}
	return body, nil
}
