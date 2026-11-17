package service

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	// OpenAIModelUnavailableReason preserves account-specific model availability
	// after the existing account-selection budget is exhausted. It does not grant
	// another attempt, change the model, or establish an account's capabilities.
	OpenAIModelUnavailableReason        = GatewayFailureReason("openai_model_unavailable")
	OpenAIModelUnavailableClientMessage = "The requested model is unavailable for this request. Check the model name and account access."
)

// IsOpenAIModelUnavailableFailover identifies the semantic hint assigned by
// the upstream classifier. StatusCode remains the actual transport status;
// handlers should use ClientStatusCode for the final client error only.
func IsOpenAIModelUnavailableFailover(err *UpstreamFailoverError) bool {
	return err != nil && err.Reason == OpenAIModelUnavailableReason && err.ClientStatusCode == http.StatusBadRequest
}

// OpenAIModelUnavailableClientErrorBody supplies a fixed model-parameter error
// for HTTP and stream exhaustion writers. No upstream text, model identifier,
// request content, or credential is copied into this payload.
func OpenAIModelUnavailableClientErrorBody() []byte {
	return []byte(`{"error":{"type":"invalid_request_error","code":"model_not_found","param":"model","message":"` + OpenAIModelUnavailableClientMessage + `"}}`)
}

// isOpenAIModelUnavailableError intentionally differs from the older routing
// heuristic: final client semantics require unambiguous structured evidence.
// Only a transport 400 or an intermediary's 502 wrapping the same model error
// qualifies. HTTP 200, auth failures, rate limits and arbitrary 5xx do not.
func isOpenAIModelUnavailableError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusBadGateway {
		return false
	}
	if len(body) == 0 || len(body) > maxOpenAIErrorNormalizationBytes || !openAIUnambiguousJSONObject(body) {
		return false
	}
	root := parseRawJSONView(body)
	if response := root.Get("response"); response.Exists() {
		if !response.IsObject() || !response.Get("error").IsObject() {
			return false
		}
		if status := response.Get("status"); status.Exists() && (status.Type != gjson.String || status.String() != "failed") {
			return false
		}
	}
	if normalized, ok := normalizeOpenAIUpstreamError(body); ok {
		if normalized.StatusCode != 0 && normalized.StatusCode != http.StatusBadRequest {
			return false
		}
		typ := strings.TrimSpace(normalized.Type)
		param := strings.TrimSpace(normalized.Param)
		if (typ != "" && typ != "invalid_request_error") || (param != "" && param != "model") {
			return false
		}
		// A second provider error field may not contradict the selected error.
		if detail := root.Get("detail"); detail.Exists() && detail.Type != gjson.Null &&
			(detail.Type != gjson.String || detail.String() != normalized.Message) {
			return false
		}
		switch strings.TrimSpace(normalized.Code) {
		case "model_not_found":
			return true
		case "model_not_available", "unsupported_model", "invalid_model":
			return param == "model"
		case "", "invalid_value":
			return isOpenAICodexUnsupportedModelSentence(normalized.Message)
		default:
			return false
		}
	}
	// ChatGPT's HTTP API also emits {"detail":"..."}. Accept this observed
	// schema only, not a detail field embedded in response data or another error.
	detail := root.Get("detail")
	if detail.Type != gjson.String || !isOpenAICodexUnsupportedModelSentence(detail.String()) {
		return false
	}
	valid := true
	root.ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "detail":
		case "status", "status_code":
			if value.Type != gjson.Null {
				status, err := strconv.Atoi(value.String())
				valid = (value.Type == gjson.Number || value.Type == gjson.String) && err == nil && status == http.StatusBadRequest
			}
		default:
			valid = false
		}
		return valid
	})
	return valid
}

// The full provider sentence is the evidence; the token is bounded syntax,
// never a model denylist. Quotes, whitespace, markup and JSON cannot be model
// identifiers here, so quoted examples cannot masquerade as this error.
func isOpenAICodexUnsupportedModelSentence(message string) bool {
	const prefix = "The '"
	const suffix = "' model is not supported when using Codex with a ChatGPT account."
	if !strings.HasPrefix(message, prefix) || !strings.HasSuffix(message, suffix) {
		return false
	}
	model := strings.TrimSuffix(strings.TrimPrefix(message, prefix), suffix)
	if len(model) == 0 || len(model) > 128 {
		return false
	}
	for _, char := range model {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == ':' || char == '/') {
			return false
		}
	}
	return true
}
