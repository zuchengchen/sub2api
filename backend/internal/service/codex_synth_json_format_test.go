package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func codexJSONFormatTestRequest(t *testing.T, fields string) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(fields), &body))
	body["model"] = "gpt-6-luna"
	if _, exists := body["input"]; !exists {
		body["input"] = []any{map[string]any{"type": "message", "role": "user", "content": "Return JSON."}}
	}
	return body
}

func codexJSONFormatTestSynthesize(t *testing.T, body map[string]any, capability string, lite bool) codexSynthResult {
	t.Helper()
	account := &Account{
		ID: 7801, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Extra: map[string]any{codexClientCapabilityExtraKey: capability},
	}
	result, err := synthesizeCodexOAuthRequest(body, account, "gpt-6-luna", codexSynthOptions{
		ResponsesLite: lite,
		WireProfile:   codexWireProfileMain,
		Now:           time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	return result
}

func TestCodexSynthesisJSONFormatDetection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fields   string
		wantJSON bool
	}{
		{"json object", `{"text":{"format":{"type":"json_object"}}}`, true},
		{"json schema", `{"text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}}}}`, true},
		{"text format", `{"text":{"format":{"type":"text"}}}`, false},
		{"absent text", `{}`, false},
		{"null text", `{"text":null}`, false},
		{"string text", `{"text":"json_object"}`, false},
		{"array text", `{"text":[{"format":{"type":"json_object"}}]}`, false},
		{"absent format", `{"text":{"verbosity":"low"}}`, false},
		{"null format", `{"text":{"format":null}}`, false},
		{"string format", `{"text":{"format":"json_object"}}`, false},
		{"array format", `{"text":{"format":[{"type":"json_object"}]}}`, false},
		{"absent type", `{"text":{"format":{}}}`, false},
		{"numeric type", `{"text":{"format":{"type":1}}}`, false},
		{"array type", `{"text":{"format":{"type":["json_object"]}}}`, false},
		{"object type", `{"text":{"format":{"type":{"type":"json_object"}}}}`, false},
		{"unknown type", `{"text":{"format":{"type":"json"}}}`, false},
		{"uppercase type", `{"text":{"format":{"type":"JSON_OBJECT"}}}`, false},
		{"padded type", `{"text":{"format":{"type":" json_schema "}}}`, false},
		{"unconverted chat format", `{"response_format":{"type":"json_object"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := codexJSONFormatTestRequest(t, tc.fields)
			originalText, err := json.Marshal(body["text"])
			require.NoError(t, err)
			require.Equal(t, tc.wantJSON, codexSynthesisHasJSONOutputFormat(body))
			result := codexJSONFormatTestSynthesize(t, body, "", false)
			require.True(t, result.Modified)
			require.Equal(t, codexCapabilityGeneric, result.Capability)
			require.Equal(t, !tc.wantJSON, result.WebSearchInjected)
			sent, err := json.Marshal(body)
			require.NoError(t, err)
			require.Equal(t, !tc.wantJSON, gjson.GetBytes(sent, `tools.#(type=="web_search")`).Exists())
			if _, existed := gjson.Parse(tc.fields).Map()["text"]; existed {
				currentText, err := json.Marshal(body["text"])
				require.NoError(t, err)
				require.JSONEq(t, string(originalText), string(currentText), "valid and malformed format fields remain untouched")
			}
		})
	}
}

func TestCodexSynthesisJSONFormatPreservesClientTools(t *testing.T) {
	for _, lite := range []bool{false, true} {
		for _, format := range []string{"json_object", "json_schema"} {
			name := format + "/standard"
			if lite {
				name = format + "/lite"
			}
			t.Run(name, func(t *testing.T) {
				body := codexJSONFormatTestRequest(t, `{"tool_choice":{"type":"function","name":"lookup"},"parallel_tool_calls":false}`)
				body["text"] = map[string]any{"format": map[string]any{"type": format}}
				tools := []any{map[string]any{
					"type": "function", "name": "lookup", "description": "Read local data.", "strict": true,
					"parameters": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}, "required": []any{"key"}, "additionalProperties": false},
				}}
				body["tools"] = tools
				if lite {
					delete(body, "tools")
					body["input"] = append(body["input"].([]any), map[string]any{"type": "additional_tools", "role": "developer", "tools": tools})
				}
				originalTools, err := json.Marshal(tools)
				require.NoError(t, err)
				result := codexJSONFormatTestSynthesize(t, body, "", lite)
				require.False(t, result.WebSearchInjected)
				sent, err := json.Marshal(body)
				require.NoError(t, err)
				toolsPath := "tools"
				if lite {
					require.False(t, gjson.GetBytes(sent, "tools").Exists())
					toolsPath = `input.#(type=="additional_tools").tools`
				}
				require.JSONEq(t, string(originalTools), gjson.GetBytes(sent, toolsPath).Raw)
				require.JSONEq(t, `{"type":"function","name":"lookup"}`, gjson.GetBytes(sent, "tool_choice").Raw)
				require.Equal(t, false, body["parallel_tool_calls"])
				require.Equal(t, format, gjson.GetBytes(sent, "text.format.type").String())
			})
		}
	}
}

func TestCodexSynthesisExplicitSearchPreventsDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		toolType string
		lite     bool
	}{
		{"standard web search", "web_search", false},
		{"standard preview", "web_search_preview", false},
		{"lite preview", "web_search_preview", true},
		{"lite dated preview", "web_search_preview_2025_03_11", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := codexJSONFormatTestRequest(t, `{"text":{"format":{"type":"text"}},"tool_choice":"required"}`)
			tools := []any{map[string]any{"type": tc.toolType, "search_context_size": "low", "user_location": map[string]any{"type": "approximate", "country": "CA"}}}
			if tc.lite {
				body["input"] = append(body["input"].([]any), map[string]any{"type": "additional_tools", "role": "developer", "tools": tools})
			} else {
				body["tools"] = tools
			}
			originalTools, err := json.Marshal(tools)
			require.NoError(t, err)
			result := codexJSONFormatTestSynthesize(t, body, "", tc.lite)
			require.False(t, result.WebSearchInjected)
			sent, err := json.Marshal(body)
			require.NoError(t, err)
			toolsPath := "tools"
			if tc.lite {
				require.False(t, gjson.GetBytes(sent, "tools").Exists())
				toolsPath = `input.#(type=="additional_tools").tools`
			}
			require.JSONEq(t, string(originalTools), gjson.GetBytes(sent, toolsPath).Raw)
			require.Equal(t, "required", body["tool_choice"])
		})
	}
}

func TestCodexSynthesisJSONFormatCapabilityBoundaries(t *testing.T) {
	for _, capability := range []string{"native", "compatible"} {
		for _, format := range []string{"text", "json_object", "json_schema"} {
			t.Run(capability+"/"+format, func(t *testing.T) {
				body := codexJSONFormatTestRequest(t, `{"tool_choice":"required","tools":[{"type":"web_search_preview","search_context_size":"low"}]}`)
				body["text"] = map[string]any{"format": map[string]any{"type": format}}
				original, err := json.Marshal(body)
				require.NoError(t, err)
				result := codexJSONFormatTestSynthesize(t, body, capability, false)
				require.Equal(t, codexCapability(capability), result.Capability)
				require.False(t, result.WebSearchInjected)
				sent, err := json.Marshal(body)
				require.NoError(t, err)
				if capability == "native" {
					require.False(t, result.Modified)
					require.JSONEq(t, string(original), string(sent))
				}
				require.JSONEq(t, gjson.GetBytes(original, "tools").Raw, gjson.GetBytes(sent, "tools").Raw)
				require.Equal(t, "required", body["tool_choice"])
				require.Equal(t, format, gjson.GetBytes(sent, "text.format.type").String())
			})
		}
	}
}
