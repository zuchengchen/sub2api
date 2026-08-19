package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
)

const (
	ContentModerationDeepSeekPromptVersion           = "deepseek-v4-flash-audit-v3"
	DefaultContentModerationDeepSeekModel            = "deepseek-v4-flash"
	DefaultContentModerationDeepSeekBaseURL          = "https://api.deepseek.com"
	DefaultContentModerationDeepSeekChannelTimeoutMS = 3000
	DefaultContentModerationDeepSeekTotalTimeoutMS   = 10000
	DefaultContentModerationDeepSeekThreshold        = 0.80
	maxContentModerationDeepSeekChannels             = 8
	maxContentModerationDeepSeekChannelNameRunes     = 128
	maxContentModerationDeepSeekBaseURLBytes         = 2048
	maxContentModerationDeepSeekModelRunes           = 128
)

func contentModerationRemoteReviewersEnabled(cfg *ContentModerationConfig) bool {
	if cfg == nil {
		return false
	}
	// A zero schema version identifies an in-memory/legacy configuration that
	// only knows the DeepSeek toggle. Once the pool schema is persisted, the
	// provider-neutral toggle is authoritative and can be explicitly disabled.
	return cfg.RemoteReviewersEnabled || (cfg.RemoteReviewersVersion == 0 && cfg.DeepSeekEnabled)
}

func contentModerationRemoteConsensusVotesRequired(_ *ContentModerationConfig) int {
	// One usable reviewer is sufficient. The remaining configured providers are
	// failover options and no longer form a quorum gate.
	return contentModerationRemoteConsensusVotes
}

const (
	ContentModerationRemoteProviderDeepSeek      = "deepseek"
	ContentModerationRemoteProviderQwen          = "alibaba_qwen"
	ContentModerationRemoteProviderGLM           = "zhipu_glm"
	ContentModerationRemoteProviderMiMo          = "mimo"
	ContentModerationRemoteUnavailableFailClosed = "fail_closed"
	ContentModerationYuFengModeShadow            = "shadow"
)

var contentModerationDeepSeekChannelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type ContentModerationDeepSeekChannel struct {
	ID             string                               `json:"id"`
	Name           string                               `json:"name"`
	Provider       string                               `json:"provider,omitempty"`
	BaseURL        string                               `json:"base_url"`
	Model          string                               `json:"model"`
	Enabled        bool                                 `json:"enabled"`
	Order          int                                  `json:"order"`
	TimeoutMS      int                                  `json:"timeout_ms"`
	APIKeyEnvelope *ContentModerationCredentialEnvelope `json:"api_key_envelope,omitempty"`
	APIKey         string                               `json:"-"`
}

// ContentModerationDeepSeekChannelInput keeps write-only secrets out of both
// persisted configuration and response DTOs.
type ContentModerationDeepSeekChannelInput struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Provider    string `json:"provider,omitempty"`
	BaseURL     string `json:"base_url"`
	Model       string `json:"model"`
	Enabled     bool   `json:"enabled"`
	Order       int    `json:"order"`
	TimeoutMS   int    `json:"timeout_ms"`
	APIKey      string `json:"api_key,omitempty"`
	ClearAPIKey bool   `json:"clear_api_key,omitempty"`
}

type ContentModerationDeepSeekChannelView struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	Provider            string     `json:"provider"`
	BaseURL             string     `json:"base_url"`
	Model               string     `json:"model"`
	Enabled             bool       `json:"enabled"`
	Order               int        `json:"order"`
	TimeoutMS           int        `json:"timeout_ms"`
	APIKeyConfigured    bool       `json:"api_key_configured"`
	APIKeyMasked        string     `json:"api_key_masked"`
	HealthStatus        string     `json:"health_status"`
	LastHealthCheckedAt *time.Time `json:"last_health_checked_at,omitempty"`
	BreakerStatus       string     `json:"breaker_status"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	LastLatencyMS       int        `json:"last_latency_ms"`
	LastError           string     `json:"last_error,omitempty"`
	HeartbeatStatus     string     `json:"heartbeat_status"`
	LastHeartbeatAt     *time.Time `json:"last_heartbeat_at,omitempty"`
	HeartbeatLatencyMS  int        `json:"heartbeat_latency_ms"`
	HeartbeatHTTPStatus int        `json:"heartbeat_http_status,omitempty"`
	HeartbeatError      string     `json:"heartbeat_error,omitempty"`
}

type TestContentModerationDeepSeekChannelResult struct {
	ChannelID   string     `json:"channel_id"`
	Provider    string     `json:"provider,omitempty"`
	Model       string     `json:"model,omitempty"`
	TestType    string     `json:"test_type,omitempty"`
	Reachable   bool       `json:"reachable"`
	HealthValid bool       `json:"health_valid"`
	LatencyMS   int        `json:"latency_ms"`
	HTTPStatus  int        `json:"http_status,omitempty"`
	Verdict     string     `json:"verdict,omitempty"`
	Category    string     `json:"category,omitempty"`
	Confidence  float64    `json:"confidence,omitempty"`
	Error       string     `json:"error,omitempty"`
	CheckedAt   *time.Time `json:"checked_at,omitempty"`
}

// defaultContentModerationDeepSeekChannels preserves the pre-pool in-memory
// default used by legacy callers. Persisted configurations are upgraded by
// parseContentModerationConfig to the provider pool below.
func defaultContentModerationDeepSeekChannels() []ContentModerationDeepSeekChannel {
	return []ContentModerationDeepSeekChannel{{
		ID: "deepseek-official", Name: "DeepSeek", Provider: ContentModerationRemoteProviderDeepSeek,
		BaseURL: DefaultContentModerationDeepSeekBaseURL, Model: DefaultContentModerationDeepSeekModel,
		Enabled: true, Order: 0, TimeoutMS: DefaultContentModerationDeepSeekChannelTimeoutMS,
	}}
}

func defaultContentModerationRemoteReviewerChannels() []ContentModerationDeepSeekChannel {
	return []ContentModerationDeepSeekChannel{
		{
			ID: "deepseek-primary", Name: "DeepSeek", Provider: ContentModerationRemoteProviderDeepSeek,
			BaseURL: DefaultContentModerationDeepSeekBaseURL, Model: DefaultContentModerationDeepSeekModel,
			Enabled: true, Order: 0, TimeoutMS: DefaultContentModerationDeepSeekChannelTimeoutMS,
		},
		{
			ID: "qwen-primary", Name: "Qwen", Provider: ContentModerationRemoteProviderQwen,
			BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "qwen3.7-flash",
			Enabled: true, Order: 1, TimeoutMS: DefaultContentModerationDeepSeekChannelTimeoutMS,
		},
		{
			ID: "glm-primary", Name: "GLM", Provider: ContentModerationRemoteProviderGLM,
			BaseURL: "https://open.bigmodel.cn/api/paas/v4", Model: "glm-4.7-flashx",
			Enabled: true, Order: 2, TimeoutMS: DefaultContentModerationDeepSeekChannelTimeoutMS,
		},
		{
			ID: "mimo-primary", Name: "MiMo", Provider: ContentModerationRemoteProviderMiMo,
			BaseURL: "https://api.xiaomimimo.com/v1", Model: "mimo-v2.5",
			Enabled: true, Order: 3, TimeoutMS: DefaultContentModerationDeepSeekChannelTimeoutMS,
		},
	}
}

func contentModerationRemoteProviderOrder(provider string) int {
	switch normalizeContentModerationRemoteProvider(provider) {
	case ContentModerationRemoteProviderDeepSeek:
		return 0
	case ContentModerationRemoteProviderQwen:
		return 1
	case ContentModerationRemoteProviderGLM:
		return 2
	case ContentModerationRemoteProviderMiMo:
		return 3
	default:
		return 99
	}
}

func normalizeContentModerationRemoteProvider(raw string) string {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	switch normalized {
	case "", "deepseek", "ds":
		return ContentModerationRemoteProviderDeepSeek
	case "qwen", "alibaba_qwen", "alibaba-qwen":
		return ContentModerationRemoteProviderQwen
	case "glm", "zhipu_glm", "zhipu-glm":
		return ContentModerationRemoteProviderGLM
	case "mimo", "mi-mo":
		return ContentModerationRemoteProviderMiMo
	default:
		// Preserve an explicit unknown value so validation can reject it. It
		// must never silently become the DeepSeek channel.
		return normalized
	}
}

func isSupportedContentModerationRemoteProvider(provider string) bool {
	switch normalizeContentModerationRemoteProvider(provider) {
	case ContentModerationRemoteProviderDeepSeek, ContentModerationRemoteProviderQwen,
		ContentModerationRemoteProviderGLM, ContentModerationRemoteProviderMiMo:
		return true
	default:
		return false
	}
}

func contentModerationRemoteProviderPreset(provider string) (openai_compat.CompatibleProviderPreset, bool) {
	switch normalizeContentModerationRemoteProvider(provider) {
	case ContentModerationRemoteProviderQwen:
		return openai_compat.CompatibleProviderPresetByID(string(openai_compat.ProviderAlibabaQwen))
	case ContentModerationRemoteProviderGLM:
		return openai_compat.CompatibleProviderPresetByID(string(openai_compat.ProviderZhipuGLM))
	case ContentModerationRemoteProviderMiMo:
		return openai_compat.CompatibleProviderPresetByID(string(openai_compat.ProviderMiMo))
	default:
		return openai_compat.CompatibleProviderPreset{}, false
	}
}

func normalizeContentModerationDeepSeekChannels(channels []ContentModerationDeepSeekChannel) []ContentModerationDeepSeekChannel {
	out := make([]ContentModerationDeepSeekChannel, 0, len(channels))
	for _, channel := range channels {
		channel.ID = strings.TrimSpace(channel.ID)
		channel.Name = strings.TrimSpace(channel.Name)
		channel.Provider = normalizeContentModerationRemoteProvider(channel.Provider)
		channel.BaseURL = strings.TrimRight(strings.TrimSpace(channel.BaseURL), "/")
		channel.Model = strings.TrimSpace(channel.Model)
		channel.APIKey = strings.TrimSpace(channel.APIKey)
		if channel.Name == "" {
			channel.Name = channel.ID
		}
		if channel.Provider == "" {
			channel.Provider = ContentModerationRemoteProviderDeepSeek
		}
		if preset, ok := contentModerationRemoteProviderPreset(channel.Provider); ok {
			if channel.BaseURL == "" {
				channel.BaseURL = strings.TrimRight(preset.BaseURL, "/")
			}
			if channel.Model == "" && len(preset.Models) > 0 {
				channel.Model = preset.Models[0]
			}
		}
		if channel.Model == "" {
			channel.Model = DefaultContentModerationDeepSeekModel
		}
		if channel.TimeoutMS <= 0 {
			channel.TimeoutMS = DefaultContentModerationDeepSeekChannelTimeoutMS
		}
		if channel.TimeoutMS > maxContentModerationTimeoutMS {
			channel.TimeoutMS = maxContentModerationTimeoutMS
		}
		channel.APIKeyEnvelope = cloneContentModerationCredentialEnvelope(channel.APIKeyEnvelope)
		out = append(out, channel)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if providerOrder := contentModerationRemoteProviderOrder(out[i].Provider) - contentModerationRemoteProviderOrder(out[j].Provider); providerOrder != 0 {
			return providerOrder < 0
		}
		if out[i].Order == out[j].Order {
			return out[i].ID < out[j].ID
		}
		return out[i].Order < out[j].Order
	})
	return out
}

// migrateContentModerationRemoteReviewerChannels upgrades pre-pool settings
// that only contained a DeepSeek channel. Existing channel credentials and
// names are preserved; missing managed providers are appended in policy order.
func migrateContentModerationRemoteReviewerChannels(channels []ContentModerationDeepSeekChannel) []ContentModerationDeepSeekChannel {
	channels = normalizeContentModerationDeepSeekChannels(channels)
	if len(channels) == 0 {
		return defaultContentModerationRemoteReviewerChannels()
	}
	seenProviders := make(map[string]struct{}, len(channels))
	seenIDs := make(map[string]struct{}, len(channels))
	maxOrder := -1
	for _, channel := range channels {
		seenProviders[contentModerationRemoteProvider(channel)] = struct{}{}
		seenIDs[channel.ID] = struct{}{}
		if channel.Order > maxOrder {
			maxOrder = channel.Order
		}
	}
	for _, preset := range defaultContentModerationRemoteReviewerChannels() {
		if _, exists := seenProviders[preset.Provider]; exists {
			continue
		}
		candidate := preset
		candidate.Order = maxOrder + 1
		maxOrder++
		if _, exists := seenIDs[candidate.ID]; exists {
			base := candidate.ID
			for suffix := 2; ; suffix++ {
				candidate.ID = fmt.Sprintf("%s-%d", base, suffix)
				if _, used := seenIDs[candidate.ID]; !used {
					break
				}
			}
		}
		channels = append(channels, candidate)
		seenProviders[candidate.Provider] = struct{}{}
		seenIDs[candidate.ID] = struct{}{}
	}
	return normalizeContentModerationDeepSeekChannels(channels)
}

func validateContentModerationDeepSeekChannels(channels []ContentModerationDeepSeekChannel) error {
	if len(channels) > maxContentModerationDeepSeekChannels {
		return fmt.Errorf("DeepSeek 渠道数量不能超过 %d", maxContentModerationDeepSeekChannels)
	}
	ids := make(map[string]struct{}, len(channels))
	orders := make(map[int]struct{}, len(channels))
	for _, channel := range channels {
		if !contentModerationDeepSeekChannelIDPattern.MatchString(channel.ID) {
			return fmt.Errorf("DeepSeek 渠道 ID %q 无效", channel.ID)
		}
		if _, exists := ids[channel.ID]; exists {
			return fmt.Errorf("DeepSeek 渠道 ID %q 重复", channel.ID)
		}
		ids[channel.ID] = struct{}{}
		if channel.Order < 0 {
			return fmt.Errorf("DeepSeek 渠道 %q 的顺序不能为负数", channel.ID)
		}
		if !isSupportedContentModerationRemoteProvider(channel.Provider) {
			return fmt.Errorf("审核供应商 %q 不受支持", channel.Provider)
		}
		if channel.Provider == "" {
			return errors.New("审核供应商不能为空")
		}
		if channel.Provider != ContentModerationRemoteProviderDeepSeek {
			preset, ok := contentModerationRemoteProviderPreset(channel.Provider)
			if !ok {
				return fmt.Errorf("审核供应商 %q 不受支持", channel.Provider)
			}
			if strings.TrimRight(channel.BaseURL, "/") != strings.TrimRight(preset.BaseURL, "/") {
				return fmt.Errorf("审核供应商 %q 的 Base URL 必须使用受管预设", channel.Provider)
			}
			if !preset.SupportsModel(channel.Model) {
				return fmt.Errorf("审核供应商 %q 不支持模型 %q", channel.Provider, channel.Model)
			}
		}
		if _, exists := orders[channel.Order]; exists {
			return fmt.Errorf("DeepSeek 渠道顺序 %d 重复", channel.Order)
		}
		orders[channel.Order] = struct{}{}
		if strings.TrimSpace(channel.Name) == "" {
			return fmt.Errorf("DeepSeek 渠道 %q 的名称不能为空", channel.ID)
		}
		if len([]rune(channel.Name)) > maxContentModerationDeepSeekChannelNameRunes {
			return fmt.Errorf("DeepSeek 渠道 %q 的名称过长", channel.ID)
		}
		if err := validateContentModerationDeepSeekBaseURL(channel.BaseURL); err != nil {
			return fmt.Errorf("DeepSeek 渠道 %q: %w", channel.ID, err)
		}
		if strings.TrimSpace(channel.Model) == "" {
			return fmt.Errorf("DeepSeek 渠道 %q 的模型不能为空", channel.ID)
		}
		if len([]rune(channel.Model)) > maxContentModerationDeepSeekModelRunes {
			return fmt.Errorf("DeepSeek 渠道 %q 的模型名过长", channel.ID)
		}
		if channel.TimeoutMS < minContentModerationSecondLayerTimeoutMS || channel.TimeoutMS > maxContentModerationTimeoutMS {
			return fmt.Errorf("DeepSeek 渠道 %q 的超时必须在 %d-%d 毫秒之间", channel.ID, minContentModerationSecondLayerTimeoutMS, maxContentModerationTimeoutMS)
		}
	}
	return nil
}

func validateContentModerationDeepSeekBaseURL(raw string) error {
	if len(strings.TrimSpace(raw)) > maxContentModerationDeepSeekBaseURLBytes {
		return errors.New("base URL 过长")
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("base URL 无效")
	}
	host := strings.Trim(parsed.Hostname(), "[]")
	if parsed.Scheme == "https" {
		if ip := net.ParseIP(host); ip != nil && !contentModerationDeepSeekPublicIP(ip) {
			return errors.New("HTTPS Base URL 不能使用本机或私有地址")
		}
		return nil
	}
	if parsed.Scheme != "http" {
		return errors.New("base URL 必须使用 HTTPS；仅回环地址可使用 HTTP")
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return errors.New("HTTP Base URL 仅允许回环地址")
	}
	return nil
}

func contentModerationDeepSeekPublicIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}

func cloneContentModerationCredentialEnvelope(in *ContentModerationCredentialEnvelope) *ContentModerationCredentialEnvelope {
	if in == nil {
		return nil
	}
	out := *in
	out.Nonce = append([]byte(nil), in.Nonce...)
	out.Ciphertext = append([]byte(nil), in.Ciphertext...)
	return &out
}

func cloneContentModerationDeepSeekChannels(channels []ContentModerationDeepSeekChannel) []ContentModerationDeepSeekChannel {
	out := make([]ContentModerationDeepSeekChannel, len(channels))
	for i := range channels {
		out[i] = channels[i]
		out[i].APIKeyEnvelope = cloneContentModerationCredentialEnvelope(channels[i].APIKeyEnvelope)
	}
	return out
}

func (s *ContentModerationService) ConfigureContentModerationCredentialKeyRing(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		s.credentialCipher = nil
		return
	}
	s.credentialCipher = NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))
}

func (s *ContentModerationService) setContentModerationCredentialCipherForTest(cipher *ContentModerationCredentialCipher) {
	s.credentialCipher = cipher
}

func (s *ContentModerationService) hydrateContentModerationDeepSeekSecrets(cfg *ContentModerationConfig) error {
	if cfg == nil {
		return errors.New("content moderation config is nil")
	}
	for i := range cfg.DeepSeekChannels {
		channel := &cfg.DeepSeekChannels[i]
		channel.APIKey = ""
		if !channel.Enabled || channel.APIKeyEnvelope == nil {
			continue
		}
		if s == nil || s.credentialCipher == nil {
			err := fmt.Errorf("decrypt DeepSeek channel %q API key: %w", channel.ID, ErrModerationCredentialKeyUnavailable)
			if s != nil {
				s.deepSeekChannelState(*channel).markCredentialUnavailable(time.Now(), err)
			}
			continue
		}
		apiKey, err := s.credentialCipher.DecryptDeepSeekAPIKey(channel.ID, channel.APIKeyEnvelope)
		if err != nil {
			wrapped := fmt.Errorf("decrypt DeepSeek channel %q API key: %w", channel.ID, err)
			s.deepSeekChannelState(*channel).markCredentialUnavailable(time.Now(), wrapped)
			continue
		}
		channel.APIKey = apiKey
	}
	return nil
}

func (s *ContentModerationService) mergeContentModerationDeepSeekChannelInputs(
	existing []ContentModerationDeepSeekChannel,
	inputs []ContentModerationDeepSeekChannelInput,
) ([]ContentModerationDeepSeekChannel, error) {
	oldByID := make(map[string]ContentModerationDeepSeekChannel, len(existing))
	for _, channel := range existing {
		oldByID[strings.TrimSpace(channel.ID)] = channel
	}
	merged := make([]ContentModerationDeepSeekChannel, 0, len(inputs))
	pendingKeys := make(map[string]string, len(inputs))
	for _, input := range inputs {
		id := strings.TrimSpace(input.ID)
		apiKey := strings.TrimSpace(input.APIKey)
		if input.ClearAPIKey && apiKey != "" {
			return nil, fmt.Errorf("DeepSeek 渠道 %q 不能同时设置和清除 API Key", id)
		}
		old, hasOld := oldByID[id]
		provider := strings.TrimSpace(input.Provider)
		if provider == "" && hasOld {
			// Older admin clients did not send provider. Preserve a managed
			// provider identity when editing such a channel instead of silently
			// converting it to the DeepSeek default.
			provider = old.Provider
		}
		channel := ContentModerationDeepSeekChannel{
			ID:        id,
			Name:      input.Name,
			Provider:  provider,
			BaseURL:   input.BaseURL,
			Model:     input.Model,
			Enabled:   input.Enabled,
			Order:     input.Order,
			TimeoutMS: input.TimeoutMS,
		}
		if hasOld && !input.ClearAPIKey && apiKey == "" &&
			normalizeContentModerationRemoteProvider(old.Provider) == normalizeContentModerationRemoteProvider(channel.Provider) {
			channel.APIKeyEnvelope = cloneContentModerationCredentialEnvelope(old.APIKeyEnvelope)
			channel.APIKey = old.APIKey
		}
		if apiKey != "" {
			pendingKeys[id] = apiKey
		}
		merged = append(merged, channel)
	}
	merged = normalizeContentModerationDeepSeekChannels(merged)
	if err := validateContentModerationDeepSeekChannels(merged); err != nil {
		return nil, err
	}
	for i := range merged {
		apiKey := pendingKeys[merged[i].ID]
		if apiKey == "" {
			continue
		}
		if s == nil || s.credentialCipher == nil {
			return nil, fmt.Errorf("encrypt DeepSeek channel %q API key: %w", merged[i].ID, ErrModerationCredentialKeyUnavailable)
		}
		envelope, err := s.credentialCipher.EncryptDeepSeekAPIKey(merged[i].ID, apiKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt DeepSeek channel %q API key: %w", merged[i].ID, err)
		}
		merged[i].APIKeyEnvelope = envelope
		merged[i].APIKey = apiKey
	}
	return merged, nil
}

func contentModerationDeepSeekChannelViews(channels []ContentModerationDeepSeekChannel) []ContentModerationDeepSeekChannelView {
	channels = normalizeContentModerationDeepSeekChannels(channels)
	out := make([]ContentModerationDeepSeekChannelView, 0, len(channels))
	for _, channel := range channels {
		health := "untested"
		breaker := "closed"
		heartbeat := "untested"
		if !channel.Enabled {
			health = "disabled"
			breaker = "disabled"
			heartbeat = "disabled"
		}
		masked := maskSecretTail(channel.APIKey)
		if channel.APIKeyEnvelope != nil && masked == "" {
			masked = "********"
		}
		out = append(out, ContentModerationDeepSeekChannelView{
			ID:               channel.ID,
			Name:             channel.Name,
			Provider:         channel.Provider,
			BaseURL:          channel.BaseURL,
			Model:            channel.Model,
			Enabled:          channel.Enabled,
			Order:            channel.Order,
			TimeoutMS:        channel.TimeoutMS,
			APIKeyConfigured: channel.APIKeyEnvelope != nil,
			APIKeyMasked:     masked,
			HealthStatus:     health,
			BreakerStatus:    breaker,
			HeartbeatStatus:  heartbeat,
		})
	}
	return out
}

func (s *ContentModerationService) contentModerationDeepSeekChannelViews(channels []ContentModerationDeepSeekChannel) []ContentModerationDeepSeekChannelView {
	channels = normalizeContentModerationDeepSeekChannels(channels)
	out := make([]ContentModerationDeepSeekChannelView, 0, len(channels))
	for _, channel := range channels {
		out = append(out, s.contentModerationDeepSeekChannelView(channel))
	}
	return out
}

// TestDeepSeekChannel is implemented by the DeepSeek runtime module. Keeping
// the public service API here lets the admin handler remain independent of the
// connectivity transport implementation.
func (s *ContentModerationService) TestDeepSeekChannel(ctx context.Context, channelID string) (*TestContentModerationDeepSeekChannelResult, error) {
	return s.testDeepSeekChannelConnectivity(ctx, strings.TrimSpace(channelID))
}

// TestContentModerationChannelAPI sends one explicit real moderation request.
// It is intentionally separate from the legacy HEAD connectivity test because
// this action may consume provider quota.
func (s *ContentModerationService) TestContentModerationChannelAPI(ctx context.Context, channelID string) (*TestContentModerationDeepSeekChannelResult, error) {
	return s.testDeepSeekChannelReview(ctx, strings.TrimSpace(channelID))
}

func (s *ContentModerationService) contentModerationSecondLayerEnforceReadiness(cfg *ContentModerationConfig, now time.Time) (bool, string) {
	if cfg == nil {
		return false, "风控配置不可用"
	}
	if cfg.RemoteReviewersVersion == 0 && cfg.YuFengEnabled && !s.hasHealthyYuFengEndpoint(cfg, now) {
		// Legacy configurations treated the local YuFeng endpoint as part of
		// the Enforce gate. Versioned reviewer-pool configurations intentionally
		// run YuFeng as shadow-only and do not use it as a gate.
		return false, "YuFeng 没有成功完成真实审核"
	}
	if !contentModerationRemoteReviewersEnabled(cfg) {
		if cfg.RemoteReviewersVersion == 0 && cfg.YuFengEnabled && s.hasHealthyYuFengEndpoint(cfg, now) {
			// Legacy local-only configurations remain readable while they are
			// migrated. Persisted provider-pool configurations never use YuFeng
			// as an Enforce gate.
			return true, ""
		}
		if cfg.RemoteReviewersVersion == 0 && cfg.YuFengEnabled {
			return false, "YuFeng 没有成功完成真实审核"
		}
		return false, "线上审核池未启用；YuFeng 旁路审核不会单独触发 Enforce"
	}
	requiredVotes := contentModerationRemoteConsensusVotesRequired(cfg)
	configured := countConfiguredContentModerationRemoteProviders(cfg)
	if configured < requiredVotes {
		return false, "线上审核渠道尚未配置密钥"
	}
	reachable := s.countReachableContentModerationRemoteProviders(cfg, now)
	if reachable < requiredVotes {
		return false, "线上审核渠道尚未完成首次真实审核；熔断器可用状态缺失，请等待启动检查或点击测试"
	}
	return true, ""
}
