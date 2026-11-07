package service

import (
	"context"
	"net/http"
	"regexp"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// A source build reports CARGO_PKG_VERSION=0.0.0. It is not an old stable
// release. Preserve only this complete first-party wire shape, never a version
// header alone or the permissive official-client substring detector. This is
// identity preservation, not authentication or an exception to access policy.
var codexSourceBuildUAPattern = regexp.MustCompile(`^([a-z][a-z0-9_-]*)/0\.0\.0 \(([^();]+); ([A-Za-z0-9_+-]+)\) ([^()]+?)(?: \(([a-z][a-z0-9_-]*); 0\.0\.0\))?$`)

func codexSourceBuildClient(name string) bool {
	switch name {
	case "codex_cli_rs", "codex-tui", "codex_exec", "codex_vscode", "codex_vscode_copilot", "codex_app", "codex_chatgpt_desktop", "codex_atlas", "codex_sdk_ts":
		return true
	default:
		return false
	}
}

type codexSourceBuildAuthKey struct{}
type codexSourceBuildAuthSnapshot struct {
	accountID int64
	identity  codexOutboundIdentity
}

// Bind only this recovery attempt to its current credential owner. No account
// state or session-global map is modified; unrelated/background refreshes keep
// the administrator's normal identity and minimum-version policy.
func withCodexSourceBuildAuthIdentity(ctx context.Context, account *Account, headers http.Header) context.Context {
	identity, ok := codexSourceBuildIdentityFromHeaders(headers, account)
	if ctx == nil || !ok || account.ID <= 0 {
		if ctx != nil && ctx.Value(codexSourceBuildAuthKey{}) != nil {
			return context.WithValue(ctx, codexSourceBuildAuthKey{}, codexSourceBuildAuthSnapshot{})
		}
		return ctx
	}
	return context.WithValue(ctx, codexSourceBuildAuthKey{}, codexSourceBuildAuthSnapshot{accountID: account.ID, identity: identity})
}

func codexSourceBuildAuthIdentity(ctx context.Context) (codexOutboundIdentity, bool) {
	if ctx == nil {
		return codexOutboundIdentity{}, false
	}
	snapshot, ok := ctx.Value(codexSourceBuildAuthKey{}).(codexSourceBuildAuthSnapshot)
	if !ok || snapshot.accountID <= 0 || snapshot.accountID != openai.CodexAuthAccountFromContext(ctx) {
		return codexOutboundIdentity{}, false
	}
	return snapshot.identity, true
}

func codexSourceBuildIdentity(userAgent, originator string) (codexOutboundIdentity, bool) {
	if len(userAgent) == 0 || len(userAgent) > 512 {
		return codexOutboundIdentity{}, false
	}
	for _, c := range userAgent {
		if c < 0x20 || c > 0x7e {
			return codexOutboundIdentity{}, false
		}
	}
	parts := codexSourceBuildUAPattern.FindStringSubmatch(userAgent)
	if parts == nil || !codexSourceBuildClient(parts[1]) || originator != parts[1] || (parts[5] != "" && !codexSourceBuildClient(parts[5])) {
		return codexOutboundIdentity{}, false
	}
	return codexOutboundIdentity{userAgent: userAgent, originator: originator, version: "0.0.0"}, true
}

func codexSourceBuildIdentityFromHeaders(headers http.Header, account *Account) (codexOutboundIdentity, bool) {
	if !isCodexOAuthAccount(account) || len(headers.Values("User-Agent")) != 1 || len(headers.Values("Originator")) != 1 {
		return codexOutboundIdentity{}, false
	}
	versions := headers.Values("Version")
	if len(versions) > 1 || (len(versions) == 1 && versions[0] != "0.0.0") {
		return codexOutboundIdentity{}, false
	}
	return codexSourceBuildIdentity(headers.Get("User-Agent"), headers.Get("Originator"))
}
