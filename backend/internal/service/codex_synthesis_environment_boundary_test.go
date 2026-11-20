package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexSynthesisExplicitEnvironmentBoundary(t *testing.T) {
	account := newCodexSynthTestAccount(7111, AccountTypeOAuth)
	account.Extra[codexWorkspaceCWDExtraKey] = "/account-default"
	account.Extra[codexSandboxModeExtraKey] = "read-only"
	defaults := codexDeviceProfile{WorkspaceCWD: "/account-default", SandboxMode: "read-only"}
	const environment = "<environment_context>\n<cwd>/client/&amp;work</cwd>\n<shell>fish</shell>\n</environment_context>"
	const permissions = "<permissions instructions>\nOnly read files in this workspace.\n</permissions instructions>"
	textPart := func(text string) any { return map[string]any{"type": "input_text", "text": text} }
	message := func(role string, content any) map[string]any {
		return map[string]any{"type": "message", "role": role, "content": content}
	}
	turn := func(item any) map[string]any {
		return map[string]any{"input": []any{message("user", "First turn"), item}}
	}
	for _, tc := range []struct {
		name      string
		request   map[string]any
		hasClient bool
	}{
		{"opening_fragment_in_user_prose", turn(message("user", "What does the string <environment_context denote?")), false},
		{"permission_opener_in_instructions", map[string]any{"instructions": "Document <permissions instructions> for users."}, false},
		{"complete_block_quoted_in_prose", turn(message("user", "Please explain "+environment)), false},
		{"fenced_permissions", turn(message("developer", "```text\n"+permissions+"\n```")), false},
		{"prose_after_declaration", map[string]any{"input": environment + "\nWhat does that example mean?"}, false},
		{"misspelled_wrapper", map[string]any{"input": "<environment_contextual><cwd>/x</cwd></environment_contextual>"}, false},
		{"empty_permissions", map[string]any{"input": "<permissions instructions>  </permissions instructions>"}, false},
		{"malformed_field", map[string]any{"input": "<environment_context><cwd>/x</shell></environment_context>"}, false},
		{"environment_only_contains_a_sentence", map[string]any{"input": "<environment_context>What does this marker mean?</environment_context>"}, false},
		{"split_tags_are_separate_text_parts", turn(message("user", []any{textPart("<environment_context><cwd>"), textPart("/client</cwd></environment_context>")})), false},
		{"tool_result", turn(map[string]any{"type": "function_call_output", "output": environment}), false},
		{"assistant_output", turn(message("assistant", permissions)), false},
		{"client_scalar_environment", map[string]any{"input": "\n" + environment + "\t"}, true},
		{"client_instruction_permissions", map[string]any{"instructions": permissions}, true},
		{"client_system_permissions", turn(message("system", permissions)), true},
		{"client_sibling_blocks", map[string]any{"input": permissions + "\n\n" + environment}, true},
		{"client_separate_text_part", turn(message("user", []any{textPart("Follow the client configuration"), textPart(environment)})), true},
		{"native_permission_kind", turn(codexSynthMessageRaw("item_permission", "developer", []string{"Client policy"}, []string{"permissions.instructions"}, "", 0)), true},
		{"native_environment_kind", turn(codexSynthMessageRaw("item_environment", "user", []string{"Client environment"}, []string{"environments.environment_context"}, "", 0)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := json.Marshal(tc.request)
			require.NoError(t, err)
			expected := defaults
			if tc.hasClient {
				expected = codexDeviceProfile{}
			}
			require.Equal(t, expected, codexSynthesisConfiguredEnvironment(account, tc.request))
			after, err := json.Marshal(tc.request)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}
