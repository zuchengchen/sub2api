package service

import (
	"context"
	"net/http"
)

type openAIRequestCompressionPolicyKey struct{}

// Keep the decision local to this HTTP attempt. It must not become an upstream
// header or be inferred from another attempt's encoding.
func setOpenAIRequestCompressionPolicy(request *http.Request, policy string) {
	if request == nil {
		return
	}
	policy = normalizeOpenAIRequestCompressionPolicy(policy)
	*request = *request.WithContext(context.WithValue(request.Context(), openAIRequestCompressionPolicyKey{}, policy))
}

func openAIRequestCompressionPolicy(request *http.Request) string {
	if request == nil {
		return "unknown"
	}
	policy, _ := request.Context().Value(openAIRequestCompressionPolicyKey{}).(string)
	return normalizeOpenAIRequestCompressionPolicy(policy)
}

func normalizeOpenAIRequestCompressionPolicy(policy string) string {
	switch policy {
	case "disabled", "zstd", "ineligible", "existing_encoding":
		return policy
	default:
		return "unknown"
	}
}
