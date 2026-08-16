package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
	contentmoderationassets "github.com/Wei-Shaw/sub2api/resources/content-moderation"
)

const (
	ContentModerationModeOff           = "off"
	ContentModerationModePreBlock      = "pre_block"
	legacyContentModerationModeObserve = "observe"

	contentModerationAPIKeysModeAppend  = "append"
	contentModerationAPIKeysModeReplace = "replace"

	ContentModerationActionAllow             = "allow"
	ContentModerationActionBlock             = "block"
	ContentModerationActionHashBlock         = "hash_block"
	ContentModerationActionKeywordBlock      = "keyword_block"
	ContentModerationActionFirstLayerShadow  = "first_layer_shadow"
	ContentModerationActionSecondLayerBlock  = "second_layer_block"
	ContentModerationActionSecondLayerShadow = "second_layer_shadow"
	ContentModerationActionWhitelistShadow   = "whitelist_shadow"
	ContentModerationActionCacheBlock        = "cache_block"
	ContentModerationActionBudgetRejected    = "budget_rejected"
	ContentModerationActionReviewUnavailable = "review_unavailable"
	ContentModerationActionError             = "error"
	ContentModerationActionCyberPolicy       = "cyber_policy" // cyber_policy 硬阻断的风控日志 action（封号计数排除按此值过滤）

	ContentModerationLogResultBlocked        = "blocked"
	ContentModerationLogResultCyberPolicy    = "cyber_policy"
	ContentModerationLogResultContentBlocked = "content_blocked"
	ContentModerationLogResultRiskyShadow    = "risky_shadow"
	ContentModerationLogResultReviewFailure  = "review_unavailable"

	contentModerationKeywordCategory = "keyword"

	ContentModerationKeywordModeKeywordOnly   = "keyword_only"
	ContentModerationKeywordModeKeywordAndAPI = "keyword_and_api"
	ContentModerationKeywordModeAPIOnly       = "api_only"

	ContentModerationModelFilterAll     = "all"
	ContentModerationModelFilterInclude = "include"
	ContentModerationModelFilterExclude = "exclude"

	ContentModerationProtocolAnthropicMessages = "anthropic_messages"
	ContentModerationProtocolOpenAIResponses   = "openai_responses"
	ContentModerationProtocolOpenAIChat        = "openai_chat_completions"
	ContentModerationProtocolGemini            = "gemini"
	ContentModerationProtocolOpenAIImages      = "openai_images"

	defaultContentModerationBaseURL   = "https://api.openai.com"
	defaultContentModerationModel     = "omni-moderation-latest"
	defaultContentModerationTimeoutMS = 3000
	maxContentModerationTimeoutMS     = 30000
	maxModerationInputRunes           = 12000
	maxModerationExcerptRunes         = 1024
	maxModerationErrorRunes           = 960

	defaultContentModerationBanThreshold               = 10
	defaultContentModerationViolationWindowHours       = 720
	defaultContentModerationBlockHTTPStatus            = http.StatusForbidden
	defaultContentModerationBlockMessage               = "内容审计命中风险规则，请调整输入后重试"
	defaultContentModerationRetryCount                 = 2
	maxContentModerationRetryCount                     = 5
	defaultContentModerationHitRetentionDays           = 180
	defaultContentModerationNonHitRetentionDays        = 3
	maxContentModerationRetentionDays                  = 3650
	maxContentModerationNonHitRetentionDays            = 3
	contentModerationKeyRateLimitFreezeDuration        = time.Minute
	contentModerationKeyAuthFreezeDuration             = 10 * time.Minute
	contentModerationKeyHTTPErrorFreezeDuration        = 10 * time.Second
	maxContentModerationInputImages                    = 1
	maxContentModerationTestImages                     = maxContentModerationInputImages
	maxContentModerationTestImageBytes                 = 8 * 1024 * 1024
	maxContentModerationTestImageDataURLBytes          = 12 * 1024 * 1024
	maxContentModerationBlockedKeywords                = 10000
	maxContentModerationBlockedKeywordRunes            = 200
	maxContentModerationUserEmailWhitelist             = 1000
	maxContentModerationUserEmailRunes                 = 254
	maxContentModerationModelFilterModels              = 1000
	maxContentModerationModelFilterRunes               = 200
	defaultContentModerationCacheVersion               = "v1"
	defaultContentModerationCacheMaxEntries            = 250000
	defaultContentModerationCacheMaxBytes        int64 = 64 * 1024 * 1024
	maxContentModerationCacheMaxEntries                = 5000000
	maxContentModerationCacheMaxBytes            int64 = 4 * 1024 * 1024 * 1024

	contentModerationCleanupInterval = 24 * time.Hour
	contentModerationCleanupTimeout  = 30 * time.Minute
	contentModerationCleanupDelay    = 5 * time.Minute

	contentModerationRuntimeCacheTTL       = time.Second
	contentModerationRuntimeRefreshTimeout = 5 * time.Second
)

var contentModerationCategoryOrder = []string{
	"harassment",
	"harassment/threatening",
	"hate",
	"hate/threatening",
	"illicit",
	"illicit/violent",
	"self-harm",
	"self-harm/intent",
	"self-harm/instructions",
	"sexual",
	"sexual/minors",
	"violence",
	"violence/graphic",
}

func ContentModerationDefaultThresholds() map[string]float64 {
	return map[string]float64{
		"harassment":             0.98,
		"harassment/threatening": 0.90,
		"hate":                   0.65,
		"hate/threatening":       0.65,
		"illicit":                0.95,
		"illicit/violent":        0.95,
		"self-harm":              0.65,
		"self-harm/intent":       0.85,
		"self-harm/instructions": 0.65,
		"sexual":                 0.65,
		"sexual/minors":          0.65,
		"violence":               0.95,
		"violence/graphic":       0.95,
	}
}

func ContentModerationCategories() []string {
	out := make([]string, len(contentModerationCategoryOrder))
	copy(out, contentModerationCategoryOrder)
	return out
}

type ContentModerationConfig struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	// ProxyID 指定审计请求使用的代理服务器（IP管理-代理服务器），nil 表示直连。
	ProxyID                  *int64                       `json:"proxy_id,omitempty"`
	APIKey                   string                       `json:"api_key,omitempty"`
	APIKeys                  []string                     `json:"api_keys,omitempty"`
	TimeoutMS                int                          `json:"timeout_ms"`
	SampleRate               int                          `json:"sample_rate"`
	AllGroups                bool                         `json:"all_groups"`
	GroupIDs                 []int64                      `json:"group_ids"`
	UserEmailWhitelist       []string                     `json:"user_email_whitelist"`
	RecordNonHits            bool                         `json:"record_non_hits"`
	Thresholds               map[string]float64           `json:"thresholds"`
	BlockStatus              int                          `json:"block_status"`
	BlockMessage             string                       `json:"block_message"`
	EmailOnHit               bool                         `json:"email_on_hit"`
	AutoBanEnabled           bool                         `json:"auto_ban_enabled"`
	BanThreshold             int                          `json:"ban_threshold"`
	ViolationWindowHours     int                          `json:"violation_window_hours"`
	RetryCount               int                          `json:"retry_count"`
	HitRetentionDays         int                          `json:"hit_retention_days"`
	NonHitRetentionDays      int                          `json:"non_hit_retention_days"`
	PreHashCheckEnabled      bool                         `json:"pre_hash_check_enabled"`
	BlockedKeywords          []string                     `json:"blocked_keywords"`
	KeywordBlockingMode      string                       `json:"keyword_blocking_mode"`
	ModelFilter              ContentModerationModelFilter `json:"model_filter"`
	CacheVersion             string                       `json:"cache_version"`
	CacheMaxEntries          int                          `json:"cache_max_entries"`
	CacheMaxBytes            int64                        `json:"cache_max_bytes"`
	FragmentBlockTTLSeconds  int                          `json:"fragment_block_ttl_seconds"`
	FragmentAllowTTLSeconds  int                          `json:"fragment_allow_ttl_seconds"`
	FragmentTTLPolicyVersion string                       `json:"fragment_ttl_policy_version"`
	FirstLayerStage          string                       `json:"first_layer_stage"`
	SecondLayerEnabled       bool                         `json:"second_layer_enabled"`
	SecondLayerStage         string                       `json:"second_layer_stage"`
	SecondLayerEndpoints     []ContentModerationEndpoint  `json:"second_layer_endpoints"`
	SecondLayerScanners      []string                     `json:"second_layer_scanners"`
	HardBlockPatterns        []string                     `json:"hard_block_patterns"`
	CandidateKeywords        []string                     `json:"candidate_keywords"`
	KeywordAllowlist         []string                     `json:"keyword_allowlist"`
	KeywordPolicyVersion     string                       `json:"keyword_policy_version"`
	ContextPolicyVersion     string                       `json:"context_policy_version"`
	EvidencePolicyVersion    string                       `json:"evidence_policy_version"`
	CandidateAsset           string                       `json:"candidate_asset"`
	CandidateEnabled         bool                         `json:"candidate_enabled"`
	// CyberPolicyExcludeFromBanCount 为 true 时，cyber_policy 命中不参与自动封号计数：
	// 当次不判定封号，且历史 cyber 行在 CountFlaggedByUserSince 中被排除。
	// 默认 false（计入，与历史行为一致；旧配置 JSON 无此字段时反序列化为 false）。
	CyberPolicyExcludeFromBanCount bool `json:"cyber_policy_exclude_from_ban_count"`
}

type ContentModerationConfigView struct {
	Enabled                        bool                            `json:"enabled"`
	Mode                           string                          `json:"mode"`
	BaseURL                        string                          `json:"base_url"`
	Model                          string                          `json:"model"`
	ProxyID                        *int64                          `json:"proxy_id"`
	APIKeyConfigured               bool                            `json:"api_key_configured"`
	APIKeyMasked                   string                          `json:"api_key_masked"`
	APIKeyCount                    int                             `json:"api_key_count"`
	APIKeyMasks                    []string                        `json:"api_key_masks"`
	APIKeyStatuses                 []ContentModerationAPIKeyStatus `json:"api_key_statuses"`
	TimeoutMS                      int                             `json:"timeout_ms"`
	SampleRate                     int                             `json:"sample_rate"`
	AllGroups                      bool                            `json:"all_groups"`
	GroupIDs                       []int64                         `json:"group_ids"`
	UserEmailWhitelist             []string                        `json:"user_email_whitelist"`
	RecordNonHits                  bool                            `json:"record_non_hits"`
	Thresholds                     map[string]float64              `json:"thresholds"`
	BlockStatus                    int                             `json:"block_status"`
	BlockMessage                   string                          `json:"block_message"`
	EmailOnHit                     bool                            `json:"email_on_hit"`
	AutoBanEnabled                 bool                            `json:"auto_ban_enabled"`
	BanThreshold                   int                             `json:"ban_threshold"`
	ViolationWindowHours           int                             `json:"violation_window_hours"`
	RetryCount                     int                             `json:"retry_count"`
	HitRetentionDays               int                             `json:"hit_retention_days"`
	NonHitRetentionDays            int                             `json:"non_hit_retention_days"`
	PreHashCheckEnabled            bool                            `json:"pre_hash_check_enabled"`
	BlockedKeywords                []string                        `json:"blocked_keywords"`
	KeywordBlockingMode            string                          `json:"keyword_blocking_mode"`
	ModelFilter                    ContentModerationModelFilter    `json:"model_filter"`
	CacheVersion                   string                          `json:"cache_version"`
	CacheMaxEntries                int                             `json:"cache_max_entries"`
	CacheMaxBytes                  int64                           `json:"cache_max_bytes"`
	FragmentBlockTTLSeconds        int                             `json:"fragment_block_ttl_seconds"`
	FragmentAllowTTLSeconds        int                             `json:"fragment_allow_ttl_seconds"`
	FragmentTTLPolicyVersion       string                          `json:"fragment_ttl_policy_version"`
	FirstLayerStage                string                          `json:"first_layer_stage"`
	SecondLayerEnabled             bool                            `json:"second_layer_enabled"`
	SecondLayerStage               string                          `json:"second_layer_stage"`
	SecondLayerEndpoints           []ContentModerationEndpointView `json:"second_layer_endpoints"`
	SecondLayerScanners            []string                        `json:"second_layer_scanners"`
	HardBlockPatterns              []string                        `json:"hard_block_patterns"`
	CandidateKeywords              []string                        `json:"candidate_keywords"`
	KeywordAllowlist               []string                        `json:"keyword_allowlist"`
	KeywordPolicyVersion           string                          `json:"keyword_policy_version"`
	ContextPolicyVersion           string                          `json:"context_policy_version"`
	EvidencePolicyVersion          string                          `json:"evidence_policy_version"`
	CandidateAsset                 string                          `json:"candidate_asset"`
	CandidateEnabled               bool                            `json:"candidate_enabled"`
	CandidateLayer1Count           int                             `json:"candidate_layer1_count"`
	CandidateLayer2Count           int                             `json:"candidate_layer2_count"`
	CandidateSourceCommit          string                          `json:"candidate_source_commit"`
	CandidateEndpoints             []ContentModerationEndpointView `json:"candidate_endpoints"`
	Layer1Keywords                 []string                        `json:"layer1_keywords"`
	Layer2Keywords                 []string                        `json:"layer2_keywords"`
	CandidateSystemReady           bool                            `json:"candidate_system_ready"`
	CandidateSystemError           string                          `json:"candidate_system_error,omitempty"`
	CyberPolicyExcludeFromBanCount bool                            `json:"cyber_policy_exclude_from_ban_count"`
}

type ContentModerationEndpoint struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	BaseURL       string   `json:"base_url"`
	Model         string   `json:"model"`
	Profile       string   `json:"profile"`
	ModelRevision string   `json:"model_revision,omitempty"`
	PromptVersion string   `json:"prompt_version,omitempty"`
	StopTokens    []string `json:"stop_tokens,omitempty"`
	Token         string   `json:"token,omitempty"`
	Enabled       bool     `json:"enabled"`
	TimeoutMS     int      `json:"timeout_ms"`
	InputLimit    int      `json:"input_limit"`
}

type ContentModerationEndpointView struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	BaseURL         string   `json:"base_url"`
	Model           string   `json:"model"`
	Profile         string   `json:"profile"`
	ModelRevision   string   `json:"model_revision,omitempty"`
	PromptVersion   string   `json:"prompt_version,omitempty"`
	StopTokens      []string `json:"stop_tokens,omitempty"`
	Enabled         bool     `json:"enabled"`
	TimeoutMS       int      `json:"timeout_ms"`
	InputLimit      int      `json:"input_limit"`
	TokenConfigured bool     `json:"token_configured"`
	TokenMasked     string   `json:"token_masked"`
}

type ContentModerationAPIKeyStatus struct {
	Index          int        `json:"index"`
	KeyHash        string     `json:"key_hash"`
	Masked         string     `json:"masked"`
	Status         string     `json:"status"`
	FailureCount   int        `json:"failure_count"`
	SuccessCount   int64      `json:"success_count"`
	LastError      string     `json:"last_error"`
	LastCheckedAt  *time.Time `json:"last_checked_at,omitempty"`
	FrozenUntil    *time.Time `json:"frozen_until,omitempty"`
	LastLatencyMS  int        `json:"last_latency_ms"`
	LastHTTPStatus int        `json:"last_http_status"`
	LastTested     bool       `json:"last_tested"`
	Configured     bool       `json:"configured"`
}

type ContentModerationAPIKeyLoad struct {
	Index          int    `json:"index"`
	KeyHash        string `json:"key_hash"`
	Masked         string `json:"masked"`
	Status         string `json:"status"`
	Active         int64  `json:"active"`
	Total          int64  `json:"total"`
	Success        int64  `json:"success"`
	Errors         int64  `json:"errors"`
	AvgLatencyMS   int64  `json:"avg_latency_ms"`
	LastLatencyMS  int    `json:"last_latency_ms"`
	LastHTTPStatus int    `json:"last_http_status"`
}

type TestContentModerationAPIKeysInput struct {
	APIKeys   []string `json:"api_keys"`
	BaseURL   string   `json:"base_url"`
	Model     string   `json:"model"`
	TimeoutMS int      `json:"timeout_ms"`
	// ProxyID nil 表示沿用已保存配置的代理；<=0 表示强制直连测试；>0 表示指定代理测试。
	ProxyID *int64   `json:"proxy_id"`
	Prompt  string   `json:"prompt"`
	Images  []string `json:"images"`
}

type TestContentModerationAPIKeysResult struct {
	Items       []ContentModerationAPIKeyStatus   `json:"items"`
	AuditResult *ContentModerationTestAuditResult `json:"audit_result,omitempty"`
	ImageCount  int                               `json:"image_count"`
}

type ContentModerationTestAuditResult struct {
	Flagged         bool               `json:"flagged"`
	HighestCategory string             `json:"highest_category"`
	HighestScore    float64            `json:"highest_score"`
	CompositeScore  float64            `json:"composite_score"`
	CategoryScores  map[string]float64 `json:"category_scores"`
	Thresholds      map[string]float64 `json:"thresholds"`
}

type UpdateContentModerationConfigInput struct {
	Enabled *bool   `json:"enabled"`
	Mode    *string `json:"mode"`
	BaseURL *string `json:"base_url"`
	Model   *string `json:"model"`
	// ProxyID nil 表示不修改；<=0 表示清除代理（恢复直连）；>0 表示指定代理。
	ProxyID                        *int64                        `json:"proxy_id"`
	APIKey                         *string                       `json:"api_key"`
	APIKeys                        *[]string                     `json:"api_keys"`
	APIKeysMode                    string                        `json:"api_keys_mode"`
	DeleteAPIKeyHashes             *[]string                     `json:"delete_api_key_hashes"`
	ClearAPIKey                    bool                          `json:"clear_api_key"`
	TimeoutMS                      *int                          `json:"timeout_ms"`
	SampleRate                     *int                          `json:"sample_rate"`
	AllGroups                      *bool                         `json:"all_groups"`
	GroupIDs                       *[]int64                      `json:"group_ids"`
	UserEmailWhitelist             *[]string                     `json:"user_email_whitelist"`
	RecordNonHits                  *bool                         `json:"record_non_hits"`
	Thresholds                     *map[string]float64           `json:"thresholds"`
	BlockStatus                    *int                          `json:"block_status"`
	BlockMessage                   *string                       `json:"block_message"`
	EmailOnHit                     *bool                         `json:"email_on_hit"`
	AutoBanEnabled                 *bool                         `json:"auto_ban_enabled"`
	BanThreshold                   *int                          `json:"ban_threshold"`
	ViolationWindowHours           *int                          `json:"violation_window_hours"`
	RetryCount                     *int                          `json:"retry_count"`
	HitRetentionDays               *int                          `json:"hit_retention_days"`
	NonHitRetentionDays            *int                          `json:"non_hit_retention_days"`
	PreHashCheckEnabled            *bool                         `json:"pre_hash_check_enabled"`
	BlockedKeywords                *[]string                     `json:"blocked_keywords"`
	KeywordBlockingMode            *string                       `json:"keyword_blocking_mode"`
	ModelFilter                    *ContentModerationModelFilter `json:"model_filter"`
	CacheVersion                   *string                       `json:"cache_version"`
	CacheMaxEntries                *int                          `json:"cache_max_entries"`
	CacheMaxBytes                  *int64                        `json:"cache_max_bytes"`
	FragmentBlockTTLSeconds        *int                          `json:"fragment_block_ttl_seconds"`
	FragmentAllowTTLSeconds        *int                          `json:"fragment_allow_ttl_seconds"`
	FragmentTTLPolicyVersion       *string                       `json:"fragment_ttl_policy_version"`
	FirstLayerStage                *string                       `json:"first_layer_stage"`
	SecondLayerEnabled             *bool                         `json:"second_layer_enabled"`
	SecondLayerStage               *string                       `json:"second_layer_stage"`
	SecondLayerEndpoints           *[]ContentModerationEndpoint  `json:"second_layer_endpoints"`
	SecondLayerScanners            *[]string                     `json:"second_layer_scanners"`
	HardBlockPatterns              *[]string                     `json:"hard_block_patterns"`
	CandidateKeywords              *[]string                     `json:"candidate_keywords"`
	Layer1Keywords                 *[]string                     `json:"layer1_keywords"`
	Layer2Keywords                 *[]string                     `json:"layer2_keywords"`
	KeywordAllowlist               *[]string                     `json:"keyword_allowlist"`
	KeywordPolicyVersion           *string                       `json:"keyword_policy_version"`
	ContextPolicyVersion           *string                       `json:"context_policy_version"`
	EvidencePolicyVersion          *string                       `json:"evidence_policy_version"`
	CandidateAsset                 *string                       `json:"candidate_asset"`
	CandidateEnabled               *bool                         `json:"candidate_enabled"`
	CyberPolicyExcludeFromBanCount *bool                         `json:"cyber_policy_exclude_from_ban_count"`
}

type ContentModerationModelFilter struct {
	Type   string   `json:"type"`
	Models []string `json:"models"`
}

type ContentModerationCheckInput struct {
	RequestID   string
	UserID      int64
	UserEmail   string
	APIKeyID    int64
	APIKeyName  string
	GroupID     *int64
	GroupName   string
	Endpoint    string
	Provider    string
	Model       string
	Protocol    string
	Body        []byte
	Scope       *ContentModerationScopeSnapshot
	RawRequest  ContentModerationRawRequest
	UserRole    string
	Reservation *ContentModerationPendingReservation
}

type ContentModerationInput struct {
	Text   string
	Images []string
}

func (in *ContentModerationInput) Normalize() {
	if in == nil {
		return
	}
	in.Text = trimRunes(normalizeContentModerationText(in.Text), maxModerationInputRunes)
	in.Images = normalizeModerationImages(in.Images)
}

func (in ContentModerationInput) IsEmpty() bool {
	return strings.TrimSpace(in.Text) == "" && len(in.Images) == 0
}

func (in ContentModerationInput) ModerationInput() any {
	images := limitContentModerationImages(in.Images)
	if len(images) == 0 {
		return in.Text
	}
	parts := make([]moderationAPIInputPart, 0, len(images)+1)
	if strings.TrimSpace(in.Text) != "" {
		parts = append(parts, moderationAPIInputPart{Type: "text", Text: in.Text})
	}
	for _, image := range images {
		parts = append(parts, moderationAPIInputPart{
			Type:     "image_url",
			ImageURL: &moderationAPIImageURLRef{URL: image},
		})
	}
	return parts
}

func (in ContentModerationInput) ExcerptText() string {
	return in.Text
}

func (in ContentModerationInput) Hash() string {
	h := sha256.New()
	_, _ = h.Write([]byte("text:"))
	_, _ = h.Write([]byte(in.Text))
	for _, image := range in.Images {
		imageHash := sha256.Sum256([]byte(image))
		_, _ = h.Write([]byte("\nimage:"))
		_, _ = h.Write([]byte(hex.EncodeToString(imageHash[:])))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type ContentModerationDecision struct {
	Allowed         bool               `json:"allowed"`
	Blocked         bool               `json:"blocked"`
	Flagged         bool               `json:"flagged"`
	Message         string             `json:"message"`
	StatusCode      int                `json:"status_code"`
	InputHash       string             `json:"input_hash,omitempty"`
	MatchedKeyword  string             `json:"matched_keyword,omitempty"`
	HighestCategory string             `json:"highest_category"`
	HighestScore    float64            `json:"highest_score"`
	CategoryScores  map[string]float64 `json:"category_scores"`
	Action          string             `json:"action"`
	RetryAfter      int                `json:"retry_after,omitempty"`
}

type ContentModerationLog struct {
	ID                      int64                             `json:"id"`
	RequestID               string                            `json:"request_id"`
	UserID                  *int64                            `json:"user_id,omitempty"`
	UserEmail               string                            `json:"user_email"`
	APIKeyID                *int64                            `json:"api_key_id,omitempty"`
	APIKeyName              string                            `json:"api_key_name"`
	GroupID                 *int64                            `json:"group_id,omitempty"`
	GroupName               string                            `json:"group_name"`
	Endpoint                string                            `json:"endpoint"`
	Provider                string                            `json:"provider"`
	Model                   string                            `json:"model"`
	Mode                    string                            `json:"mode"`
	Action                  string                            `json:"action"`
	CacheHit                bool                              `json:"cache_hit"`
	DecisionSource          string                            `json:"decision_source"`
	SourceLogID             *int64                            `json:"source_log_id,omitempty"`
	ReplayOfInputHash       string                            `json:"replay_of_input_hash,omitempty"`
	FragmentRole            string                            `json:"fragment_role,omitempty"`
	FragmentKind            string                            `json:"fragment_kind,omitempty"`
	ContextClass            string                            `json:"context_class,omitempty"`
	FragmentPath            string                            `json:"fragment_path,omitempty"`
	CacheNamespace          string                            `json:"cache_namespace,omitempty"`
	PolicyVersion           string                            `json:"policy_version,omitempty"`
	ModelProfile            string                            `json:"model_profile,omitempty"`
	PromptVersion           string                            `json:"prompt_version,omitempty"`
	EvidencePolicyVersion   string                            `json:"evidence_policy_version,omitempty"`
	KeywordTier             string                            `json:"keyword_tier,omitempty"`
	KeywordRuleID           string                            `json:"keyword_rule_id,omitempty"`
	EvidenceMode            string                            `json:"evidence_mode,omitempty"`
	EvidenceTruncated       bool                              `json:"evidence_truncated"`
	EvidenceWindows         []ContentModerationEvidenceWindow `json:"evidence_windows"`
	ParserStatus            string                            `json:"parser_status,omitempty"`
	Flagged                 bool                              `json:"flagged"`
	HighestCategory         string                            `json:"highest_category"`
	HighestScore            float64                           `json:"highest_score"`
	MatchedKeyword          string                            `json:"matched_keyword"`
	CategoryScores          map[string]float64                `json:"category_scores"`
	ThresholdSnapshot       map[string]float64                `json:"threshold_snapshot"`
	InputExcerpt            string                            `json:"input_excerpt"`
	UpstreamLatencyMS       *int                              `json:"upstream_latency_ms,omitempty"`
	Error                   string                            `json:"error"`
	ViolationCount          int                               `json:"violation_count"`
	AutoBanned              bool                              `json:"auto_banned"`
	EmailSent               bool                              `json:"email_sent"`
	EmailDeliveryStatus     string                            `json:"email_delivery_status"`
	EmailDeliveryClaimedAt  *time.Time                        `json:"email_delivery_claimed_at,omitempty"`
	UserStatus              string                            `json:"user_status"`
	QueueDelayMS            *int                              `json:"queue_delay_ms,omitempty"`
	Protocol                string                            `json:"protocol"`
	Transport               string                            `json:"transport"`
	RequestStage            string                            `json:"request_stage"`
	RequestTarget           string                            `json:"request_target"`
	InputHash               string                            `json:"input_hash"`
	ArchiveID               string                            `json:"archive_id,omitempty"`
	ArchiveVersion          int                               `json:"archive_version,omitempty"`
	ArchiveKeyID            string                            `json:"archive_key_id,omitempty"`
	ArchiveSHA256           []byte                            `json:"-"`
	ArchiveBytes            int64                             `json:"archive_bytes"`
	ArchiveStatus           string                            `json:"archive_status"`
	ArchiveIncomplete       bool                              `json:"archive_incomplete"`
	ArchiveContentLost      bool                              `json:"archive_content_lost"`
	ArchiveDeletedAt        *time.Time                        `json:"archive_deleted_at,omitempty"`
	DispositionStatus       string                            `json:"disposition_status"`
	DispositionTarget       string                            `json:"disposition_target"`
	DispositionTransitioned bool                              `json:"disposition_transitioned"`
	LegacySourceJobID       *int64                            `json:"legacy_source_job_id,omitempty"`
	LegacyStatus            string                            `json:"legacy_status,omitempty"`
	LegacyEventCount        int                               `json:"legacy_event_count,omitempty"`
	LegacyMetadata          json.RawMessage                   `json:"-"`
	CreatedAt               time.Time                         `json:"created_at"`
}

const (
	ContentModerationArchiveStatusNone      = "none"
	ContentModerationArchiveStatusAvailable = "available"
	ContentModerationArchiveStatusEmergency = "emergency"
	ContentModerationArchiveStatusRetrying  = "retrying"
	ContentModerationArchiveStatusLost      = "content_lost"
	ContentModerationArchiveStatusDeleted   = "deleted"
)

type ContentModerationArchiveEnvelope struct {
	ArchiveID         string                                     `json:"archive_id"`
	Version           int                                        `json:"version"`
	CapturedAt        time.Time                                  `json:"captured_at"`
	Request           ContentModerationArchiveRequest            `json:"request"`
	InputHash         string                                     `json:"input_hash"`
	Action            string                                     `json:"action"`
	DispositionStatus string                                     `json:"disposition_status"`
	DispositionTarget string                                     `json:"disposition_target"`
	Incomplete        bool                                       `json:"incomplete"`
	LegacyPromptAudit *ContentModerationLegacyPromptAuditArchive `json:"legacy_prompt_audit,omitempty"`
}

// ContentModerationLegacyPromptAuditArchive retains every historical event
// when multiple Prompt Audit events refer to one job. It is present only in
// encrypted legacy_prompt_only archives.
type ContentModerationLegacyPromptAuditArchive struct {
	SourceJobID int64                                     `json:"source_job_id"`
	Status      string                                    `json:"status"`
	Events      []ContentModerationLegacyPromptAuditEvent `json:"events"`
}

type ContentModerationLegacyPromptAuditEvent struct {
	SourceEventID    int64           `json:"source_event_id"`
	RequestID        string          `json:"request_id"`
	UserID           *int64          `json:"user_id,omitempty"`
	Username         string          `json:"username_snapshot"`
	UserEmail        string          `json:"user_email_snapshot"`
	APIKeyID         *int64          `json:"api_key_id,omitempty"`
	APIKeyName       string          `json:"api_key_name_snapshot"`
	GroupID          *int64          `json:"group_id,omitempty"`
	GroupName        string          `json:"group_name"`
	Provider         string          `json:"provider"`
	Endpoint         string          `json:"endpoint"`
	Protocol         string          `json:"protocol"`
	Model            string          `json:"model"`
	PromptHash       string          `json:"prompt_hash"`
	RedactedPreview  string          `json:"redacted_preview"`
	Stage            string          `json:"stage"`
	Decision         string          `json:"decision"`
	RiskLevel        string          `json:"risk_level"`
	Action           string          `json:"action"`
	Categories       json.RawMessage `json:"categories"`
	MatchedScanners  json.RawMessage `json:"matched_scanners"`
	ScannerScores    json.RawMessage `json:"scanner_scores"`
	ScannerEvidence  json.RawMessage `json:"scanner_evidence"`
	ScannerBackend   string          `json:"scanner_backend"`
	ScannerVersion   string          `json:"scanner_version"`
	GuardEndpointID  string          `json:"guard_endpoint_id"`
	PolicyID         string          `json:"policy_id"`
	PolicyVersion    int             `json:"policy_version"`
	ConfigVersion    int64           `json:"config_version"`
	ChunkTotal       int             `json:"chunk_total"`
	LatencyMS        int             `json:"latency_ms"`
	FullPromptBase64 string          `json:"full_prompt_base64"`
	CreatedAt        time.Time       `json:"created_at"`
}

type ContentModerationArchiveRequest struct {
	Method     string      `json:"method"`
	Target     string      `json:"target"`
	Headers    http.Header `json:"headers"`
	BodyBase64 string      `json:"body_base64"`
	Transport  string      `json:"transport"`
	Stage      string      `json:"stage"`
}

type ContentModerationArchiveAccess struct {
	LogID       int64
	ActorUserID int64
	Action      string
	RequestID   string
	Result      string
	BytesServed int64
	Detail      string
}

const ContentModerationArchivePreviewMaxBytes = 1 << 20

type ContentModerationArchivePreview struct {
	Content       string `json:"content"`
	ReturnedBytes int64  `json:"returned_bytes"`
	TotalBytes    int64  `json:"total_bytes"`
	Truncated     bool   `json:"truncated"`
}

type ContentModerationArchiveRepository interface {
	CreateLogWithArchive(ctx context.Context, log *ContentModerationLog, archive *ContentModerationEncryptedArchive) error
	CreateContentLostLog(ctx context.Context, log *ContentModerationLog) error
	GetArchive(ctx context.Context, logID int64) (*ContentModerationLog, *ContentModerationEncryptedArchive, error)
	DeleteArchive(ctx context.Context, access ContentModerationArchiveAccess) (bool, error)
	RecordArchiveAccess(ctx context.Context, access ContentModerationArchiveAccess) error
	ReferencedArchiveKeyIDs(ctx context.Context) ([]string, error)
}

type ContentModerationDispositionRepository interface {
	DisableUserIfActive(ctx context.Context, userID int64) (bool, error)
	DisableAPIKeyIfActive(ctx context.Context, apiKeyID int64) (credential string, transitioned bool, err error)
}

type ContentModerationDispositionCountRepository interface {
	CountFlaggedByUserSinceExcludingArchive(ctx context.Context, userID int64, since time.Time, excludeCyberPolicy bool, archiveID string) (int, error)
}

type ContentModerationDispositionLogRepository interface {
	UpdateLogDispositionByArchiveID(ctx context.Context, archiveID, status, target string, transitioned, autoBanned bool, violationCount int) error
}

type ContentModerationEmailDeliveryClaim struct {
	Exists  bool
	Claimed bool
	Status  string
}

type ContentModerationEmailDeliveryRepository interface {
	ClaimLogEmailDelivery(ctx context.Context, logID int64) (ContentModerationEmailDeliveryClaim, error)
	ClaimLogEmailDeliveryByArchiveID(ctx context.Context, archiveID string) (ContentModerationEmailDeliveryClaim, error)
	CompleteLogEmailDelivery(ctx context.Context, logID int64, sent bool) error
	CompleteLogEmailDeliveryByArchiveID(ctx context.Context, archiveID string, sent bool) error
}

type contentModerationEmailDeliveryOutcome struct {
	Sent               bool
	SendRequired       bool
	CompletionRequired bool
	DeliveryErr        error
	StateErr           error
}

type ContentModerationLogFilter struct {
	Pagination     pagination.PaginationParams
	LogID          *int64
	Result         string
	GroupID        *int64
	Endpoint       string
	ContextClass   string
	ModelProfile   string
	DecisionSource string
	Search         string
	From           *time.Time
	To             *time.Time
}

type ContentModerationCleanupResult struct {
	DeletedHit    int64     `json:"deleted_hit"`
	DeletedNonHit int64     `json:"deleted_non_hit"`
	FinishedAt    time.Time `json:"finished_at"`
}

type ContentModerationRuntimeStatus struct {
	Enabled                      bool                                  `json:"enabled"`
	RiskControlEnabled           bool                                  `json:"risk_control_enabled"`
	Mode                         string                                `json:"mode"`
	PreBlockActive               int                                   `json:"pre_block_active"`
	PreBlockChecked              int64                                 `json:"pre_block_checked"`
	PreBlockAllowed              int64                                 `json:"pre_block_allowed"`
	PreBlockBlocked              int64                                 `json:"pre_block_blocked"`
	PreBlockErrors               int64                                 `json:"pre_block_errors"`
	PreBlockAvgLatencyMS         int64                                 `json:"pre_block_avg_latency_ms"`
	PreBlockAPIKeyActive         int64                                 `json:"pre_block_api_key_active"`
	PreBlockAPIKeyAvailableCount int64                                 `json:"pre_block_api_key_available_count"`
	PreBlockAPIKeyTotalCalls     int64                                 `json:"pre_block_api_key_total_calls"`
	PreBlockAPIKeyLoads          []ContentModerationAPIKeyLoad         `json:"pre_block_api_key_loads"`
	APIKeyStatuses               []ContentModerationAPIKeyStatus       `json:"api_key_statuses"`
	FlaggedHashCount             int64                                 `json:"flagged_hash_count"`
	LastCleanupAt                *time.Time                            `json:"last_cleanup_at,omitempty"`
	LastCleanupDeletedHit        int64                                 `json:"last_cleanup_deleted_hit"`
	LastCleanupDeletedNonHit     int64                                 `json:"last_cleanup_deleted_non_hit"`
	PendingBodyBytes             int64                                 `json:"pending_body_bytes"`
	PendingBodyMaxSeen           int64                                 `json:"pending_body_max_seen"`
	PendingBodyBudgetBytes       int64                                 `json:"pending_body_budget_bytes"`
	PendingBodyRejections        int64                                 `json:"pending_body_rejections"`
	ObservedRequestBodyMax       int64                                 `json:"observed_request_body_max"`
	RequestBodyHistogram         []ContentModerationBodySizeBucket     `json:"request_body_histogram"`
	FragmentCacheHits            int64                                 `json:"fragment_cache_hits"`
	FragmentCacheMisses          int64                                 `json:"fragment_cache_misses"`
	FragmentCacheExpired         int64                                 `json:"fragment_cache_expired"`
	FragmentCacheReplays         int64                                 `json:"fragment_cache_replays"`
	FragmentCacheErrors          int64                                 `json:"fragment_cache_errors"`
	FragmentCacheWrites          int64                                 `json:"fragment_cache_writes"`
	FragmentCacheWriteErrors     int64                                 `json:"fragment_cache_write_errors"`
	SecondLayerMetrics           []ContentModerationSecondLayerMetric  `json:"second_layer_metrics"`
	SecondLayerShadowQueued      int64                                 `json:"second_layer_shadow_queued"`
	SecondLayerShadowDropped     int64                                 `json:"second_layer_shadow_dropped"`
	SecondLayerShadowCompleted   int64                                 `json:"second_layer_shadow_completed"`
	SecondLayerShadowQueueDepth  int                                   `json:"second_layer_shadow_queue_depth"`
	ArchiveRuntime               ContentModerationArchiveRuntimeStatus `json:"archive_runtime"`
}

type ContentModerationSecondLayerMetric struct {
	EndpointID     string `json:"endpoint_id"`
	Profile        string `json:"profile"`
	ContextClass   string `json:"context_class"`
	EvidenceMode   string `json:"evidence_mode"`
	KeywordTier    string `json:"keyword_tier"`
	Requests       int64  `json:"requests"`
	Safe           int64  `json:"safe"`
	Blocked        int64  `json:"blocked"`
	Uncertain      int64  `json:"uncertain"`
	ParserFailures int64  `json:"parser_failures"`
	Timeouts       int64  `json:"timeouts"`
	AvgLatencyMS   int64  `json:"avg_latency_ms"`
}

type ContentModerationBodySizeBucket struct {
	UpperBoundBytes int64 `json:"upper_bound_bytes"`
	Count           int64 `json:"count"`
}

type ContentModerationUnbanUserResult struct {
	UserID int64  `json:"user_id"`
	Status string `json:"status"`
}

type ContentModerationDeleteHashResult struct {
	InputHash      string `json:"input_hash"`
	Deleted        bool   `json:"deleted"`
	CacheVersion   string `json:"cache_version"`
	CacheNamespace string `json:"cache_namespace"`
}

type ContentModerationClearHashesResult struct {
	Deleted        int64  `json:"deleted"`
	CacheVersion   string `json:"cache_version"`
	CacheNamespace string `json:"cache_namespace"`
}

type ContentModerationRepository interface {
	CreateLog(ctx context.Context, log *ContentModerationLog) error
	ListLogs(ctx context.Context, filter ContentModerationLogFilter) ([]ContentModerationLog, *pagination.PaginationResult, error)
	// CountFlaggedByUserSince 统计窗口内计入封号的违规次数（排除 hash_block/cache_block；
	// excludeCyberPolicy 为 true 时额外排除 cyber_policy 行）。
	CountFlaggedByUserSince(ctx context.Context, userID int64, since time.Time, excludeCyberPolicy bool) (int, error)
	CleanupExpiredLogs(ctx context.Context, hitBefore time.Time, nonHitBefore time.Time) (*ContentModerationCleanupResult, error)
}

type ContentModerationHashCache interface {
	RecordFlaggedInputHash(ctx context.Context, inputHash string) error
	HasFlaggedInputHash(ctx context.Context, inputHash string) (bool, error)
	DeleteFlaggedInputHash(ctx context.Context, inputHash string) (bool, error)
	ClearFlaggedInputHashes(ctx context.Context) (int64, error)
	CountFlaggedInputHashes(ctx context.Context) (int64, error)
}

const (
	ContentModerationFragmentAllow = "allow"
	ContentModerationFragmentBlock = "block"
)

type ContentModerationFragmentCache interface {
	GetFragmentResult(ctx context.Context, namespace, fragmentHash string) (result string, found bool, err error)
	PutFragmentResult(ctx context.Context, namespace, fragmentHash, result string, estimatedBytes int64, maxEntries int, maxBytes int64) error
	DeleteFragmentResult(ctx context.Context, namespace, fragmentHash string) (bool, error)
	ClearFragmentResults(ctx context.Context, namespace string) (int64, error)
	CountFragmentResults(ctx context.Context, namespace string) (int64, error)
}

// ContentModerationFragmentCacheEntry carries replay provenance without
// changing the legacy cache interface implemented by existing fakes.
type ContentModerationFragmentCacheEntry struct {
	Result            string    `json:"result"`
	SourceLogID       *int64    `json:"source_log_id,omitempty"`
	ReplayOfInputHash string    `json:"replay_of_input_hash,omitempty"`
	DecisionSource    string    `json:"decision_source,omitempty"`
	Category          string    `json:"category,omitempty"`
	MatchedKeyword    string    `json:"matched_keyword,omitempty"`
	ModelProfile      string    `json:"model_profile,omitempty"`
	PromptVersion     string    `json:"prompt_version,omitempty"`
	KeywordTier       string    `json:"keyword_tier,omitempty"`
	KeywordRuleID     string    `json:"keyword_rule_id,omitempty"`
	EvidenceMode      string    `json:"evidence_mode,omitempty"`
	EvidenceTruncated bool      `json:"evidence_truncated,omitempty"`
	ParserStatus      string    `json:"parser_status,omitempty"`
	ExpiresAt         time.Time `json:"expires_at,omitempty"`
	Expired           bool      `json:"-"`
}

// ContentModerationFragmentTTLCache is an optional capability. Services use
// it when available and fall back to ContentModerationFragmentCache for old
// Redis clients and test doubles.
type ContentModerationFragmentTTLCache interface {
	GetFragmentCacheEntry(ctx context.Context, namespace, fragmentHash string) (ContentModerationFragmentCacheEntry, bool, error)
	PutFragmentCacheEntry(ctx context.Context, namespace, fragmentHash string, entry ContentModerationFragmentCacheEntry, estimatedBytes int64, maxEntries int, maxBytes int64, ttl time.Duration) error
}

type ContentModerationFragmentAliasCache interface {
	DeleteFragmentResultAliases(ctx context.Context, fragmentHash string) (int64, error)
	ClearAllFragmentResults(ctx context.Context) (int64, error)
}

type ContentModerationService struct {
	settingRepo              SettingRepository
	repo                     ContentModerationRepository
	hashCache                ContentModerationHashCache
	groupRepo                GroupRepository
	userRepo                 UserRepository
	proxyRepo                ProxyRepository
	authCacheInvalidator     APIKeyAuthCacheInvalidator
	emailService             *EmailService
	apiKeyRepo               APIKeyRepository
	archiveRuntime           *contentModerationArchiveRuntime
	dispositionRepo          ContentModerationDispositionRepository
	httpClient               *http.Client
	moderationProxyCache     atomic.Pointer[moderationProxyURLCacheEntry]
	apiKeyCursor             atomic.Uint64
	preBlockActive           atomic.Int64
	preBlockChecked          atomic.Int64
	preBlockAllowed          atomic.Int64
	preBlockBlocked          atomic.Int64
	preBlockErrors           atomic.Int64
	preBlockLatencyTotalMS   atomic.Int64
	lastCleanupUnix          atomic.Int64
	lastCleanupDeletedHit    atomic.Int64
	lastCleanupDeletedNonHit atomic.Int64
	runtimeSnapshot          atomic.Pointer[contentModerationRuntimeSnapshot]
	runtimeRefreshMu         sync.Mutex
	runtimeCacheTTL          time.Duration
	runtimeRefreshRetryAt    atomic.Int64
	keyHealthMu              sync.Mutex
	keyHealth                map[string]*contentModerationKeyHealth
	pendingBodyBudget        *ContentModerationPendingBodyBudget
	pendingBodyBudgetOnce    sync.Once
	pendingBodyBudgetBytes   atomic.Int64
	observedRequestBodyMax   atomic.Int64
	requestBodyBuckets       [6]atomic.Int64
	fragmentCacheHits        atomic.Int64
	fragmentCacheMisses      atomic.Int64
	fragmentCacheExpired     atomic.Int64
	fragmentCacheReplays     atomic.Int64
	fragmentCacheErrors      atomic.Int64
	fragmentCacheWrites      atomic.Int64
	fragmentCacheWriteErrors atomic.Int64
	fragmentDecisionMu       sync.Mutex
	fragmentDecisionLocks    map[string]*contentModerationFragmentDecisionLock
	secondLayerClients       sync.Map
	secondLayerEndpointSlots sync.Map
	secondLayerMetrics       sync.Map
	secondLayerShadowOnce    sync.Once
	secondLayerShadowMu      sync.RWMutex
	secondLayerShadowQueue   chan func()
	secondLayerShadowQueued  atomic.Int64
	secondLayerShadowDropped atomic.Int64
	secondLayerShadowDone    atomic.Int64
}

type contentModerationSecondLayerMetricCounter struct {
	endpointID     string
	profile        string
	contextClass   string
	evidenceMode   string
	keywordTier    string
	requests       atomic.Int64
	safe           atomic.Int64
	blocked        atomic.Int64
	uncertain      atomic.Int64
	parserFailures atomic.Int64
	timeouts       atomic.Int64
	latencyTotalMS atomic.Int64
}

type contentModerationFragmentDecisionLock struct {
	mu   sync.Mutex
	refs int
}

type contentModerationRuntimeSnapshot struct {
	riskControlEnabled          bool
	config                      *ContentModerationConfig
	keywordMatcher              *contentModerationKeywordMatcher
	unconditionalKeywordMatcher *contentModerationKeywordMatcher
	contextualKeywordMatcher    *contentModerationKeywordMatcher
	secondLayerPrefilterMatcher *contentModerationPrefilterMatcher
	fragmentCacheNamespace      string
	configDigest                [sha256.Size]byte
	loadedAt                    time.Time
}

type contentModerationKeyHealth struct {
	Hash           string
	Masked         string
	FailureCount   int
	SuccessCount   int64
	LastError      string
	LastCheckedAt  time.Time
	FrozenUntil    time.Time
	LastLatencyMS  int
	LastHTTPStatus int
	LastTested     bool
	SyncActive     int64
	SyncTotal      int64
	SyncSuccess    int64
	SyncErrors     int64
	SyncLatencyMS  int64
}

func NewContentModerationService(
	settingRepo SettingRepository,
	repo ContentModerationRepository,
	hashCache ContentModerationHashCache,
	groupRepo GroupRepository,
	userRepo UserRepository,
	proxyRepo ProxyRepository,
	authCacheInvalidator APIKeyAuthCacheInvalidator,
	emailService *EmailService,
) *ContentModerationService {
	svc := &ContentModerationService{
		settingRepo:          settingRepo,
		repo:                 repo,
		hashCache:            hashCache,
		groupRepo:            groupRepo,
		userRepo:             userRepo,
		proxyRepo:            proxyRepo,
		authCacheInvalidator: authCacheInvalidator,
		emailService:         emailService,
		httpClient:           servertiming.InstrumentClient(nil),
		keyHealth:            make(map[string]*contentModerationKeyHealth),
		pendingBodyBudget:    NewContentModerationPendingBodyBudget(),
	}
	svc.pendingBodyBudgetBytes.Store(DefaultContentModerationPendingBodyBudgetBytes)
	if dispositionRepo, ok := repo.(ContentModerationDispositionRepository); ok {
		svc.dispositionRepo = dispositionRepo
	}
	if settingRepo != nil && repo != nil {
		go svc.cleanupWorker()
	}
	return svc
}

func (s *ContentModerationService) GetConfig(ctx context.Context) (*ContentModerationConfigView, error) {
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	return s.configView(cfg), nil
}

func (s *ContentModerationService) UpdateConfig(ctx context.Context, input UpdateContentModerationConfigInput) (*ContentModerationConfigView, error) {
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	if input.Enabled != nil {
		cfg.Enabled = *input.Enabled
	}
	if input.Mode != nil {
		mode := strings.TrimSpace(*input.Mode)
		if err := validateContentModerationMode(mode); err != nil {
			return nil, err
		}
		cfg.Mode = mode
	}
	if input.BaseURL != nil {
		cfg.BaseURL = strings.TrimSpace(*input.BaseURL)
	}
	if input.Model != nil {
		cfg.Model = strings.TrimSpace(*input.Model)
	}
	if input.ProxyID != nil {
		if *input.ProxyID > 0 {
			id := *input.ProxyID
			cfg.ProxyID = &id
		} else {
			cfg.ProxyID = nil
		}
	}
	if input.TimeoutMS != nil {
		cfg.TimeoutMS = *input.TimeoutMS
	}
	if input.SampleRate != nil {
		cfg.SampleRate = *input.SampleRate
	}
	if input.BlockStatus != nil {
		cfg.BlockStatus = *input.BlockStatus
	}
	if input.BlockMessage != nil {
		cfg.BlockMessage = strings.TrimSpace(*input.BlockMessage)
	}
	if input.EmailOnHit != nil {
		cfg.EmailOnHit = *input.EmailOnHit
	}
	if input.AutoBanEnabled != nil {
		cfg.AutoBanEnabled = *input.AutoBanEnabled
	}
	if input.BanThreshold != nil {
		cfg.BanThreshold = *input.BanThreshold
	}
	if input.ViolationWindowHours != nil {
		cfg.ViolationWindowHours = *input.ViolationWindowHours
	}
	if input.RetryCount != nil {
		cfg.RetryCount = *input.RetryCount
	}
	if input.HitRetentionDays != nil {
		cfg.HitRetentionDays = *input.HitRetentionDays
	}
	if input.NonHitRetentionDays != nil {
		cfg.NonHitRetentionDays = *input.NonHitRetentionDays
	}
	if input.PreHashCheckEnabled != nil {
		cfg.PreHashCheckEnabled = *input.PreHashCheckEnabled
	}
	if input.BlockedKeywords != nil {
		cfg.BlockedKeywords = normalizeBlockedKeywords(*input.BlockedKeywords)
	}
	if input.KeywordBlockingMode != nil {
		cfg.KeywordBlockingMode = strings.TrimSpace(*input.KeywordBlockingMode)
	}
	if input.ModelFilter != nil {
		cfg.ModelFilter = *input.ModelFilter
	}
	oldCacheNamespace := cfg.fragmentCacheNamespace()
	if input.CacheVersion != nil {
		cfg.CacheVersion = strings.TrimSpace(*input.CacheVersion)
	}
	if input.CacheMaxEntries != nil {
		cfg.CacheMaxEntries = *input.CacheMaxEntries
	}
	if input.CacheMaxBytes != nil {
		cfg.CacheMaxBytes = *input.CacheMaxBytes
	}
	if input.FragmentBlockTTLSeconds != nil {
		cfg.FragmentBlockTTLSeconds = *input.FragmentBlockTTLSeconds
	}
	if input.FragmentAllowTTLSeconds != nil {
		cfg.FragmentAllowTTLSeconds = *input.FragmentAllowTTLSeconds
	}
	if input.FragmentTTLPolicyVersion != nil {
		cfg.FragmentTTLPolicyVersion = strings.TrimSpace(*input.FragmentTTLPolicyVersion)
	}
	if input.FirstLayerStage != nil {
		cfg.FirstLayerStage = strings.TrimSpace(*input.FirstLayerStage)
	}
	if input.SecondLayerEnabled != nil {
		cfg.SecondLayerEnabled = *input.SecondLayerEnabled
	}
	if input.SecondLayerStage != nil {
		cfg.SecondLayerStage = strings.TrimSpace(*input.SecondLayerStage)
	}
	if input.SecondLayerEndpoints != nil {
		cfg.SecondLayerEndpoints = mergeContentModerationEndpointTokens(cfg.SecondLayerEndpoints, *input.SecondLayerEndpoints)
	}
	if input.SecondLayerScanners != nil {
		cfg.SecondLayerScanners = normalizeContentModerationScannerIDs(*input.SecondLayerScanners)
	}
	if input.HardBlockPatterns != nil {
		cfg.HardBlockPatterns = normalizeBlockedKeywords(*input.HardBlockPatterns)
	}
	if input.CandidateKeywords != nil {
		cfg.CandidateKeywords = normalizeBlockedKeywords(*input.CandidateKeywords)
	}
	if input.Layer1Keywords != nil {
		cfg.HardBlockPatterns = normalizeBlockedKeywords(*input.Layer1Keywords)
	}
	if input.Layer2Keywords != nil {
		cfg.CandidateKeywords = normalizeBlockedKeywords(*input.Layer2Keywords)
	}
	if input.Layer1Keywords != nil || input.Layer2Keywords != nil {
		// Canonical two-layer updates replace the legacy mixed keyword bucket.
		cfg.BlockedKeywords = []string{}
	}
	if input.KeywordAllowlist != nil {
		cfg.KeywordAllowlist = normalizeBlockedKeywords(*input.KeywordAllowlist)
	}
	if input.KeywordPolicyVersion != nil {
		cfg.KeywordPolicyVersion = strings.TrimSpace(*input.KeywordPolicyVersion)
	}
	if input.ContextPolicyVersion != nil {
		cfg.ContextPolicyVersion = strings.TrimSpace(*input.ContextPolicyVersion)
	}
	if input.EvidencePolicyVersion != nil {
		cfg.EvidencePolicyVersion = strings.TrimSpace(*input.EvidencePolicyVersion)
	}
	if input.CandidateAsset != nil {
		cfg.CandidateAsset = strings.TrimSpace(*input.CandidateAsset)
	}
	if input.CandidateEnabled != nil {
		cfg.CandidateEnabled = *input.CandidateEnabled
	}
	if input.AllGroups != nil {
		cfg.AllGroups = *input.AllGroups
	}
	if input.GroupIDs != nil {
		cfg.GroupIDs = normalizeInt64IDs(*input.GroupIDs)
	}
	if input.UserEmailWhitelist != nil {
		cfg.UserEmailWhitelist = normalizeContentModerationUserEmailWhitelist(*input.UserEmailWhitelist)
	}
	if input.RecordNonHits != nil {
		cfg.RecordNonHits = *input.RecordNonHits
	}
	if input.CyberPolicyExcludeFromBanCount != nil {
		cfg.CyberPolicyExcludeFromBanCount = *input.CyberPolicyExcludeFromBanCount
	}
	if input.Thresholds != nil {
		cfg.Thresholds = mergeContentModerationThresholds(ContentModerationDefaultThresholds(), *input.Thresholds)
	}
	if input.ClearAPIKey {
		cfg.APIKey = ""
		cfg.APIKeys = []string{}
	} else {
		apiKeysMode := normalizeContentModerationAPIKeysMode(input.APIKeysMode)
		if input.DeleteAPIKeyHashes != nil && apiKeysMode != contentModerationAPIKeysModeReplace {
			cfg.APIKeys = deleteModerationAPIKeysByHash(cfg.apiKeys(), *input.DeleteAPIKeyHashes)
			cfg.APIKey = ""
		}
		if input.APIKeys != nil {
			if apiKeysMode == contentModerationAPIKeysModeReplace {
				cfg.APIKeys = normalizeModerationAPIKeys(*input.APIKeys)
			} else {
				cfg.APIKeys = normalizeModerationAPIKeys(append(cfg.apiKeys(), *input.APIKeys...))
			}
			cfg.APIKey = ""
		}
		if input.APIKey != nil && strings.TrimSpace(*input.APIKey) != "" {
			cfg.APIKeys = normalizeModerationAPIKeys(append(cfg.APIKeys, *input.APIKey))
			cfg.APIKey = ""
		}
	}
	if err := s.validateConfig(ctx, cfg); err != nil {
		return nil, err
	}
	cfg.normalize()
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal content moderation config: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyContentModerationConfig, string(raw)); err != nil {
		return nil, fmt.Errorf("save content moderation config: %w", err)
	}
	s.replaceRuntimeConfig(cfg, raw)
	if fragmentCache, ok := s.hashCache.(ContentModerationFragmentCache); ok {
		newCacheNamespace := cfg.fragmentCacheNamespace()
		if oldCacheNamespace != "" && oldCacheNamespace != newCacheNamespace {
			go func(namespace string) {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if _, err := fragmentCache.ClearFragmentResults(cleanupCtx, namespace); err != nil {
					slog.Warn("content_moderation.fragment_cache_old_namespace_cleanup_failed", "error", err)
				}
			}(oldCacheNamespace)
		}
	}
	// 代理选择可能已变化，丢弃已解析的代理 URL 缓存，下次调用即时生效。
	s.moderationProxyCache.Store(nil)
	return s.configView(cfg), nil
}

func (s *ContentModerationService) TestAPIKeys(ctx context.Context, input TestContentModerationAPIKeysInput) (*TestContentModerationAPIKeysResult, error) {
	cfg := defaultContentModerationConfig()
	var err error
	if s.settingRepo != nil {
		cfg, err = s.loadConfig(ctx)
		if err != nil {
			return nil, err
		}
	}
	keys := normalizeModerationAPIKeys(input.APIKeys)
	configured := false
	if len(keys) == 0 {
		keys = cfg.apiKeys()
		configured = true
	}
	if strings.TrimSpace(input.BaseURL) != "" {
		cfg.BaseURL = input.BaseURL
	}
	if strings.TrimSpace(input.Model) != "" {
		cfg.Model = input.Model
	}
	if input.TimeoutMS > 0 {
		cfg.TimeoutMS = input.TimeoutMS
	}
	if input.ProxyID != nil {
		if *input.ProxyID > 0 {
			id := *input.ProxyID
			cfg.ProxyID = &id
		} else {
			cfg.ProxyID = nil
		}
	}
	cfg.normalize()
	testInput, imageCount, err := buildModerationTestInput(input.Prompt, input.Images)
	if err != nil {
		return nil, err
	}
	auditOnly := contentModerationTestHasAuditInput(input.Prompt, input.Images)
	if configured && auditOnly {
		key, ok := s.nextUsableAPIKey(cfg)
		if !ok {
			return &TestContentModerationAPIKeysResult{
				Items:      s.apiKeyStatuses(keys),
				ImageCount: imageCount,
			}, nil
		}
		keys = []string{key}
	}
	if len(keys) == 0 {
		return &TestContentModerationAPIKeysResult{Items: []ContentModerationAPIKeyStatus{}, ImageCount: imageCount}, nil
	}
	items := make([]ContentModerationAPIKeyStatus, 0, len(keys))
	var auditResult *ContentModerationTestAuditResult
	for idx, key := range keys {
		start := time.Now()
		httpStatus := 0
		result, err := s.callModerationOnceWithInput(ctx, cfg, key, testInput, &httpStatus)
		latency := int(time.Since(start).Milliseconds())
		keyHash := moderationAPIKeyHash(key)
		if err != nil {
			s.markAPIKeyError(key, err.Error(), latency, httpStatus)
		} else {
			s.markAPIKeySuccess(key, latency, httpStatus)
			if auditResult == nil {
				auditResult = buildContentModerationTestAuditResult(result, cfg.Thresholds)
			}
		}
		status := s.apiKeyStatusForHash(idx, keyHash, maskSecretTail(key), configured)
		status.LastTested = true
		items = append(items, status)
	}
	return &TestContentModerationAPIKeysResult{Items: items, AuditResult: auditResult, ImageCount: imageCount}, nil
}

func (s *ContentModerationService) Check(ctx context.Context, input ContentModerationCheckInput) (*ContentModerationDecision, error) {
	allow := &ContentModerationDecision{Allowed: true, Action: ContentModerationActionAllow}
	if s == nil || s.settingRepo == nil || s.repo == nil {
		slog.Info("content_moderation.skip_unavailable",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"endpoint", input.Endpoint,
			"protocol", input.Protocol)
		return allow, nil
	}
	runtimeSnapshot, err := s.loadRuntimeSnapshot(ctx)
	if err != nil {
		slog.Warn("content_moderation.skip_config_load_failed",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"endpoint", input.Endpoint,
			"protocol", input.Protocol,
			"error", err)
		return allow, nil
	}
	cfg := runtimeSnapshot.config
	if input.Scope != nil {
		return s.checkUnifiedFragments(ctx, input, runtimeSnapshot), nil
	}
	whitelistShadow := cfg != nil && cfg.includesUserEmail(input.UserEmail)
	if !runtimeSnapshot.riskControlEnabled {
		slog.Info("content_moderation.skip_feature_disabled",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"endpoint", input.Endpoint,
			"protocol", input.Protocol)
		return allow, nil
	}
	inGroupScope := cfg.includesGroup(input.GroupID)
	inModelScope := cfg.includesModel(input.Model)
	slog.Info("content_moderation.config_loaded",
		"user_id", input.UserID,
		"api_key_id", input.APIKeyID,
		"group_id", contentModerationLogGroupID(input.GroupID),
		"group_name", input.GroupName,
		"endpoint", input.Endpoint,
		"provider", input.Provider,
		"protocol", input.Protocol,
		"model", input.Model,
		"enabled", cfg.Enabled,
		"mode", cfg.Mode,
		"all_groups", cfg.AllGroups,
		"configured_group_ids", cfg.GroupIDs,
		"in_group_scope", inGroupScope,
		"model_filter_type", cfg.ModelFilter.Type,
		"configured_models", cfg.ModelFilter.Models,
		"in_model_scope", inModelScope,
		"sample_rate", cfg.SampleRate,
		"api_key_count", len(cfg.apiKeys()),
		"pre_hash_check_enabled", cfg.PreHashCheckEnabled,
		"record_non_hits", cfg.RecordNonHits)
	if !cfg.Enabled {
		slog.Info("content_moderation.skip_config_disabled",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"endpoint", input.Endpoint,
			"protocol", input.Protocol)
		return allow, nil
	}
	if cfg.Mode == ContentModerationModeOff {
		slog.Info("content_moderation.skip_mode_off",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"endpoint", input.Endpoint,
			"protocol", input.Protocol)
		return allow, nil
	}
	if !inGroupScope {
		slog.Info("content_moderation.skip_group_out_of_scope",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"group_name", input.GroupName,
			"endpoint", input.Endpoint,
			"protocol", input.Protocol,
			"all_groups", cfg.AllGroups,
			"configured_group_ids", cfg.GroupIDs)
		return allow, nil
	}
	if !inModelScope {
		slog.Info("content_moderation.skip_model_out_of_scope",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"group_name", input.GroupName,
			"endpoint", input.Endpoint,
			"protocol", input.Protocol,
			"model", input.Model,
			"model_filter_type", cfg.ModelFilter.Type,
			"configured_models", cfg.ModelFilter.Models)
		return allow, nil
	}
	content := ExtractContentModerationInput(input.Protocol, input.Body)
	if content.IsEmpty() {
		slog.Info("content_moderation.skip_empty_input",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"endpoint", input.Endpoint,
			"protocol", input.Protocol,
			"body_bytes", len(input.Body))
		return allow, nil
	}
	content.Normalize()
	slog.Info("content_moderation.input_extracted",
		"user_id", input.UserID,
		"api_key_id", input.APIKeyID,
		"group_id", contentModerationLogGroupID(input.GroupID),
		"endpoint", input.Endpoint,
		"protocol", input.Protocol,
		"text_runes", len([]rune(content.Text)),
		"image_count", len(content.Images))
	hashText := content.Hash()
	if cfg.Mode == ContentModerationModePreBlock {
		if cfg.KeywordBlockingMode != ContentModerationKeywordModeAPIOnly && len(cfg.BlockedKeywords) > 0 {
			if keyword, hit := runtimeSnapshot.matchBlockedKeyword(content.Text); hit {
				action := ContentModerationActionKeywordBlock
				logFlagged := true
				if whitelistShadow {
					action = ContentModerationActionWhitelistShadow
					logFlagged = false
				} else if cfg.FirstLayerStage == ContentModerationFirstLayerStageShadow {
					action = ContentModerationActionFirstLayerShadow
					logFlagged = false
				}
				s.recordPreBlockSyncMetric(0, action)
				slog.Info("content_moderation.keyword_match",
					"user_id", input.UserID,
					"api_key_id", input.APIKeyID,
					"group_id", contentModerationLogGroupID(input.GroupID),
					"endpoint", input.Endpoint,
					"protocol", input.Protocol,
					"keyword_blocking_mode", cfg.KeywordBlockingMode,
					"action", action,
					"keyword", keyword)
				scores := map[string]float64{contentModerationKeywordCategory: 1.0}
				log := s.buildLog(input, cfg, action, logFlagged, contentModerationKeywordCategory, 1.0, scores, content.ExcerptText(), nil, nil, "")
				log.MatchedKeyword = keyword
				s.persistContentModerationLogWithInput(ctx, cfg, log, hashText, false, logFlagged, &input)
				if logFlagged {
					return &ContentModerationDecision{
						Allowed:         false,
						Blocked:         true,
						Flagged:         true,
						Message:         cfg.BlockMessage,
						StatusCode:      cfg.BlockStatus,
						MatchedKeyword:  keyword,
						HighestCategory: contentModerationKeywordCategory,
						HighestScore:    1.0,
						CategoryScores:  scores,
						Action:          ContentModerationActionKeywordBlock,
					}, nil
				}
			}
		}
		if cfg.KeywordBlockingMode == ContentModerationKeywordModeKeywordOnly {
			s.recordPreBlockSyncMetric(0, ContentModerationActionAllow)
			slog.Info("content_moderation.skip_api_keyword_only",
				"user_id", input.UserID,
				"api_key_id", input.APIKeyID,
				"group_id", contentModerationLogGroupID(input.GroupID),
				"endpoint", input.Endpoint,
				"protocol", input.Protocol)
			return allow, nil
		}
	}
	if cfg.PreHashCheckEnabled && s.hashCache != nil {
		matched, err := s.hashCache.HasFlaggedInputHash(ctx, hashText)
		if err != nil {
			slog.Warn("content_moderation.hash_check_failed", "user_id", input.UserID, "endpoint", input.Endpoint, "error", err)
		}
		if matched {
			action := ContentModerationActionHashBlock
			logFlagged := true
			if whitelistShadow {
				action = ContentModerationActionWhitelistShadow
				logFlagged = false
			}
			if cfg.Mode == ContentModerationModePreBlock {
				s.recordPreBlockSyncMetric(0, action)
			}
			slog.Info("content_moderation.hash_block",
				"user_id", input.UserID,
				"api_key_id", input.APIKeyID,
				"group_id", contentModerationLogGroupID(input.GroupID),
				"endpoint", input.Endpoint,
				"protocol", input.Protocol,
				"input_hash", hashText)
			message := cfg.BlockMessage
			if message != "" {
				message = fmt.Sprintf("%s（hash: %s）", message, hashText)
			}
			scores := map[string]float64{"hash": 1.0}
			log := s.buildLog(input, cfg, action, logFlagged, "hash", 1.0, scores, content.ExcerptText(), nil, nil, "")
			s.persistContentModerationLogWithInput(ctx, cfg, log, hashText, false, false, &input)
			if !whitelistShadow {
				return &ContentModerationDecision{
					Allowed:    false,
					Blocked:    true,
					Flagged:    true,
					Message:    message,
					StatusCode: cfg.BlockStatus,
					InputHash:  hashText,
					Action:     ContentModerationActionHashBlock,
				}, nil
			}
		}
	}
	if !cfg.shouldSample(hashText) {
		if cfg.Mode == ContentModerationModePreBlock {
			s.recordPreBlockSyncMetric(0, ContentModerationActionAllow)
		}
		slog.Info("content_moderation.skip_sample_rate",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"endpoint", input.Endpoint,
			"protocol", input.Protocol,
			"sample_rate", cfg.SampleRate)
		return allow, nil
	}
	if len(cfg.apiKeys()) == 0 {
		if cfg.Mode == ContentModerationModePreBlock {
			s.recordPreBlockSyncMetric(0, ContentModerationActionError)
		}
		slog.Warn("content_moderation.skip_no_audit_api_keys",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"endpoint", input.Endpoint,
			"protocol", input.Protocol)
		return allow, nil
	}
	return s.checkSync(ctx, input, cfg, content, hashText), nil
}

func (s *ContentModerationService) checkSync(ctx context.Context, input ContentModerationCheckInput, cfg *ContentModerationConfig, content ContentModerationInput, hashText string) *ContentModerationDecision {
	allow := &ContentModerationDecision{Allowed: true, Action: ContentModerationActionAllow}
	whitelistShadow := cfg != nil && cfg.includesUserEmail(input.UserEmail)
	trackPreBlock := cfg != nil && cfg.Mode == ContentModerationModePreBlock
	if trackPreBlock {
		s.preBlockActive.Add(1)
		defer s.preBlockActive.Add(-1)
	}
	start := time.Now()
	result, err := s.callModeration(ctx, cfg, content.ModerationInput(), trackPreBlock)
	latency := int(time.Since(start).Milliseconds())
	if err != nil {
		if trackPreBlock {
			s.recordPreBlockSyncMetric(latency, ContentModerationActionError)
		}
		slog.Warn("content_moderation.audit_api_failed",
			"user_id", input.UserID,
			"api_key_id", input.APIKeyID,
			"group_id", contentModerationLogGroupID(input.GroupID),
			"endpoint", input.Endpoint,
			"protocol", input.Protocol,
			"mode", cfg.Mode,
			"latency_ms", latency,
			"error", err)
		if cfg.RecordNonHits {
			log := s.buildLog(input, cfg, ContentModerationActionError, false, "", 0, nil, content.ExcerptText(), &latency, nil, err.Error())
			_ = s.repo.CreateLog(ctx, log)
		}
		return allow
	}

	flagged, highestCategory, highestScore := evaluateModerationScores(result.CategoryScores, cfg.Thresholds)
	action := ContentModerationActionAllow
	blocked := false
	if flagged && whitelistShadow {
		action = ContentModerationActionWhitelistShadow
	} else if flagged && cfg.Mode == ContentModerationModePreBlock {
		action = ContentModerationActionBlock
		blocked = true
	}
	if trackPreBlock {
		s.recordPreBlockSyncMetric(latency, action)
	}
	slog.Info("content_moderation.audit_result",
		"user_id", input.UserID,
		"api_key_id", input.APIKeyID,
		"group_id", contentModerationLogGroupID(input.GroupID),
		"group_name", input.GroupName,
		"endpoint", input.Endpoint,
		"protocol", input.Protocol,
		"mode", cfg.Mode,
		"flagged", flagged,
		"blocked", blocked,
		"action", action,
		"highest_category", highestCategory,
		"highest_score", highestScore,
		"latency_ms", latency)
	if flagged || cfg.RecordNonHits {
		logFlagged := flagged && !whitelistShadow
		log := s.buildLog(input, cfg, action, logFlagged, highestCategory, highestScore, result.CategoryScores, content.ExcerptText(), &latency, nil, "")
		s.persistContentModerationLogWithInput(ctx, cfg, log, hashText, logFlagged, logFlagged, &input)
	}
	if blocked {
		return &ContentModerationDecision{
			Allowed:         false,
			Blocked:         true,
			Flagged:         true,
			Message:         cfg.BlockMessage,
			StatusCode:      cfg.BlockStatus,
			HighestCategory: highestCategory,
			HighestScore:    highestScore,
			CategoryScores:  result.CategoryScores,
			Action:          action,
		}
	}
	return &ContentModerationDecision{
		Allowed:         true,
		Flagged:         flagged && !whitelistShadow,
		Message:         "",
		HighestCategory: highestCategory,
		HighestScore:    highestScore,
		CategoryScores:  result.CategoryScores,
		Action:          action,
	}
}

func (s *ContentModerationService) recordPreBlockSyncMetric(latencyMS int, action string) {
	if s == nil {
		return
	}
	s.preBlockChecked.Add(1)
	if latencyMS < 0 {
		latencyMS = 0
	}
	s.preBlockLatencyTotalMS.Add(int64(latencyMS))
	switch action {
	case ContentModerationActionBlock, ContentModerationActionHashBlock, ContentModerationActionKeywordBlock:
		s.preBlockBlocked.Add(1)
	case ContentModerationActionError:
		s.preBlockErrors.Add(1)
	default:
		s.preBlockAllowed.Add(1)
	}
}

func (s *ContentModerationService) ListLogs(ctx context.Context, filter ContentModerationLogFilter) ([]ContentModerationLog, *pagination.PaginationResult, error) {
	// Keep the audit endpoint fail-closed to incident views. Unknown and legacy
	// values retain the original combined blocked view.
	switch result := strings.ToLower(strings.TrimSpace(filter.Result)); result {
	case ContentModerationLogResultCyberPolicy,
		ContentModerationLogResultContentBlocked,
		ContentModerationLogResultRiskyShadow,
		ContentModerationLogResultReviewFailure:
		filter.Result = result
	default:
		filter.Result = ContentModerationLogResultBlocked
	}
	if filter.Pagination.Page <= 0 {
		filter.Pagination.Page = 1
	}
	if filter.Pagination.PageSize <= 0 {
		filter.Pagination.PageSize = 20
	}
	if filter.Pagination.PageSize > 100 {
		filter.Pagination.PageSize = 100
	}
	if filter.Pagination.SortOrder == "" {
		filter.Pagination.SortOrder = pagination.SortOrderDesc
	}
	return s.repo.ListLogs(ctx, filter)
}

func (s *ContentModerationService) UnbanUser(ctx context.Context, userID int64) (*ContentModerationUnbanUserResult, error) {
	if s == nil || s.userRepo == nil {
		return nil, infraerrors.InternalServer("CONTENT_MODERATION_USER_REPOSITORY_UNAVAILABLE", "用户仓储不可用")
	}
	if userID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_USER_ID", "用户 ID 无效")
	}
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, infraerrors.NotFound("USER_NOT_FOUND", "用户不存在")
		}
		return nil, fmt.Errorf("get content moderation unban user: %w", err)
	}
	if user.Status != StatusActive {
		user.Status = StatusActive
		if err := s.userRepo.Update(ctx, user, UserUpdateFields{Status: true}); err != nil {
			return nil, fmt.Errorf("update content moderation unban user: %w", err)
		}
	}
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, userID)
	}
	return &ContentModerationUnbanUserResult{
		UserID: userID,
		Status: StatusActive,
	}, nil
}

func (s *ContentModerationService) DeleteFlaggedInputHash(ctx context.Context, inputHash string) (*ContentModerationDeleteHashResult, error) {
	inputHash = normalizeContentModerationHash(inputHash)
	if inputHash == "" {
		return nil, infraerrors.BadRequest("INVALID_CONTENT_MODERATION_HASH", "风险输入哈希无效")
	}
	if s == nil || s.hashCache == nil {
		return nil, infraerrors.InternalServer("CONTENT_MODERATION_HASH_CACHE_UNAVAILABLE", "内容审计哈希缓存不可用")
	}
	cfg := defaultContentModerationConfig()
	var err error
	if s.settingRepo != nil {
		cfg, err = s.loadConfig(ctx)
		if err != nil {
			return nil, err
		}
	}
	namespace := cfg.fragmentCacheNamespace()
	deleted := false
	if aliasCache, ok := s.hashCache.(ContentModerationFragmentAliasCache); ok {
		count, deleteErr := aliasCache.DeleteFragmentResultAliases(ctx, inputHash)
		if deleteErr != nil {
			return nil, fmt.Errorf("delete content moderation fragment aliases: %w", deleteErr)
		}
		deleted = count > 0
	} else if fragmentCache, ok := s.hashCache.(ContentModerationFragmentCache); ok {
		deleted, err = fragmentCache.DeleteFragmentResult(ctx, namespace, inputHash)
		if err != nil {
			return nil, fmt.Errorf("delete content moderation fragment result: %w", err)
		}
	}
	legacyDeleted, legacyErr := s.hashCache.DeleteFlaggedInputHash(ctx, inputHash)
	if legacyErr != nil {
		slog.Warn("content_moderation.delete_legacy_flagged_hash_failed", "error", legacyErr)
	}
	return &ContentModerationDeleteHashResult{
		InputHash: inputHash, Deleted: deleted || legacyDeleted,
		CacheVersion: cfg.CacheVersion, CacheNamespace: namespace,
	}, nil
}

func (s *ContentModerationService) ClearFlaggedInputHashes(ctx context.Context) (*ContentModerationClearHashesResult, error) {
	if s == nil || s.hashCache == nil {
		return nil, infraerrors.InternalServer("CONTENT_MODERATION_HASH_CACHE_UNAVAILABLE", "内容审计哈希缓存不可用")
	}
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	namespace := cfg.fragmentCacheNamespace()
	deleted := int64(0)
	if aliasCache, ok := s.hashCache.(ContentModerationFragmentAliasCache); ok {
		deleted, err = aliasCache.ClearAllFragmentResults(ctx)
		if err != nil {
			return nil, fmt.Errorf("clear all content moderation fragment results: %w", err)
		}
	} else if fragmentCache, ok := s.hashCache.(ContentModerationFragmentCache); ok {
		deleted, err = fragmentCache.ClearFragmentResults(ctx, namespace)
		if err != nil {
			return nil, fmt.Errorf("clear content moderation fragment results: %w", err)
		}
	}
	legacyDeleted, legacyErr := s.hashCache.ClearFlaggedInputHashes(ctx)
	if legacyErr != nil {
		slog.Warn("content_moderation.clear_legacy_flagged_hashes_failed", "error", legacyErr)
	} else if _, ok := s.hashCache.(ContentModerationFragmentCache); !ok {
		deleted = legacyDeleted
	}
	return &ContentModerationClearHashesResult{Deleted: deleted, CacheVersion: cfg.CacheVersion, CacheNamespace: namespace}, nil
}

func (s *ContentModerationService) PreviewArchive(ctx context.Context, logID, actorUserID int64, requestID string) (*ContentModerationArchivePreview, error) {
	archiveRaw, repo, err := s.decryptContentModerationArchive(ctx, logID, actorUserID, requestID, "preview")
	if err != nil {
		return nil, err
	}
	raw, err := contentModerationArchiveBody(archiveRaw)
	if err != nil {
		s.recordFailedArchiveAccess(ctx, repo, logID, actorUserID, "preview", requestID, err)
		return nil, err
	}
	returned := len(raw)
	truncated := returned > ContentModerationArchivePreviewMaxBytes
	if truncated {
		returned = ContentModerationArchivePreviewMaxBytes
	}
	if err := repo.RecordArchiveAccess(ctx, ContentModerationArchiveAccess{
		LogID: logID, ActorUserID: actorUserID, Action: "preview", RequestID: requestID,
		Result: "success", BytesServed: int64(returned),
	}); err != nil {
		return nil, fmt.Errorf("record content moderation archive preview before serving: %w", err)
	}
	return &ContentModerationArchivePreview{
		Content:       string(raw[:returned]),
		ReturnedBytes: int64(returned), TotalBytes: int64(len(raw)), Truncated: truncated,
	}, nil
}

func contentModerationArchiveBody(raw []byte) ([]byte, error) {
	var envelope ContentModerationArchiveEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode archive envelope", ErrModerationArchiveIntegrity)
	}
	body, err := base64.StdEncoding.DecodeString(envelope.Request.BodyBase64)
	if err != nil {
		return nil, fmt.Errorf("%w: decode archived request body", ErrModerationArchiveIntegrity)
	}
	return body, nil
}

func contentModerationArchiveDownload(raw []byte) ([]byte, error) {
	body, err := contentModerationArchiveBody(raw)
	if err != nil {
		return nil, err
	}

	var exported map[string]json.RawMessage
	if err := json.Unmarshal(raw, &exported); err != nil {
		return nil, fmt.Errorf("%w: decode archive export", ErrModerationArchiveIntegrity)
	}
	requestRaw, ok := exported["request"]
	if !ok {
		return nil, fmt.Errorf("%w: archived request is missing", ErrModerationArchiveIntegrity)
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(requestRaw, &request); err != nil {
		return nil, fmt.Errorf("%w: decode archived request", ErrModerationArchiveIntegrity)
	}
	if request == nil {
		return nil, fmt.Errorf("%w: archived request is invalid", ErrModerationArchiveIntegrity)
	}
	delete(request, "body_base64")
	request["body"] = contentModerationReadableJSON(body)
	requestRaw, err = json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: encode archived request", ErrModerationArchiveIntegrity)
	}
	exported["request"] = requestRaw

	if legacyRaw, ok := exported["legacy_prompt_audit"]; ok && !bytes.Equal(bytes.TrimSpace(legacyRaw), []byte("null")) {
		var legacy map[string]json.RawMessage
		if err := json.Unmarshal(legacyRaw, &legacy); err != nil {
			return nil, fmt.Errorf("%w: decode legacy prompt audit", ErrModerationArchiveIntegrity)
		}
		eventsRaw, ok := legacy["events"]
		if !ok {
			return nil, fmt.Errorf("%w: legacy prompt audit events are missing", ErrModerationArchiveIntegrity)
		}
		var events []map[string]json.RawMessage
		if err := json.Unmarshal(eventsRaw, &events); err != nil {
			return nil, fmt.Errorf("%w: decode legacy prompt audit events", ErrModerationArchiveIntegrity)
		}
		for index := range events {
			promptRaw, ok := events[index]["full_prompt_base64"]
			if !ok {
				return nil, fmt.Errorf("%w: legacy prompt audit event %d is missing content", ErrModerationArchiveIntegrity, index)
			}
			var promptBase64 string
			if err := json.Unmarshal(promptRaw, &promptBase64); err != nil {
				return nil, fmt.Errorf("%w: decode legacy prompt audit event %d content", ErrModerationArchiveIntegrity, index)
			}
			prompt, err := base64.StdEncoding.DecodeString(promptBase64)
			if err != nil {
				return nil, fmt.Errorf("%w: decode legacy prompt audit event %d content", ErrModerationArchiveIntegrity, index)
			}
			delete(events[index], "full_prompt_base64")
			events[index]["full_prompt"] = contentModerationReadableJSON(prompt)
		}
		eventsRaw, err = json.Marshal(events)
		if err != nil {
			return nil, fmt.Errorf("%w: encode legacy prompt audit events", ErrModerationArchiveIntegrity)
		}
		legacy["events"] = eventsRaw
		legacyRaw, err = json.Marshal(legacy)
		if err != nil {
			return nil, fmt.Errorf("%w: encode legacy prompt audit", ErrModerationArchiveIntegrity)
		}
		exported["legacy_prompt_audit"] = legacyRaw
	}

	download, err := json.MarshalIndent(exported, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: encode archive export", ErrModerationArchiveIntegrity)
	}
	return append(download, '\n'), nil
}

func contentModerationReadableJSON(raw []byte) json.RawMessage {
	if json.Valid(raw) {
		return append(json.RawMessage(nil), raw...)
	}
	encoded, _ := json.Marshal(string(raw))
	return encoded
}

func (s *ContentModerationService) DownloadArchive(ctx context.Context, logID, actorUserID int64, requestID string) ([]byte, error) {
	archiveRaw, repo, err := s.decryptContentModerationArchive(ctx, logID, actorUserID, requestID, "download")
	if err != nil {
		return nil, err
	}
	raw, err := contentModerationArchiveDownload(archiveRaw)
	if err != nil {
		s.recordFailedArchiveAccess(ctx, repo, logID, actorUserID, "download", requestID, err)
		return nil, err
	}
	if err := repo.RecordArchiveAccess(ctx, ContentModerationArchiveAccess{
		LogID: logID, ActorUserID: actorUserID, Action: "download", RequestID: requestID,
		Result: "success", BytesServed: int64(len(raw)),
	}); err != nil {
		return nil, fmt.Errorf("record content moderation archive download before serving: %w", err)
	}
	return raw, nil
}

func (s *ContentModerationService) decryptContentModerationArchive(ctx context.Context, logID, actorUserID int64, requestID, action string) ([]byte, ContentModerationArchiveRepository, error) {
	if logID <= 0 {
		return nil, nil, infraerrors.BadRequest("INVALID_CONTENT_MODERATION_LOG_ID", "风控日志 ID 无效")
	}
	repo, ok := s.repo.(ContentModerationArchiveRepository)
	if !ok || s.archiveRuntime == nil || s.archiveRuntime.cipher == nil {
		return nil, nil, infraerrors.InternalServer("CONTENT_MODERATION_ARCHIVE_UNAVAILABLE", "风控原文归档不可用")
	}
	log, archive, err := repo.GetArchive(ctx, logID)
	if err != nil {
		s.recordFailedArchiveAccess(ctx, repo, logID, actorUserID, action, requestID, err)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, infraerrors.NotFound("CONTENT_MODERATION_ARCHIVE_NOT_FOUND", "风控原文归档不存在")
		}
		return nil, nil, fmt.Errorf("get content moderation archive: %w", err)
	}
	raw, err := s.archiveRuntime.cipher.Decrypt(archive)
	if err != nil {
		s.recordFailedArchiveAccess(ctx, repo, logID, actorUserID, action, requestID, err)
		return nil, nil, fmt.Errorf("decrypt content moderation archive: %w", err)
	}
	if log != nil && log.ArchiveBytes > 0 && log.ArchiveBytes != int64(len(raw)) {
		err = ErrModerationArchiveIntegrity
		s.recordFailedArchiveAccess(ctx, repo, logID, actorUserID, action, requestID, err)
		return nil, nil, err
	}
	return raw, repo, nil
}

func (s *ContentModerationService) recordFailedArchiveAccess(ctx context.Context, repo ContentModerationArchiveRepository, logID, actorUserID int64, action, requestID string, cause error) {
	if repo == nil {
		return
	}
	detail := boundedModerationArchiveError(cause)
	if err := repo.RecordArchiveAccess(ctx, ContentModerationArchiveAccess{
		LogID: logID, ActorUserID: actorUserID, Action: action, RequestID: requestID,
		Result: "failed", Detail: detail,
	}); err != nil {
		slog.Error("content_moderation.archive_failure_audit_failed", "log_id", logID, "action", action, "error", err)
	}
}

func (s *ContentModerationService) DeleteArchive(ctx context.Context, logID, actorUserID int64, requestID string) (bool, error) {
	if logID <= 0 {
		return false, infraerrors.BadRequest("INVALID_CONTENT_MODERATION_LOG_ID", "风控日志 ID 无效")
	}
	repo, ok := s.repo.(ContentModerationArchiveRepository)
	if !ok {
		return false, infraerrors.InternalServer("CONTENT_MODERATION_ARCHIVE_UNAVAILABLE", "风控原文归档不可用")
	}
	log, _, err := repo.GetArchive(ctx, logID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("get archive before deletion: %w", err)
	}
	deleted, err := repo.DeleteArchive(ctx, ContentModerationArchiveAccess{
		LogID: logID, ActorUserID: actorUserID, Action: "delete", RequestID: requestID,
		Result: "success", Detail: "ciphertext removed; summary and deletion audit retained",
	})
	if err != nil {
		return false, err
	}
	if log != nil && strings.TrimSpace(log.ArchiveID) != "" && s.archiveRuntime != nil {
		if err := s.archiveRuntime.RemoveLocalCopies(log.ArchiveID); err != nil {
			return true, fmt.Errorf("database archive deleted but local retry cleanup failed: %w", err)
		}
	}
	return deleted, nil
}

func (s *ContentModerationService) GetStatus(ctx context.Context) (*ContentModerationRuntimeStatus, error) {
	if s == nil {
		return &ContentModerationRuntimeStatus{}, nil
	}
	s.ensurePendingBodyBudget()
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	riskEnabled := s.isRiskControlEnabled(ctx)
	preBlockActive := int(s.preBlockActive.Load())
	if preBlockActive < 0 {
		preBlockActive = 0
	}
	preBlockChecked := s.preBlockChecked.Load()
	preBlockAvgLatency := int64(0)
	if preBlockChecked > 0 {
		preBlockAvgLatency = s.preBlockLatencyTotalMS.Load() / preBlockChecked
	}
	var flaggedHashCount int64
	if s.hashCache != nil {
		if fragmentCache, ok := s.hashCache.(ContentModerationFragmentCache); ok {
			if n, err := fragmentCache.CountFragmentResults(ctx, cfg.fragmentCacheNamespace()); err == nil {
				flaggedHashCount = n
			} else {
				slog.Warn("content_moderation.fragment_count_failed", "error", err)
			}
		} else if n, err := s.hashCache.CountFlaggedInputHashes(ctx); err == nil {
			flaggedHashCount = n
		} else {
			slog.Warn("content_moderation.hash_count_failed", "error", err)
		}
	}
	var lastCleanupAt *time.Time
	if unix := s.lastCleanupUnix.Load(); unix > 0 {
		t := time.Unix(unix, 0)
		lastCleanupAt = &t
	}
	pendingBodyBudgetBytes := s.pendingBodyBudgetBytes.Load()
	if pendingBodyBudgetBytes <= 0 {
		pendingBodyBudgetBytes = DefaultContentModerationPendingBodyBudgetBytes
	}
	return &ContentModerationRuntimeStatus{
		Enabled:                      cfg.Enabled,
		RiskControlEnabled:           riskEnabled,
		Mode:                         cfg.Mode,
		PreBlockActive:               preBlockActive,
		PreBlockChecked:              preBlockChecked,
		PreBlockAllowed:              s.preBlockAllowed.Load(),
		PreBlockBlocked:              s.preBlockBlocked.Load(),
		PreBlockErrors:               s.preBlockErrors.Load(),
		PreBlockAvgLatencyMS:         preBlockAvgLatency,
		PreBlockAPIKeyActive:         s.preBlockAPIKeyActive(cfg.apiKeys()),
		PreBlockAPIKeyAvailableCount: s.preBlockAPIKeyAvailableCount(cfg.apiKeys()),
		PreBlockAPIKeyTotalCalls:     s.preBlockAPIKeyTotalCalls(cfg.apiKeys()),
		PreBlockAPIKeyLoads:          s.preBlockAPIKeyLoads(cfg.apiKeys()),
		APIKeyStatuses:               s.apiKeyStatuses(cfg.apiKeys()),
		FlaggedHashCount:             flaggedHashCount,
		LastCleanupAt:                lastCleanupAt,
		LastCleanupDeletedHit:        s.lastCleanupDeletedHit.Load(),
		LastCleanupDeletedNonHit:     s.lastCleanupDeletedNonHit.Load(),
		PendingBodyBytes:             s.pendingBodyBudget.InUse(),
		PendingBodyMaxSeen:           s.pendingBodyBudget.MaxSeen(),
		PendingBodyBudgetBytes:       pendingBodyBudgetBytes,
		PendingBodyRejections:        s.pendingBodyBudget.Rejections(),
		ObservedRequestBodyMax:       s.observedRequestBodyMax.Load(),
		RequestBodyHistogram:         s.contentModerationBodySizeHistogram(),
		FragmentCacheHits:            s.fragmentCacheHits.Load(),
		FragmentCacheMisses:          s.fragmentCacheMisses.Load(),
		FragmentCacheExpired:         s.fragmentCacheExpired.Load(),
		FragmentCacheReplays:         s.fragmentCacheReplays.Load(),
		FragmentCacheErrors:          s.fragmentCacheErrors.Load(),
		FragmentCacheWrites:          s.fragmentCacheWrites.Load(),
		FragmentCacheWriteErrors:     s.fragmentCacheWriteErrors.Load(),
		SecondLayerMetrics:           s.contentModerationSecondLayerMetrics(),
		SecondLayerShadowQueued:      s.secondLayerShadowQueued.Load(),
		SecondLayerShadowDropped:     s.secondLayerShadowDropped.Load(),
		SecondLayerShadowCompleted:   s.secondLayerShadowDone.Load(),
		SecondLayerShadowQueueDepth:  s.contentModerationShadowQueueDepth(),
		ArchiveRuntime:               s.archiveRuntime.Status(),
	}, nil
}

func (s *ContentModerationService) cleanupWorker() {
	timer := time.NewTimer(contentModerationCleanupDelay)
	defer timer.Stop()
	for {
		<-timer.C
		s.runCleanupOnce()
		timer.Reset(contentModerationCleanupInterval)
	}
}

func (s *ContentModerationService) runCleanupOnce() {
	if s == nil || s.repo == nil || s.settingRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), contentModerationCleanupTimeout)
	defer cancel()
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		slog.Warn("content_moderation.cleanup_load_config_failed", "error", err)
		return
	}
	now := time.Now()
	hitBefore := now.AddDate(0, 0, -cfg.HitRetentionDays)
	nonHitBefore := now.AddDate(0, 0, -cfg.NonHitRetentionDays)
	result, err := s.repo.CleanupExpiredLogs(ctx, hitBefore, nonHitBefore)
	if err != nil {
		slog.Warn("content_moderation.cleanup_failed", "error", err)
		return
	}
	if result == nil {
		return
	}
	s.lastCleanupUnix.Store(result.FinishedAt.Unix())
	s.lastCleanupDeletedHit.Store(result.DeletedHit)
	s.lastCleanupDeletedNonHit.Store(result.DeletedNonHit)
}

func (s *ContentModerationService) loadConfig(ctx context.Context) (*ContentModerationConfig, error) {
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyContentModerationConfig)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return parseContentModerationConfig("")
		}
		return nil, fmt.Errorf("get content moderation config: %w", err)
	}
	return parseContentModerationConfig(raw)
}

func parseContentModerationConfig(raw string) (*ContentModerationConfig, error) {
	cfg := defaultContentModerationConfig()
	if strings.TrimSpace(raw) == "" {
		cfg.normalize()
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		return nil, infraerrors.BadRequest("INVALID_CONTENT_MODERATION_CONFIG", "内容审计配置不是有效 JSON")
	}
	var storedPolicy struct {
		FragmentTTLPolicyVersion *string `json:"fragment_ttl_policy_version"`
	}
	if err := json.Unmarshal([]byte(raw), &storedPolicy); err == nil && storedPolicy.FragmentTTLPolicyVersion == nil {
		cfg.FragmentTTLPolicyVersion = contentModerationLegacyFragmentTTLPolicyVersion
	}
	if strings.TrimSpace(cfg.Mode) == legacyContentModerationModeObserve {
		cfg.Mode = ContentModerationModePreBlock
	}
	cfg.normalize()
	return cfg, nil
}

func (s *ContentModerationService) loadRuntimeSnapshot(ctx context.Context) (*contentModerationRuntimeSnapshot, error) {
	if s == nil || s.settingRepo == nil {
		return nil, errors.New("content moderation setting repository unavailable")
	}
	now := time.Now()
	if snapshot := s.runtimeSnapshot.Load(); snapshot != nil {
		if now.Sub(snapshot.loadedAt) < s.runtimeSnapshotTTL() {
			return snapshot, nil
		}
		s.triggerRuntimeSnapshotRefresh()
		return snapshot, nil
	}

	s.runtimeRefreshMu.Lock()
	defer s.runtimeRefreshMu.Unlock()
	if snapshot := s.runtimeSnapshot.Load(); snapshot != nil {
		return snapshot, nil
	}
	return s.refreshRuntimeSnapshot(ctx)
}

func (s *ContentModerationService) runtimeSnapshotTTL() time.Duration {
	if s != nil && s.runtimeCacheTTL > 0 {
		return s.runtimeCacheTTL
	}
	return contentModerationRuntimeCacheTTL
}

func (s *ContentModerationService) triggerRuntimeSnapshotRefresh() {
	if s == nil || s.runtimeRefreshDeferred() || !s.runtimeRefreshMu.TryLock() {
		return
	}
	if s.runtimeRefreshDeferred() {
		s.runtimeRefreshMu.Unlock()
		return
	}
	go func() {
		defer s.runtimeRefreshMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), contentModerationRuntimeRefreshTimeout)
		defer cancel()
		if _, err := s.refreshRuntimeSnapshot(ctx); err != nil {
			s.runtimeRefreshRetryAt.Store(time.Now().Add(s.runtimeSnapshotTTL()).UnixNano())
			slog.Warn("content_moderation.runtime_snapshot_refresh_failed", "error", err)
		}
	}()
}

func (s *ContentModerationService) runtimeRefreshDeferred() bool {
	if s == nil {
		return false
	}
	return time.Now().UnixNano() < s.runtimeRefreshRetryAt.Load()
}

func (s *ContentModerationService) refreshRuntimeSnapshot(ctx context.Context) (*contentModerationRuntimeSnapshot, error) {
	values, err := s.settingRepo.GetMultiple(ctx, []string{
		SettingKeyRiskControlEnabled,
		SettingKeyContentModerationConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("get content moderation runtime settings: %w", err)
	}
	rawConfig := values[SettingKeyContentModerationConfig]
	configDigest := sha256.Sum256([]byte(rawConfig))
	if current := s.runtimeSnapshot.Load(); current != nil && current.configDigest == configDigest {
		snapshot := &contentModerationRuntimeSnapshot{
			riskControlEnabled:          values[SettingKeyRiskControlEnabled] == "true",
			config:                      current.config,
			keywordMatcher:              current.keywordMatcher,
			unconditionalKeywordMatcher: current.unconditionalKeywordMatcher,
			contextualKeywordMatcher:    current.contextualKeywordMatcher,
			secondLayerPrefilterMatcher: current.secondLayerPrefilterMatcher,
			fragmentCacheNamespace:      current.fragmentCacheNamespace,
			configDigest:                configDigest,
			loadedAt:                    time.Now(),
		}
		s.runtimeSnapshot.Store(snapshot)
		s.runtimeRefreshRetryAt.Store(0)
		return snapshot, nil
	}
	cfg, err := parseContentModerationConfig(rawConfig)
	if err != nil {
		return nil, err
	}
	effectiveKeywords, err := effectiveContentModerationKeywords(cfg)
	if err != nil {
		if s.runtimeSnapshot.Load() != nil {
			return nil, err
		}
		// Legacy configs can reference an asset that is no longer available.
		// Preserve their explicit local block list while keeping layer two off.
		slog.Error("content_moderation.candidate_asset_unavailable_at_startup", "error", err)
		keywordMatcher, unconditionalKeywordMatcher, contextualKeywordMatcher := newContentModerationRuntimeKeywordMatchers(normalizeBlockedKeywords(cfg.BlockedKeywords))
		snapshot := &contentModerationRuntimeSnapshot{
			riskControlEnabled:          values[SettingKeyRiskControlEnabled] == "true",
			config:                      cfg,
			keywordMatcher:              keywordMatcher,
			unconditionalKeywordMatcher: unconditionalKeywordMatcher,
			contextualKeywordMatcher:    contextualKeywordMatcher,
			fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
			configDigest:                configDigest,
			loadedAt:                    time.Now(),
		}
		s.runtimeSnapshot.Store(snapshot)
		s.runtimeRefreshRetryAt.Store(0)
		return snapshot, nil
	}
	effectiveSecondLayerKeywords, err := effectiveContentModerationSecondLayerKeywords(cfg)
	if err != nil {
		if s.runtimeSnapshot.Load() != nil {
			return nil, err
		}
		// A cold start must not disable deterministic high-confidence blocking
		// because the optional candidate policy is invalid. Keep layer one live,
		// expose layer two as unavailable, and avoid caching candidate decisions.
		slog.Error("content_moderation.candidate_system_unavailable_at_startup", "error", err)
		keywordMatcher, unconditionalKeywordMatcher, contextualKeywordMatcher := newContentModerationRuntimeKeywordMatchers(effectiveKeywords)
		snapshot := &contentModerationRuntimeSnapshot{
			riskControlEnabled:          values[SettingKeyRiskControlEnabled] == "true",
			config:                      cfg,
			keywordMatcher:              keywordMatcher,
			unconditionalKeywordMatcher: unconditionalKeywordMatcher,
			contextualKeywordMatcher:    contextualKeywordMatcher,
			fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
			configDigest:                configDigest,
			loadedAt:                    time.Now(),
		}
		s.runtimeSnapshot.Store(snapshot)
		s.runtimeRefreshRetryAt.Store(0)
		return snapshot, nil
	}
	keywordMatcher, unconditionalKeywordMatcher, contextualKeywordMatcher := newContentModerationRuntimeKeywordMatchers(effectiveKeywords)
	snapshot := &contentModerationRuntimeSnapshot{
		riskControlEnabled:          values[SettingKeyRiskControlEnabled] == "true",
		config:                      cfg,
		keywordMatcher:              keywordMatcher,
		unconditionalKeywordMatcher: unconditionalKeywordMatcher,
		contextualKeywordMatcher:    contextualKeywordMatcher,
		secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher(effectiveSecondLayerKeywords),
		fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
		configDigest:                configDigest,
		loadedAt:                    time.Now(),
	}
	s.runtimeSnapshot.Store(snapshot)
	s.runtimeRefreshRetryAt.Store(0)
	return snapshot, nil
}

func (s *ContentModerationService) replaceRuntimeConfig(cfg *ContentModerationConfig, raw []byte) {
	if s == nil || cfg == nil {
		return
	}
	s.runtimeRefreshMu.Lock()
	hasSnapshot := s.runtimeSnapshot.Load() != nil
	s.runtimeRefreshMu.Unlock()
	if !hasSnapshot {
		return
	}
	config := cloneContentModerationConfig(cfg)
	effectiveKeywords, err := effectiveContentModerationKeywords(cfg)
	if err != nil {
		slog.Error("content_moderation.candidate_asset_invalid_after_validation", "error", err)
		return
	}
	effectiveSecondLayerKeywords, err := effectiveContentModerationSecondLayerKeywords(cfg)
	if err != nil {
		slog.Error("content_moderation.candidate_asset_invalid_after_validation", "error", err)
		return
	}
	keywordMatcher, unconditionalKeywordMatcher, contextualKeywordMatcher := newContentModerationRuntimeKeywordMatchers(effectiveKeywords)
	secondLayerPrefilterMatcher := newContentModerationPrefilterMatcher(effectiveSecondLayerKeywords)
	fragmentCacheNamespace := cfg.fragmentCacheNamespace()
	configDigest := sha256.Sum256(raw)

	s.runtimeRefreshMu.Lock()
	defer s.runtimeRefreshMu.Unlock()
	current := s.runtimeSnapshot.Load()
	if current == nil {
		return
	}
	s.runtimeSnapshot.Store(&contentModerationRuntimeSnapshot{
		riskControlEnabled:          current.riskControlEnabled,
		config:                      config,
		keywordMatcher:              keywordMatcher,
		unconditionalKeywordMatcher: unconditionalKeywordMatcher,
		contextualKeywordMatcher:    contextualKeywordMatcher,
		secondLayerPrefilterMatcher: secondLayerPrefilterMatcher,
		fragmentCacheNamespace:      fragmentCacheNamespace,
		configDigest:                configDigest,
		loadedAt:                    time.Now(),
	})
}

func newContentModerationRuntimeKeywordMatchers(keywords []string) (*contentModerationKeywordMatcher, *contentModerationKeywordMatcher, *contentModerationKeywordMatcher) {
	unconditional := make([]string, 0, len(keywords))
	contextual := make([]string, 0, len(maliciousMacroContextKeywords))
	for _, keyword := range keywords {
		if _, configured := maliciousMacroContextKeywords[normalizedContentModerationKeywordKey(keyword)]; configured {
			contextual = append(contextual, keyword)
			continue
		}
		unconditional = append(unconditional, keyword)
	}
	return newContentModerationKeywordMatcher(keywords),
		newContentModerationKeywordMatcher(unconditional),
		newContentModerationKeywordMatcher(contextual)
}

func (s *contentModerationRuntimeSnapshot) matchBlockedKeyword(text string) (string, bool) {
	if s == nil || s.config == nil {
		return "", false
	}
	if s.keywordMatcher != nil {
		return s.keywordMatcher.Match(text)
	}
	return matchBlockedKeyword(text, s.config.BlockedKeywords)
}

func (s *ContentModerationService) isRiskControlEnabled(ctx context.Context) bool {
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyRiskControlEnabled)
	if err != nil {
		return false
	}
	return raw == "true"
}

// IsUserEmailWhitelisted reports whether an exact user email uses local
// shadow-only moderation. Matching is case-insensitive.
func (s *ContentModerationService) IsUserEmailWhitelisted(ctx context.Context, email string) (bool, error) {
	if strings.TrimSpace(email) == "" {
		return false, nil
	}
	runtimeSnapshot, err := s.loadRuntimeSnapshot(ctx)
	if err != nil {
		return false, err
	}
	return runtimeSnapshot != nil && runtimeSnapshot.config != nil && runtimeSnapshot.config.includesUserEmail(email), nil
}

func (s *ContentModerationService) validateConfig(ctx context.Context, cfg *ContentModerationConfig) error {
	if err := s.validateUnifiedConfig(cfg); err != nil {
		return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_SECOND_LAYER", err.Error())
	}
	if cfg == nil {
		return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_CONFIG", "内容审计配置不能为空")
	}
	cfg.normalize()
	if _, err := contentmoderationassets.Load(cfg.CandidateAsset); err != nil {
		return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_CANDIDATE_ASSET", err.Error())
	}
	if _, err := effectiveContentModerationKeywords(cfg); err != nil {
		return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_CANDIDATE_ASSET", err.Error())
	}
	if _, err := effectiveContentModerationSecondLayerKeywords(cfg); err != nil {
		return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_CANDIDATE_SYSTEM", err.Error())
	}
	if err := validateContentModerationMode(cfg.Mode); err != nil {
		return err
	}
	if _, err := url.ParseRequestURI(cfg.BaseURL); err != nil {
		return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_BASE_URL", "OpenAI Base URL 无效")
	}
	if cfg.ProxyID != nil && s.proxyRepo != nil {
		if _, err := s.proxyRepo.GetByID(ctx, *cfg.ProxyID); err != nil {
			return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_PROXY", fmt.Sprintf("代理服务器不存在: %d", *cfg.ProxyID))
		}
	}
	if cfg.BlockStatus < 400 || cfg.BlockStatus > 599 {
		return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_BLOCK_STATUS", "拦截 HTTP 状态码必须在 400-599 之间")
	}
	if cfg.ModelFilter.Type != ContentModerationModelFilterAll && len(cfg.ModelFilter.Models) == 0 {
		return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_MODEL_FILTER", "指定或排除模型时至少需要配置 1 个模型")
	}
	if len(cfg.UserEmailWhitelist) > maxContentModerationUserEmailWhitelist {
		return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_USER_EMAIL_WHITELIST", fmt.Sprintf("用户邮箱白名单最多配置 %d 项", maxContentModerationUserEmailWhitelist))
	}
	for _, email := range cfg.UserEmailWhitelist {
		if len([]rune(email)) > maxContentModerationUserEmailRunes {
			return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_USER_EMAIL_WHITELIST", fmt.Sprintf("用户邮箱白名单地址过长: %s", email))
		}
		address, err := mail.ParseAddress(email)
		if err != nil || !strings.EqualFold(address.Address, email) {
			return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_USER_EMAIL_WHITELIST", fmt.Sprintf("用户邮箱白名单地址无效: %s", email))
		}
	}
	if !cfg.AllGroups && len(cfg.GroupIDs) > 0 && s.groupRepo != nil {
		for _, groupID := range cfg.GroupIDs {
			if _, err := s.groupRepo.GetByIDLite(ctx, groupID); err != nil {
				return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_GROUP", fmt.Sprintf("审计分组不存在: %d", groupID))
			}
		}
	}
	return nil
}

func validateContentModerationMode(mode string) error {
	switch strings.TrimSpace(mode) {
	case ContentModerationModeOff, ContentModerationModePreBlock:
		return nil
	default:
		return infraerrors.BadRequest("INVALID_CONTENT_MODERATION_MODE", "内容审计模式仅支持 off 或 pre_block")
	}
}

func (s *ContentModerationService) callModeration(ctx context.Context, cfg *ContentModerationConfig, input any, trackKeyLoad ...bool) (*moderationAPIResult, error) {
	attempts := cfg.RetryCount + 1
	if attempts <= 0 {
		attempts = 1
	}
	if attempts > maxContentModerationRetryCount+1 {
		attempts = maxContentModerationRetryCount + 1
	}
	trackLoad := len(trackKeyLoad) > 0 && trackKeyLoad[0]
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		key, ok := s.nextUsableAPIKey(cfg)
		if !ok {
			lastErr = errors.New("no moderation api key available")
			break
		}
		if trackLoad {
			s.beginModerationAPIKeyCall(key)
		}
		start := time.Now()
		httpStatus := 0
		result, err := s.callModerationOnceWithInput(ctx, cfg, key, input, &httpStatus)
		latency := int(time.Since(start).Milliseconds())
		if err == nil {
			if trackLoad {
				s.finishModerationAPIKeyCall(key, latency, true)
			}
			s.markAPIKeySuccess(key, latency, httpStatus)
			return result, nil
		}
		if trackLoad {
			s.finishModerationAPIKeyCall(key, latency, false)
		}
		s.markAPIKeyError(key, err.Error(), latency, httpStatus)
		lastErr = err
		if httpStatus == http.StatusBadRequest {
			break
		}
		if attempt == attempts-1 {
			break
		}
		wait := time.Duration(100*(attempt+1)) * time.Millisecond
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

func (s *ContentModerationService) callModerationOnceWithInput(ctx context.Context, cfg *ContentModerationConfig, apiKey string, input any, httpStatus *int) (*moderationAPIResult, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	endpoint, err := url.JoinPath(base, "/v1/moderations")
	if err != nil {
		return nil, err
	}
	payload := moderationAPIRequest{
		Model: cfg.Model,
		Input: input,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client, err := s.moderationHTTPClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if httpStatus != nil {
		*httpStatus = resp.StatusCode
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("moderation api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out moderationAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Results) == 0 {
		return nil, errors.New("moderation api returned empty results")
	}
	return &out.Results[0], nil
}

// moderationProxyURLCacheEntry 缓存 proxy_id 到代理 URL 的解析结果，
// 避免审计热路径上每次调用都查询数据库。
type moderationProxyURLCacheEntry struct {
	proxyID   int64
	url       string
	expiresAt time.Time
}

const contentModerationProxyURLCacheTTL = time.Minute

// moderationHTTPClient 返回本次审计调用应使用的 HTTP 客户端。
// 未配置代理时沿用默认客户端；配置了代理时通过共享客户端池构建，
// 代理解析/构建失败直接返回错误，绝不回退直连（避免 IP 关联风险）。
func (s *ContentModerationService) moderationHTTPClient(ctx context.Context, cfg *ContentModerationConfig) (*http.Client, error) {
	if cfg == nil || cfg.ProxyID == nil {
		if s.httpClient == nil {
			return http.DefaultClient, nil
		}
		return s.httpClient, nil
	}
	proxyURL, err := s.resolveModerationProxyURL(ctx, *cfg.ProxyID)
	if err != nil {
		return nil, err
	}
	client, err := httpclient.GetClient(httpclient.Options{ProxyURL: proxyURL})
	if err != nil {
		return nil, fmt.Errorf("build moderation proxy client: %w", err)
	}
	return client, nil
}

func (s *ContentModerationService) resolveModerationProxyURL(ctx context.Context, proxyID int64) (string, error) {
	now := time.Now()
	prev := s.moderationProxyCache.Load()
	if prev != nil && prev.proxyID == proxyID && now.Before(prev.expiresAt) {
		return prev.url, nil
	}
	if s.proxyRepo == nil {
		return "", errors.New("moderation proxy repository unavailable")
	}
	px, err := s.proxyRepo.GetByID(ctx, proxyID)
	if err != nil {
		return "", fmt.Errorf("resolve moderation proxy %d: %w", proxyID, err)
	}
	if !px.IsActive() || px.IsExpired(now) {
		slog.Warn("content_moderation.proxy_not_active",
			"proxy_id", proxyID,
			"proxy_name", px.Name,
			"status", px.Status,
			"expired", px.IsExpired(now))
	}
	proxyURL := px.URL()
	if prev == nil || prev.proxyID != proxyID || prev.url != proxyURL {
		// 不打印完整 URL（可能含认证信息），仅记录可定位的地址。
		slog.Info("content_moderation.proxy_enabled",
			"proxy_id", proxyID,
			"proxy_name", px.Name,
			"proxy_addr", fmt.Sprintf("%s://%s:%d", px.Protocol, px.Host, px.Port))
	}
	s.moderationProxyCache.Store(&moderationProxyURLCacheEntry{
		proxyID:   proxyID,
		url:       proxyURL,
		expiresAt: now.Add(contentModerationProxyURLCacheTTL),
	})
	return proxyURL, nil
}

func (s *ContentModerationService) buildLog(input ContentModerationCheckInput, cfg *ContentModerationConfig, action string, flagged bool, highestCategory string, highestScore float64, scores map[string]float64, text string, latency *int, queueDelay *int, errText string) *ContentModerationLog {
	var userID *int64
	if input.UserID > 0 {
		userID = &input.UserID
	}
	var apiKeyID *int64
	if input.APIKeyID > 0 {
		apiKeyID = &input.APIKeyID
	}
	transport := strings.TrimSpace(input.RawRequest.Transport)
	if transport == "" {
		transport = "http"
	}
	stage := strings.TrimSpace(input.RawRequest.Stage)
	if stage == "" {
		stage = "http"
	}
	return &ContentModerationLog{
		RequestID:         input.RequestID,
		UserID:            userID,
		UserEmail:         input.UserEmail,
		APIKeyID:          apiKeyID,
		APIKeyName:        input.APIKeyName,
		GroupID:           cloneInt64Ptr(input.GroupID),
		GroupName:         input.GroupName,
		Endpoint:          input.Endpoint,
		Provider:          input.Provider,
		Model:             input.Model,
		Mode:              cfg.Mode,
		Action:            action,
		Flagged:           flagged,
		HighestCategory:   highestCategory,
		HighestScore:      highestScore,
		CategoryScores:    cloneFloatMap(scores),
		ThresholdSnapshot: cloneFloatMap(cfg.Thresholds),
		InputExcerpt:      trimRunes(redactContentModerationSecrets(text), maxModerationExcerptRunes),
		UpstreamLatencyMS: latency,
		QueueDelayMS:      queueDelay,
		Error:             errText,
		Protocol:          input.Protocol,
		Transport:         transport,
		RequestStage:      stage,
		RequestTarget:     input.RawRequest.Target,
		ArchiveStatus:     ContentModerationArchiveStatusNone,
	}
}

func (s *ContentModerationService) persistContentModerationLog(ctx context.Context, cfg *ContentModerationConfig, log *ContentModerationLog, hashText string, recordHash bool, applySideEffects bool) {
	s.persistContentModerationLogWithInput(ctx, cfg, log, hashText, recordHash, applySideEffects, nil)
}

func (s *ContentModerationService) persistContentModerationLogWithInput(ctx context.Context, cfg *ContentModerationConfig, log *ContentModerationLog, hashText string, recordHash bool, applySideEffects bool, input *ContentModerationCheckInput) bool {
	if s == nil || log == nil {
		return false
	}
	log.InputHash = hashText
	isReplay := log.CacheHit || log.Action == ContentModerationActionCacheBlock || log.DecisionSource == "cache_replay"
	if isReplay {
		recordHash = false
		applySideEffects = false
		log.CacheHit = true
		log.DecisionSource = "cache_replay"
		log.ViolationCount = 0
		log.DispositionStatus = "not_counted"
		log.AutoBanned = false
		log.EmailSent = false
	}
	if recordHash && s.hashCache != nil {
		if err := s.hashCache.RecordFlaggedInputHash(ctx, hashText); err != nil {
			slog.Warn("content_moderation.record_hash_failed", "user_id", contentModerationEmailUserID(log), "endpoint", log.Endpoint, "error", err)
		}
	}
	var dispositionErr error
	autoBanJustApplied := false
	if applySideEffects {
		role := ""
		if input != nil {
			role = input.UserRole
		}
		autoBanJustApplied, dispositionErr = s.applyFlaggedAccountSideEffectsWithRole(ctx, cfg, log, role)
		if dispositionErr != nil {
			log.DispositionStatus = "retry_required"
			log.Error = appendContentModerationError(log.Error, "disposition_error", dispositionErr)
			slog.Error("content_moderation.local_disposition_failed", "user_id", contentModerationEmailUserID(log), "action", log.Action, "error", dispositionErr)
		}
	}
	var archiveErr error
	persisted := false
	if !isReplay && input != nil && isSevereContentModerationAction(log.Action) && s.archiveRuntime != nil {
		archiveErr = s.persistContentModerationArchive(ctx, log, *input)
		if archiveErr != nil {
			slog.Error("content_moderation.archive_persist_deferred", "user_id", contentModerationEmailUserID(log), "endpoint", log.Endpoint, "action", log.Action, "archive_status", log.ArchiveStatus, "error", archiveErr)
		} else {
			persisted = true
		}
	} else if s.repo != nil {
		if err := s.repo.CreateLog(ctx, log); err != nil {
			slog.Warn("content_moderation.create_log_failed", "user_id", contentModerationEmailUserID(log), "endpoint", log.Endpoint, "action", log.Action, "error", err)
		} else {
			persisted = true
		}
	}

	notificationConfigured := cfg != nil && cfg.EmailOnHit && s.emailService != nil && strings.TrimSpace(log.UserEmail) != ""
	emailEnabled := autoBanJustApplied && notificationConfigured
	emailRequired := emailEnabled
	emailCompletionRequired := false
	var emailDeliveryErr, emailStateErr error
	if emailEnabled {
		outcome := s.deliverClaimedContentModerationEmail(ctx, log, func() error {
			return s.sendAccountDisabledEmail(ctx, cfg, log)
		})
		log.EmailSent = outcome.Sent
		emailRequired = outcome.SendRequired
		emailCompletionRequired = outcome.CompletionRequired
		emailDeliveryErr = outcome.DeliveryErr
		emailStateErr = outcome.StateErr
		if emailDeliveryErr != nil {
			slog.Warn("content_moderation.ban_email_delivery_failed", "user_id", contentModerationEmailUserID(log), "recipient_hash", notificationEmailHash(log.UserEmail), "error", emailDeliveryErr)
		}
		if emailStateErr != nil {
			slog.Error("content_moderation.ban_email_state_failed", "user_id", contentModerationEmailUserID(log), "recipient_hash", notificationEmailHash(log.UserEmail), "error", emailStateErr)
		}
	}

	needsDispositionRetry := dispositionErr != nil || emailStateErr != nil
	if needsDispositionRetry && input != nil && s.archiveRuntime != nil {
		cause := errors.Join(dispositionErr, emailStateErr, archiveErr)
		if err := s.archiveRuntime.QueueLocalDispositionRetry(*input, cfg, log, dispositionErr == nil, notificationConfigured, emailRequired, emailCompletionRequired, log.EmailSent, cause); err != nil {
			slog.Error("content_moderation.local_disposition_retry_persist_failed", "user_id", input.UserID, "action", log.Action, "error", err)
		}
	}
	return persisted
}

func (s *ContentModerationService) applyFlaggedAccountSideEffects(ctx context.Context, cfg *ContentModerationConfig, log *ContentModerationLog) bool {
	transitioned, _ := s.applyFlaggedAccountSideEffectsWithRole(ctx, cfg, log, "")
	return transitioned
}

func (s *ContentModerationService) applyFlaggedAccountSideEffectsWithRole(ctx context.Context, cfg *ContentModerationConfig, log *ContentModerationLog, role string) (bool, error) {
	if s == nil || cfg == nil || log == nil || !log.Flagged || log.UserID == nil || *log.UserID <= 0 {
		return false, nil
	}
	count := 1
	if s.repo != nil && cfg.ViolationWindowHours > 0 {
		since := time.Now().Add(-time.Duration(cfg.ViolationWindowHours) * time.Hour)
		var n int
		var err error
		if counter, ok := s.repo.(ContentModerationDispositionCountRepository); ok {
			n, err = counter.CountFlaggedByUserSinceExcludingArchive(ctx, *log.UserID, since, cfg.CyberPolicyExcludeFromBanCount, log.ArchiveID)
		} else {
			n, err = s.repo.CountFlaggedByUserSince(ctx, *log.UserID, since, cfg.CyberPolicyExcludeFromBanCount)
		}
		if err != nil {
			return false, fmt.Errorf("count local content moderation violations: %w", err)
		}
		count = n + 1
	}
	log.ViolationCount = count
	log.DispositionStatus = "not_required"
	if !cfg.AutoBanEnabled || cfg.BanThreshold <= 0 || count < cfg.BanThreshold {
		return false, nil
	}
	role = strings.TrimSpace(role)
	if s.userRepo != nil {
		user, err := s.userRepo.GetByID(ctx, *log.UserID)
		if err != nil {
			return false, fmt.Errorf("load local content moderation user: %w", err)
		}
		role = user.Role
	}
	if role == RoleAdmin {
		log.DispositionStatus = "skipped_admin"
		log.DispositionTarget = "user"
		slog.Warn("content_moderation.autoban_skipped_admin", "user_id", *log.UserID, "role", role, "count", count, "threshold", cfg.BanThreshold)
		return false, nil
	}
	log.DispositionTarget = "user"
	transitioned, err := s.disableCyberPolicyUser(ctx, *log.UserID)
	if err != nil {
		return false, fmt.Errorf("disable local content moderation user: %w", err)
	}
	log.DispositionTransitioned = transitioned
	log.AutoBanned = transitioned
	if transitioned {
		log.DispositionStatus = "disabled"
		if s.authCacheInvalidator != nil {
			s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, *log.UserID)
		}
	} else {
		log.DispositionStatus = "already_disabled"
	}
	return transitioned, nil
}

func appendContentModerationError(existing, label string, cause error) string {
	if cause == nil {
		return existing
	}
	part := strings.TrimSpace(label) + "=" + redactContentModerationSecrets(cause.Error())
	return trimRunes(strings.TrimSpace(strings.TrimSpace(existing)+"\n"+part), maxModerationErrorRunes)
}

func (s *ContentModerationService) deliverClaimedContentModerationEmail(ctx context.Context, log *ContentModerationLog, send func() error) contentModerationEmailDeliveryOutcome {
	outcome := contentModerationEmailDeliveryOutcome{SendRequired: true}
	if s == nil || log == nil || send == nil {
		outcome.StateErr = errors.New("content moderation email delivery is unavailable")
		return outcome
	}
	repo, ok := s.repo.(ContentModerationEmailDeliveryRepository)
	if !ok {
		outcome.StateErr = errors.New("content moderation email delivery repository is unavailable")
		return outcome
	}
	claim, err := s.claimContentModerationEmailDelivery(ctx, repo, log)
	if err != nil {
		outcome.StateErr = err
		return outcome
	}
	if !claim.Exists {
		outcome.StateErr = sql.ErrNoRows
		return outcome
	}
	if !claim.Claimed {
		outcome.SendRequired = false
		outcome.Sent = claim.Status == "sent"
		log.EmailDeliveryStatus = claim.Status
		return outcome
	}

	outcome.SendRequired = false
	log.EmailDeliveryStatus = "claimed"
	outcome.DeliveryErr = send()
	outcome.Sent = outcome.DeliveryErr == nil
	if err := s.completeContentModerationEmailDelivery(ctx, repo, log, outcome.Sent); err != nil {
		outcome.CompletionRequired = true
		outcome.StateErr = err
		return outcome
	}
	if outcome.Sent {
		log.EmailDeliveryStatus = "sent"
	} else {
		log.EmailDeliveryStatus = "failed"
	}
	return outcome
}

func (s *ContentModerationService) claimContentModerationEmailDelivery(ctx context.Context, repo ContentModerationEmailDeliveryRepository, log *ContentModerationLog) (ContentModerationEmailDeliveryClaim, error) {
	if log.ID > 0 {
		return repo.ClaimLogEmailDelivery(ctx, log.ID)
	}
	if strings.TrimSpace(log.ArchiveID) != "" {
		return repo.ClaimLogEmailDeliveryByArchiveID(ctx, log.ArchiveID)
	}
	return ContentModerationEmailDeliveryClaim{}, sql.ErrNoRows
}

func (s *ContentModerationService) completeContentModerationEmailDelivery(ctx context.Context, repo ContentModerationEmailDeliveryRepository, log *ContentModerationLog, sent bool) error {
	if log.ID > 0 {
		return repo.CompleteLogEmailDelivery(ctx, log.ID, sent)
	}
	if strings.TrimSpace(log.ArchiveID) != "" {
		return repo.CompleteLogEmailDeliveryByArchiveID(ctx, log.ArchiveID, sent)
	}
	return sql.ErrNoRows
}

func (s *ContentModerationService) sendAccountDisabledEmail(ctx context.Context, cfg *ContentModerationConfig, log *ContentModerationLog) error {
	siteName := s.siteName(ctx)
	if s.emailService.notificationEmailService != nil {
		if err := s.emailService.notificationEmailService.Send(ctx, NotificationEmailSendInput{
			Event:          NotificationEmailEventContentModerationDisabled,
			RecipientEmail: log.UserEmail,
			RecipientName:  emailRecipientName(log.UserEmail),
			UserID:         contentModerationEmailUserID(log),
			SourceType:     "content_moderation",
			SourceID:       contentModerationEmailSourceID(log),
			Variables:      contentModerationEmailVariables(log, cfg),
		}); err == nil {
			return nil
		} else {
			if !shouldFallbackNotificationEmail(err) {
				return err
			}
			slog.Warn("template content moderation disabled email failed; falling back to built-in body", "log_id", log.ID, "recipient_hash", notificationEmailHash(log.UserEmail), "err", err.Error())
		}
	}
	subject := fmt.Sprintf("[%s] 账户已被禁用 / Account Disabled", sanitizeEmailHeader(siteName))
	body := buildContentModerationAccountDisabledEmailBody(siteName, log, cfg)
	return s.emailService.SendEmail(ctx, log.UserEmail, subject, body)
}

func contentModerationEmailUserID(log *ContentModerationLog) int64 {
	if log == nil || log.UserID == nil {
		return 0
	}
	return *log.UserID
}

func contentModerationEmailSourceID(log *ContentModerationLog) string {
	if log == nil {
		return ""
	}
	if log.ID > 0 {
		return fmt.Sprintf("%d", log.ID)
	}
	if archiveID := strings.TrimSpace(log.ArchiveID); archiveID != "" {
		return "archive:" + archiveID
	}
	return ""
}

func contentModerationEmailVariables(log *ContentModerationLog, cfg *ContentModerationConfig) map[string]string {
	variables := map[string]string{
		"triggered_at":        time.Now().UTC().Format(time.RFC3339),
		"group_name":          "-",
		"moderation_category": "-",
		"moderation_score":    "0.000",
		"violation_count":     "0",
		"ban_threshold":       "0",
	}
	if log != nil {
		if !log.CreatedAt.IsZero() {
			variables["triggered_at"] = log.CreatedAt.UTC().Format(time.RFC3339)
		}
		if strings.TrimSpace(log.GroupName) != "" {
			variables["group_name"] = strings.TrimSpace(log.GroupName)
		}
		if strings.TrimSpace(log.HighestCategory) != "" {
			variables["moderation_category"] = strings.TrimSpace(log.HighestCategory)
		}
		variables["moderation_score"] = fmt.Sprintf("%.3f", log.HighestScore)
		variables["violation_count"] = fmt.Sprintf("%d", log.ViolationCount)
	}
	if cfg != nil {
		variables["ban_threshold"] = fmt.Sprintf("%d", cfg.BanThreshold)
	}
	return variables
}

func (s *ContentModerationService) siteName(ctx context.Context) string {
	if s == nil || s.settingRepo == nil {
		return "Sub2API"
	}
	name, err := s.settingRepo.GetValue(ctx, SettingKeySiteName)
	if err != nil || strings.TrimSpace(name) == "" {
		return "Sub2API"
	}
	return strings.TrimSpace(name)
}

func defaultContentModerationConfig() *ContentModerationConfig {
	return &ContentModerationConfig{
		Enabled:              false,
		Mode:                 ContentModerationModePreBlock,
		BaseURL:              defaultContentModerationBaseURL,
		Model:                defaultContentModerationModel,
		TimeoutMS:            defaultContentModerationTimeoutMS,
		SampleRate:           100,
		AllGroups:            true,
		GroupIDs:             []int64{},
		UserEmailWhitelist:   []string{},
		RecordNonHits:        false,
		Thresholds:           ContentModerationDefaultThresholds(),
		BlockStatus:          defaultContentModerationBlockHTTPStatus,
		BlockMessage:         defaultContentModerationBlockMessage,
		EmailOnHit:           true,
		AutoBanEnabled:       true,
		BanThreshold:         defaultContentModerationBanThreshold,
		ViolationWindowHours: defaultContentModerationViolationWindowHours,
		RetryCount:           defaultContentModerationRetryCount,
		HitRetentionDays:     defaultContentModerationHitRetentionDays,
		NonHitRetentionDays:  defaultContentModerationNonHitRetentionDays,
		PreHashCheckEnabled:  false,
		BlockedKeywords:      []string{},
		KeywordBlockingMode:  ContentModerationKeywordModeKeywordAndAPI,
		ModelFilter: ContentModerationModelFilter{
			Type:   ContentModerationModelFilterAll,
			Models: []string{},
		},
		CacheVersion:                   defaultContentModerationCacheVersion,
		CacheMaxEntries:                defaultContentModerationCacheMaxEntries,
		CacheMaxBytes:                  defaultContentModerationCacheMaxBytes,
		FragmentBlockTTLSeconds:        DefaultContentModerationFragmentBlockTTLSeconds,
		FragmentAllowTTLSeconds:        DefaultContentModerationFragmentAllowTTLSeconds,
		FragmentTTLPolicyVersion:       ContentModerationFragmentTTLPolicyVersion,
		FirstLayerStage:                ContentModerationFirstLayerStageEnforce,
		SecondLayerEnabled:             false,
		SecondLayerStage:               ContentModerationSecondLayerStageEnforce,
		SecondLayerEndpoints:           []ContentModerationEndpoint{},
		SecondLayerScanners:            []string{},
		HardBlockPatterns:              []string{},
		CandidateKeywords:              []string{},
		KeywordAllowlist:               []string{},
		KeywordPolicyVersion:           ContentModerationKeywordPolicyVersion,
		ContextPolicyVersion:           ContentModerationContextPolicyVersion,
		EvidencePolicyVersion:          ContentModerationEvidencePolicyVersion,
		CandidateAsset:                 "legacy-prompt-audit-v1",
		CandidateEnabled:               false,
		CyberPolicyExcludeFromBanCount: false,
	}
}

func cloneContentModerationConfig(cfg *ContentModerationConfig) *ContentModerationConfig {
	if cfg == nil {
		return nil
	}
	clone := *cfg
	clone.ProxyID = cloneInt64Ptr(cfg.ProxyID)
	clone.APIKeys = append([]string(nil), cfg.APIKeys...)
	clone.GroupIDs = append([]int64(nil), cfg.GroupIDs...)
	clone.UserEmailWhitelist = append([]string(nil), cfg.UserEmailWhitelist...)
	clone.BlockedKeywords = append([]string(nil), cfg.BlockedKeywords...)
	clone.HardBlockPatterns = append([]string(nil), cfg.HardBlockPatterns...)
	clone.CandidateKeywords = append([]string(nil), cfg.CandidateKeywords...)
	clone.KeywordAllowlist = append([]string(nil), cfg.KeywordAllowlist...)
	clone.Thresholds = cloneFloatMap(cfg.Thresholds)
	clone.ModelFilter = ContentModerationModelFilter{
		Type:   cfg.ModelFilter.Type,
		Models: append([]string(nil), cfg.ModelFilter.Models...),
	}
	clone.SecondLayerEndpoints = append([]ContentModerationEndpoint(nil), cfg.SecondLayerEndpoints...)
	for i := range clone.SecondLayerEndpoints {
		clone.SecondLayerEndpoints[i].StopTokens = append([]string(nil), cfg.SecondLayerEndpoints[i].StopTokens...)
	}
	clone.SecondLayerScanners = append([]string(nil), cfg.SecondLayerScanners...)
	return &clone
}

func (cfg *ContentModerationConfig) normalize() {
	if cfg.APIKey != "" {
		cfg.APIKeys = normalizeModerationAPIKeys(append(cfg.APIKeys, cfg.APIKey))
		cfg.APIKey = ""
	} else {
		cfg.APIKeys = normalizeModerationAPIKeys(cfg.APIKeys)
	}
	if cfg.Mode == "" || cfg.Mode == legacyContentModerationModeObserve {
		cfg.Mode = ContentModerationModePreBlock
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultContentModerationBaseURL
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.Model == "" {
		cfg.Model = defaultContentModerationModel
	}
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.ProxyID != nil && *cfg.ProxyID <= 0 {
		cfg.ProxyID = nil
	}
	if cfg.TimeoutMS <= 0 {
		cfg.TimeoutMS = defaultContentModerationTimeoutMS
	}
	if cfg.TimeoutMS > maxContentModerationTimeoutMS {
		cfg.TimeoutMS = maxContentModerationTimeoutMS
	}
	if cfg.SampleRate < 0 {
		cfg.SampleRate = 0
	}
	if cfg.SampleRate > 100 {
		cfg.SampleRate = 100
	}
	if strings.TrimSpace(cfg.BlockMessage) == "" {
		cfg.BlockMessage = defaultContentModerationBlockMessage
	}
	cfg.BlockMessage = strings.TrimSpace(cfg.BlockMessage)
	if cfg.BlockStatus <= 0 {
		cfg.BlockStatus = defaultContentModerationBlockHTTPStatus
	}
	if cfg.BanThreshold <= 0 {
		cfg.BanThreshold = defaultContentModerationBanThreshold
	}
	if cfg.ViolationWindowHours <= 0 {
		cfg.ViolationWindowHours = defaultContentModerationViolationWindowHours
	}
	if cfg.RetryCount < 0 {
		cfg.RetryCount = 0
	}
	if cfg.RetryCount > maxContentModerationRetryCount {
		cfg.RetryCount = maxContentModerationRetryCount
	}
	if cfg.HitRetentionDays <= 0 {
		cfg.HitRetentionDays = defaultContentModerationHitRetentionDays
	}
	if cfg.HitRetentionDays > maxContentModerationRetentionDays {
		cfg.HitRetentionDays = maxContentModerationRetentionDays
	}
	if cfg.NonHitRetentionDays <= 0 {
		cfg.NonHitRetentionDays = defaultContentModerationNonHitRetentionDays
	}
	if cfg.NonHitRetentionDays > maxContentModerationNonHitRetentionDays {
		cfg.NonHitRetentionDays = maxContentModerationNonHitRetentionDays
	}
	cfg.GroupIDs = normalizeInt64IDs(cfg.GroupIDs)
	cfg.UserEmailWhitelist = normalizeContentModerationUserEmailWhitelist(cfg.UserEmailWhitelist)
	cfg.Thresholds = mergeContentModerationThresholds(ContentModerationDefaultThresholds(), cfg.Thresholds)
	cfg.BlockedKeywords = normalizeBlockedKeywords(cfg.BlockedKeywords)
	cfg.KeywordBlockingMode = normalizeKeywordBlockingMode(cfg.KeywordBlockingMode)
	cfg.ModelFilter = normalizeContentModerationModelFilter(cfg.ModelFilter)
	if strings.TrimSpace(cfg.CacheVersion) == "" {
		cfg.CacheVersion = defaultContentModerationCacheVersion
	}
	cfg.CacheVersion = normalizeContentModerationCacheVersion(cfg.CacheVersion)
	if cfg.CacheMaxEntries <= 0 {
		cfg.CacheMaxEntries = defaultContentModerationCacheMaxEntries
	}
	if cfg.CacheMaxEntries > maxContentModerationCacheMaxEntries {
		cfg.CacheMaxEntries = maxContentModerationCacheMaxEntries
	}
	if cfg.CacheMaxBytes <= 0 {
		cfg.CacheMaxBytes = defaultContentModerationCacheMaxBytes
	}
	if cfg.CacheMaxBytes > maxContentModerationCacheMaxBytes {
		cfg.CacheMaxBytes = maxContentModerationCacheMaxBytes
	}
	ttlPolicyVersion := strings.TrimSpace(cfg.FragmentTTLPolicyVersion)
	if ttlPolicyVersion == "" || ttlPolicyVersion == contentModerationLegacyFragmentTTLPolicyVersion {
		cfg.FragmentBlockTTLSeconds = DefaultContentModerationFragmentBlockTTLSeconds
		cfg.FragmentAllowTTLSeconds = DefaultContentModerationFragmentAllowTTLSeconds
		cfg.FragmentTTLPolicyVersion = ContentModerationFragmentTTLPolicyVersion
	}
	if cfg.FragmentBlockTTLSeconds <= 0 {
		cfg.FragmentBlockTTLSeconds = DefaultContentModerationFragmentBlockTTLSeconds
	}
	if cfg.FragmentBlockTTLSeconds < MinContentModerationFragmentBlockTTLSeconds {
		cfg.FragmentBlockTTLSeconds = MinContentModerationFragmentBlockTTLSeconds
	}
	if cfg.FragmentBlockTTLSeconds > MaxContentModerationFragmentBlockTTLSeconds {
		cfg.FragmentBlockTTLSeconds = MaxContentModerationFragmentBlockTTLSeconds
	}
	if cfg.FragmentAllowTTLSeconds <= 0 {
		cfg.FragmentAllowTTLSeconds = DefaultContentModerationFragmentAllowTTLSeconds
	}
	if cfg.FragmentAllowTTLSeconds > MaxContentModerationFragmentAllowTTLSeconds {
		cfg.FragmentAllowTTLSeconds = MaxContentModerationFragmentAllowTTLSeconds
	}
	cfg.FragmentTTLPolicyVersion = normalizeContentModerationCacheVersion(cfg.FragmentTTLPolicyVersion)
	cfg.SecondLayerEndpoints = normalizeContentModerationEndpoints(cfg.SecondLayerEndpoints)
	cfg.FirstLayerStage = normalizeContentModerationFirstLayerStage(cfg.FirstLayerStage)
	cfg.SecondLayerStage = normalizeContentModerationSecondLayerStage(cfg.SecondLayerStage)
	cfg.SecondLayerScanners = normalizeContentModerationScannerIDs(cfg.SecondLayerScanners)
	cfg.HardBlockPatterns = normalizeBlockedKeywords(cfg.HardBlockPatterns)
	cfg.CandidateKeywords = normalizeBlockedKeywords(cfg.CandidateKeywords)
	cfg.KeywordAllowlist = normalizeBlockedKeywords(cfg.KeywordAllowlist)
	if keywordPolicyVersion := strings.TrimSpace(cfg.KeywordPolicyVersion); keywordPolicyVersion == "" || keywordPolicyVersion == contentModerationPreviousKeywordPolicyVersion {
		cfg.KeywordPolicyVersion = ContentModerationKeywordPolicyVersion
	}
	if contextPolicyVersion := strings.TrimSpace(cfg.ContextPolicyVersion); contextPolicyVersion == "" || contextPolicyVersion == contentModerationLegacyContextPolicyVersion || contextPolicyVersion == contentModerationPreviousContextPolicyVersion {
		cfg.ContextPolicyVersion = ContentModerationContextPolicyVersion
	}
	if evidencePolicyVersion := strings.TrimSpace(cfg.EvidencePolicyVersion); evidencePolicyVersion == "" || evidencePolicyVersion == contentModerationLegacyEvidencePolicyVersion || evidencePolicyVersion == contentModerationOlderEvidencePolicyVersion || evidencePolicyVersion == contentModerationPreviousEvidencePolicyVersion {
		cfg.EvidencePolicyVersion = ContentModerationEvidencePolicyVersion
	}
	cfg.KeywordPolicyVersion = normalizeContentModerationCacheVersion(cfg.KeywordPolicyVersion)
	cfg.ContextPolicyVersion = normalizeContentModerationCacheVersion(cfg.ContextPolicyVersion)
	cfg.EvidencePolicyVersion = normalizeContentModerationCacheVersion(cfg.EvidencePolicyVersion)
	cfg.CandidateAsset = strings.TrimSpace(cfg.CandidateAsset)
	if cfg.CandidateAsset == "" {
		cfg.CandidateAsset = "legacy-prompt-audit-v1"
	}
}

func (cfg *ContentModerationConfig) includesGroup(groupID *int64) bool {
	if cfg.AllGroups {
		return true
	}
	if groupID == nil {
		return false
	}
	for _, id := range cfg.GroupIDs {
		if id == *groupID {
			return true
		}
	}
	return false
}

func (cfg *ContentModerationConfig) includesUserEmail(email string) bool {
	if cfg == nil {
		return false
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	for _, allowed := range cfg.UserEmailWhitelist {
		if email == strings.ToLower(strings.TrimSpace(allowed)) {
			return true
		}
	}
	return false
}

func (cfg *ContentModerationConfig) includesModel(model string) bool {
	if cfg == nil {
		return true
	}
	filter := normalizeContentModerationModelFilter(cfg.ModelFilter)
	switch filter.Type {
	case ContentModerationModelFilterInclude:
		return contentModerationModelListContains(filter.Models, model)
	case ContentModerationModelFilterExclude:
		return !contentModerationModelListContains(filter.Models, model)
	default:
		return true
	}
}

func contentModerationLogGroupID(groupID *int64) int64 {
	if groupID == nil {
		return 0
	}
	return *groupID
}

func (cfg *ContentModerationConfig) shouldSample(hashText string) bool {
	if cfg.SampleRate >= 100 {
		return true
	}
	if cfg.SampleRate <= 0 {
		return false
	}
	raw, err := hex.DecodeString(hashText)
	if err != nil || len(raw) < 2 {
		return true
	}
	return int(binary.BigEndian.Uint16(raw[:2])%100) < cfg.SampleRate
}

func (cfg *ContentModerationConfig) apiKeys() []string {
	if cfg == nil {
		return nil
	}
	return normalizeModerationAPIKeys(cfg.APIKeys)
}

func (s *ContentModerationService) nextUsableAPIKey(cfg *ContentModerationConfig) (string, bool) {
	keys := cfg.apiKeys()
	if len(keys) == 0 {
		return "", false
	}
	now := time.Now()
	for i := 0; i < len(keys); i++ {
		idx := int(s.apiKeyCursor.Add(1)-1) % len(keys)
		key := keys[idx]
		if !s.isAPIKeyFrozen(key, now) {
			return key, true
		}
	}
	return "", false
}

func (s *ContentModerationService) isAPIKeyFrozen(key string, now time.Time) bool {
	hash := moderationAPIKeyHash(key)
	if hash == "" || s == nil {
		return false
	}
	s.keyHealthMu.Lock()
	defer s.keyHealthMu.Unlock()
	state := s.keyHealth[hash]
	return state != nil && state.FrozenUntil.After(now)
}

func (s *ContentModerationService) beginModerationAPIKeyCall(key string) {
	hash := moderationAPIKeyHash(key)
	if hash == "" || s == nil {
		return
	}
	s.keyHealthMu.Lock()
	defer s.keyHealthMu.Unlock()
	state := s.ensureAPIKeyHealthLocked(hash, maskSecretTail(key))
	state.SyncActive++
}

func (s *ContentModerationService) finishModerationAPIKeyCall(key string, latencyMS int, success bool) {
	hash := moderationAPIKeyHash(key)
	if hash == "" || s == nil {
		return
	}
	if latencyMS < 0 {
		latencyMS = 0
	}
	s.keyHealthMu.Lock()
	defer s.keyHealthMu.Unlock()
	state := s.ensureAPIKeyHealthLocked(hash, maskSecretTail(key))
	if state.SyncActive > 0 {
		state.SyncActive--
	}
	state.SyncTotal++
	state.SyncLatencyMS += int64(latencyMS)
	if success {
		state.SyncSuccess++
		return
	}
	state.SyncErrors++
}

func (s *ContentModerationService) markAPIKeySuccess(key string, latencyMS int, httpStatus int) {
	hash := moderationAPIKeyHash(key)
	if hash == "" || s == nil {
		return
	}
	s.keyHealthMu.Lock()
	defer s.keyHealthMu.Unlock()
	state := s.ensureAPIKeyHealthLocked(hash, maskSecretTail(key))
	state.FailureCount = 0
	state.SuccessCount++
	state.LastError = ""
	state.LastCheckedAt = time.Now()
	state.FrozenUntil = time.Time{}
	state.LastLatencyMS = latencyMS
	state.LastHTTPStatus = httpStatus
	state.LastTested = true
}

func (s *ContentModerationService) markAPIKeyError(key string, errText string, latencyMS int, httpStatus int) {
	hash := moderationAPIKeyHash(key)
	if hash == "" || s == nil {
		return
	}
	s.keyHealthMu.Lock()
	defer s.keyHealthMu.Unlock()
	state := s.ensureAPIKeyHealthLocked(hash, maskSecretTail(key))
	if contentModerationFreezeDurationForHTTPStatus(httpStatus) > 0 {
		state.FailureCount++
	}
	state.LastError = trimRunes(errText, 180)
	state.LastCheckedAt = time.Now()
	state.LastLatencyMS = latencyMS
	state.LastHTTPStatus = httpStatus
	state.LastTested = true
	if freezeDuration := contentModerationFreezeDurationForHTTPStatus(httpStatus); freezeDuration > 0 {
		state.FrozenUntil = time.Now().Add(freezeDuration)
	}
}

func contentModerationFreezeDurationForHTTPStatus(httpStatus int) time.Duration {
	switch httpStatus {
	case 0, http.StatusBadRequest:
		return 0
	case http.StatusUnauthorized, http.StatusForbidden:
		return contentModerationKeyAuthFreezeDuration
	case http.StatusTooManyRequests, 529:
		return contentModerationKeyRateLimitFreezeDuration
	default:
		return contentModerationKeyHTTPErrorFreezeDuration
	}
}

func (s *ContentModerationService) ensureAPIKeyHealthLocked(hash string, masked string) *contentModerationKeyHealth {
	if s.keyHealth == nil {
		s.keyHealth = make(map[string]*contentModerationKeyHealth)
	}
	state := s.keyHealth[hash]
	if state == nil {
		state = &contentModerationKeyHealth{Hash: hash}
		s.keyHealth[hash] = state
	}
	if strings.TrimSpace(masked) != "" {
		state.Masked = masked
	}
	return state
}

func (s *ContentModerationService) configView(cfg *ContentModerationConfig) *ContentModerationConfigView {
	keys := cfg.apiKeys()
	masks := make([]string, 0, len(keys))
	for _, key := range keys {
		masks = append(masks, maskSecretTail(key))
	}
	apiKeyMasked := ""
	if len(masks) > 0 {
		apiKeyMasked = masks[0]
	}
	asset, assetErr := contentmoderationassets.Load(cfg.CandidateAsset)
	candidateEndpoints := make([]ContentModerationEndpointView, 0)
	if assetErr == nil {
		candidateEndpoints = candidateEndpointViews(asset.Manifest.CandidateEndpoints)
	}
	layer1Keywords, layer1Err := effectiveContentModerationKeywords(cfg)
	layer2Keywords, layer2Err := contentModerationSecondLayerKeywordValues(cfg)
	effectiveLayer2Keywords := canonicalContentModerationPrefilterKeywords(layer2Keywords)
	candidateSystemError := ""
	switch {
	case layer1Err != nil:
		candidateSystemError = layer1Err.Error()
	case layer2Err != nil:
		candidateSystemError = layer2Err.Error()
	case len(effectiveLayer2Keywords) == 0:
		candidateSystemError = "Layer 2 candidate keywords are empty; YuFeng review is health-protected and skipped"
	}
	view := &ContentModerationConfigView{
		Enabled:                        cfg.Enabled,
		Mode:                           cfg.Mode,
		BaseURL:                        cfg.BaseURL,
		Model:                          cfg.Model,
		ProxyID:                        cloneInt64Ptr(cfg.ProxyID),
		APIKeyConfigured:               len(keys) > 0,
		APIKeyMasked:                   apiKeyMasked,
		APIKeyCount:                    len(keys),
		APIKeyMasks:                    masks,
		APIKeyStatuses:                 s.apiKeyStatuses(keys),
		TimeoutMS:                      cfg.TimeoutMS,
		SampleRate:                     cfg.SampleRate,
		AllGroups:                      cfg.AllGroups,
		GroupIDs:                       append([]int64(nil), cfg.GroupIDs...),
		UserEmailWhitelist:             append([]string(nil), cfg.UserEmailWhitelist...),
		RecordNonHits:                  cfg.RecordNonHits,
		Thresholds:                     cloneFloatMap(cfg.Thresholds),
		BlockStatus:                    cfg.BlockStatus,
		BlockMessage:                   cfg.BlockMessage,
		EmailOnHit:                     cfg.EmailOnHit,
		AutoBanEnabled:                 cfg.AutoBanEnabled,
		BanThreshold:                   cfg.BanThreshold,
		ViolationWindowHours:           cfg.ViolationWindowHours,
		RetryCount:                     cfg.RetryCount,
		HitRetentionDays:               cfg.HitRetentionDays,
		NonHitRetentionDays:            cfg.NonHitRetentionDays,
		PreHashCheckEnabled:            cfg.PreHashCheckEnabled,
		BlockedKeywords:                append([]string(nil), cfg.BlockedKeywords...),
		KeywordBlockingMode:            cfg.KeywordBlockingMode,
		ModelFilter:                    cloneContentModerationModelFilter(cfg.ModelFilter),
		CacheVersion:                   cfg.CacheVersion,
		CacheMaxEntries:                cfg.CacheMaxEntries,
		CacheMaxBytes:                  cfg.CacheMaxBytes,
		FragmentBlockTTLSeconds:        cfg.FragmentBlockTTLSeconds,
		FragmentAllowTTLSeconds:        cfg.FragmentAllowTTLSeconds,
		FragmentTTLPolicyVersion:       cfg.FragmentTTLPolicyVersion,
		FirstLayerStage:                cfg.FirstLayerStage,
		SecondLayerEnabled:             cfg.SecondLayerEnabled,
		SecondLayerStage:               cfg.SecondLayerStage,
		SecondLayerEndpoints:           contentModerationEndpointViews(cfg.SecondLayerEndpoints),
		SecondLayerScanners:            append([]string(nil), cfg.SecondLayerScanners...),
		HardBlockPatterns:              append([]string(nil), cfg.HardBlockPatterns...),
		CandidateKeywords:              append([]string(nil), cfg.CandidateKeywords...),
		KeywordAllowlist:               append([]string(nil), cfg.KeywordAllowlist...),
		KeywordPolicyVersion:           cfg.KeywordPolicyVersion,
		ContextPolicyVersion:           cfg.ContextPolicyVersion,
		EvidencePolicyVersion:          cfg.EvidencePolicyVersion,
		CandidateAsset:                 cfg.CandidateAsset,
		CandidateEnabled:               cfg.CandidateEnabled,
		CandidateEndpoints:             candidateEndpoints,
		Layer1Keywords:                 append([]string(nil), layer1Keywords...),
		Layer2Keywords:                 append([]string(nil), layer2Keywords...),
		CandidateSystemReady:           candidateSystemError == "",
		CandidateSystemError:           candidateSystemError,
		CyberPolicyExcludeFromBanCount: cfg.CyberPolicyExcludeFromBanCount,
	}
	if assetErr == nil {
		view.CandidateLayer1Count = len(asset.Layer1)
		view.CandidateLayer2Count = len(asset.Layer2)
		view.CandidateSourceCommit = asset.Manifest.SourceCommit
	}
	return view
}

func effectiveContentModerationKeywords(cfg *ContentModerationConfig) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	policyActive := len(cfg.HardBlockPatterns) > 0 || len(cfg.CandidateKeywords) > 0
	if policyActive {
		return normalizeBlockedKeywords(cfg.HardBlockPatterns), nil
	}
	// Legacy configs retain their historical behavior until an administrator
	// opts into the explicit hard/candidate policy fields.
	keywords := append([]string(nil), cfg.BlockedKeywords...)
	if !cfg.CandidateEnabled {
		return normalizeBlockedKeywords(keywords), nil
	}
	asset, err := contentmoderationassets.Load(cfg.CandidateAsset)
	if err != nil {
		return nil, err
	}
	keywords = filterCandidateLayer1Overrides(keywords, asset)
	return normalizeBlockedKeywords(append(keywords, asset.Layer1...)), nil
}

func filterCandidateLayer1Overrides(values []string, asset contentmoderationassets.Asset) []string {
	overridden := make(map[string]struct{}, len(asset.Layer1Demotions)+len(asset.Layer1Suppressions))
	for _, term := range append(append([]string(nil), asset.Layer1Demotions...), asset.Layer1Suppressions...) {
		overridden[strings.ToLower(strings.TrimSpace(term))] = struct{}{}
	}
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := overridden[strings.ToLower(strings.TrimSpace(value))]; !exists {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func effectiveContentModerationSecondLayerKeywords(cfg *ContentModerationConfig) ([]string, error) {
	keywords, err := contentModerationSecondLayerKeywordValues(cfg)
	if err != nil {
		return nil, err
	}
	keywords = canonicalContentModerationPrefilterKeywords(keywords)
	if cfg != nil && cfg.SecondLayerEnabled && cfg.KeywordBlockingMode != ContentModerationKeywordModeKeywordOnly && len(keywords) == 0 {
		return nil, errors.New("second-layer candidate keywords are empty")
	}
	return keywords, nil
}

func contentModerationSecondLayerKeywordValues(cfg *ContentModerationConfig) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	keywords := append([]string(nil), cfg.CandidateKeywords...)
	if len(cfg.HardBlockPatterns) > 0 || len(cfg.CandidateKeywords) > 0 {
		keywords = append(keywords, cfg.BlockedKeywords...)
	}
	if cfg.CandidateEnabled {
		asset, err := contentmoderationassets.Load(cfg.CandidateAsset)
		if err != nil {
			return nil, err
		}
		keywords = append(keywords, asset.Layer2...)
	}
	return normalizeBlockedKeywords(keywords), nil
}

func candidateEndpointViews(endpoints []contentmoderationassets.CandidateEndpoint) []ContentModerationEndpointView {
	out := make([]ContentModerationEndpointView, 0, len(endpoints))
	for _, endpoint := range endpoints {
		out = append(out, ContentModerationEndpointView{
			ID: endpoint.ID, Name: endpoint.Name, BaseURL: endpoint.BaseURL, Model: endpoint.Model,
			Enabled: endpoint.Enabled, TimeoutMS: endpoint.TimeoutMS, InputLimit: endpoint.InputLimit,
			TokenConfigured: false, TokenMasked: "",
		})
	}
	return out
}

func (s *ContentModerationService) apiKeyStatuses(keys []string) []ContentModerationAPIKeyStatus {
	out := make([]ContentModerationAPIKeyStatus, 0, len(keys))
	for idx, key := range keys {
		out = append(out, s.apiKeyStatusForHash(idx, moderationAPIKeyHash(key), maskSecretTail(key), true))
	}
	return out
}

func (s *ContentModerationService) preBlockAPIKeyLoads(keys []string) []ContentModerationAPIKeyLoad {
	out := make([]ContentModerationAPIKeyLoad, 0, len(keys))
	for idx, key := range keys {
		out = append(out, s.preBlockAPIKeyLoadForHash(idx, moderationAPIKeyHash(key), maskSecretTail(key)))
	}
	return out
}

func (s *ContentModerationService) preBlockAPIKeyActive(keys []string) int64 {
	var total int64
	for _, item := range s.preBlockAPIKeyLoads(keys) {
		total += item.Active
	}
	return total
}

func (s *ContentModerationService) preBlockAPIKeyAvailableCount(keys []string) int64 {
	now := time.Now()
	var count int64
	for _, key := range keys {
		if !s.isAPIKeyFrozen(key, now) {
			count++
		}
	}
	return count
}

func (s *ContentModerationService) preBlockAPIKeyTotalCalls(keys []string) int64 {
	var total int64
	for _, item := range s.preBlockAPIKeyLoads(keys) {
		total += item.Total
	}
	return total
}

func (s *ContentModerationService) preBlockAPIKeyLoadForHash(index int, hash string, masked string) ContentModerationAPIKeyLoad {
	load := ContentModerationAPIKeyLoad{
		Index:   index,
		KeyHash: hash,
		Masked:  masked,
		Status:  "unknown",
	}
	status := s.apiKeyStatusForHash(index, hash, masked, true)
	load.Status = status.Status
	load.LastLatencyMS = status.LastLatencyMS
	load.LastHTTPStatus = status.LastHTTPStatus
	if hash == "" || s == nil {
		return load
	}
	s.keyHealthMu.Lock()
	defer s.keyHealthMu.Unlock()
	state := s.keyHealth[hash]
	if state == nil {
		return load
	}
	load.Active = state.SyncActive
	load.Total = state.SyncTotal
	load.Success = state.SyncSuccess
	load.Errors = state.SyncErrors
	if state.SyncTotal > 0 {
		load.AvgLatencyMS = state.SyncLatencyMS / state.SyncTotal
	}
	return load
}

func (s *ContentModerationService) apiKeyStatusForHash(index int, hash string, masked string, configured bool) ContentModerationAPIKeyStatus {
	status := ContentModerationAPIKeyStatus{
		Index:      index,
		KeyHash:    hash,
		Masked:     masked,
		Status:     "unknown",
		Configured: configured,
	}
	if hash == "" || s == nil {
		return status
	}
	now := time.Now()
	s.keyHealthMu.Lock()
	defer s.keyHealthMu.Unlock()
	state := s.keyHealth[hash]
	if state == nil {
		return status
	}
	status.FailureCount = state.FailureCount
	status.SuccessCount = state.SuccessCount
	status.LastError = state.LastError
	status.LastLatencyMS = state.LastLatencyMS
	status.LastHTTPStatus = state.LastHTTPStatus
	status.LastTested = state.LastTested
	if !state.LastCheckedAt.IsZero() {
		t := state.LastCheckedAt
		status.LastCheckedAt = &t
	}
	if state.FrozenUntil.After(now) {
		t := state.FrozenUntil
		status.FrozenUntil = &t
		status.Status = "frozen"
		return status
	}
	if state.LastError != "" {
		status.Status = "error"
		return status
	}
	if state.SuccessCount > 0 || state.LastTested {
		status.Status = "ok"
	}
	return status
}

func moderationAPIKeyHash(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func buildModerationTestInput(prompt string, images []string) (any, int, error) {
	prompt = trimRunes(normalizeContentModerationText(prompt), maxModerationInputRunes)
	normalizedImages := make([]string, 0, len(images))
	for _, image := range images {
		image = strings.TrimSpace(image)
		if image == "" {
			continue
		}
		if len(normalizedImages) >= maxContentModerationTestImages {
			return nil, 0, infraerrors.BadRequest("TOO_MANY_MODERATION_TEST_IMAGES", fmt.Sprintf("最多上传 %d 张测试图片", maxContentModerationTestImages))
		}
		if err := validateModerationTestImageDataURL(image); err != nil {
			return nil, 0, err
		}
		normalizedImages = append(normalizedImages, image)
	}
	if prompt == "" && len(normalizedImages) == 0 {
		return "hello", 0, nil
	}
	if len(normalizedImages) == 0 {
		return prompt, 0, nil
	}
	parts := make([]moderationAPIInputPart, 0, len(normalizedImages)+1)
	if prompt != "" {
		parts = append(parts, moderationAPIInputPart{Type: "text", Text: prompt})
	}
	for _, image := range normalizedImages {
		parts = append(parts, moderationAPIInputPart{
			Type:     "image_url",
			ImageURL: &moderationAPIImageURLRef{URL: image},
		})
	}
	return parts, len(normalizedImages), nil
}

func contentModerationTestHasAuditInput(prompt string, images []string) bool {
	if normalizeContentModerationText(prompt) != "" {
		return true
	}
	for _, image := range images {
		if strings.TrimSpace(image) != "" {
			return true
		}
	}
	return false
}

func validateModerationTestImageDataURL(value string) error {
	if len(value) > maxContentModerationTestImageDataURLBytes {
		return infraerrors.BadRequest("MODERATION_TEST_IMAGE_TOO_LARGE", "测试图片不能超过 8MB")
	}
	if !strings.HasPrefix(value, "data:image/") {
		return infraerrors.BadRequest("INVALID_MODERATION_TEST_IMAGE", "测试图片必须是 data:image/* base64")
	}
	parts := strings.SplitN(value, ",", 2)
	if len(parts) != 2 || !strings.Contains(parts[0], ";base64") {
		return infraerrors.BadRequest("INVALID_MODERATION_TEST_IMAGE", "测试图片必须是 base64 data URL")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return infraerrors.BadRequest("INVALID_MODERATION_TEST_IMAGE", "测试图片 base64 无效")
	}
	if len(raw) > maxContentModerationTestImageBytes {
		return infraerrors.BadRequest("MODERATION_TEST_IMAGE_TOO_LARGE", "测试图片不能超过 8MB")
	}
	return nil
}

func buildContentModerationTestAuditResult(result *moderationAPIResult, thresholds map[string]float64) *ContentModerationTestAuditResult {
	if result == nil {
		return nil
	}
	scores := make(map[string]float64, len(result.CategoryScores))
	for category, score := range result.CategoryScores {
		scores[category] = score
	}
	thresholdSnapshot := mergeContentModerationThresholds(ContentModerationDefaultThresholds(), thresholds)
	flagged, highestCategory, highestScore := evaluateModerationScores(scores, thresholdSnapshot)
	compositeScore := highestScore
	return &ContentModerationTestAuditResult{
		Flagged:         flagged,
		HighestCategory: highestCategory,
		HighestScore:    highestScore,
		CompositeScore:  compositeScore,
		CategoryScores:  scores,
		Thresholds:      thresholdSnapshot,
	}
}

type moderationAPIRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"`
}

type moderationAPIInputPart struct {
	Type     string                    `json:"type"`
	Text     string                    `json:"text,omitempty"`
	ImageURL *moderationAPIImageURLRef `json:"image_url,omitempty"`
}

type moderationAPIImageURLRef struct {
	URL string `json:"url"`
}

type moderationAPIResponse struct {
	Results []moderationAPIResult `json:"results"`
}

type moderationAPIResult struct {
	Flagged        bool               `json:"flagged"`
	CategoryScores map[string]float64 `json:"category_scores"`
}

func evaluateModerationScores(scores map[string]float64, thresholds map[string]float64) (bool, string, float64) {
	flagged := false
	highestCategory := ""
	highestScore := 0.0
	for _, category := range contentModerationCategoryOrder {
		score := scores[category]
		if score > highestScore || highestCategory == "" {
			highestScore = score
			highestCategory = category
		}
		if score >= thresholds[category] {
			flagged = true
		}
	}
	for category, score := range scores {
		if score > highestScore || highestCategory == "" {
			highestScore = score
			highestCategory = category
		}
	}
	return flagged, highestCategory, highestScore
}

func mergeContentModerationThresholds(base map[string]float64, override map[string]float64) map[string]float64 {
	out := cloneFloatMap(base)
	if out == nil {
		out = map[string]float64{}
	}
	for _, category := range contentModerationCategoryOrder {
		if v, ok := override[category]; ok {
			if v < 0 {
				v = 0
			}
			if v > 1 {
				v = 1
			}
			out[category] = v
		}
	}
	return out
}

func normalizeInt64IDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return []int64{}
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func normalizeContentModerationUserEmailWhitelist(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		email := strings.ToLower(strings.TrimSpace(raw))
		if email == "" {
			continue
		}
		if _, ok := seen[email]; ok {
			continue
		}
		seen[email] = struct{}{}
		out = append(out, email)
	}
	return out
}

func normalizeBlockedKeywords(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		kw := strings.TrimSpace(raw)
		if kw == "" {
			continue
		}
		kw = trimRunes(kw, maxContentModerationBlockedKeywordRunes)
		key := strings.ToLower(kw)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, kw)
		if len(out) >= maxContentModerationBlockedKeywords {
			break
		}
	}
	return out
}

func normalizeKeywordBlockingMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case ContentModerationKeywordModeKeywordOnly:
		return ContentModerationKeywordModeKeywordOnly
	case ContentModerationKeywordModeAPIOnly:
		return ContentModerationKeywordModeAPIOnly
	case ContentModerationKeywordModeKeywordAndAPI:
		return ContentModerationKeywordModeKeywordAndAPI
	default:
		return ContentModerationKeywordModeKeywordAndAPI
	}
}

func normalizeContentModerationModelFilter(filter ContentModerationModelFilter) ContentModerationModelFilter {
	out := ContentModerationModelFilter{
		Type:   normalizeContentModerationModelFilterType(filter.Type),
		Models: normalizeContentModerationModelNames(filter.Models),
	}
	if out.Type == ContentModerationModelFilterAll {
		out.Models = []string{}
	}
	return out
}

func cloneContentModerationModelFilter(filter ContentModerationModelFilter) ContentModerationModelFilter {
	normalized := normalizeContentModerationModelFilter(filter)
	normalized.Models = append([]string(nil), normalized.Models...)
	return normalized
}

func normalizeContentModerationModelFilterType(filterType string) string {
	switch strings.ToLower(strings.TrimSpace(filterType)) {
	case ContentModerationModelFilterInclude:
		return ContentModerationModelFilterInclude
	case ContentModerationModelFilterExclude:
		return ContentModerationModelFilterExclude
	case ContentModerationModelFilterAll:
		return ContentModerationModelFilterAll
	default:
		return ContentModerationModelFilterAll
	}
}

func normalizeContentModerationModelNames(models []string) []string {
	if len(models) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, raw := range models {
		model := trimRunes(strings.TrimSpace(raw), maxContentModerationModelFilterRunes)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
		if len(out) >= maxContentModerationModelFilterModels {
			break
		}
	}
	return out
}

func contentModerationModelListContains(models []string, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	for _, candidate := range models {
		if strings.ToLower(strings.TrimSpace(candidate)) == model {
			return true
		}
	}
	return false
}

func matchBlockedKeyword(text string, keywords []string) (string, bool) {
	if text == "" || len(keywords) == 0 {
		return "", false
	}
	lower := strings.ToLower(text)
	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		if containsContentModerationHardKeyword(lower, strings.ToLower(kw)) {
			return kw, true
		}
	}
	return "", false
}

func normalizeModerationAPIKeys(keys []string) []string {
	if len(keys) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func deleteModerationAPIKeysByHash(keys []string, hashes []string) []string {
	keys = normalizeModerationAPIKeys(keys)
	deleteHashes := make(map[string]struct{}, len(hashes))
	for _, hash := range hashes {
		hash = normalizeContentModerationHash(hash)
		if hash != "" {
			deleteHashes[hash] = struct{}{}
		}
	}
	if len(deleteHashes) == 0 {
		return keys
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := deleteHashes[moderationAPIKeyHash(key)]; ok {
			continue
		}
		out = append(out, key)
	}
	return out
}

func normalizeContentModerationAPIKeysMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case contentModerationAPIKeysModeReplace:
		return contentModerationAPIKeysModeReplace
	default:
		return contentModerationAPIKeysModeAppend
	}
}

func normalizeContentModerationHash(inputHash string) string {
	inputHash = strings.ToLower(strings.TrimSpace(inputHash))
	if len(inputHash) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(inputHash); err != nil {
		return ""
	}
	return inputHash
}

func cloneFloatMap(in map[string]float64) map[string]float64 {
	if in == nil {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneInt64Ptr(in *int64) *int64 {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func trimRunes(text string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}

func maskSecretTail(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	if len(secret) <= 4 {
		return "****"
	}
	return strings.Repeat("*", 8) + secret[len(secret)-4:]
}

// CyberPolicyRecordInput 是一次 cyber_policy 硬阻断的风控记录入参。
type CyberPolicyRecordInput struct {
	RequestID       string                          `json:"request_id"`
	UserID          int64                           `json:"user_id"`
	UserEmail       string                          `json:"user_email"`
	APIKeyID        int64                           `json:"api_key_id"`
	APIKeyName      string                          `json:"api_key_name"`
	GroupID         *int64                          `json:"group_id,omitempty"`
	GroupName       string                          `json:"group_name"`
	Endpoint        string                          `json:"endpoint"`
	Model           string                          `json:"model"`
	UpstreamMessage string                          `json:"-"`
	UpstreamBody    string                          `json:"-"`
	UpstreamStatus  int                             `json:"upstream_status"`
	UpstreamInTok   int                             `json:"upstream_input_tokens"`
	UpstreamOutTok  int                             `json:"upstream_output_tokens"`
	Protocol        string                          `json:"protocol"`
	Scope           *ContentModerationScopeSnapshot `json:"scope,omitempty"`
	RawRequest      ContentModerationRawRequest     `json:"-"`
	UserRole        string                          `json:"user_role"`
}

// RecordCyberPolicyEvent 对一次 cyber_policy 硬阻断立即执行账户处置，随后写入风控归档
// 并按配置发送通知。账户处置不受本地累计违规阈值约束。
// 仅受请求时的 GPT 分组 scope 快照约束；不受 risk_control_enabled 总开关和内容审核
// Enabled/Mode/group/model/sample 约束，确保严重违规始终留痕并处置。
func (s *ContentModerationService) RecordCyberPolicyEvent(ctx context.Context, in CyberPolicyRecordInput) {
	if s == nil || s.repo == nil {
		return
	}
	if in.Scope != nil && !in.Scope.InScope {
		return
	}
	cfg := &ContentModerationConfig{}
	if runtimeSnapshot, err := s.loadRuntimeSnapshot(ctx); err != nil {
		slog.Warn("content_moderation.cyber_runtime_snapshot_load_failed", "error", err)
	} else if runtimeSnapshot.config != nil {
		cfg = runtimeSnapshot.config
	}
	var userID *int64
	if in.UserID > 0 {
		userID = &in.UserID
	}
	var apiKeyID *int64
	if in.APIKeyID > 0 {
		apiKeyID = &in.APIKeyID
	}
	errBody := strings.TrimSpace(in.UpstreamMessage)
	if b := strings.TrimSpace(in.UpstreamBody); b != "" {
		// 原始 body 不在此预脱敏；写入 log.Error 前由 redactContentModerationSecrets 统一脱敏。
		errBody = strings.TrimSpace(errBody + "\n" + b)
	}
	if in.UpstreamInTok > 0 || in.UpstreamOutTok > 0 {
		errBody = fmt.Sprintf("%s\nupstream_usage=in:%d,out:%d", errBody, in.UpstreamInTok, in.UpstreamOutTok)
	}
	log := &ContentModerationLog{
		RequestID:       in.RequestID,
		UserID:          userID,
		UserEmail:       in.UserEmail,
		APIKeyID:        apiKeyID,
		APIKeyName:      in.APIKeyName,
		GroupID:         cloneInt64Ptr(in.GroupID),
		GroupName:       in.GroupName,
		Endpoint:        in.Endpoint,
		Provider:        "openai",
		Model:           in.Model,
		Mode:            "post_upstream",
		Action:          ContentModerationActionCyberPolicy,
		Flagged:         true,
		HighestCategory: "cyber_policy",
		HighestScore:    1.0,
		Error:           trimRunes(redactContentModerationSecrets(errBody), maxModerationErrorRunes),
		CreatedAt:       time.Now(),
		Protocol:        in.Protocol,
		Transport:       defaultContentModerationString(in.RawRequest.Transport, "http"),
		RequestStage:    defaultContentModerationString(in.RawRequest.Stage, "http"),
		RequestTarget:   in.RawRequest.Target,
		ArchiveStatus:   ContentModerationArchiveStatusNone,
	}
	transitioned, dispositionErr := s.applyCyberPolicyDisposition(ctx, in, log)
	if dispositionErr != nil {
		log.DispositionStatus = "retry_required"
		log.Error = trimRunes(log.Error+"\ndisposition_error="+redactContentModerationSecrets(dispositionErr.Error()), maxModerationErrorRunes)
		slog.Error("content_moderation.cyber_disposition_failed", "user_id", in.UserID, "api_key_id", in.APIKeyID, "error", dispositionErr)
	}
	log.EmailSent = false
	var archiveErr error
	if s.archiveRuntime != nil {
		checkInput := ContentModerationCheckInput{RawRequest: in.RawRequest}
		archiveErr = s.persistContentModerationArchive(ctx, log, checkInput)
		if archiveErr != nil {
			slog.Warn("content_moderation.cyber_archive_failed", "user_id", in.UserID, "error", archiveErr)
		}
	} else if createErr := s.repo.CreateLog(ctx, log); createErr != nil {
		archiveErr = createErr
		slog.Warn("content_moderation.cyber_create_log_failed", "user_id", in.UserID, "error", createErr)
	}
	emailEnabled := transitioned && cfg.EmailOnHit && s.emailService != nil && strings.TrimSpace(log.UserEmail) != ""
	emailRequired := emailEnabled
	emailCompletionRequired := false
	var emailDeliveryErr, emailStateErr error
	if emailEnabled {
		outcome := s.deliverClaimedContentModerationEmail(ctx, log, func() error {
			if log.DispositionTarget == "api_key" {
				return s.sendCyberPolicyEmail(ctx, log)
			}
			return s.sendAccountDisabledEmail(ctx, cfg, log)
		})
		log.EmailSent = outcome.Sent
		emailRequired = outcome.SendRequired
		emailCompletionRequired = outcome.CompletionRequired
		emailDeliveryErr = outcome.DeliveryErr
		emailStateErr = outcome.StateErr
		if emailDeliveryErr != nil {
			slog.Warn("content_moderation.cyber_disposition_email_delivery_failed", "user_id", in.UserID, "target", log.DispositionTarget, "error", emailDeliveryErr)
		}
		if emailStateErr != nil {
			slog.Error("content_moderation.cyber_disposition_email_state_failed", "user_id", in.UserID, "target", log.DispositionTarget, "error", emailStateErr)
		}
	}
	needsRetry := dispositionErr != nil || emailStateErr != nil
	if needsRetry && s.archiveRuntime != nil {
		cause := errors.Join(dispositionErr, emailStateErr, archiveErr)
		if err := s.archiveRuntime.QueueCyberDispositionRetry(in, log, dispositionErr == nil, cfg.EmailOnHit && s.emailService != nil, emailRequired, emailCompletionRequired, log.EmailSent, cause); err != nil {
			slog.Error("content_moderation.cyber_disposition_retry_persist_failed", "user_id", in.UserID, "api_key_id", in.APIKeyID, "error", err)
		}
	}
}

func (s *ContentModerationService) retryCyberPolicyDisposition(ctx context.Context, entry *contentModerationDispositionRetryFile) error {
	if s == nil || entry == nil {
		return errors.New("content moderation disposition retry entry unavailable")
	}
	kind := strings.TrimSpace(entry.Kind)
	if kind == "" {
		kind = contentModerationDispositionCyber
	}
	if kind != contentModerationDispositionCyber && kind != contentModerationDispositionLocal {
		return fmt.Errorf("unknown content moderation disposition retry kind %q", kind)
	}
	if kind == contentModerationDispositionLocal && strings.TrimSpace(entry.UserEmail) != "" && s.settingRepo != nil {
		whitelisted, err := s.IsUserEmailWhitelisted(ctx, entry.UserEmail)
		if err != nil {
			return fmt.Errorf("load content moderation user email whitelist: %w", err)
		}
		if whitelisted {
			slog.Info("content_moderation.skip_user_email_whitelist",
				"user_id", entry.UserID,
				"api_key_id", entry.APIKeyID,
				"source", kind+"_retry")
			return nil
		}
	}
	log := &ContentModerationLog{
		ID: entry.LogID, ArchiveID: entry.ArchiveID, Action: ContentModerationActionCyberPolicy,
		CreatedAt: entry.CreatedAt, DispositionTarget: entry.DispositionTarget,
		DispositionStatus: entry.DispositionStatus, DispositionTransitioned: entry.DispositionTransitioned,
		AutoBanned: entry.AutoBanned, ViolationCount: entry.ViolationCount, UserEmail: entry.UserEmail,
	}
	if kind == contentModerationDispositionLocal {
		log.Action = ContentModerationActionBlock
	}
	if entry.UserID > 0 {
		log.UserID = &entry.UserID
	}
	if entry.APIKeyID > 0 {
		log.APIKeyID = &entry.APIKeyID
	}
	if !entry.DispositionComplete {
		var transitioned bool
		var err error
		if kind == contentModerationDispositionLocal {
			if entry.BanThreshold <= 0 {
				return errors.New("local content moderation disposition retry is missing a ban threshold")
			}
			cfg := &ContentModerationConfig{
				AutoBanEnabled: true, BanThreshold: entry.BanThreshold,
				ViolationWindowHours:           entry.ViolationWindowHours,
				CyberPolicyExcludeFromBanCount: entry.ExcludeCyberPolicy,
			}
			log.Flagged = true
			transitioned, err = s.applyFlaggedAccountSideEffectsWithRole(ctx, cfg, log, entry.UserRole)
		} else {
			transitioned, err = s.applyCyberPolicyDisposition(ctx, CyberPolicyRecordInput{
				UserID: entry.UserID, APIKeyID: entry.APIKeyID, UserRole: entry.UserRole,
			}, log)
		}
		if err != nil {
			return err
		}
		entry.DispositionComplete = true
		entry.DispositionTarget = log.DispositionTarget
		entry.DispositionStatus = log.DispositionStatus
		entry.DispositionTransitioned = transitioned
		entry.AutoBanned = log.AutoBanned
		entry.ViolationCount = log.ViolationCount
		entry.EmailRequired = entry.EmailEnabled && transitioned && !entry.EmailSent
	}
	if err := s.updateRetriedContentModerationDisposition(ctx, entry); err != nil {
		return err
	}
	if entry.EmailRequired && s.emailService == nil {
		return errors.New("email service unavailable for content moderation disposition retry")
	}
	if entry.EmailRequired {
		if strings.TrimSpace(log.UserEmail) == "" {
			if s.userRepo == nil {
				return errors.New("user repository unavailable for disposition email")
			}
			user, err := s.userRepo.GetByID(ctx, entry.UserID)
			if err != nil {
				return err
			}
			log.UserEmail = user.Email
		}
		if strings.TrimSpace(log.UserEmail) == "" {
			entry.EmailRequired = false
			return nil
		}
		cfg, loadErr := s.loadConfig(ctx)
		if loadErr != nil {
			cfg = &ContentModerationConfig{}
		}
		outcome := s.deliverClaimedContentModerationEmail(ctx, log, func() error {
			if kind == contentModerationDispositionCyber && entry.DispositionTarget == "api_key" {
				return s.sendCyberPolicyEmail(ctx, log)
			}
			return s.sendAccountDisabledEmail(ctx, cfg, log)
		})
		entry.EmailRequired = outcome.SendRequired
		entry.EmailCompletionRequired = outcome.CompletionRequired
		entry.EmailSent = outcome.Sent
		if outcome.DeliveryErr != nil {
			slog.Warn("content_moderation.disposition_retry_email_delivery_failed", "operation_key", entry.OperationKey, "error", outcome.DeliveryErr)
		}
		if outcome.StateErr != nil {
			return outcome.StateErr
		}
	}
	if entry.EmailCompletionRequired {
		repo, ok := s.repo.(ContentModerationEmailDeliveryRepository)
		if !ok {
			return errors.New("content moderation email delivery repository is unavailable")
		}
		if err := s.completeContentModerationEmailDelivery(ctx, repo, log, entry.EmailSent); err != nil {
			return err
		}
		entry.EmailCompletionRequired = false
	}
	return nil
}

func (s *ContentModerationService) updateRetriedContentModerationDisposition(ctx context.Context, entry *contentModerationDispositionRetryFile) error {
	if entry == nil || strings.TrimSpace(entry.ArchiveID) == "" {
		return nil
	}
	repo, ok := s.repo.(ContentModerationDispositionLogRepository)
	if !ok {
		return nil
	}
	err := repo.UpdateLogDispositionByArchiveID(ctx, entry.ArchiveID, entry.DispositionStatus, entry.DispositionTarget, entry.DispositionTransitioned, entry.AutoBanned, entry.ViolationCount)
	if errors.Is(err, sql.ErrNoRows) && s.archiveRuntime != nil && s.archiveRuntime.hasPendingArchive(entry.ArchiveID) {
		return fmt.Errorf("content moderation archive is not imported yet: %w", err)
	}
	return err
}

func (s *ContentModerationService) applyCyberPolicyDisposition(ctx context.Context, in CyberPolicyRecordInput, log *ContentModerationLog) (bool, error) {
	if s == nil || log == nil || in.UserID <= 0 {
		return false, errors.New("cyber policy disposition requires a user")
	}
	role := strings.TrimSpace(in.UserRole)
	if s.userRepo != nil {
		user, err := s.userRepo.GetByID(ctx, in.UserID)
		if err != nil {
			return false, err
		}
		role = user.Role
	}
	if role == RoleAdmin {
		log.DispositionTarget = "api_key"
		if in.APIKeyID <= 0 {
			return false, errors.New("administrator cyber policy disposition requires an API key")
		}
		credential, transitioned, err := s.disableCyberPolicyAPIKey(ctx, in.APIKeyID)
		if err != nil {
			return false, err
		}
		log.DispositionTransitioned = transitioned
		if transitioned {
			log.DispositionStatus = "disabled"
			if s.authCacheInvalidator != nil {
				s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, credential)
			}
		} else {
			log.DispositionStatus = "already_disabled"
		}
		return transitioned, nil
	}

	log.DispositionTarget = "user"
	transitioned, err := s.disableCyberPolicyUser(ctx, in.UserID)
	if err != nil {
		return false, err
	}
	log.DispositionTransitioned = transitioned
	log.AutoBanned = transitioned
	if transitioned {
		log.DispositionStatus = "disabled"
		if s.authCacheInvalidator != nil {
			s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, in.UserID)
		}
	} else {
		log.DispositionStatus = "already_disabled"
	}
	return transitioned, nil
}

func (s *ContentModerationService) disableCyberPolicyUser(ctx context.Context, userID int64) (bool, error) {
	if s.dispositionRepo != nil {
		return s.dispositionRepo.DisableUserIfActive(ctx, userID)
	}
	if s.userRepo == nil {
		return false, errors.New("user repository unavailable")
	}
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return false, err
	}
	if user.Status != StatusActive {
		return false, nil
	}
	user.Status = StatusDisabled
	if err := s.userRepo.Update(ctx, user, UserUpdateFields{Status: true}); err != nil {
		return false, err
	}
	return true, nil
}

func (s *ContentModerationService) disableCyberPolicyAPIKey(ctx context.Context, apiKeyID int64) (string, bool, error) {
	if s.dispositionRepo != nil {
		return s.dispositionRepo.DisableAPIKeyIfActive(ctx, apiKeyID)
	}
	if s.apiKeyRepo == nil {
		return "", false, errors.New("API key repository unavailable")
	}
	key, err := s.apiKeyRepo.GetByID(ctx, apiKeyID)
	if err != nil {
		return "", false, err
	}
	if key.Status != StatusActive {
		return "", false, nil
	}
	key.Status = StatusAPIKeyDisabled
	if err := s.apiKeyRepo.Update(ctx, key, APIKeyUpdateFields{Status: true}); err != nil {
		return "", false, err
	}
	return key.Key, true, nil
}

func (s *ContentModerationService) sendCyberPolicyEmail(ctx context.Context, log *ContentModerationLog) error {
	siteName := s.siteName(ctx)
	if s.emailService.notificationEmailService != nil {
		variables := map[string]string{
			"triggered_at":     log.CreatedAt.UTC().Format(time.RFC3339),
			"model":            defaultContentModerationString(log.Model, "-"),
			"group_name":       defaultContentModerationString(log.GroupName, "-"),
			"upstream_message": defaultContentModerationString(log.Error, "-"),
		}
		err := s.emailService.notificationEmailService.Send(ctx, NotificationEmailSendInput{
			Event:          NotificationEmailEventCyberPolicyNotice,
			RecipientEmail: log.UserEmail,
			RecipientName:  emailRecipientName(log.UserEmail),
			UserID:         contentModerationEmailUserID(log),
			SourceType:     "content_moderation",
			SourceID:       contentModerationEmailSourceID(log),
			Variables:      variables,
		})
		if err == nil {
			return nil
		}
		if !shouldFallbackNotificationEmail(err) {
			return err
		}
		slog.Warn("template cyber policy email failed; falling back", "err", err.Error())
	}
	subject := fmt.Sprintf("[%s] 网络安全策略拦截 / Cyber Policy Notice", sanitizeEmailHeader(siteName))
	return s.emailService.SendEmail(ctx, log.UserEmail, subject, buildCyberPolicyNoticeEmailBody(siteName, log))
}
