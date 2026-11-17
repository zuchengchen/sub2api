package service

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	openAIResponsesValidationMaxDetailBytes   = 4 * 1024
	openAIResponsesValidationPairingLimit     = 4096
	openAIResponsesValidationMaxCallIDBytes   = 4096
	openAIResponsesValidationSearchContextKey = "openai_responses_validation_search_sources"
)

// OpenAIResponsesValidationSearchSource describes an observed transformation,
// never a source inferred from the final request's tools.
type OpenAIResponsesValidationSearchSource uint8

const (
	OpenAIResponsesValidationSearchClient OpenAIResponsesValidationSearchSource = iota + 1
	OpenAIResponsesValidationSearchSynthesis
	OpenAIResponsesValidationSearchHistory
)

type openAIResponsesValidationSearchSources struct {
	client, synthesis, history bool
}

// ResetOpenAIResponsesValidationSearchSources starts attribution for one forward
// attempt. Missing context remains distinguishable from a tracked no-search request.
func ResetOpenAIResponsesValidationSearchSources(c *gin.Context) {
	if c != nil {
		c.Set(openAIResponsesValidationSearchContextKey, openAIResponsesValidationSearchSources{})
	}
}

func MarkOpenAIResponsesValidationSearchSource(c *gin.Context, source OpenAIResponsesValidationSearchSource) {
	if c == nil {
		return
	}
	value, _ := c.Get(openAIResponsesValidationSearchContextKey)
	sources, _ := value.(openAIResponsesValidationSearchSources)
	switch source {
	case OpenAIResponsesValidationSearchClient:
		sources.client = true
	case OpenAIResponsesValidationSearchSynthesis:
		sources.synthesis = true
	case OpenAIResponsesValidationSearchHistory:
		sources.history = true
	default:
		return
	}
	c.Set(openAIResponsesValidationSearchContextKey, sources)
}

type openAIResponsesValidationItemStats struct {
	Count    int `json:"count"`
	Bytes    int `json:"bytes"`
	MaxBytes int `json:"max_bytes"`
}

// This summary deliberately contains only fixed categories and counts. Even
// unknown item types are counted as "other", never copied from the request.
// It is retained with Ops errors regardless of verbose body logging settings.
type openAIResponsesValidationSummary struct {
	RequestBytes int `json:"request_bytes"`
	// The caller supplies structural Responses JSON. Request construction may
	// still alter metadata or encode it; this is not a measurement of wire bytes.
	RequestBytesBasis         string                                        `json:"request_bytes_basis"`
	HTTPRequestContentLength  *int64                                        `json:"http_request_content_length,omitempty"`
	HTTPContentEncoding       string                                        `json:"http_content_encoding,omitempty"`
	HTTPCompressionPolicy     string                                        `json:"http_request_compression_policy"`
	HTTPProtocol              string                                        `json:"http_protocol"`
	ResponseBytes             int                                           `json:"response_bytes"`
	Attempt                   int                                           `json:"attempt"`
	Transport                 string                                        `json:"transport"`
	WSHTTPBridge              bool                                          `json:"ws_http_bridge"`
	WSFallbackReason          string                                        `json:"ws_fallback_reason,omitempty"`
	InputKind                 string                                        `json:"input_kind"`
	InputItems                int                                           `json:"input_items"`
	MessageItems              int                                           `json:"message_items"`
	ReasoningItems            int                                           `json:"reasoning_items"`
	ToolCallItems             int                                           `json:"tool_call_items"`
	ToolOutputItems           int                                           `json:"tool_output_items"`
	ReferenceItems            int                                           `json:"reference_items"`
	OtherItems                int                                           `json:"other_items"`
	HasPreviousResponseID     bool                                          `json:"has_previous_response_id"`
	RejectedInputIndex        *int                                          `json:"rejected_input_index,omitempty"`
	TextFormat                string                                        `json:"text_format"`
	SearchPresent             bool                                          `json:"search_present"`
	SearchTopLevel            bool                                          `json:"search_top_level,omitempty"`
	SearchAdditionalTools     bool                                          `json:"search_additional_tools,omitempty"`
	SearchHistoryPresent      bool                                          `json:"search_history_present,omitempty"`
	SearchSources             []string                                      `json:"search_sources"`
	InputBytes                int                                           `json:"input_bytes"`
	MaxInputItemBytes         int                                           `json:"max_input_item_bytes"`
	InputTypes                map[string]openAIResponsesValidationItemStats `json:"input_types,omitempty"`
	MessageContentTypes       map[string]int                                `json:"message_content_types,omitempty"`
	ToolPairingScope          string                                        `json:"tool_pairing_scope,omitempty"`
	ToolPairingComplete       bool                                          `json:"tool_pairing_complete"`
	ToolPairingUntrackedItems int                                           `json:"tool_pairing_untracked_items,omitempty"`
	UnmatchedToolCalls        int                                           `json:"unmatched_tool_call_items,omitempty"`
	UnmatchedToolOutputs      int                                           `json:"unmatched_tool_output_items,omitempty"`
	ToolCallsWithoutID        int                                           `json:"tool_call_items_without_call_id,omitempty"`
	ToolOutputsWithoutID      int                                           `json:"tool_output_items_without_call_id,omitempty"`
	UpstreamCodeCategory      string                                        `json:"upstream_code_category,omitempty"`
	UpstreamParamCategory     string                                        `json:"upstream_param_category,omitempty"`
	SummaryTruncated          bool                                          `json:"summary_truncated,omitempty"`
}

func appendOpenAIResponsesValidationDiagnostic(c *gin.Context, account *Account, requestBody, responseBody []byte, resp *http.Response, attempt int) {
	if c == nil || account == nil || !account.IsOpenAI() || resp == nil || resp.StatusCode != http.StatusBadRequest {
		return
	}
	index, _, rejectedMessageID := openAIResponsesRejectedMessageID(responseBody)
	upstreamMessage := strings.TrimSpace(extractUpstreamErrorMessage(responseBody))
	jsonSearchConflict := strings.EqualFold(upstreamMessage, "Web Search cannot be used with JSON mode.") ||
		strings.EqualFold(upstreamMessage, "Web Search cannot be used with JSON mode")
	if !rejectedMessageID && !jsonSearchConflict && !strings.EqualFold(upstreamMessage, "Bad Request") {
		return
	}
	summary := openAIResponsesValidationSummary{
		RequestBytes: len(requestBody), ResponseBytes: len(responseBody), Attempt: attempt,
		Transport: "http", WSHTTPBridge: c.GetBool("openai_ws_http_bridge"),
		RequestBytesBasis: "responses_json", ToolPairingComplete: true,
		HTTPProtocol:          openAIValidationHTTPProtocol(resp.ProtoMajor),
		HTTPCompressionPolicy: openAIRequestCompressionPolicy(resp.Request),
	}
	if resp.Request != nil {
		// net/http treats zero length with a non-empty Body as unknown, not as
		// proof that no bytes were sent. Do not turn that into a false zero.
		if length := resp.Request.ContentLength; length > 0 || length == 0 && (resp.Request.Body == nil || resp.Request.Body == http.NoBody) {
			summary.HTTPRequestContentLength = &length
		}
		summary.HTTPContentEncoding = openAIValidationContentEncoding(resp.Request.Header.Get("Content-Encoding"))
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
	} else if jsonSearchConflict {
		reason = "json_search_conflict"
	}
	root := parseRawJSONView(requestBody)
	summary.TextFormat = openAIValidationTextFormat(root)
	summary.SearchTopLevel = openAIValidationToolsContainSearch(root.Get("tools"))
	summary.UpstreamCodeCategory = openAIValidationUpstreamCodeCategory(extractUpstreamErrorCode(responseBody))
	summary.UpstreamParamCategory = openAIValidationUpstreamParamCategory(gjson.GetBytes(responseBody, "error.param"))
	summary.HasPreviousResponseID = root.Get("previous_response_id").Type == gjson.String && root.Get("previous_response_id").String() != ""
	input := root.Get("input")
	summary.InputBytes = len(input.Raw)
	pairing := openAIValidationToolPairing{}
	switch {
	case input.IsArray():
		summary.InputKind = "array"
		input.ForEach(func(_, item gjson.Result) bool {
			summary.InputItems++
			itemType := openAIValidationInputType(item)
			if summary.InputTypes == nil {
				summary.InputTypes = make(map[string]openAIResponsesValidationItemStats)
			}
			stats := summary.InputTypes[itemType]
			stats.Count++
			stats.Bytes += len(item.Raw)
			stats.MaxBytes = max(stats.MaxBytes, len(item.Raw))
			summary.InputTypes[itemType] = stats
			summary.MaxInputItemBytes = max(summary.MaxInputItemBytes, len(item.Raw))
			switch itemType {
			case "message":
				summary.MessageItems++
				openAIValidationCountContent(&summary, item.Get("content"))
			case "reasoning":
				summary.ReasoningItems++
			case "function_call", "custom_tool_call", "tool_search_call", "local_shell_call", "computer_call", "shell_call", "apply_patch_call":
				summary.ToolCallItems++
			case "function_call_output", "custom_tool_call_output", "tool_search_output", "local_shell_call_output", "computer_call_output", "shell_call_output", "apply_patch_call_output":
				summary.ToolOutputItems++
			case "item_reference":
				summary.ReferenceItems++
			default:
				summary.OtherItems++
			}
			if itemType == "web_search_call" {
				summary.SearchHistoryPresent = true
			}
			if itemType == "additional_tools" && openAIValidationToolsContainSearch(item.Get("tools")) {
				summary.SearchAdditionalTools = true
			}
			pairing.add(item, itemType)
			return true
		})
	case input.Type == gjson.String:
		summary.InputKind = "string"
	case !input.Exists():
		summary.InputKind = "absent"
	default:
		summary.InputKind = "other"
	}
	pairing.apply(&summary)
	summary.SearchPresent = summary.SearchTopLevel || summary.SearchAdditionalTools
	summary.SearchSources = openAIValidationSearchSourceNames(c, summary.SearchPresent || summary.SearchHistoryPresent)
	detail, err := json.Marshal(summary)
	if err != nil {
		return
	}
	if len(detail) > openAIResponsesValidationMaxDetailBytes {
		// Fixed categories already bound cardinality. This additional hard guard
		// retains the aggregate counters if future fields exceed the size budget.
		summary.InputTypes = nil
		summary.MessageContentTypes = nil
		summary.SummaryTruncated = true
		detail, err = json.Marshal(summary)
		if err != nil || len(detail) > openAIResponsesValidationMaxDetailBytes {
			return
		}
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
		Kind: "request_validation", Stage: "forward", Reason: reason,
		Message: "Responses request validation failed", Detail: string(detail),
	})
}

func openAIValidationContentEncoding(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "identity":
		return "identity"
	case "gzip":
		return "gzip"
	case "deflate":
		return "deflate"
	case "br":
		return "br"
	case "zstd":
		return "zstd"
	default:
		return "other"
	}
}

func openAIValidationHTTPProtocol(major int) string {
	switch major {
	case 0:
		return "unknown"
	case 1:
		return "h1"
	case 2:
		return "h2"
	case 3:
		return "h3"
	default:
		return "other"
	}
}

func openAIValidationTextFormat(root gjson.Result) string {
	text := root.Get("text")
	if !text.Exists() {
		return "absent"
	}
	if !text.IsObject() {
		return "invalid"
	}
	format := text.Get("format")
	if !format.Exists() {
		return "absent"
	}
	if !format.IsObject() || format.Get("type").Type != gjson.String {
		return "invalid"
	}
	switch format.Get("type").String() {
	case "text":
		return "text"
	case "json_object":
		return "json_object"
	case "json_schema":
		return "json_schema"
	default:
		return "other"
	}
}

func openAIValidationToolsContainSearch(tools gjson.Result) bool {
	if !tools.IsArray() {
		return false
	}
	found := false
	tools.ForEach(func(_, tool gjson.Result) bool {
		// Unknown types never become diagnostic labels or wildcard matches.
		switch tool.Get("type").String() {
		case "web_search", "web_search_preview", "web_search_preview_2025_03_11":
			found = true
			return false
		default:
			return true
		}
	})
	return found
}

func openAIValidationSearchSourceNames(c *gin.Context, present bool) []string {
	names := make([]string, 0, 3)
	value, tracked := c.Get(openAIResponsesValidationSearchContextKey)
	sources, _ := value.(openAIResponsesValidationSearchSources)
	if sources.client {
		names = append(names, "client")
	}
	if sources.synthesis {
		names = append(names, "synthesis")
	}
	if sources.history {
		names = append(names, "history")
	}
	if !tracked || present && len(names) == 0 {
		names = append(names, "unknown")
	}
	return names
}

func openAIValidationInputType(item gjson.Result) string {
	if !item.IsObject() {
		return "other"
	}
	typeValue := item.Get("type")
	if typeValue.String() == "" && item.Get("role").Type == gjson.String && item.Get("content").Exists() {
		return "message"
	}
	switch typeValue.String() {
	case "message", "reasoning", "function_call", "custom_tool_call", "tool_search_call", "local_shell_call",
		"computer_call", "shell_call", "apply_patch_call", "mcp_call", "web_search_call", "file_search_call",
		"code_interpreter_call", "image_generation_call", "function_call_output", "custom_tool_call_output",
		"tool_search_output", "local_shell_call_output", "computer_call_output", "shell_call_output",
		"apply_patch_call_output", "item_reference", "additional_tools", "compaction", "compaction_trigger",
		"mcp_approval_request", "mcp_approval_response":
		return typeValue.String()
	case "tool_search_call_output":
		return "tool_search_output"
	default:
		return "other"
	}
}

func openAIValidationCountContent(summary *openAIResponsesValidationSummary, content gjson.Result) {
	if summary.MessageContentTypes == nil {
		summary.MessageContentTypes = make(map[string]int)
	}
	if content.Type == gjson.String {
		summary.MessageContentTypes["string"]++
		return
	}
	if !content.IsArray() {
		summary.MessageContentTypes["other"]++
		return
	}
	content.ForEach(func(_, part gjson.Result) bool {
		kind := "other"
		switch part.Get("type").String() {
		case "input_text", "output_text", "text", "input_image", "image_url", "input_file", "file",
			"input_audio", "audio", "output_audio", "refusal", "reasoning_text":
			kind = part.Get("type").String()
		}
		summary.MessageContentTypes[kind]++
		return true
	})
}

func openAIValidationUpstreamCodeCategory(code string) string {
	switch code {
	case "":
		return ""
	case "invalid_request_error", "invalid_json", "invalid_function_parameters", "invalid_value", "invalid_type",
		"unsupported_parameter", "unsupported_value", "context_length_exceeded", "invalid_encrypted_content":
		return code
	default:
		return "other"
	}
}

func openAIValidationUpstreamParamCategory(param gjson.Result) string {
	if !param.Exists() || param.Type == gjson.Null {
		return ""
	}
	if param.Type != gjson.String {
		return "other"
	}
	value := param.String()
	for _, category := range []string{"text.format", "response_format", "tools", "input", "tool_choice", "instructions", "previous_response_id", "model"} {
		if value == category || strings.HasPrefix(value, category+".") || strings.HasPrefix(value, category+"[") {
			return category
		}
	}
	return "other"
}

type openAIValidationToolPairingKey struct {
	family string
	digest [sha256.Size]byte
}

type openAIValidationToolPairing struct {
	items                        map[openAIValidationToolPairingKey][2]int
	incomplete                   bool
	missingCalls, missingOutputs int
	untrackedItems               int
}

func (p *openAIValidationToolPairing) add(item gjson.Result, itemType string) {
	family, output := "", false
	switch itemType {
	case "function_call", "custom_tool_call", "local_shell_call", "computer_call", "shell_call", "apply_patch_call", "tool_search_call":
		family = itemType
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output", "computer_call_output", "shell_call_output", "apply_patch_call_output":
		family, output = strings.TrimSuffix(itemType, "_output"), true
	case "tool_search_output":
		family, output = "tool_search_call", true
	default:
		return
	}
	id := item.Get("call_id")
	if len(id.Raw) > openAIResponsesValidationMaxCallIDBytes {
		p.incomplete = true
		p.untrackedItems++
		return
	}
	if id.Type != gjson.String || id.String() == "" {
		if output {
			p.missingOutputs++
		} else {
			p.missingCalls++
		}
		return
	}
	// Only bounded hashes survive this iteration. IDs are never stored in the
	// gin context, Ops event, summary or log, including on overflow.
	key := openAIValidationToolPairingKey{family: family, digest: sha256.Sum256([]byte(id.String()))}
	counts, exists := p.items[key]
	if !exists && len(p.items) >= openAIResponsesValidationPairingLimit {
		p.incomplete = true
		p.untrackedItems++
		return
	}
	if p.items == nil {
		p.items = make(map[openAIValidationToolPairingKey][2]int)
	}
	if output {
		counts[1]++
	} else {
		counts[0]++
	}
	p.items[key] = counts
}

func (p *openAIValidationToolPairing) apply(summary *openAIResponsesValidationSummary) {
	summary.ToolPairingScope = "request_input"
	summary.ToolPairingComplete = !p.incomplete
	summary.ToolPairingUntrackedItems = p.untrackedItems
	summary.ToolCallsWithoutID, summary.ToolOutputsWithoutID = p.missingCalls, p.missingOutputs
	for _, counts := range p.items {
		summary.UnmatchedToolCalls += max(counts[0]-counts[1], 0)
		summary.UnmatchedToolOutputs += max(counts[1]-counts[0], 0)
	}
}
