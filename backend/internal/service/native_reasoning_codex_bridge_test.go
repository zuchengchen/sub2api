package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNativeReasoningReplayCodexBridgeHistoryProjection(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_native","summary":[],"encrypted_content":"opaque","future":9007199254740993}`),
		json.RawMessage(`{"type":"reasoning","id":"rs_empty","summary":[],"content":[],"encrypted_content":"opaque-empty","internal_chat_message_metadata_passthrough":{"turn_id":"earlier","create_time":123}}`),
		json.RawMessage(`{"type":"reasoning","id":"rs_text","summary":[],"content":[{"type":"reasoning_text","text":"preserve"}],"encrypted_content":"opaque-text"}`),
		json.RawMessage(`{"type":"compaction","id":"cmp_native","encrypted_content":"opaque-checkpoint"}`),
	}
	before := make([]string, len(items))
	for i := range items {
		before[i] = string(items[i])
	}
	projected, err := codexBridgeOutputTurnReferences(items, []byte(`{"client_metadata":{"turn_id":"producing-turn"}}`))
	require.NoError(t, err)
	require.Equal(t, "null", gjson.GetBytes(projected[0], "content").Raw, "fixed Codex serializes absent reasoning content as null")
	require.Equal(t, "producing-turn", gjson.GetBytes(projected[0], "internal_chat_message_metadata_passthrough.turn_id").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(projected[0], "future").Raw)
	require.False(t, gjson.GetBytes(projected[1], "content").Exists(), "an empty typed reasoning content array is omitted")
	require.Equal(t, "earlier", gjson.GetBytes(projected[1], "internal_chat_message_metadata_passthrough.turn_id").String())
	require.Equal(t, "123", gjson.GetBytes(projected[1], "internal_chat_message_metadata_passthrough.create_time").Raw)
	require.Equal(t, "preserve", gjson.GetBytes(projected[2], "content.0.text").String())
	require.Equal(t, before[3], string(projected[3]), "compaction replacement does not run ordinary history stamping")
	for i := range items {
		require.Equal(t, before[i], string(items[i]), "source output remains immutable")
	}
}

func TestNativeReasoningReplayCodexToolOutputIDAcrossCompatibilityBoundary(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-6-astra","stream":true,"store":false,"input":[{"type":"custom_tool_call","id":"ctc_native","call_id":"call_native","name":"exec","input":"text(1)"},{"type":"custom_tool_call_output","id":"ctco_native","call_id":"call_native","output":[{"type":"input_text","text":"1"}]}]}`)
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKey := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	for _, tc := range []struct {
		name    string
		account *Account
		keep    bool
	}{{"codex_oauth", oauth, true}, {"public_apikey", apiKey, false}} {
		t.Run(tc.name, func(t *testing.T) {
			next, _, err := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, tc.account, false)
			require.NoError(t, err)
			id := gjson.GetBytes(next, "input.1.id")
			if tc.keep {
				require.Equal(t, "ctco_native", id.String())
			} else {
				require.False(t, id.Exists())
			}
			require.Equal(t, "call_native", gjson.GetBytes(next, "input.1.call_id").String())
			require.Equal(t, "1", gjson.GetBytes(next, "input.1.output.0.text").String())
		})
	}
}

func TestNativeReasoningReplayCodexReasoningContentBoundary(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":[{"type":"reasoning","id":"rs_native","summary":[],"content":[{"type":"reasoning_text","text":"preserve native content"}],"encrypted_content":"opaque"}]}`)
	for _, tc := range []struct {
		name    string
		account *Account
		keep    bool
	}{
		{"native_oauth", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, true},
		{"public_apikey", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, _, err := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, tc.account, false)
			require.NoError(t, err)
			if tc.keep {
				require.Equal(t, "preserve native content", gjson.GetBytes(next, "input.0.content.0.text").String())
			} else {
				require.False(t, gjson.GetBytes(next, "input.0.content").Exists())
			}
			require.Equal(t, "opaque", gjson.GetBytes(next, "input.0.encrypted_content").String())
		})
	}
	unknown := json.RawMessage(`{"type":"reasoning","id":"rs_future","content":[{"type":"future_reasoning_part","value":7}],"encrypted_content":"opaque"}`)
	items, err := codexBridgeOutputTurnReferences([]json.RawMessage{unknown}, []byte(`{"client_metadata":{"turn_id":"turn"}}`))
	require.NoError(t, err)
	require.JSONEq(t, gjson.GetBytes(unknown, "content").Raw, gjson.GetBytes(items[0], "content").Raw, "unknown extensions are not discarded by typed compatibility")
}

func TestNativeReasoningReplayCodexGenericTransformKeepsNativeOutputID(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":[{"type":"custom_tool_call","id":"ctc_native","call_id":"call_native","name":"inspect","input":"synthetic"},{"type":"custom_tool_call_output","id":"ctco_native","call_id":"call_native","output":"ok"},{"type":"item_reference","id":"ctco_native"}]}`)
	var request map[string]any
	require.NoError(t, decodeOpenAIJSONUseNumber(body, &request))
	result := applyCodexOAuthTransformWithOptions(request, codexOAuthTransformOptions{NativeItemIDs: true})
	require.NoError(t, result.Error)
	input := request["input"].([]any)
	require.Len(t, input, 3)
	call := input[0].(map[string]any)
	output := input[1].(map[string]any)
	reference := input[2].(map[string]any)
	require.Equal(t, "ctco_native", output["id"], "generic OAuth transform must not undo destination-aware preservation")
	require.Equal(t, "ctco_native", reference["id"])
	require.Equal(t, call["call_id"], output["call_id"], "legacy pairing normalization remains consistent")
	require.Equal(t, "ok", output["output"])
}
