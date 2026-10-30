package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthInputInternalMetadata(t *testing.T) {
	const field = "internal_chat_message_metadata_passthrough"
	// The identically named field in user content must not be recursively stripped.
	const body = `{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"hello","internal_chat_message_metadata_passthrough":{"keep":true}}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["text"]}},{"type":"function_call","call_id":"fc_123","name":"echo","arguments":"{\"internal_chat_message_metadata_passthrough\":true}","internal_chat_message_metadata_passthrough":null},{"type":"function_call_output","call_id":"fc_123","output":"result"}],"internal_chat_message_metadata_passthrough":{"keep":true}}`
	tests := []struct {
		name      string
		normalize func([]byte) ([]byte, bool, error)
		strip     bool
	}{
		{"transformed OAuth", func(b []byte) ([]byte, bool, error) {
			var req map[string]any
			if err := json.Unmarshal(b, &req); err != nil {
				return nil, false, err
			}
			result := applyCodexOAuthTransform(req, false, false)
			out, err := json.Marshal(req)
			return out, result.Modified, err
		}, true},
		{"OAuth passthrough", func(b []byte) ([]byte, bool, error) { return normalizeOpenAIPassthroughOAuthBody(b, false) }, true},
		{"OAuth compact", func(b []byte) ([]byte, bool, error) { return normalizeOpenAIPassthroughOAuthBody(b, true) }, true},
		{"OAuth websocket", func(b []byte) ([]byte, bool, error) {
			return normalizeOpenAIResponsesWebSocketCompatibilityBody(b, &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, false)
		}, false},
		{"Codex OAuth passthrough", func(b []byte) ([]byte, bool, error) {
			return normalizeOpenAIPassthroughOAuthBodyWithMetadata(b, false, true, false)
		}, false},
		{"Codex OAuth compact exception", func(b []byte) ([]byte, bool, error) {
			return normalizeOpenAIPassthroughOAuthBodyWithMetadata(b, true, true, false)
		}, true},
		{"SetupToken websocket", func(b []byte) ([]byte, bool, error) {
			return normalizeOpenAIResponsesWebSocketCompatibilityBody(b, &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}, false)
		}, true},
		{"API key websocket", func(b []byte) ([]byte, bool, error) {
			return normalizeOpenAIResponsesWebSocketCompatibilityBody(b, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, false)
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := tt.normalize([]byte(body))
			require.NoError(t, err)
			require.Equal(t, !tt.strip, gjson.GetBytes(out, "input.0."+field).Exists())
			require.Equal(t, !tt.strip, gjson.GetBytes(out, "input.1."+field).Exists())
			require.Equal(t, "hello", gjson.GetBytes(out, "input.0.content.0.text").String())
			require.True(t, gjson.GetBytes(out, "input.0.content.0."+field+".keep").Bool())
			require.True(t, gjson.GetBytes(out, field+".keep").Bool())
			require.Equal(t, `{"internal_chat_message_metadata_passthrough":true}`, gjson.GetBytes(out, "input.1.arguments").String())
			require.Equal(t, "fc_123", gjson.GetBytes(out, "input.1.call_id").String())
			require.Equal(t, "result", gjson.GetBytes(out, "input.2.output").String())
		})
	}
}

func TestCodexInputMetadataMatchesOfficialProvider(t *testing.T) {
	const body = `{"model":"gpt-5.5","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"client instructions"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"],"content_item_metadata":[{"kind":"text"}],"turn_id":"01900000-0000-7000-8000-000000000001"}}]}`
	for _, lite := range []bool{false, true} {
		out, _, err := normalizeOpenAIResponsesWebSocketCompatibilityBody([]byte(body), &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, lite)
		require.NoError(t, err)
		const meta = "input.0.internal_chat_message_metadata_passthrough."
		require.Equal(t, "model.base_instructions", gjson.GetBytes(out, meta+"content_item_kinds.0").String())
		require.Equal(t, lite, gjson.GetBytes(out, meta+"content_item_metadata").Exists())
		require.Equal(t, "01900000-0000-7000-8000-000000000001", gjson.GetBytes(out, meta+"turn_id").String())
		var req map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &req))
		result := applyCodexOAuthTransformWithOptions(req, codexOAuthTransformOptions{IsCodexCLI: true, ResponsesLite: lite, WireProfile: &codexWireProfileMain})
		require.NoError(t, result.Error)
		encoded, err := json.Marshal(req)
		require.NoError(t, err)
		require.Equal(t, "model.base_instructions", gjson.GetBytes(encoded, meta+"content_item_kinds.0").String())
		require.Equal(t, lite, gjson.GetBytes(encoded, meta+"content_item_metadata").Exists())
	}
}

func TestCodexInputMetadataEmptyObjectAndLegacyProfile(t *testing.T) {
	const input = `{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"hi","internal_chat_message_metadata_passthrough":{"content_item_metadata":[{"kind":"text"}]}}]}`
	const path = "input.0.internal_chat_message_metadata_passthrough"
	for _, keep := range []bool{false, true} {
		out, _, err := normalizeOpenAIOAuthResponsesCompatibilityBodyWithMetadata([]byte(input), true, keep)
		require.NoError(t, err)
		require.Equal(t, keep, gjson.GetBytes(out, path).Exists())
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(input), &decoded))
		normalizeOpenAIOAuthResponsesCompatibilityFieldsWithMetadata(decoded, true, keep)
		encoded, err := json.Marshal(decoded)
		require.NoError(t, err)
		require.JSONEq(t, string(out), string(encoded))
	}
	var legacy map[string]any
	require.NoError(t, json.Unmarshal([]byte(input), &legacy))
	result := applyCodexOAuthTransformWithOptions(legacy, codexOAuthTransformOptions{IsCodexCLI: true, WireProfile: &codexWireProfileStable})
	require.NoError(t, result.Error)
	encoded, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(encoded, path+".content_item_metadata").Exists(), "the existing explicit stable profile keeps its clear=false contract")
}

func TestOAuthInputInternalMetadataCompatibility(t *testing.T) {
	for _, body := range []string{
		`{"input":"hello"}`,
		`{"input":{"internal_chat_message_metadata_passthrough":true}}`,
		`{"input":[null,"hello",{"role":"user","content":"hello"}]}`,
		`{"input":[{"content":{"internal_chat_message_metadata_passthrough":true}}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			out, changed, err := normalizeOpenAIOAuthResponsesCompatibilityBody([]byte(body))
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, body, string(out))
			var req map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &req))
			require.False(t, normalizeOpenAIOAuthResponsesCompatibilityFields(req))
		})
	}
	// Legacy prompt aliases and later-turn payloads use the same boundary.
	body := []byte(`{"prompt":[{"role":"user","content":"hello","internal_chat_message_metadata_passthrough":{}}],"previous_response_id":"resp_123"}`)
	out, changed, err := normalizeOpenAIOAuthResponsesCompatibilityBody(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(out, "input.0.internal_chat_message_metadata_passthrough").Exists())
	require.Equal(t, "resp_123", gjson.GetBytes(out, "previous_response_id").String())
	var req map[string]any
	require.NoError(t, json.Unmarshal(body, &req))
	require.True(t, normalizeOpenAIOAuthResponsesCompatibilityFields(req))
	input, ok := req["input"].([]any)
	require.True(t, ok)
	require.NotContains(t, input[0], "internal_chat_message_metadata_passthrough")
}
