package service

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCodexNativeInputKeepsTypedIdentityWithoutChangingCompatibilityInputs(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{true: "native", false: "compatibility"}[native], func(t *testing.T) {
			original := `[{"type":"message","id":"msg_original","role":"developer","content":[{"type":"input_text","text":"keep"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}},{"type":"function_call","id":"fc_original","call_id":"call_original","name":"lookup","arguments":"{}","status":"completed"},{"type":"function_call_output","id":"fco_original","call_id":"call_original","output":"done","internal_chat_message_metadata_passthrough":{"executed_tool_calls":[{"name":"lookup","arguments":{},"tool_result_metadata":{"result":"late"}}]}}]`
			var input []any
			require.NoError(t, json.Unmarshal([]byte(original), &input))
			request := map[string]any{"model": "gpt-5.5", "input": input, "reasoning": map[string]any{"effort": "high"}}
			profile, _ := resolveCodexWireProfile("0.166.0", codexWireProfileModeAuto)
			result := applyCodexOAuthTransformWithOptions(request, codexOAuthTransformOptions{SkipDefaultInstructions: true, PreserveNativeInput: native, PreserveToolCallIDs: native, WireProfile: &profile})
			require.NoError(t, result.Error)
			encoded, err := json.Marshal(request["input"])
			require.NoError(t, err)
			if native {
				require.JSONEq(t, original, string(encoded))
			} else {
				require.Contains(t, string(encoded), `"call_id":"fc_original"`, "legacy call-ID normalization remains available")
			}
			require.Equal(t, "gpt-5.5", request["model"])
			require.Equal(t, "high", request["reasoning"].(map[string]any)["effort"])
		})
	}
}
