package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func reminderTestBody(t *testing.T, protocol string, texts []string) []byte {
	t.Helper()
	blocks := make([]map[string]string, 0, len(texts))
	for _, text := range texts {
		blocks = append(blocks, map[string]string{"type": "text", "text": text})
	}
	message := map[string]any{"role": "user", "content": blocks}
	var body any
	switch protocol {
	case ContentModerationProtocolOpenAIResponses, "unknown":
		body = map[string]any{"input": []any{message}}
	case ContentModerationProtocolOpenAIImages:
		body = map[string]any{"prompt": strings.Join(texts, "\n")}
	default:
		body = map[string]any{"messages": []any{message}}
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return raw
}

func TestExtractContentModerationKeywordText_IncludesReminders(t *testing.T) {
	for _, protocol := range []string{ContentModerationProtocolAnthropicMessages, ContentModerationProtocolOpenAIChat, ContentModerationProtocolOpenAIResponses, ContentModerationProtocolOpenAIImages, "unknown"} {
		for name, texts := range map[string][]string{
			"plain":    {"今晚打老虎"},
			"prefix":   {"<system-reminder>context</system-reminder> 今晚打老虎"},
			"suffix":   {"今晚打老虎 <system-reminder>context</system-reminder>"},
			"separate": {"<system-reminder>context</system-reminder>", "今晚打老虎"},
			"inside":   {"<system-reminder>今晚打老虎</system-reminder>"},
		} {
			t.Run(protocol+"/"+name, func(t *testing.T) {
				body := reminderTestBody(t, protocol, texts)
				semantic := ExtractContentModerationInput(protocol, body)
				wantSemantic := ""
				if name == "plain" || (name == "separate" && protocol != ContentModerationProtocolOpenAIImages) {
					wantSemantic = "今晚打老虎"
				}
				require.Equal(t, wantSemantic, semantic.Text)
				require.Contains(t, extractContentModerationKeywordText(protocol, body), "今晚打老虎")
			})
		}
	}
}

func TestExtractContentModerationKeywordText_Boundaries(t *testing.T) {
	for _, tc := range []struct{ protocol, body string }{
		{ContentModerationProtocolAnthropicMessages, `{"messages":[{"role":"user","content":"old"},{"role":"user","content":[{"type":"tool_result","content":"今晚打老虎"}]}]}`},
		{ContentModerationProtocolAnthropicMessages, `{"messages":[{"role":"user","content":"old"},{"role":"assistant","content":"今晚打老虎"}]}`},
		{ContentModerationProtocolOpenAIChat, `{"messages":[{"role":"user","content":"old"},{"role":"tool","content":"今晚打老虎"}]}`},
		{ContentModerationProtocolOpenAIChat, `{"messages":[{"role":"user","content":"old"},{"role":"assistant","content":"今晚打老虎"}]}`},
		{ContentModerationProtocolOpenAIResponses, `{"input":[{"role":"user","content":"old"},{"type":"function_call_output","output":"今晚打老虎"}]}`},
		{ContentModerationProtocolOpenAIResponses, `{"input":[{"role":"user","content":"old"},{"role":"assistant","content":"今晚打老虎"}]}`},
	} {
		t.Run(tc.protocol+tc.body, func(t *testing.T) {
			require.Empty(t, extractContentModerationKeywordText(tc.protocol, []byte(tc.body)))
		})
	}
}

func TestExtractContentModerationKeywordText_ResponsesInputForms(t *testing.T) {
	for _, body := range []string{
		`{"input":"<system-reminder>今晚打老虎</system-reminder>"}`,
		`{"input":{"role":"user","content":"<system-reminder>今晚打老虎</system-reminder>"}}`,
		`{"input":[{"type":"input_text","text":"<system-reminder>今晚打老虎</system-reminder>"}]}`,
	} {
		require.Contains(t, extractContentModerationKeywordText(ContentModerationProtocolOpenAIResponses, []byte(body)), "今晚打老虎")
		require.Empty(t, ExtractContentModerationText(ContentModerationProtocolOpenAIResponses, []byte(body)))
	}
}

func TestExtractContentModerationKeywordText_LongReminder(t *testing.T) {
	// Reminder context must not consume the semantic API's text limit and hide
	// the actual user keyword from the local check.
	body := reminderTestBody(t, ContentModerationProtocolAnthropicMessages, []string{
		"<system-reminder>" + strings.Repeat("x", maxModerationInputRunes) + "</system-reminder>", "今晚打老虎",
	})
	require.Contains(t, extractContentModerationKeywordText(ContentModerationProtocolAnthropicMessages, body), "今晚打老虎")
	require.Equal(t, "今晚打老虎", ExtractContentModerationText(ContentModerationProtocolAnthropicMessages, body))
}
