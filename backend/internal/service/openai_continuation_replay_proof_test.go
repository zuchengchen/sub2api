//go:build unit

package service

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOAuthReliabilityContinuationProofInlineFamilies(t *testing.T) {
	cases := map[string]string{
		"function":                      `{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}`,
		"native_mcp_uses_function_wire": `{"type":"function_call","call_id":"c","name":"lookup","namespace":"mcp__server","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":"ok"}]}`,
		"custom":                        `{"type":"custom_tool_call","call_id":"c","name":"apply_patch","input":"patch"},{"type":"custom_tool_call_output","call_id":"c","output":"ok"}`,
		"native_local_shell":            `{"type":"local_shell_call","call_id":"c","status":"completed","action":{"type":"exec","command":["true"]}},{"type":"function_call_output","call_id":"c","output":"ok"}`,
		"client_tool_search":            `{"type":"tool_search_call","call_id":"c","execution":"client","arguments":{"query":"lookup"}},{"type":"tool_search_output","call_id":"c","execution":"client","status":"completed","tools":[]}`,
		"computer":                      `{"type":"computer_call","call_id":"c","action":{"type":"screenshot"}},{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,c3ludGhldGlj"}}`,
		"local_shell_api":               `{"type":"shell_call","call_id":"c","environment":{"type":"local"},"action":{"commands":["true"]}},{"type":"shell_call_output","call_id":"c","output":[{"stdout":"ok","stderr":"","outcome":{"type":"exit","exit_code":0}}]}`,
		"apply_patch":                   `{"type":"apply_patch_call","call_id":"c","status":"completed","operation":{"type":"delete_file","path":"synthetic.txt"}},{"type":"apply_patch_call_output","call_id":"c","status":"completed"}`,
		"apply_patch_error_result":      `{"type":"apply_patch_call","call_id":"c","status":"completed","operation":{"type":"update_file","path":"synthetic.txt","diff":"synthetic patch"}},{"type":"apply_patch_call_output","call_id":"c","status":"failed","output":"No such file"}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"previous_response_id":"resp_synthetic","input":[` + input + `]}`)
			original := bytes.Clone(body)
			proof := AnalyzeOpenAIContinuationReplayProof(body)
			require.Equal(t, OpenAIContinuationProofInline, proof.Verdict, "%+v", proof)
			require.Equal(t, "complete_inline_tool_context", proof.Reason)
			require.True(t, proof.HasToolOutput)
			require.True(t, proof.HasPreviousResponseID)
			require.Equal(t, 1, proof.Calls)
			require.Equal(t, 1, proof.Outputs)
			require.Equal(t, original, body)
		})
	}
}

func TestOAuthReliabilityContinuationProofDetectsEveryOutputFamily(t *testing.T) {
	for _, kind := range []string{
		"function_call_output", "custom_tool_call_output", "tool_search_output", "mcp_tool_call_output",
		"local_shell_call_output", "computer_call_output", "shell_call_output", "apply_patch_call_output",
		"program_output",
	} {
		t.Run(kind, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"previous_response_id":"resp_keep","input":[{"type":%q,"call_id":"c","id":"legacy_local_shell","output":"synthetic"}]}`, kind))
			original := bytes.Clone(body)
			proof := AnalyzeOpenAIContinuationReplayProof(body)
			require.True(t, proof.HasToolOutput, "a missed family must never authorize removing a continuation anchor")
			require.Equal(t, 1, proof.Outputs)
			require.Equal(t, OpenAIContinuationProofUnproven, proof.Verdict)
			require.True(t, proof.HasPreviousResponseID)
			require.Equal(t, original, body)
		})
	}
}

func TestOAuthReliabilityContinuationProofRejectsFalseCoverage(t *testing.T) {
	cases := map[string]struct {
		input  string
		reason string
	}{
		"item_reference_is_not_inline":            {`{"type":"item_reference","id":"c"},{"type":"function_call_output","call_id":"c","output":"preserve"}`, "unresolved_item_reference"},
		"implicit_item_reference":                 {`{"id":"c"},{"type":"function_call_output","call_id":"c","output":"preserve"}`, "unresolved_item_reference"},
		"cross_family_call_id":                    {`{"type":"custom_tool_call","call_id":"c","name":"apply_patch","input":"patch"},{"type":"function_call_output","call_id":"c","output":"preserve"}`, "tool_family_mismatch"},
		"orphan":                                  {`{"type":"function_call_output","call_id":"c","output":"preserve"}`, "missing_preceding_call"},
		"result_before_call":                      {`{"type":"function_call_output","call_id":"c","output":"preserve"},{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"}`, "missing_preceding_call"},
		"duplicate_calls":                         {`{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"preserve"}`, "duplicate_call_id"},
		"duplicate_results":                       {`{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"preserve"},{"type":"function_call_output","call_id":"c","output":"again"}`, "duplicate_output"},
		"missing_call_fields":                     {`{"type":"function_call","call_id":"c"},{"type":"function_call_output","call_id":"c","output":"preserve"}`, "incomplete_call"},
		"missing_output":                          {`{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"}`, "missing_call_output"},
		"partial_output":                          {`{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","status":"in_progress","output":"partial"}`, "incomplete_output"},
		"partial_call":                            {`{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}","status":"in_progress"},{"type":"function_call_output","call_id":"c","output":"partial"}`, "incomplete_call"},
		"partial_shell_output":                    {`{"type":"shell_call","call_id":"c","environment":{"type":"local"},"action":{"commands":["true"]}},{"type":"shell_call_output","call_id":"c","output":[{"stdout":"partial"}]}`, "incomplete_output"},
		"unproven_computer_action":                {`{"type":"computer_call","call_id":"c","action":{"type":"click"}},{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","image_url":"synthetic"}}`, "unsupported_typed_call"},
		"numeric_call_id":                         {`{"type":"function_call","call_id":1,"name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"1","output":"preserve"}`, "missing_or_invalid_call_id"},
		"trimmed_call_id_collision":               {`{"type":"function_call","call_id":" c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"preserve"}`, "missing_or_invalid_call_id"},
		"name_contradiction":                      {`{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","name":"other","output":"preserve"}`, "tool_identity_mismatch"},
		"namespace_contradiction":                 {`{"type":"function_call","call_id":"c","name":"lookup","namespace":"mcp_one","arguments":"{}"},{"type":"function_call_output","call_id":"c","namespace":"mcp_two","output":"preserve"}`, "tool_identity_mismatch"},
		"native_mcp_not_a_new_wire_family":        {`{"type":"mcp_tool_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"preserve"}`, "unproven_wire_family"},
		"legacy_local_shell_identity_not_guessed": {`{"type":"local_shell_call","id":"lsc_c","call_id":"c","action":{"type":"exec","command":["true"]}},{"type":"local_shell_call_output","id":"c","output":"preserve"}`, "unproven_wire_family"},
		"unknown_item":                            {`{"type":"future_tool_output","call_id":"c","output":"preserve"}`, "unsupported_or_incomplete_item"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"input":[` + tc.input + `]}`)
			original := bytes.Clone(body)
			proof := AnalyzeOpenAIContinuationReplayProof(body)
			require.Equal(t, OpenAIContinuationProofUnproven, proof.Verdict)
			require.Equal(t, tc.reason, proof.Reason)
			require.Equal(t, original, body)
		})
	}
}

func TestOAuthReliabilityContinuationProofPreservesOpaqueState(t *testing.T) {
	for _, input := range []string{
		`{"type":"reasoning","summary":[],"encrypted_content":"synthetic-ciphertext"}`,
		`{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file_synthetic"}]}`,
		`{"type":"shell_call","call_id":"c","environment":{"type":"container_reference","container_id":"cntr_synthetic"},"action":{"commands":["true"]}}`,
		`{"type":"function_call","call_id":"c","caller":{"type":"program","caller_id":"program_c"},"name":"lookup","arguments":"{}"}`,
		`{"type":"mcp_approval_response","approval_request_id":"approval_synthetic","approve":true}`,
		`{"type":"tool_search_output","execution":"server","status":"completed","tools":[]}`,
		`{"type":"program_output","call_id":"c","result":"synthetic","status":"completed"}`,
	} {
		body := []byte(`{"input":[` + input + `]}`)
		original := bytes.Clone(body)
		proof := AnalyzeOpenAIContinuationReplayProof(body)
		require.Equal(t, OpenAIContinuationProofUnproven, proof.Verdict)
		require.True(t, proof.HasOpaqueState)
		require.Equal(t, "credential_or_hosted_state", proof.Reason)
		require.Equal(t, original, body)
		require.NotContains(t, fmt.Sprintf("%+v", proof), "synthetic")
	}
}

func TestOAuthReliabilityContinuationProofRejectsAmbiguousJSON(t *testing.T) {
	for _, body := range []string{
		`{"input":[],"input":[{"type":"function_call_output","call_id":"c","output":"preserve"}]}`,
		`{"input":[{"type":"message","role":"user","content":"safe","role":"tool"}]}`,
		`{"input":[{"type":"function_call","call_id":"c","call\u005fid":"other","name":"lookup","arguments":"{}"}]}`,
		`{"input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":"one","text":"two"}]}]}`,
		`{"input":[]`,
	} {
		proof := AnalyzeOpenAIContinuationReplayProof([]byte(body))
		require.Equal(t, OpenAIContinuationProofUnproven, proof.Verdict)
		require.Equal(t, "invalid_ambiguous_or_bounded_json", proof.Reason)
	}
}

func TestOAuthReliabilityContinuationProofBoundsAndLargeResults(t *testing.T) {
	largeResult := strings.Repeat("x", 2<<20)
	body := []byte(`{"input":[{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"` + largeResult + `"}]}`)
	original := bytes.Clone(body)
	require.Equal(t, OpenAIContinuationProofInline, AnalyzeOpenAIContinuationReplayProof(body).Verdict)
	require.Equal(t, original, body)
	tooMany := []byte(`{"input":[` + strings.Repeat(`{"role":"user","content":"ok"},`, openAIContinuationProofMaxItems) + `{"role":"user","content":"ok"}]}`)
	proof := AnalyzeOpenAIContinuationReplayProof(tooMany)
	require.Equal(t, OpenAIContinuationProofUnproven, proof.Verdict)
	require.Equal(t, "analysis_limit", proof.Reason)
	tooDeep := []byte(`{"input":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + `}`)
	require.Equal(t, OpenAIContinuationProofUnproven, AnalyzeOpenAIContinuationReplayProof(tooDeep).Verdict)
	longID := strings.Repeat("c", openAIContinuationProofMaxID+1)
	require.Equal(t, "missing_or_invalid_call_id", AnalyzeOpenAIContinuationReplayProof([]byte(`{"input":{"type":"function_call_output","call_id":"`+longID+`","output":"preserve"}}`)).Reason)
	tooLarge := make([]byte, openAIContinuationProofMaxBytes+1)
	require.Equal(t, "analysis_limit", AnalyzeOpenAIContinuationReplayProof(tooLarge).Reason)
}

func TestOAuthReliabilityContinuationProofNamedDelegationAndQuotedData(t *testing.T) {
	body := []byte(`{"previous_response_id":"resp_keep","input":[{"role":"user","content":"quoted item_reference and file_id are text"},{"type":"function_call_output","name":"send_message_to_thread","namespace":"codex_app","output":"{\"encrypted_content\":\"synthetic\",\"type\":\"item_reference\"}"}]}`)
	original := bytes.Clone(body)
	proof := AnalyzeOpenAIContinuationReplayProof(body)
	require.Equal(t, OpenAIContinuationProofInline, proof.Verdict)
	require.True(t, proof.HasToolOutput)
	require.False(t, proof.HasOpaqueState)
	require.False(t, proof.HasItemReference)
	require.Equal(t, 0, proof.Calls)
	require.Equal(t, 1, proof.Outputs)
	require.Equal(t, original, body)
	for _, plain := range []string{`{"input":"hi"}`, `{"input":[]}`, `{"input":[{"role":"user","content":"hello"}]}`} {
		require.Equal(t, OpenAIContinuationProofNoDependency, AnalyzeOpenAIContinuationReplayProof([]byte(plain)).Verdict)
	}
}
