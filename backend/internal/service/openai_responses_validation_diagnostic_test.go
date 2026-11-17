package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesValidationDiagnosticContainsOnlyStructuralMetadata(t *testing.T) {
	const secret = "private-conversation-and-credential-marker"
	body := []byte(`{"input":[{"type":"message","role":"user","content":"` + secret + `"},
		{"type":"reasoning","encrypted_content":"` + secret + `"},
		{"type":"function_call","name":"` + secret + `","arguments":"` + secret + `"},
		{"type":"function_call_output","output":"` + secret + `"},
		{"type":"item_reference","id":"` + secret + `"},
		{"type":"` + secret + `","content":"` + secret + `"}],"previous_response_id":"` + secret + `","` + secret + `":"` + secret + `"}`)
	c := newOpenAIRejectedFieldTestContext(body)
	c.Request.Header.Set("Authorization", "Bearer "+secret)
	c.Set("openai_ws_session_fallback_reason", "payload_too_large")
	response := newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, "")
	response.Header.Set("x-request-id", "req_upstream_validation")
	appendOpenAIResponsesValidationDiagnostic(c, newOpenAIOAuthNamespaceTestAccount(), body,
		[]byte(`{"error":{"message":"Bad Request","type":"invalid_request_error","unknown":"`+secret+`"}}`), response, 2)
	events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	event := events[0]
	require.Equal(t, "req_upstream_validation", event.UpstreamRequestID)
	require.Equal(t, "unspecified_bad_request", event.Reason)
	require.NotContains(t, event.Detail+event.Message+event.UpstreamResponseBody, secret)
	require.Equal(t, int64(len(body)), gjson.Get(event.Detail, "request_bytes").Int())
	require.Equal(t, int64(6), gjson.Get(event.Detail, "input_items").Int())
	for _, field := range []string{"message_items", "reasoning_items", "tool_call_items", "tool_output_items", "reference_items", "other_items"} {
		require.Equal(t, int64(1), gjson.Get(event.Detail, field).Int(), field)
	}
	require.True(t, gjson.Get(event.Detail, "has_previous_response_id").Bool())
	require.Equal(t, "payload_too_large", gjson.Get(event.Detail, "ws_fallback_reason").String())
	require.LessOrEqual(t, len(event.Detail), openAIResponsesValidationMaxDetailBytes)
}

func validationDiagnosticEvent(t *testing.T, c *gin.Context, body string, response *http.Response) *OpsUpstreamErrorEvent {
	t.Helper()
	if response == nil {
		response = newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, "")
	}
	appendOpenAIResponsesValidationDiagnostic(c, newOpenAIOAuthNamespaceTestAccount(), []byte(body),
		[]byte(`{"detail":"Bad Request"}`), response, 1)
	events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
	require.NotEmpty(t, events)
	return events[len(events)-1]
}

func TestResponsesValidationDiagnosticTextFormatAndSearchSources(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"absent", ``, "absent"},
		{"text", `,"text":{"format":{"type":"text"}}`, "text"},
		{"json object", `,"text":{"format":{"type":"json_object"}}`, "json_object"},
		{"json schema", `,"text":{"format":{"type":"json_schema","name":"SECRET","schema":{"SECRET":"SECRET"}}}`, "json_schema"},
		{"unknown type", `,"text":{"format":{"type":"SECRET"}}`, "other"},
		{"scalar text", `,"text":"SECRET"`, "invalid"},
		{"scalar format", `,"text":{"format":"SECRET"}`, "invalid"},
		{"null format", `,"text":{"format":null}`, "invalid"},
		{"non string type", `,"text":{"format":{"type":7}}`, "invalid"},
		{"case sensitive", `,"text":{"format":{"type":"JSON_OBJECT"}}`, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"input":"SECRET"` + tc.text + `}`
			c := newOpenAIRejectedFieldTestContext([]byte(body))
			ResetOpenAIResponsesValidationSearchSources(c)
			event := validationDiagnosticEvent(t, c, body, nil)
			require.Equal(t, tc.want, gjson.Get(event.Detail, "text_format").String())
			require.JSONEq(t, `[]`, gjson.Get(event.Detail, "search_sources").Raw)
			require.NotContains(t, event.Detail, "SECRET")
		})
	}

	t.Run("explicit sources are recorded and reset", func(t *testing.T) {
		body := `{"input":[{"type":"web_search_call","action":{"url":"https://SECRET"}},{"type":"additional_tools","tools":[{"type":"web_search_preview"}]}],"tools":[{"type":"web_search"}]}`
		c := newOpenAIRejectedFieldTestContext([]byte(body))
		unknown := validationDiagnosticEvent(t, c, body, nil)
		require.JSONEq(t, `["unknown"]`, gjson.Get(unknown.Detail, "search_sources").Raw)
		ResetOpenAIResponsesValidationSearchSources(c)
		for _, source := range []OpenAIResponsesValidationSearchSource{
			OpenAIResponsesValidationSearchHistory, OpenAIResponsesValidationSearchSynthesis,
			OpenAIResponsesValidationSearchClient, OpenAIResponsesValidationSearchClient,
			OpenAIResponsesValidationSearchSource(255),
		} {
			MarkOpenAIResponsesValidationSearchSource(c, source)
		}
		event := validationDiagnosticEvent(t, c, body, nil)
		require.JSONEq(t, `["client","synthesis","history"]`, gjson.Get(event.Detail, "search_sources").Raw)
		for _, key := range []string{"search_present", "search_top_level", "search_additional_tools", "search_history_present"} {
			require.True(t, gjson.Get(event.Detail, key).Bool(), key)
		}
		require.NotContains(t, event.Detail, "SECRET")
		ResetOpenAIResponsesValidationSearchSources(c)
		reset := validationDiagnosticEvent(t, c, `{"input":"SECRET","tools":[{"type":"web_search_SECRET"}]}`, nil)
		require.False(t, gjson.Get(reset.Detail, "search_present").Bool())
		require.JSONEq(t, `[]`, gjson.Get(reset.Detail, "search_sources").Raw)
	})
}

func TestResponsesValidationDiagnosticContentLengthsAreNotWireBytes(t *testing.T) {
	body := `{"input":"` + strings.Repeat("z", 1000) + `"}`
	for _, tc := range []struct {
		name, encoding, wantEncoding string
		request                      bool
		body                         bool
		length                       int64
		protocol                     int
		wantProtocol                 string
	}{
		{name: "unknown request", wantProtocol: "unknown"},
		{name: "gzip request", request: true, encoding: "gzip", wantEncoding: "gzip", length: 72, protocol: 2, wantProtocol: "h2"},
		{name: "empty request", request: true, wantEncoding: "identity", length: 0, protocol: 1, wantProtocol: "h1"},
		{name: "zero with body means unknown", request: true, body: true, wantEncoding: "identity", length: 0, protocol: 3, wantProtocol: "h3"},
		{name: "unknown length", request: true, wantEncoding: "identity", length: -1, wantProtocol: "unknown"},
		{name: "unknown encoding", request: true, encoding: "SECRET", wantEncoding: "other", length: 50, protocol: 42, wantProtocol: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newOpenAIRejectedFieldTestContext([]byte(body))
			response := newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, "")
			response.ProtoMajor = tc.protocol
			if tc.request {
				response.Request = httptest.NewRequest(http.MethodPost, "http://example.test/SECRET", nil)
				if tc.body {
					response.Request = httptest.NewRequest(http.MethodPost, "http://example.test/SECRET", strings.NewReader("SECRET"))
				}
				response.Request.ContentLength = tc.length
				response.Request.Header.Set("Content-Encoding", tc.encoding)
				response.Request.Header.Set("Authorization", "Bearer SECRET")
			}
			event := validationDiagnosticEvent(t, c, body, response)
			require.Equal(t, int64(len(body)), gjson.Get(event.Detail, "request_bytes").Int())
			require.Equal(t, "responses_json", gjson.Get(event.Detail, "request_bytes_basis").String())
			length := gjson.Get(event.Detail, "http_request_content_length")
			require.Equal(t, tc.request && (tc.length > 0 || tc.length == 0 && !tc.body), length.Exists())
			if length.Exists() {
				require.Equal(t, tc.length, length.Int())
			}
			require.Equal(t, tc.wantEncoding, gjson.Get(event.Detail, "http_content_encoding").String())
			require.Equal(t, tc.wantProtocol, gjson.Get(event.Detail, "http_protocol").String())
			require.NotContains(t, event.Detail, "SECRET")
			require.NotContains(t, event.Detail, "wire_bytes")
		})
	}
}

func TestResponsesValidationDiagnosticCountsKindsSizesAndLocalPairing(t *testing.T) {
	items := []string{
		`{"type":"function_call","call_id":"SECRET_PAIR","name":"SECRET","arguments":"SECRET"}`,
		`{"type":"function_call_output","call_id":"SECRET_PAIR","output":"SECRET"}`,
		`{"type":"custom_tool_call","call_id":"SECRET_UNPAIRED_CALL"}`,
		`{"type":"custom_tool_call_output","call_id":"SECRET_UNPAIRED_OUTPUT"}`,
		`{"type":"tool_search_call","call_id":"SECRET_SEARCH"}`,
		`{"type":"tool_search_call_output","call_id":"SECRET_SEARCH","tools":[]}`,
		`{"type":"function_call"}`,
		`{"type":"function_call_output","call_id":null}`,
		`{"type":"reasoning","encrypted_content":"SECRET"}`,
		`{"type":"message","content":[{"type":"input_text","text":"SECRET"},{"type":"input_image","image_url":"https://SECRET"},{"type":"input_file","file_data":"SECRET"},{"type":"SECRET","SECRET":"SECRET"}]}`,
		`{"type":"","role":"user","content":"SECRET"}`,
		`{"type":"SECRET","SECRET":"SECRET"}`,
		`{"type":"compaction","encrypted_content":"SECRET"}`,
	}
	body := `{"previous_response_id":"SECRET","input":[` + strings.Join(items, ",") + `]}`
	c := newOpenAIRejectedFieldTestContext([]byte(body))
	event := validationDiagnosticEvent(t, c, body, nil)
	var summary openAIResponsesValidationSummary
	require.NoError(t, json.Unmarshal([]byte(event.Detail), &summary))
	require.Equal(t, len(items), summary.InputItems)
	require.Equal(t, 2, summary.MessageItems)
	require.Equal(t, 1, summary.UnmatchedToolCalls)
	require.Equal(t, 1, summary.UnmatchedToolOutputs)
	require.Equal(t, 1, summary.ToolCallsWithoutID)
	require.Equal(t, 1, summary.ToolOutputsWithoutID)
	require.True(t, summary.ToolPairingComplete)
	require.Equal(t, "request_input", summary.ToolPairingScope, "a previous response may legitimately contain the missing pair")
	require.Equal(t, len(items[8]), summary.InputTypes["reasoning"].Bytes)
	require.Equal(t, len(items[8]), summary.InputTypes["reasoning"].MaxBytes)
	require.Equal(t, 1, summary.InputTypes["compaction"].Count)
	require.Equal(t, 1, summary.InputTypes["other"].Count)
	require.Equal(t, map[string]int{"input_text": 1, "input_image": 1, "input_file": 1, "other": 1, "string": 1}, summary.MessageContentTypes)
	totalBytes, maxBytes := 0, 0
	for _, item := range items {
		totalBytes += len(item)
		maxBytes = max(maxBytes, len(item))
	}
	countedBytes := 0
	for _, stats := range summary.InputTypes {
		countedBytes += stats.Bytes
	}
	require.Equal(t, totalBytes, countedBytes)
	require.Equal(t, maxBytes, summary.MaxInputItemBytes)
	require.Equal(t, len(gjson.Get(body, "input").Raw), summary.InputBytes)
	require.NotContains(t, event.Detail, "SECRET")
}

func TestResponsesValidationDiagnosticPairingAndOutputAreBounded(t *testing.T) {
	items := make([]string, 0, openAIResponsesValidationPairingLimit+3)
	for i := 0; i < openAIResponsesValidationPairingLimit+2; i++ {
		items = append(items, fmt.Sprintf(`{"type":"function_call","call_id":"SECRET-%d","arguments":"SECRET"}`, i))
	}
	items = append(items, `{"type":"function_call_output","call_id":"`+strings.Repeat("SECRET", 1000)+`"}`)
	body := `{"input":[` + strings.Join(items, ",") + `]}`
	c := newOpenAIRejectedFieldTestContext([]byte(body))
	event := validationDiagnosticEvent(t, c, body, nil)
	require.LessOrEqual(t, len(event.Detail), openAIResponsesValidationMaxDetailBytes)
	require.False(t, gjson.Get(event.Detail, "tool_pairing_complete").Bool())
	require.Equal(t, int64(3), gjson.Get(event.Detail, "tool_pairing_untracked_items").Int())
	require.NotContains(t, event.Detail, "SECRET")

	pairing := openAIValidationToolPairing{}
	for _, raw := range items {
		pairing.add(gjson.Parse(raw), gjson.Get(raw, "type").String())
	}
	require.Len(t, pairing.items, openAIResponsesValidationPairingLimit)
}

func TestResponsesValidationDiagnosticCategoryCardinalityIsFixed(t *testing.T) {
	knownTypes := []string{"message", "reasoning", "function_call", "custom_tool_call", "tool_search_call", "local_shell_call",
		"computer_call", "shell_call", "apply_patch_call", "mcp_call", "web_search_call", "file_search_call",
		"code_interpreter_call", "image_generation_call", "function_call_output", "custom_tool_call_output",
		"tool_search_output", "local_shell_call_output", "computer_call_output", "shell_call_output",
		"apply_patch_call_output", "item_reference", "additional_tools", "compaction", "compaction_trigger",
		"mcp_approval_request", "mcp_approval_response"}
	items := make([]string, 0, len(knownTypes)+501)
	for _, kind := range knownTypes {
		items = append(items, fmt.Sprintf(`{"type":%q,"content":"SECRET","call_id":"SECRET","SECRET":"SECRET"}`, kind))
	}
	for i := 0; i < 500; i++ {
		items = append(items, fmt.Sprintf(`{"type":"SECRET_%d","content":"SECRET"}`, i))
	}
	items = append(items, `{"type":"message","content":[{"type":"input_text","text":"SECRET"},{"type":"output_text","text":"SECRET"},{"type":"input_image","image_url":"SECRET"},{"type":"input_audio","audio":"SECRET"},{"type":"SECRET","SECRET":"SECRET"}]}`)
	body := `{"input":[` + strings.Join(items, ",") + `],"text":{"format":{"type":"json_schema","schema":{"SECRET":"SECRET"}}},"tools":[{"type":"web_search_preview","filters":{"allowed_domains":["SECRET"]}}]}`
	c := newOpenAIRejectedFieldTestContext([]byte(body))
	event := validationDiagnosticEvent(t, c, body, nil)
	require.LessOrEqual(t, len(event.Detail), openAIResponsesValidationMaxDetailBytes)
	require.NotContains(t, event.Detail, "SECRET")
	var summary openAIResponsesValidationSummary
	require.NoError(t, json.Unmarshal([]byte(event.Detail), &summary))
	require.Equal(t, len(items), summary.InputItems)
	if !summary.SummaryTruncated {
		require.Len(t, summary.InputTypes, len(knownTypes)+1)
		require.Equal(t, 500, summary.InputTypes["other"].Count)
	}
}

func TestResponsesValidationDiagnosticExplicitJSONSearchConflict(t *testing.T) {
	for _, tc := range []struct {
		name, errorBody, wantReason string
		status                      int
	}{
		{"explicit conflict", `{"error":{"message":"Web Search cannot be used with JSON mode.","code":"invalid_request_error","param":"tools[0].SECRET"}}`, "json_search_conflict", 400},
		{"generic internal detail", `{"detail":"Bad Request","error":{"code":"SECRET","param":"SECRET"}}`, "unspecified_bad_request", 400},
		{"different error", `{"error":{"message":"SECRET"}}`, "", 400},
		{"different status", `{"detail":"Bad Request"}`, "", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"text":{"format":{"type":"json_object"}},"tools":[{"type":"web_search"}],"input":"SECRET"}`)
			c := newOpenAIRejectedFieldTestContext(body)
			response := newOpenAIRejectedFieldTestResponse(tc.status, "")
			appendOpenAIResponsesValidationDiagnostic(c, newOpenAIOAuthNamespaceTestAccount(), body, []byte(tc.errorBody), response, 1)
			if tc.wantReason == "" {
				_, exists := c.Get(OpsUpstreamErrorsKey)
				require.False(t, exists)
				return
			}
			events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
			require.Len(t, events, 1)
			require.Equal(t, tc.wantReason, events[0].Reason)
			require.NotContains(t, events[0].Detail, "SECRET")
			if tc.name == "explicit conflict" {
				require.Equal(t, "tools", gjson.Get(events[0].Detail, "upstream_param_category").String())
				require.Equal(t, "invalid_request_error", gjson.Get(events[0].Detail, "upstream_code_category").String())
			} else {
				require.Equal(t, "other", gjson.Get(events[0].Detail, "upstream_param_category").String())
				require.Equal(t, "other", gjson.Get(events[0].Detail, "upstream_code_category").String())
			}
		})
	}
}

func TestResponsesValidationDiagnosticGeneric400DoesNotRetry(t *testing.T) {
	body := rejectedMessageIDHistory(t, 1, 0)
	upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusBadRequest,
		`{"error":{"type":"invalid_request_error","message":"Bad Request"}}`)}
	c := newOpenAIRejectedFieldTestContext(body)
	_, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIOAuthNamespaceTestAccount(), body)
	require.Error(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Equal(t, "item_BAD", gjson.GetBytes(upstream.bodies[0], "input.1.id").String())
	events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
	require.GreaterOrEqual(t, len(events), 1)
	require.Equal(t, "request_validation", events[0].Kind)
	require.Equal(t, "unspecified_bad_request", events[0].Reason)
	require.NotContains(t, events[0].Detail, "Keep the conversation")
	require.False(t, strings.Contains(events[0].Detail, "item_BAD"))
}
