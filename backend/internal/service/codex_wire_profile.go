package service

import (
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
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

// codexWireProfileUnknownVersionWarned 避免未知版本每个请求刷日志。
var codexWireProfileUnknownVersionWarned sync.Map

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
		// 0.164 是当前已知的最新 profile；更高的版本先按 main 行为处理并
		// 由调用方告警（官方尚未发布稳定版）。
		return codexWireProfileMain, CompareVersions(version, "0.165.0") < 0
	}
	return codexWireProfileStable, true
}

// codexWireProfileForAccount 返回本请求应使用的 profile。非 OAuth 账号返回
// ok=false，所有调用方必须先检查 ok 再读取任何字段。
func codexWireProfileForAccount(account *Account, cfg *config.Config) (codexWireProfile, bool) {
	if !isCodexOAuthAccount(account) {
		return codexWireProfile{}, false
	}
	mode := codexWireProfileModeAuto
	strict := false
	if cfg != nil {
		mode = cfg.Gateway.CodexWire.Profile
		strict = cfg.Gateway.CodexWire.Strict
	}
	version := CodexCanonicalClientVersion()
	profile, known := resolveCodexWireProfile(version, mode)
	if !known {
		key := version + "|" + mode
		if _, loaded := codexWireProfileUnknownVersionWarned.LoadOrStore(key, struct{}{}); !loaded {
			logger.LegacyPrintf("service.openai_codex",
				"[Codex] unknown wire profile version %s (mode=%s strict=%v); using %s",
				version, mode, strict, profile.Name)
		}
	}
	return profile, true
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
