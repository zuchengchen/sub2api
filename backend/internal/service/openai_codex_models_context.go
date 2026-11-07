package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

type codexModelsBootstrapKey struct{}
type codexModelsSourceBuildKey struct{}

// WithCodexModelsClientContext retains the official CLI initialization stage,
// not its claimed device identity. The selected account still supplies the
// outbound family, version and platform. Other clients keep the normal profile.
// The narrowly validated 0.0.0 source-build identity is retained separately;
// its consumer must also verify the Codex OAuth account and query version.
func WithCodexModelsClientContext(ctx context.Context, userAgent, originator string) context.Context {
	if identity, ok := codexSourceBuildIdentity(userAgent, originator); ok {
		ctx = context.WithValue(ctx, codexModelsSourceBuildKey{}, identity)
	}
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

func codexModelsSourceBuildIdentity(ctx context.Context, account *Account, clientVersion string) (codexOutboundIdentity, bool) {
	if !isCodexOAuthAccount(account) || clientVersion != "0.0.0" || ctx == nil {
		return codexOutboundIdentity{}, false
	}
	identity, ok := ctx.Value(codexModelsSourceBuildKey{}).(codexOutboundIdentity)
	return identity, ok
}

func codexModelsBootstrapRequest(ctx context.Context) bool {
	value, _ := ctx.Value(codexModelsBootstrapKey{}).(bool)
	return value
}
