package service

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	grokChatReasoningHeartbeatMark     = "…"
	grokChatReasoningHeartbeatZWSP     = "\u200b"
	grokChatReasoningDeadlineWarnText  = "Grok reasoning is approaching the 58m gateway deadline"
	grokChatReasoningDeadlineAbortMsg  = "Grok reasoning exceeded 58m gateway deadline"
	grokChatReasoningIdleTimeoutCode   = "grok_stream_idle"
	grokChatReasoningDeadlineCode      = "grok_reasoning_deadline"
	grokChatReasoningWindowCode        = "grok_reasoning_window"
	grokChatReasoningWindowMsg         = "Grok reasoning filled the remaining context window"
	grokChatReasoningHeartbeatInterval = 15 * time.Second
	grokChatWallClockWarn              = 3300 * time.Second
	grokChatWallClockAbort             = 3500 * time.Second
	grokChatReasoningWindowTokens      = 480000
	grokChatReasoningTokensLimit       = 200000
)

func grokChatChunksHaveVisibleDelta(chunks []apicompat.ChatCompletionsChunk) bool {
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			if grokChatDeltaIsVisible(choice.Delta) {
				return true
			}
		}
	}
	return false
}

func grokChatDeltaIsVisible(delta apicompat.ChatDelta) bool {
	if delta.Content != nil && strings.TrimSpace(*delta.Content) != "" {
		return true
	}
	if delta.ReasoningContent != nil && strings.TrimSpace(stripGrokSyntheticReasoningText(*delta.ReasoningContent)) != "" {
		return true
	}
	if delta.Reasoning != nil && strings.TrimSpace(stripGrokSyntheticReasoningText(*delta.Reasoning)) != "" {
		return true
	}
	return len(delta.ToolCalls) > 0
}

func stripGrokSyntheticReasoningText(raw string) string {
	cleaned := strings.ReplaceAll(raw, grokChatReasoningHeartbeatMark, "")
	cleaned = strings.ReplaceAll(cleaned, grokChatReasoningHeartbeatZWSP, "")
	cleaned = strings.ReplaceAll(cleaned, grokChatReasoningDeadlineWarnText, "")
	return strings.TrimSpace(cleaned)
}

func grokChatReasoningHeartbeatChunk(state *apicompat.ResponsesEventToChatState, originalModel, text string) apicompat.ChatCompletionsChunk {
	mark := text
	if strings.TrimSpace(mark) == "" {
		mark = grokChatReasoningHeartbeatMark
	}
	chunk := apicompat.ChatCompletionsChunk{
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   originalModel,
		Choices: []apicompat.ChatChunkChoice{{
			Index: 0,
			Delta: apicompat.ChatDelta{ReasoningContent: &mark},
		}},
	}
	if state != nil {
		chunk.ID = state.ID
		if state.Created != 0 {
			chunk.Created = state.Created
		}
		if strings.TrimSpace(state.Model) != "" {
			chunk.Model = state.Model
		}
		chunk.ServiceTier = state.ServiceTier
	}
	return chunk
}

func grokChatWindowFuseExceeded(usage OpenAIUsage, payload []byte) bool {
	if usage.InputTokens+usage.OutputTokens >= grokChatReasoningWindowTokens {
		return true
	}
	return grokChatReasoningTokensFromPayload(payload) >= grokChatReasoningTokensLimit
}

func grokChatReasoningTokensFromPayload(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	root := gjson.ParseBytes(payload)
	for _, path := range []string{
		"usage.output_tokens_details.reasoning_tokens",
		"usage.completion_tokens_details.reasoning_tokens",
		"response.usage.output_tokens_details.reasoning_tokens",
		"response.usage.completion_tokens_details.reasoning_tokens",
	} {
		if n := int(root.Get(path).Int()); n > 0 {
			return n
		}
	}
	return 0
}

func grokChatAbortMessage(code, fallback string) string {
	switch code {
	case grokChatReasoningDeadlineCode:
		return grokChatReasoningDeadlineAbortMsg
	case grokChatReasoningWindowCode:
		return grokChatReasoningWindowMsg
	case grokChatReasoningIdleTimeoutCode:
		return fallback
	default:
		if strings.TrimSpace(fallback) != "" {
			return fallback
		}
		return "Grok stream aborted"
	}
}

func ensureGrokResponsesReasoningSummary(body []byte) ([]byte, error) {
	effort := strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String())
	if effort == "" {
		return body, nil
	}
	if strings.TrimSpace(gjson.GetBytes(body, "reasoning.summary").String()) != "" {
		return body, nil
	}
	return sjson.SetBytes(body, "reasoning.summary", "auto")
}

func sanitizeGrokChatSyntheticReasoningHistory(body []byte) ([]byte, error) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, nil
	}
	arr := messages.Array()
	kept := make([]json.RawMessage, 0, len(arr))
	changed := false
	for _, item := range arr {
		raw := []byte(item.Raw)
		if item.Get("role").String() != "assistant" {
			kept = append(kept, raw)
			continue
		}
		cleaned, itemChanged, err := sanitizeGrokChatAssistantReasoning(raw)
		if err != nil {
			return nil, err
		}
		if itemChanged {
			changed = true
		}
		if grokChatAssistantMessageEmpty(cleaned) {
			changed = true
			continue
		}
		kept = append(kept, cleaned)
	}
	if !changed {
		return body, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "messages", encoded)
}

func sanitizeGrokChatAssistantReasoning(raw []byte) ([]byte, bool, error) {
	out := raw
	changed := false
	for _, field := range []string{"reasoning_content", "reasoning"} {
		val := gjson.GetBytes(out, field)
		if !val.Exists() || val.Type != gjson.String {
			continue
		}
		stripped := stripGrokSyntheticReasoningText(val.String())
		if stripped == val.String() {
			continue
		}
		var err error
		if stripped == "" {
			out, err = sjson.DeleteBytes(out, field)
		} else {
			out, err = sjson.SetBytes(out, field, stripped)
		}
		if err != nil {
			return nil, false, err
		}
		changed = true
	}
	return out, changed, nil
}

func grokChatAssistantMessageEmpty(raw []byte) bool {
	if gjson.GetBytes(raw, "tool_calls").Exists() && len(gjson.GetBytes(raw, "tool_calls").Array()) > 0 {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(raw, "reasoning_content").String()) != "" {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(raw, "reasoning").String()) != "" {
		return false
	}
	content := gjson.GetBytes(raw, "content")
	if !content.Exists() || content.Type == gjson.Null {
		return true
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()) == ""
	}
	if content.IsArray() {
		return len(content.Array()) == 0
	}
	return strings.TrimSpace(content.Raw) == "" || content.Raw == "null"
}
