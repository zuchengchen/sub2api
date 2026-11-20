package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Codex local compaction replays hosted web_search_call history with tools:[];
// ChatGPT then fails with "response protection is unavailable" (#7927).
const openAIWebSearchHistoryCompactionBody = `{"model":"gpt-5.5","instructions":"x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"look it up"}]},{"type":"web_search_call","id":"ws_123","status":"completed","action":{"type":"search","query":"kwin inputmethod"}},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"found"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"summarize"}]}],"tools":[],"tool_choice":"auto","parallel_tool_calls":false,"stream":true,"store":false}`

func TestOpenAIResponsesHasWebSearchHistory(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		found      bool
	}{
		{"replayed item", openAIWebSearchHistoryCompactionBody, true},
		{"escaped field names and value", `{"in\u0070ut":[{"t\u0079pe":"we\u0062_search_call","id":"ws_fixture"}]}`, true},
		{"escaped input text", `{"input":"we\u0062_search_call"}`, false},
		{"message and nested content", `{"input":[{"type":"message","content":[{"type":"web_search_call"}]}]}`, false},
		{"tool arguments and results", `{"input":[{"type":"function_call","arguments":"{\"type\":\"web_search_call\"}"},{"type":"function_call_output","output":{"type":"web_search_call"}}]}`, false},
		{"tool declaration and choice", `{"tools":[{"type":"web_search","description":"web_search_call"}],"tool_choice":{"type":"web_search"},"input":[]}`, false},
		{"output and metadata", `{"output":[{"type":"web_search_call"}],"metadata":{"input":[{"type":"web_search_call"}]}}`, false},
		{"object input", `{"input":{"type":"web_search_call"}}`, false},
		{"nested array", `{"input":[[{"type":"web_search_call"}]]}`, false},
		{"wrong type field", `{"input":[{"id":"web_search_call","type":["web_search_call"]},{"type":" web_search_call"}]}`, false},
		{"numeric item before genuine history", `{"input":[{"type":0},{"type":"web_search_call"}]}`, true},
		{"trailing document", `{"input":[{"type":"web_search_call"}]}{}`, false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.found, openAIResponsesHasWebSearchHistory([]byte(tc.body)))
		})
	}
}

func TestOAuthWebSearchHistoryToolAcrossPaths(t *testing.T) {
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	transform := func(compact bool) func([]byte) ([]byte, bool, error) {
		return func(b []byte) ([]byte, bool, error) {
			var req map[string]any
			if err := json.Unmarshal(b, &req); err != nil {
				return nil, false, err
			}
			result := applyCodexOAuthTransform(req, true, compact)
			out, err := json.Marshal(req)
			return out, result.Modified, err
		}
	}
	tests := []struct {
		name      string
		normalize func([]byte) ([]byte, bool, error)
		inject    bool
	}{
		{"transformed OAuth", transform(false), true},
		{"transformed OAuth compact", transform(true), false},
		{"OAuth websocket", func(b []byte) ([]byte, bool, error) {
			return normalizeOpenAIResponsesWebSocketCompatibilityBody(b, oauth, false)
		}, true},
		{"OAuth passthrough compact", func(b []byte) ([]byte, bool, error) {
			return normalizeOpenAIResponsesCompatibilityBodyWithOptions(b, oauth, openAIResponsesCompatibilityOptions{Compact: true})
		}, false},
		{"API key websocket", func(b []byte) ([]byte, bool, error) {
			return normalizeOpenAIResponsesWebSocketCompatibilityBody(b, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, false)
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := tt.normalize([]byte(openAIWebSearchHistoryCompactionBody))
			require.NoError(t, err)
			require.Equal(t, "web_search_call", gjson.GetBytes(out, "input.1.type").String())
			if !tt.inject {
				require.False(t, gjsonToolsContainWebSearch(gjson.GetBytes(out, "tools")))
				return
			}
			tools := gjson.GetBytes(out, "tools").Array()
			require.Len(t, tools, 1)
			require.Equal(t, "web_search", tools[0].Get("type").String())
			require.False(t, tools[0].Get("external_web_access").Bool())
			require.True(t, tools[0].Get("external_web_access").Exists())
			require.Equal(t, "none", gjson.GetBytes(out, "tool_choice").String())
		})
	}
}

func TestEnsureOpenAIOAuthWebSearchToolForHistory(t *testing.T) {
	const (
		user    = `{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}`
		search  = `{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"q"}}`
		trigger = `{"type":"compaction_trigger"}`
		history = `[` + user + `,` + search + `]`
	)
	tests := []struct {
		name       string
		body       string
		lite       bool
		changed    bool
		toolCount  int
		toolChoice string
	}{
		{"tools absent", `{"input":` + history + `}`, false, true, 1, "none"},
		{"tools null", `{"input":` + history + `,"tools":null,"tool_choice":null}`, false, true, 1, "none"},
		{"explicit none kept", `{"input":` + history + `,"tools":[],"tool_choice":"none"}`, false, true, 1, "none"},
		{"required without caller tools cannot force an undeclared search", `{"input":` + history + `,"tools":[],"tool_choice":"required"}`, false, false, 0, ""},
		{"missing forced tool stays invalid", `{"input":` + history + `,"tool_choice":{"type":"function","name":"missing"}}`, false, false, 0, ""},
		{"caller tools keep choice", `{"input":` + history + `,"tools":[{"type":"function","name":"echo"}],"tool_choice":"auto"}`, false, true, 2, "auto"},
		{"malformed top-level tools preserved", `{"input":` + history + `,"tools":{"type":"function","name":"echo"}}`, false, false, 0, ""},
		{"malformed additional tools preserved", `{"input":[{"type":"additional_tools","tools":"opaque"},` + search + `]}`, false, false, 0, ""},
		{"malformed discovered tools preserved", `{"input":[{"type":"tool_search_output","tools":42},` + search + `]}`, false, false, 0, ""},
		{"web_search already declared", `{"input":` + history + `,"tools":[{"type":"web_search"}]}`, false, false, 0, ""},
		{"web_search_preview declared", `{"input":` + history + `,"tools":[{"type":"web_search_preview"}]}`, false, false, 0, ""},
		{"declared via additional_tools", `{"input":[{"type":"additional_tools","tools":[{"type":"web_search"}]},` + search + `]}`, false, false, 0, ""},
		{"explicit discovered search options kept", `{"input":[{"type":"tool_search_output","tools":[{"type":"web_search","external_web_access":true}]},` + search + `]}`, false, false, 0, ""},
		{"discovered tools keep required", `{"input":[{"type":"tool_search_output","tools":[{"type":"function","name":"echo"}]},` + search + `],"tool_choice":"required"}`, false, true, 1, "required"},
		{"escaped history item", `{"in\u0070ut":[{"t\u0079pe":"we\u0062_search_call"}],"to\u006fls":[]}`, false, true, 1, "none"},
		{"no web_search_call item", `{"input":[{"type":"message","role":"user","content":"web_search_call"}],"tools":[]}`, false, false, 0, ""},
		{"string input", `{"input":"web_search_call"}`, false, false, 0, ""},
		{"lite preserves compaction history", `{"input":[` + user + `,` + search + `,` + trigger + `]}`, true, false, 0, ""},
		{"lite preserves auto choice", `{"input":` + history + `,"tool_choice":"auto"}`, true, false, 0, ""},
		{"lite preserves caller additional_tools", `{"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"echo"}]},` + search + `,` + trigger + `],"tool_choice":"auto"}`, true, false, 0, ""},
		{"lite preserves empty additional_tools", `{"input":[{"type":"additional_tools","role":"developer"},` + search + `]}`, true, false, 0, ""},
		{"lite preserves top-level tools", `{"input":` + history + `,"tools":[{"type":"function","name":"echo"}]}`, true, false, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, changed, err := ensureOpenAIOAuthWebSearchToolForHistoryBody([]byte(tt.body), tt.lite)
			require.NoError(t, err)
			require.Equal(t, tt.changed, changed)
			var req map[string]any
			require.NoError(t, json.Unmarshal([]byte(tt.body), &req))
			require.Equal(t, tt.changed, ensureOpenAIOAuthWebSearchToolForHistory(req, tt.lite))
			if !tt.changed {
				require.JSONEq(t, tt.body, string(out))
				mapOut, marshalErr := json.Marshal(req)
				require.NoError(t, marshalErr)
				require.JSONEq(t, tt.body, string(mapOut))
				if tt.lite {
					require.ErrorContains(t, validateOpenAIResponsesLiteFinalRequest(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "gpt-6-astra", "true", out), "web_search_call history")
				}
				return
			}
			require.True(t, gjson.ValidBytes(out))
			require.Equal(t, gjson.Get(tt.body, "input").Raw, gjson.GetBytes(out, "input").Raw)
			tools := gjson.GetBytes(out, "tools").Array()
			require.Len(t, tools, tt.toolCount)
			require.JSONEq(t, `{"type":"web_search","external_web_access":false}`, tools[len(tools)-1].Raw)
			require.Equal(t, tt.toolChoice, gjson.GetBytes(out, "tool_choice").String())

			// The map variant used by the Codex transform must agree.
			mapOut, err := json.Marshal(req)
			require.NoError(t, err)
			require.JSONEq(t, string(out), string(mapOut))
		})
	}
}

func TestOAuthWebSearchHistoryPreservesDiscoveredToolChoice(t *testing.T) {
	for _, tc := range []struct {
		name, declaration, choice, wantChoice string
	}{
		{"function", `{"type":"function","name":"lookup","parameters":{"type":"object"}}`, `{"type":"function","name":"lookup"}`, `{"type":"function","name":"lookup"}`},
		{"namespace", `{"type":"namespace","name":"ns","tools":[{"type":"function","name":"lookup"}]}`, `{"type":"function","name":"ns__lookup"}`, `{"type":"function","name":"ns__lookup"}`},
		{"reserved name", `{"type":"function","name":"python"}`, `{"type":"function","name":"python"}`, `{"type":"function","name":"python__custom"}`},
		{"reserved namespace child", `{"type":"namespace","name":"ns","children":[{"type":"function","name":"python"}]}`, `{"type":"function","name":"python"}`, `{"type":"function","name":"python__custom"}`},
		{"custom", `{"type":"custom","name":"patch","format":{"type":"text"}}`, `{"type":"custom","name":"patch"}`, `{"type":"custom","name":"patch"}`},
		{"allowed tools", `{"type":"function","name":"lookup"}`, `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"lookup"}]}`, `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"lookup"}]}`},
		{"undeclared reference", `{"type":"function","name":"lookup"}`, `{"type":"function","name":"absent"}`, `"auto"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(`{"model":"gpt-6-astra","input":[{"type":"tool_search_call","execution":"client","call_id":"call_fixture","arguments":{}},{"type":"tool_search_output","execution":"client","call_id":"call_fixture","tools":[` + tc.declaration + `]},{"type":"web_search_call","id":"ws_fixture","action":{"type":"search","query":"fixture"},"status":"completed"}],"tool_choice":` + tc.choice + `}`)
			raw, changed, err := ensureOpenAIOAuthWebSearchToolForHistoryBody(original, false)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, gjson.GetBytes(original, "input").Raw, gjson.GetBytes(raw, "input").Raw)
			require.Equal(t, tc.choice, gjson.GetBytes(raw, "tool_choice").Raw)
			again, changed, err := ensureOpenAIOAuthWebSearchToolForHistoryBody(raw, false)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, raw, again)

			var request map[string]any
			require.NoError(t, decodeOpenAIJSONUseNumber(original, &request))
			result := applyCodexOAuthTransformWithOptions(request, codexOAuthTransformOptions{PreserveNativeInput: true})
			require.NoError(t, result.Error)
			encoded, err := json.Marshal(request)
			require.NoError(t, err)
			require.JSONEq(t, tc.wantChoice, gjson.GetBytes(encoded, "tool_choice").Raw)
			require.JSONEq(t, `{"type":"web_search","external_web_access":false}`, gjson.GetBytes(encoded, "tools.0").Raw)
			switch tc.name {
			case "reserved name":
				require.Equal(t, "python__custom", gjson.GetBytes(encoded, "input.1.tools.0.name").Str)
			case "reserved namespace child":
				require.Equal(t, "python__custom", gjson.GetBytes(encoded, "input.1.tools.0.children.0.name").Str)
			default:
				require.JSONEq(t, tc.declaration, gjson.GetBytes(encoded, "input.1.tools.0").Raw)
			}
		})
	}
}

func TestEnsureOpenAIOAuthWebSearchToolForHistoryDoesNotShareToolMap(t *testing.T) {
	first := map[string]any{"input": []any{map[string]any{"type": "web_search_call"}}}
	second := map[string]any{"input": []any{map[string]any{"type": "web_search_call"}}}
	require.True(t, ensureOpenAIOAuthWebSearchToolForHistory(first, false))
	require.True(t, ensureOpenAIOAuthWebSearchToolForHistory(second, false))
	firstTools, ok := first["tools"].([]any)
	require.True(t, ok)
	firstTool, ok := firstTools[0].(map[string]any)
	require.True(t, ok)
	firstTool["external_web_access"] = true
	secondTools, ok := second["tools"].([]any)
	require.True(t, ok)
	secondTool, ok := secondTools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, secondTool["external_web_access"])
	require.Equal(t, false, openAIWebSearchHistoryTool["external_web_access"])
}
