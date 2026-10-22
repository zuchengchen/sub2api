package service

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// Codex device-profile keys live in Account.Extra and are read only when
// isCodexOAuthAccount is true. The same keys on SetupToken/API-key/non-OpenAI
// accounts are ignored by design.
const (
	codexClientFamilyExtraKey     = "codex_client_family"
	codexPlatformProfileExtraKey  = "codex_platform_profile"
	codexSandboxExtraKey          = "codex_sandbox"
	codexSandboxModeExtraKey      = "codex_sandbox_mode"
	codexWorkspaceCWDExtraKey     = "codex_workspace_cwd"
	codexTimezoneExtraKey         = "codex_timezone"
	codexClientCapabilityExtraKey = "codex_client_capability"
	codexResponsesLiteExtraKey    = "codex_responses_lite"
	codexTurnTriggerExtraKey      = "codex_turn_trigger"
	codexInferenceCallIDExtraKey  = "codex_inference_call_id"
	codexPromptCacheKeyExtraKey   = "codex_prompt_cache_key_policy"
	codexClientFamilyTUI          = "tui"
	codexClientFamilyExec         = "exec"
	codexClientFamilyVSCode       = "vscode"
	codexDefaultPlatformProfile   = "linux-ubuntu-26.4-x86_64-xterm"
	codexDefaultSandbox           = "seccomp"
	codexDefaultWorkspaceMode     = "workspace-write"
	codexDefaultWorkspaceCWD      = "/root"
	codexDefaultTimezone          = "Etc/UTC"
	codexExecDefaultWorkspaceMode = "read-only"
)

// codexDeviceProfile is the account-level Linux device image. It is resolved
// once per request and shared by HTTP, WS, and aux calls for that account.
type codexDeviceProfile struct {
	Family       string
	Platform     string
	Version      string
	UserAgent    string
	Originator   string
	Sandbox      string
	SandboxMode  string
	WorkspaceCWD string
	Timezone     string
}

type codexPlatformImage struct {
	suffix string
}

// Only Linux images are supported in this phase. The suffix reproduces the
// official Codex UA tail `(Ubuntu 26.4.0; x86_64) xterm-256color`.
var codexPlatformImages = map[string]codexPlatformImage{
	"linux-ubuntu-26.4-x86_64-xterm": {suffix: " (Ubuntu 26.4.0; x86_64) xterm-256color"},
	"linux-ubuntu-24.4-x86_64-xterm": {suffix: " (Ubuntu 24.4.0; x86_64) xterm-256color"},
	"linux-ubuntu-22.4-x86_64-xterm": {suffix: " (Ubuntu 22.4.0; x86_64) xterm-256color"},
}

var codexPlatformProfilePattern = regexp.MustCompile(`^linux-[a-z0-9.-]+-(x86_64|aarch64)-[a-z0-9-]+$`)

var codexDeviceProfileInvalidWarned sync.Map

func normalizeCodexClientFamily(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case codexClientFamilyExec:
		return codexClientFamilyExec
	case codexClientFamilyVSCode, "code", "vscode-extension":
		return codexClientFamilyVSCode
	default:
		return codexClientFamilyTUI
	}
}

func codexFamilyOriginator(family string) string {
	switch normalizeCodexClientFamily(family) {
	case codexClientFamilyExec:
		return "codex_exec"
	case codexClientFamilyVSCode:
		return "codex_vscode"
	default:
		return openai.CodexDefaultOriginator
	}
}

// buildCodexDeviceUserAgent builds `{originator}/{version}{suffix} ({originator}; {version})`.
func buildCodexDeviceUserAgent(family, platformProfile, version string) (string, bool) {
	image, ok := codexPlatformImages[strings.TrimSpace(platformProfile)]
	if !ok {
		if !codexPlatformProfilePattern.MatchString(strings.TrimSpace(platformProfile)) {
			return "", false
		}
		// Unknown but well-formed Linux profile: keep the canonical Ubuntu tail
		// rather than inventing an OS fingerprint the TLS lane cannot match.
		image = codexPlatformImages[codexDefaultPlatformProfile]
	}
	version = NormalizeCodexClientVersion(version)
	if version == "" {
		return "", false
	}
	originator := codexFamilyOriginator(family)
	return originator + "/" + version + image.suffix + " (" + originator + "; " + version + ")", true
}

// resolveCodexDeviceProfile returns the account's device image. ok=false for
// every account outside isCodexOAuthAccount, and for invalid extra values
// (falling back to the canonical TUI identity instead of leaking a malformed
// UA to the upstream).
func resolveCodexDeviceProfile(account *Account) (codexDeviceProfile, bool) {
	if !isCodexOAuthAccount(account) {
		return codexDeviceProfile{}, false
	}
	family := normalizeCodexClientFamily(account.GetExtraString(codexClientFamilyExtraKey))
	platform := strings.TrimSpace(account.GetExtraString(codexPlatformProfileExtraKey))
	if platform == "" {
		platform = codexDefaultPlatformProfile
	}
	version := CodexCanonicalClientVersion()
	ua, ok := buildCodexDeviceUserAgent(family, platform, version)
	if !ok {
		key := account.ID
		if _, loaded := codexDeviceProfileInvalidWarned.LoadOrStore(key, struct{}{}); !loaded {
			logger.LegacyPrintf("service.openai_codex",
				"[Codex] account %d has invalid device profile platform=%q; falling back to %s",
				account.ID, platform, codexDefaultPlatformProfile)
		}
		platform = codexDefaultPlatformProfile
		ua, _ = buildCodexDeviceUserAgent(family, platform, version)
	}
	sandbox := strings.TrimSpace(account.GetExtraString(codexSandboxExtraKey))
	if sandbox == "" {
		sandbox = codexDefaultSandbox
	}
	sandboxMode := strings.TrimSpace(account.GetExtraString(codexSandboxModeExtraKey))
	if sandboxMode == "" {
		if family == codexClientFamilyExec {
			sandboxMode = codexExecDefaultWorkspaceMode
		} else {
			sandboxMode = codexDefaultWorkspaceMode
		}
	}
	cwd := strings.TrimSpace(account.GetExtraString(codexWorkspaceCWDExtraKey))
	if cwd == "" {
		cwd = codexDefaultWorkspaceCWD
	}
	timezone := strings.TrimSpace(account.GetExtraString(codexTimezoneExtraKey))
	if timezone == "" {
		timezone = codexDefaultTimezone
	}
	return codexDeviceProfile{
		Family:       family,
		Platform:     platform,
		Version:      version,
		UserAgent:    ua,
		Originator:   codexFamilyOriginator(family),
		Sandbox:      sandbox,
		SandboxMode:  sandboxMode,
		WorkspaceCWD: cwd,
		Timezone:     timezone,
	}, true
}

// codexDeviceProfileForRequest is the guarded accessor used by synthesis and
// wire metadata. Non-OAuth accounts never reach the profile code.
func codexDeviceProfileForRequest(account *Account) (codexDeviceProfile, bool) {
	return resolveCodexDeviceProfile(account)
}

// codexAccountNodeReplAutoReviewRequired mirrors the account family: `codex
// exec` turns node-repl review off, while the interactive TUI keeps it on.
func codexAccountNodeReplAutoReviewRequired(account *Account) bool {
	profile, ok := resolveCodexDeviceProfile(account)
	if !ok {
		return true
	}
	return profile.Family != codexClientFamilyExec
}

// sanitizeCodexDeviceProfileExtra keeps only values the wire layer can honor.
// Invalid keys are removed (the request path then uses the canonical default)
// so a typo cannot silently produce a mismatched platform/TLS fingerprint.
func sanitizeCodexDeviceProfileExtra(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	sanitized := make(map[string]any, len(extra))
	for key, value := range extra {
		sanitized[key] = value
	}
	if family := strings.ToLower(strings.TrimSpace(fmt.Sprint(sanitized[codexClientFamilyExtraKey]))); family != "" {
		switch family {
		case codexClientFamilyTUI, codexClientFamilyExec, codexClientFamilyVSCode, "code", "vscode-extension":
			sanitized[codexClientFamilyExtraKey] = normalizeCodexClientFamily(family)
		default:
			delete(sanitized, codexClientFamilyExtraKey)
		}
	}
	if platform := strings.TrimSpace(fmt.Sprint(sanitized[codexPlatformProfileExtraKey])); platform != "" {
		if _, ok := codexPlatformImages[platform]; !ok && !codexPlatformProfilePattern.MatchString(platform) {
			delete(sanitized, codexPlatformProfileExtraKey)
		}
	}
	if mode := strings.TrimSpace(fmt.Sprint(sanitized[codexSandboxModeExtraKey])); mode != "" {
		switch mode {
		case "read-only", "workspace-write", "danger-full-access":
			sanitized[codexSandboxModeExtraKey] = mode
		default:
			delete(sanitized, codexSandboxModeExtraKey)
		}
	}
	if cwd := strings.TrimSpace(fmt.Sprint(sanitized[codexWorkspaceCWDExtraKey])); cwd != "" {
		if !strings.HasPrefix(cwd, "/") || strings.Contains(cwd, "..") || filepath.Clean(cwd) != cwd {
			delete(sanitized, codexWorkspaceCWDExtraKey)
		}
	}
	if tz := strings.TrimSpace(fmt.Sprint(sanitized[codexTimezoneExtraKey])); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			delete(sanitized, codexTimezoneExtraKey)
		}
	}
	return sanitized
}
