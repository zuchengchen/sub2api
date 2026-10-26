package service

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// codexPromptCacheKeyPolicy 复刻 codex-rs 0.164 的 PromptCacheKey 枚举
// (codex-rs/codex-api/src/common.rs): SessionDerived / Custom / Omit.
type codexPromptCacheKeyPolicy string

const (
	codexPromptCacheKeySessionDerived codexPromptCacheKeyPolicy = "session_derived"
	codexPromptCacheKeyCustom         codexPromptCacheKeyPolicy = "custom"
	codexPromptCacheKeyOmit           codexPromptCacheKeyPolicy = "omit"
)

// codexWireProfile 是版本级的行为开关，只在 isCodexOAuthAccount 分支读取。
// 非 OAuth 路径不得读取该结构，否则会把 Codex 专有行为泄漏给 API key /
// SetupToken / 非 OpenAI 平台账号。
type codexWireProfile struct {
	Name string

	// BaseInstructionsInInput: >=0.162 把 base prompt 放在 input[0] developer
	// message，不再发送顶层 instructions。
	BaseInstructionsInInput bool
	// ClearContentItemMetadata: >=0.164 非 lite 模型会清空每个 input item 的
	// internal_chat_message_metadata_passthrough.content_item_metadata。
	ClearContentItemMetadata bool
	// DedupeConfigurationUpdates: >=0.164 相邻 configuration_update item 只保留
	// 最后一个（codex-rs core/src/client.rs）。
	DedupeConfigurationUpdates bool
	// PromptCacheKeyPolicy 控制缺省 prompt_cache_key 是否由 session 派生。
	PromptCacheKeyPolicy codexPromptCacheKeyPolicy
	// DefaultBetaFeatures 是客户端未声明 x-codex-beta-features 时的会话级默认值。
	DefaultBetaFeatures string
	// AccessProgramsParam: >=0.164 请求体接受 access_programs。
	AccessProgramsParam bool
	// PreviousResponseIDParam / GenerateParam: >=0.164 的增量字段。
	PreviousResponseIDParam bool
	GenerateParam           bool
}

var (
	codexWireProfileStable = codexWireProfile{
		Name:                       "0.163",
		BaseInstructionsInInput:    true,
		PromptCacheKeyPolicy:       codexPromptCacheKeySessionDerived,
		DefaultBetaFeatures:        "remote_compaction_v2",
		AccessProgramsParam:        false,
		PreviousResponseIDParam:    true,
		GenerateParam:              false,
		ClearContentItemMetadata:   false,
		DedupeConfigurationUpdates: false,
	}
	codexWireProfileMain = codexWireProfile{
		Name:                       "0.164",
		BaseInstructionsInInput:    true,
		ClearContentItemMetadata:   true,
		DedupeConfigurationUpdates: true,
		PromptCacheKeyPolicy:       codexPromptCacheKeySessionDerived,
		DefaultBetaFeatures:        "remote_compaction_v2",
		AccessProgramsParam:        true,
		PreviousResponseIDParam:    true,
		GenerateParam:              true,
	}
)

const (
	codexWireProfileModeAuto   = "auto"
	codexWireProfileModeStable = "stable"
	codexWireProfileModeMain   = "main"
)

// verifiedCodexWireProfile is an exact release allowlist for automatic identity
// updates. A compatible profile range is not evidence that a new release has
// been checked. Add a release only alongside its captured reference fixtures.
func verifiedCodexWireProfile(version string) (codexWireProfile, bool) {
	switch strings.TrimSpace(version) {
	case "0.163.0":
		return codexWireProfileStable, true
	case "0.164.0":
		return codexWireProfileMain, true
	default:
		return codexWireProfile{}, false
	}
}

// resolveCodexWireProfile 选择版本 profile。known=false 表示版本高于当前
// 已知的最新版本；调用方按 gateway.codex_wire.strict 决定拒绝还是告警。
func resolveCodexWireProfile(version, mode string) (profile codexWireProfile, known bool) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case codexWireProfileModeStable:
		return codexWireProfileStable, true
	case codexWireProfileModeMain:
		return codexWireProfileMain, true
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return codexWireProfileStable, true
	}
	if CompareVersions(version, "0.164.0") >= 0 {
		// This compatibility range supports explicit administrator overrides.
		// Automatic updates use the exact verified release allowlist instead.
		return codexWireProfileMain, CompareVersions(version, "0.165.0") < 0
	}
	return codexWireProfileStable, true
}

// codexFullSynthesisEnabled is the service-level switch. A nil config (tests,
// tools) keeps the legacy OAuth path; production viper defaults enable it.
func (s *OpenAIGatewayService) codexFullSynthesisEnabled() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	return s.cfg.Gateway.CodexFullSynthesisEnabled()
}

// codexMapOrderJitterEnabled reports whether client_metadata keys are
// reordered. nil config keeps the production default (enabled).
func (s *OpenAIGatewayService) codexMapOrderJitterEnabled() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	return s.cfg.Gateway.CodexWireMapOrderJitterEnabled()
}

// codexStreamIdleTimeout is the upstream SSE idle timeout for this account.
// The OAuth Codex lane uses gateway.codex_wire.stream_idle_timeout_seconds
// (production viper default 300). A hand-built zero Config, or an explicit 0,
// falls back to gateway.stream_data_interval_timeout like every other account.
func (s *OpenAIGatewayService) codexStreamIdleTimeout(account *Account) time.Duration {
	if isCodexOAuthAccount(account) && s != nil && s.cfg != nil && s.cfg.Gateway.CodexWire.StreamIdleTimeoutSeconds > 0 {
		return time.Duration(s.cfg.Gateway.CodexWire.StreamIdleTimeoutSeconds) * time.Second
	}
	if s != nil && s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		return time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	return 0
}

const codexSynthesizedContextKey = "codex_wire_synthesized"

func markCodexSynthesized(c *gin.Context) {
	if c != nil {
		c.Set(codexSynthesizedContextKey, true)
	}
}

func codexRequestSynthesized(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get(codexSynthesizedContextKey)
	if !ok {
		return false
	}
	flag, _ := value.(bool)
	return flag
}
