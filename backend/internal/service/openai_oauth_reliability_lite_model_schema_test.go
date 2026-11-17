//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func oauthReliabilityLiteAccount() *Account {
	return &Account{ID: 9127, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
}

func TestOAuthReliabilityLiteOmitsImplicitHostedSearch(t *testing.T) {
	for _, lite := range []bool{false, true} {
		for _, format := range []string{"", "json_object", "json_schema"} {
			t.Run(fmt.Sprintf("lite=%t/format=%s", lite, format), func(t *testing.T) {
				body := map[string]any{"input": []any{map[string]any{"type": "message", "role": "user", "content": "fixture"}}}
				if format != "" {
					body["text"] = map[string]any{"format": map[string]any{"type": format}}
				}
				_, injected := codexSynthesisPrepareTools(body, openai.CodexCatalogModel{}, false, codexCapabilityGeneric, lite)
				want := !lite && format == ""
				require.Equal(t, want, injected)
				require.Equal(t, want, codexSynthesisHasWebSearchTool(body))
			})
		}
	}
	for _, lite := range []bool{false, true} {
		body := map[string]any{"tools": []any{map[string]any{"type": "web_search", "external_web_access": true}}}
		before, err := json.Marshal(body)
		require.NoError(t, err)
		_, injected := codexSynthesisPrepareTools(body, openai.CodexCatalogModel{}, false, codexCapabilityGeneric, lite)
		require.False(t, injected)
		after, err := json.Marshal(body)
		require.NoError(t, err)
		require.Equal(t, before, after, "explicit tools are preserved for final validation")
	}
}

func TestOAuthReliabilityLiteFinalToolValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, param string
	}{
		{"function", `{"tools":[{"type":"function","name":"fixture","parameters":{"properties":{"type":{"const":"web_search"}},"uniqueItems":true}}]}`, ""},
		{"custom", `{"tools":[{"type":"custom","name":"fixture","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}]}`, ""},
		{"client search", `{"tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}}]}`, ""},
		{"namespace", `{"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"fixture","tools":[{"type":"function","name":"run"},{"type":"custom","name":"patch"}]}]}]}`, ""},
		{"loaded client tools", `{"input":[{"type":"tool_search_output","execution":"client","call_id":"call_fixture","tools":[{"type":"function","name":"run"}]}]}`, ""},
		{"unknown future", `{"tools":[{"type":"future_private_tool","execution":"future"}],"input":[{"type":"additional_tools","tools":["legacy shorthand"]}]}`, ""},
		{"top hosted", `{"tools":[{"type":"web_search","name":"sensitive_name"}]}`, "tools[0].type"},
		{"additional only", `{"input":[{"type":"message","role":"user","content":"fixture"},{"type":"additional_tools","tools":[{"type":"image_generation"}]}]}`, "input[1].tools[0].type"},
		{"nested hosted", `{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"private_name","tools":[{"type":"web_search"}]}]}]}`, "input[0].tools[0].tools[0].type"},
		{"loaded hosted tools", `{"input":[{"type":"tool_search_output","execution":"client","call_id":"call_fixture","tools":[{"type":"web_search"}]}]}`, "input[0].tools[0].type"},
		{"server search", `{"tools":[{"type":"tool_search","execution":"server"}]}`, "tools[0].execution"},
		{"implicit server search", `{"input":[{"type":"additional_tools","tools":[{"type":"tool_search"}]}]}`, "input[0].tools[0].execution"},
		{"invalid search execution", `{"tools":[{"type":"tool_search","execution":true}]}`, "tools[0].execution"},
		{"tool array", `{"tools":{}}`, "tools"},
		{"additional array", `{"input":[{"type":"additional_tools","tools":"private_tool_text"}]}`, "input[0].tools"},
		{"namespace array", `{"tools":[{"type":"namespace","name":"private_name","tools":{}}]}`, "tools[0].tools"},
		{"tool result collision", `{"input":[{"type":"function_call_output","output":{"tools":[{"type":"web_search"}]}}]}`, ""},
		{"unknown history", `{"input":[{"type":"future_output","tools":[{"type":"web_search"}]}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			original := bytes.Clone(body)
			err := validateOpenAIResponsesLiteFinalRequest(oauthReliabilityLiteAccount(), "gpt-6-astra", "true", body)
			require.Equal(t, original, body)
			if tc.param == "" {
				require.NoError(t, err)
				return
			}
			var validationErr *openAIResponsesLiteValidationError
			require.True(t, errors.As(err, &validationErr))
			require.Equal(t, tc.param, validationErr.param)
			require.NotContains(t, err.Error(), "private_")
			require.NotContains(t, err.Error(), "sensitive_name")
		})
	}
	for _, toolType := range []string{"web_search", "web_search_preview", "web_search_preview_2025_03_11", "image_generation", "file_search", "code_interpreter", "mcp", "computer", "computer_use_preview", "local_shell", "shell", "apply_patch"} {
		body := []byte(fmt.Sprintf(`{"input":[{"type":"additional_tools","tools":[{"type":%q}]}]}`, toolType))
		require.Error(t, validateOpenAIResponsesLiteFinalRequest(oauthReliabilityLiteAccount(), "gpt-6-astra", "true", body), toolType)
	}
}

func TestOAuthReliabilityLiteFinalModeHonorsMappedGPT55(t *testing.T) {
	account := oauthReliabilityLiteAccount()
	plain := []byte(`{"model":"gpt-5.5","tools":[{"type":"web_search"}],"input":[{"type":"web_search_call","id":"ws_fixture","status":"completed"}]}`)
	ws := []byte(`{"type":"response.create","model":"gpt-5.5","tools":[{"type":"web_search"}],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
	for _, body := range [][]byte{plain, ws} {
		require.False(t, isOpenAIResponsesLiteFinalRequest(account, "gpt-5.5", "true", body))
		require.NoError(t, validateOpenAIResponsesLiteFinalRequest(account, "gpt-5.5", "true", body))
		require.Error(t, validateOpenAIResponsesLiteFinalRequest(account, "gpt-6-astra", "true", body))
		request, err := http.NewRequest(http.MethodPost, "https://fixture.invalid/responses", bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set(responsesLiteHeader, "true")
		require.NoError(t, applyMappedGPT55LiteCompatibility(request, account, body))
		outbound, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.False(t, isOpenAIResponsesLiteHeader(request.Header.Get(responsesLiteHeader)))
		require.False(t, isOpenAIResponsesLiteWebSocketPayload(outbound))
		require.Equal(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(outbound, "tools").Raw)
		require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(outbound, "input").Raw)
	}
	apiKey := &Account{ID: 9128, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	for _, acct := range []*Account{nil, apiKey} {
		require.NoError(t, validateOpenAIResponsesLiteFinalRequest(acct, "gpt-6-astra", "true", plain))
	}
	// Explicit HTTP header precedence agrees with the final HTTP metadata
	// projection. Native WS passes an empty header and uses frame metadata.
	for _, header := range []string{"", "false"} {
		require.NoError(t, validateOpenAIResponsesLiteFinalRequest(account, "gpt-6-astra", header, plain))
	}
	require.Error(t, validateOpenAIResponsesLiteFinalRequest(account, "gpt-6-astra", "", ws))
	require.NoError(t, validateOpenAIResponsesLiteFinalRequest(account, "gpt-6-astra", "false", ws))
}

func TestOAuthReliabilityLitePreservesHostedSearchHistory(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":[{"type":"message","role":"user","content":"fixture"},{"type":"web_search_call","id":"ws_fixture","status":"completed","action":{"type":"search","query":"fixture"}},{"type":"function_call_output","call_id":"call_fixture","output":"retained"},{"type":"compaction_trigger"}]}`)
	updated, changed, err := ensureOpenAIOAuthWebSearchToolForHistoryBody(body, true)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, updated)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	before, err := json.Marshal(decoded)
	require.NoError(t, err)
	require.False(t, ensureOpenAIOAuthWebSearchToolForHistory(decoded, true))
	after, err := json.Marshal(decoded)
	require.NoError(t, err)
	require.Equal(t, before, after)
	err = validateOpenAIResponsesLiteFinalRequest(oauthReliabilityLiteAccount(), "gpt-6-astra", "true", updated)
	var validationErr *openAIResponsesLiteValidationError
	require.True(t, errors.As(err, &validationErr))
	require.Equal(t, "input[1].type", validationErr.param)
	// Same history retains the existing declaration repair in non-Lite mode.
	standard, changed, err := ensureOpenAIOAuthWebSearchToolForHistoryBody(body, false)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(standard, "input").Raw)
	require.Equal(t, "web_search", gjson.GetBytes(standard, "tools.0.type").String())
	require.Equal(t, "none", gjson.GetBytes(standard, "tool_choice").String())
	require.NoError(t, validateOpenAIResponsesLiteFinalRequest(oauthReliabilityLiteAccount(), "gpt-5.5", "true", standard))
}

func TestOAuthReliabilityLiteFinalValidationBoundsDiagnostics(t *testing.T) {
	tool := `{"type":"web_search"}`
	for i := 0; i < 300; i++ {
		tool = `{"type":"namespace","name":"private_namespace","tools":[` + tool + `]}`
	}
	body := []byte(`{"tools":[` + tool + `]}`)
	err := validateOpenAIResponsesLiteFinalRequest(oauthReliabilityLiteAccount(), "gpt-6-astra", "true", body)
	var validationErr *openAIResponsesLiteValidationError
	require.True(t, errors.As(err, &validationErr))
	require.LessOrEqual(t, len(validationErr.param), 256)
	require.NotContains(t, err.Error(), "private_namespace")
	// Malformed JSON gets a fixed message without copying request data.
	err = validateOpenAIResponsesLiteFinalRequest(oauthReliabilityLiteAccount(), "gpt-6-astra", "true", []byte(`{"private_payload":`))
	require.ErrorContains(t, err, "valid JSON request")
	require.NotContains(t, err.Error(), "private_payload")
}

func TestOAuthReliabilityFinalAstraReasoningPreservesConfiguredPolicy(t *testing.T) {
	account := oauthReliabilityLiteAccount()
	for _, effort := range []string{"none", "minimal"} {
		body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","reasoning":{"effort":%q},"input":[{"type":"function_call_output","call_id":"call_fixture","output":"unchanged"}]}`, effort))
		err := validateOpenAIOAuthFinalReasoning(account, "gpt-6-astra", body)
		var validationErr *openAIReasoningEffortValidationError
		require.True(t, errors.As(err, &validationErr))
		require.Equal(t, "reasoning.effort", validationErr.param)
		require.NoError(t, validateOpenAIOAuthFinalReasoning(account, "gpt-6-luna", body))
		require.NoError(t, validateOpenAIOAuthFinalReasoning(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "gpt-6-astra", body))
		ctx := WithRequestedReasoningEffort(context.Background(), effort)
		ctx = WithOpenAIReasoningEffortPolicy(ctx, "", []ReasoningEffortMapping{{From: "none", To: "low", MatchType: "exact", Model: "gpt-6-astra"}}, "")
		mapped, changed, err := ApplyOpenAIReasoningEffortPolicyFromContext(ctx, body)
		require.NoError(t, err)
		if effort == "none" {
			require.True(t, changed)
			require.NoError(t, validateOpenAIOAuthFinalReasoning(account, "gpt-6-astra", mapped))
			require.Equal(t, "low", gjson.GetBytes(mapped, "reasoning.effort").String())
		} else {
			require.False(t, changed, "none mapping must not expand to minimal")
			require.Error(t, validateOpenAIOAuthFinalReasoning(account, "gpt-6-astra", mapped))
		}
		require.Equal(t, effort, *RequestedReasoningEffortFromContext(ctx))
		require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(mapped, "input").Raw)
	}
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		body := []byte(fmt.Sprintf(`{"reasoning":{"effort":%q}}`, effort))
		require.NoError(t, validateOpenAIOAuthFinalReasoning(account, "gpt-6-astra", body))
	}
	require.NoError(t, validateOpenAIOAuthFinalReasoning(account, "gpt-6-astra", []byte(`{"input":[]}`)))
	for _, field := range []string{`"reasoning_effort":"none"`, `"output_config":{"effort":"none"}`} {
		require.Error(t, validateOpenAIOAuthFinalReasoning(account, "gpt-6-astra", []byte("{"+field+"}")))
	}
}

func TestOAuthReliabilityOutputSchemaPreservesConstraints(t *testing.T) {
	schema := `{"properties":{"tags":{"items":{"type":"integer"},"uniqueItems":true},"free":{"type":null,"properties":{"a":{"type":"string"}}},"uniqueItems":{"const":"property name"},"n":{"minimum":9007199254740993}},"minProperties":1,"maxProperties":4,"required":["tags"],"additionalProperties":false,"$defs":{"entry":{"type":["object","null"],"properties":{"v":{"type":"string"}},"minProperties":1}},"anyOf":[{"properties":{"a":{"type":"string"}}},{"type":"null"}],"not":{"items":{"type":"string"},"uniqueItems":true}}`
	for _, strict := range []string{"true", "false"} {
		for _, body := range []string{
			`{ "text": { "format": { "type":"json_schema", "strict":` + strict + `,"schema":` + schema + `}}}`,
			`{ "response_format": { "type":"json_schema", "json_schema":{"name":"fixture","strict":` + strict + `,"schema":` + schema + `}}}`,
		} {
			updated, changed, err := normalizeOpenAIResponseFormatSchemasBody([]byte(body))
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, body, string(updated), "no schema rewrite or numeric/order loss")
			var decoded map[string]any
			require.NoError(t, decodeOpenAIJSONUseNumber([]byte(body), &decoded))
			before, err := json.Marshal(decoded)
			require.NoError(t, err)
			require.False(t, normalizeOpenAIResponseFormatSchemas(decoded))
			after, err := json.Marshal(decoded)
			require.NoError(t, err)
			require.Equal(t, before, after)
		}
	}
}

func TestOAuthReliabilityOutputSchemaDoesNotInferStrictContract(t *testing.T) {
	for _, format := range []string{
		`{"type":"json_schema","schema":{"properties":{"name":{"type":"string"}}}}`,
		`{"type":"json_schema","strict":true,"schema":{"properties":{"name":{"type":"string"}}}}`,
		`{"type":"json_schema","strict":false,"schema":{"items":{"type":"string"},"uniqueItems":true}}`,
	} {
		body := []byte(`{"text":{"format":` + format + `}}`)
		normalized, changed, err := normalizeOpenAIResponseFormatSchemasBody(body)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, normalized)
		require.False(t, gjson.GetBytes(normalized, "text.format.schema.type").Exists())
		require.False(t, gjson.GetBytes(normalized, "text.format.schema.required").Exists())
		require.False(t, gjson.GetBytes(normalized, "text.format.schema.additionalProperties").Exists())
	}
	// Unknown keywords remain unknown; do not attribute the historical generic
	// upstream error to an observed key or strip it to manufacture acceptance.
	body := []byte(`{"text":{"format":{"type":"json_schema","schema":{"future_private_keyword":{"value":"unchanged"}}}}}`)
	normalized, changed, err := normalizeOpenAIResponseFormatSchemasBody(body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, normalized)
	require.True(t, strings.Contains(string(normalized), "future_private_keyword"))
}
