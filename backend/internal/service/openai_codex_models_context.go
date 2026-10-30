package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

type codexModelsBootstrapKey struct{}

// WithCodexModelsClientContext retains the official CLI initialization stage,
// not its claimed device identity. The selected account still supplies the
// outbound family, version and platform. Other clients keep the normal profile.
func WithCodexModelsClientContext(ctx context.Context, userAgent, originator string) context.Context {
	if !openai.IsCodexOfficialClientByHeaders(userAgent, originator) {
		return ctx
	}
	client, _, ok := openai.PairCodexClientIdentity(userAgent)
	version := openai.CodexUserAgentVersion(userAgent)
	if !ok || version == "" {
		return ctx
	}
	bootstrap := !strings.HasSuffix(strings.TrimSpace(userAgent), " ("+client+"; "+version+")")
	return context.WithValue(ctx, codexModelsBootstrapKey{}, bootstrap)
}

func codexModelsBootstrapRequest(ctx context.Context) bool {
	value, _ := ctx.Value(codexModelsBootstrapKey{}).(bool)
	return value
}
