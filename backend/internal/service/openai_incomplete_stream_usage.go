package service

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
)

// OpenAI Responses 流里会计入 output token 的增量事件。
// reasoning 对 Luna/Astra 往往占输出大头，不能只数可见的 output_text。
func isOpenAIStreamedBillingDelta(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.output_text.delta",
		"response.reasoning_text.delta",
		"response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta":
		return true
	default:
		return false
	}
}

// applyEstimatedOpenAIUsageIfMissing 在上游没给出 usage（流中断、缺 terminal）时，
// 用请求体 + 已收到的增量文本做本地 tiktoken 估算，避免记成 0 token / $0。
// 已有真实 usage 时不覆盖。
func applyEstimatedOpenAIUsageIfMissing(usage *OpenAIUsage, model string, requestBody []byte, streamedOutput string) {
	if usage == nil || openAIUsageHasTokens(usage) {
		return
	}
	inputTokens := estimateOpenAIRequestInputTokens(model, requestBody)
	outputTokens := 0
	if text := strings.TrimSpace(streamedOutput); text != "" {
		if n, err := countOpenAITextTokens(model, text); err == nil {
			outputTokens = n
		}
	}
	if inputTokens <= 0 && outputTokens <= 0 {
		return
	}
	if inputTokens > 0 {
		usage.InputTokens = inputTokens
	}
	if outputTokens > 0 {
		usage.OutputTokens = outputTokens
	}
}

func estimateOpenAIRequestInputTokens(model string, requestBody []byte) int {
	body := bytes.TrimSpace(requestBody)
	if len(body) == 0 {
		return 0
	}
	model = strings.TrimSpace(model)

	if gjson.GetBytes(body, "messages").IsArray() {
		var chat apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(body, &chat); err == nil && len(chat.Messages) > 0 {
			if strings.TrimSpace(chat.Model) == "" {
				chat.Model = model
			}
			converted, err := apicompat.ChatCompletionsToResponses(&chat)
			if err == nil && converted != nil {
				if n, err := estimateOpenAIInputTokens(openAIInputTokensCountRequest{
					Model:        firstNonEmptyStringValue(converted.Model, model),
					Instructions: converted.Instructions,
					Input:        converted.Input,
					Tools:        converted.Tools,
					ToolChoice:   converted.ToolChoice,
				}); err == nil {
					return n
				}
			}
		}
	}

	var responses apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &responses); err == nil {
		if len(bytes.TrimSpace(responses.Input)) > 0 || strings.TrimSpace(responses.Instructions) != "" {
			if n, err := estimateOpenAIInputTokens(openAIInputTokensCountRequest{
				Model:        firstNonEmptyStringValue(responses.Model, model),
				Instructions: responses.Instructions,
				Input:        responses.Input,
				Tools:        responses.Tools,
				ToolChoice:   responses.ToolChoice,
			}); err == nil {
				return n
			}
		}
	}
	return 0
}

func countOpenAITextTokens(model, text string) (int, error) {
	codec, err := openAIInputTokensCodecForModel(model)
	if err != nil {
		return 0, err
	}
	return codec.Count(text)
}

func firstNonEmptyStringValue(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
