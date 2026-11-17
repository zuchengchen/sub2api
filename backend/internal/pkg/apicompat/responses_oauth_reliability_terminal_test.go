package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOAuthReliabilityChatTerminalRetainsTextWithoutDeltas(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.done", "response.incomplete"} {
		t.Run(eventType, func(t *testing.T) {
			state := NewResponsesEventToChatState()
			state.IncludeUsage = true
			response := &ResponsesResponse{
				Status: "completed",
				Output: []ResponsesOutput{{Type: "message", Content: []ResponsesContentPart{
					{Type: "output_text", Text: "完整"}, {Type: "output_text", Text: " answer"},
				}}},
				Usage: &ResponsesUsage{InputTokens: 5, OutputTokens: 2},
			}
			chunks := ResponsesEventToChatChunks(&ResponsesStreamEvent{Type: eventType, Response: response}, state)
			require.Len(t, chunks, 3)
			require.Equal(t, "完整 answer", *chunks[0].Choices[0].Delta.Content)
			require.NotNil(t, chunks[1].Choices[0].FinishReason)
			require.Equal(t, 7, chunks[2].Usage.TotalTokens)
			require.True(t, state.SawText)
			require.Empty(t, FinalizeResponsesChatStream(state))
		})
	}
}

func TestOAuthReliabilityChatTerminalNeverDuplicatesOrInventsOutput(t *testing.T) {
	for _, test := range []struct {
		name, eventType, priorText string
	}{
		{"already streamed", "response.completed", "partial answer"},
		{"failed terminal", "response.failed", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := NewResponsesEventToChatState()
			if test.priorText != "" {
				chunks := ResponsesEventToChatChunks(&ResponsesStreamEvent{Type: "response.output_text.delta", Delta: test.priorText}, state)
				require.Equal(t, test.priorText, *chunks[0].Choices[0].Delta.Content)
			}
			chunks := ResponsesEventToChatChunks(&ResponsesStreamEvent{Type: test.eventType, Response: &ResponsesResponse{
				Status: "failed", Output: []ResponsesOutput{{Type: "message", Content: []ResponsesContentPart{{Type: "output_text", Text: "terminal answer"}}}},
			}}, state)
			require.Len(t, chunks, 1, "only the existing finish chunk is emitted")
			require.NotNil(t, chunks[0].Choices[0].FinishReason)
			if content := chunks[0].Choices[0].Delta.Content; content != nil {
				require.Empty(t, *content)
			}
		})
	}
}
