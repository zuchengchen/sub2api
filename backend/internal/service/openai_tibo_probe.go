package service

import (
	"errors"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const openAITiboProbePrompt = "Tell me whether you know who Tibo is in OpenAI based on your knowledge without searching online. Only return True or  False."
const openAITiboProbeBodyLimit = 4 << 20

var errOpenAITiboAccountUnavailable = errors.New("tibo account is unavailable")

func openAITiboProbePayload() map[string]any {
	return map[string]any{
		"model": openAICodexTicketDefaultModel, "store": false,
		"stream": true, "reasoning": map[string]any{"effort": "low"},
		"include": []string{"reasoning.encrypted_content"},
		"input":   []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": openAITiboProbePrompt}}}},
	}
}

func openAICookieWSIsProbePayloadRaw(payload []byte) bool {
	if !gjson.ValidBytes(payload) || gjson.GetBytes(payload, "model").String() != openAICodexTicketDefaultModel ||
		strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) != "" ||
		strings.TrimSpace(gjson.GetBytes(payload, "instructions").String()) != "" ||
		len(gjson.GetBytes(payload, "tools").Array()) != 0 {
		return false
	}
	input := gjson.GetBytes(payload, "input")
	if input.Type == gjson.String {
		return input.String() == openAITiboProbePrompt
	}
	items := input.Array()
	if len(items) != 1 || items[0].Get("role").String() != "user" {
		return false
	}
	content := items[0].Get("content")
	if content.Type == gjson.String {
		return content.String() == openAITiboProbePrompt
	}
	parts := content.Array()
	return len(parts) == 1 && parts[0].Get("type").String() == "input_text" && parts[0].Get("text").String() == openAITiboProbePrompt
}

type openAITiboHTTPObservation struct {
	delta                strings.Builder
	completed            bool
	successfulCompletion bool
	failed               bool
	trueAnswer           bool
	modelMatch           bool
	answerClass          string
}

func (o *openAITiboHTTPObservation) event(payload []byte, eventType string) {
	if !gjson.ValidBytes(payload) {
		return
	}
	if eventType == "" {
		eventType = gjson.GetBytes(payload, "type").String()
	}
	switch eventType {
	case "error", "response.failed", "response.incomplete", "response.cancelled":
		o.failed = true
	case "response.output_text.delta":
		_, _ = o.delta.WriteString(gjson.GetBytes(payload, "delta").String())
	case "response.completed":
		if o.completed {
			o.failed = true
			return
		}
		o.completed = true
		model, success := openAICodexSuccessfulCompletionModel(payload, eventType)
		o.successfulCompletion = success
		// Same rule as the passive business-response check.
		o.modelMatch = success && upstreamModelsMatchForAudit(openAICodexTicketDefaultModel, model)
		if !o.modelMatch {
			o.failed = true
			return
		}
		var output strings.Builder
		for _, item := range gjson.GetBytes(payload, "response.output").Array() {
			for _, content := range item.Get("content").Array() {
				if content.Get("type").String() == "output_text" {
					_, _ = output.WriteString(content.Get("text").String())
				}
				if content.Get("type").String() == "refusal" {
					o.failed = true
				}
			}
		}
		answer := output.String()
		if answer == "" {
			answer = o.delta.String()
		}
		o.answerClass = classifyTiboAnswer(answer)
		o.trueAnswer = o.answerClass == tiboAnswerTrue
	}
}

func observeOpenAITiboHTTPProbe(body []byte) *openAITiboHTTPObservation {
	observation := &openAITiboHTTPObservation{answerClass: "none"}
	if len(body) == 0 || len(body) > openAITiboProbeBodyLimit {
		observation.failed = true
		return observation
	}
	forEachOpenAISSEFrame(string(body), func(eventType string, payload []byte) { observation.event(payload, eventType) })
	return observation
}

func openAITiboAccountProxyURL(account *Account) string {
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func openAITiboAccountSkipReason(account *Account, now time.Time) string {
	if account == nil || account.ID <= 0 || !isOpenAITiboAccount(account) {
		return "not_tibo_account"
	}
	if account.Status != StatusActive {
		return "not_active"
	}
	if !account.Schedulable {
		return "manual_unschedulable"
	}
	if account.AutoPauseOnExpired && account.ExpiresAt != nil && !now.Before(*account.ExpiresAt) {
		return "expired"
	}
	if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
		return "rate_limited"
	}
	if account.OverloadUntil != nil && now.Before(*account.OverloadUntil) {
		return "overloaded"
	}
	if account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil) {
		return "temporarily_unschedulable"
	}
	if reset := account.modelRateLimitResetAt(openAICodexTicketDefaultModel); reset != nil && now.Before(*reset) {
		return "model_rate_limited"
	}
	return ""
}
