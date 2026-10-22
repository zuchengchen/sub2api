package openai

import "context"

type codexClientUAContextKey struct{}

// WithCodexClientUserAgent carries the account-level device-profile UA through
// background calls (OAuth refresh, token exchange, whoami) so they use the same
// family/platform identity as inference.
func WithCodexClientUserAgent(ctx context.Context, userAgent string) context.Context {
	if ctx == nil || userAgent == "" {
		return ctx
	}
	return context.WithValue(ctx, codexClientUAContextKey{}, userAgent)
}

// CodexClientUserAgentFromContext returns the account-level override, if any.
func CodexClientUserAgentFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(codexClientUAContextKey{}).(string)
	return value
}
