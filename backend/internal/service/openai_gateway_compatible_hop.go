package service

import (
	"net/http"
	"strings"
)

// openaiCompatibleSessionHeaders 是 Codex 兼容网关（如本机 vm2api）用来钉槽的会话头。
// 官方 api.openai.com 不需要这些头，自定义 base_url 的 OpenAI apikey 账号才透传。
var openaiCompatibleSessionHeaders = map[string]bool{
	"session-id":        true,
	"x-session-id":      true,
	"x-conversation-id": true,
	"thread-id":         true,
}

const openaiCompatibleDefaultUserAgent = "openai-python"

func usesCustomOpenAICompatibleUpstream(account *Account) bool {
	if account == nil || !account.IsOpenAIApiKey() {
		return false
	}
	base := strings.TrimSpace(account.GetCredential("base_url"))
	if base == "" {
		return false
	}
	return !isOfficialOpenAIModelsBaseURL(base)
}

func allowOpenAIClientHeader(lowerKey string, allow map[string]bool, account *Account) bool {
	if allow[lowerKey] {
		return true
	}
	// Subagent/memgen/timing headers are part of the Codex CLI HTTP request
	// surface. They are copied only on the OAuth Codex lane and only when the
	// client actually sent them; API-key/SetupToken/non-OpenAI accounts keep
	// the legacy whitelist byte-for-byte.
	if isCodexOAuthAccount(account) && codexOAuthExtraClientHeaders[lowerKey] {
		return true
	}
	return usesCustomOpenAICompatibleUpstream(account) && openaiCompatibleSessionHeaders[lowerKey]
}

var codexOAuthExtraClientHeaders = map[string]bool{
	"x-openai-subagent":                     true,
	"x-codex-parent-thread-id":              true,
	"x-openai-memgen-request":               true,
	"x-responsesapi-include-timing-metrics": true,
}

func copyAllowedOpenAIClientHeaders(dst http.Header, src http.Header, allow map[string]bool, account *Account) {
	if dst == nil || src == nil {
		return
	}
	for key, values := range src {
		if !allowOpenAIClientHeader(strings.ToLower(key), allow, account) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func rewriteClaudeClientUAForCustomOpenAIHop(h http.Header, account *Account) {
	if h == nil || !usesCustomOpenAICompatibleUpstream(account) {
		return
	}
	ua := strings.ToLower(h.Get("User-Agent"))
	if ua == "" {
		return
	}
	if strings.Contains(ua, "claude-cli") || strings.Contains(ua, "claude-code") || strings.Contains(ua, "anthropic-ai/sdk") {
		h.Set("User-Agent", openaiCompatibleDefaultUserAgent)
	}
}
