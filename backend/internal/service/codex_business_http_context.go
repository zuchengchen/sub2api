package service

import (
	"context"
	"strings"
)

type codexBusinessHTTPContextKey struct{}

// WithCodexBusinessHTTP marks a per-request Codex API client. Apply it to a
// cloned request at the credential-owner boundary; an explicit false clears an
// inherited OAuth marker when a later attempt selects a different account type.
func WithCodexBusinessHTTP(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, codexBusinessHTTPContextKey{}, enabled)
}

func CodexBusinessHTTPFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(codexBusinessHTTPContextKey{}).(bool)
	return enabled
}

func codexBusinessHTTPAccount(account *Account) bool {
	return isCodexOAuthAccount(account) && !account.IsOpenAIPersonalAccessToken() &&
		!account.IsOpenAIAgentIdentity() && !strings.HasPrefix(strings.TrimSpace(account.GetOpenAIAccessToken()), "at-")
}
