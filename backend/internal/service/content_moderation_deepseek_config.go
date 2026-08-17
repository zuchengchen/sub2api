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
)

const (
	ContentModerationDeepSeekPromptVersion           = "deepseek-v4-flash-audit-v1"
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

var contentModerationDeepSeekChannelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type ContentModerationDeepSeekChannel struct {
	ID             string                               `json:"id"`
	Name           string                               `json:"name"`
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
	BaseURL             string     `json:"base_url"`
	Model               string     `json:"model"`
	Enabled             bool       `json:"enabled"`
	Order               int        `json:"order"`
	TimeoutMS           int        `json:"timeout_ms"`
	APIKeyConfigured    bool       `json:"api_key_configured"`
	APIKeyMasked        string     `json:"api_key_masked"`
	HealthStatus        string     `json:"health_status"`
	LastHealthCheckedAt *time.Time `json:"last_health_checked_at,omitempty"`
	HealthyUntil        *time.Time `json:"healthy_until,omitempty"`
	BreakerStatus       string     `json:"breaker_status"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	LastLatencyMS       int        `json:"last_latency_ms"`
	LastError           string     `json:"last_error,omitempty"`
}

type ContentModerationDeepSeekContractCaseResult struct {
	Passed          bool    `json:"passed"`
	ExpectedFlagged bool    `json:"expected_flagged"`
	Flagged         bool    `json:"flagged"`
	Confidence      float64 `json:"confidence"`
	Category        string  `json:"category"`
	Reason          string  `json:"reason"`
	LatencyMS       int     `json:"latency_ms"`
	Error           string  `json:"error,omitempty"`
}

type TestContentModerationDeepSeekChannelResult struct {
	ChannelID   string                                      `json:"channel_id"`
	SafeCase    ContentModerationDeepSeekContractCaseResult `json:"safe_case"`
	RiskCase    ContentModerationDeepSeekContractCaseResult `json:"risk_case"`
	HealthValid bool                                        `json:"health_valid"`
	CheckedAt   *time.Time                                  `json:"checked_at,omitempty"`
}

func defaultContentModerationDeepSeekChannels() []ContentModerationDeepSeekChannel {
	return []ContentModerationDeepSeekChannel{{
		ID:        "deepseek-official",
		Name:      "DeepSeek 官方",
		BaseURL:   DefaultContentModerationDeepSeekBaseURL,
		Model:     DefaultContentModerationDeepSeekModel,
		Enabled:   true,
		Order:     0,
		TimeoutMS: DefaultContentModerationDeepSeekChannelTimeoutMS,
	}}
}

func normalizeContentModerationDeepSeekChannels(channels []ContentModerationDeepSeekChannel) []ContentModerationDeepSeekChannel {
	out := make([]ContentModerationDeepSeekChannel, 0, len(channels))
	for _, channel := range channels {
		channel.ID = strings.TrimSpace(channel.ID)
		channel.Name = strings.TrimSpace(channel.Name)
		channel.BaseURL = strings.TrimRight(strings.TrimSpace(channel.BaseURL), "/")
		channel.Model = strings.TrimSpace(channel.Model)
		channel.APIKey = strings.TrimSpace(channel.APIKey)
		if channel.Name == "" {
			channel.Name = channel.ID
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
		if out[i].Order == out[j].Order {
			return out[i].ID < out[j].ID
		}
		return out[i].Order < out[j].Order
	})
	return out
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
		return errors.New("Base URL 过长")
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("Base URL 无效")
	}
	host := strings.Trim(parsed.Hostname(), "[]")
	if parsed.Scheme == "https" {
		if ip := net.ParseIP(host); ip != nil && !contentModerationDeepSeekPublicIP(ip) {
			return errors.New("HTTPS Base URL 不能使用本机或私有地址")
		}
		return nil
	}
	if parsed.Scheme != "http" {
		return errors.New("Base URL 必须使用 HTTPS；仅回环地址可使用 HTTP")
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
		channel := ContentModerationDeepSeekChannel{
			ID:        id,
			Name:      input.Name,
			BaseURL:   input.BaseURL,
			Model:     input.Model,
			Enabled:   input.Enabled,
			Order:     input.Order,
			TimeoutMS: input.TimeoutMS,
		}
		if old, ok := oldByID[id]; ok && !input.ClearAPIKey && apiKey == "" {
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
		if !channel.Enabled {
			health = "disabled"
			breaker = "disabled"
		}
		masked := maskSecretTail(channel.APIKey)
		if channel.APIKeyEnvelope != nil && masked == "" {
			masked = "********"
		}
		out = append(out, ContentModerationDeepSeekChannelView{
			ID:               channel.ID,
			Name:             channel.Name,
			BaseURL:          channel.BaseURL,
			Model:            channel.Model,
			Enabled:          channel.Enabled,
			Order:            channel.Order,
			TimeoutMS:        channel.TimeoutMS,
			APIKeyConfigured: channel.APIKeyEnvelope != nil,
			APIKeyMasked:     masked,
			HealthStatus:     health,
			BreakerStatus:    breaker,
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
// the public service contract here lets the admin API remain independent of
// the transport implementation.
func (s *ContentModerationService) TestDeepSeekChannel(ctx context.Context, channelID string) (*TestContentModerationDeepSeekChannelResult, error) {
	return s.testDeepSeekChannelContract(ctx, strings.TrimSpace(channelID))
}

func (s *ContentModerationService) contentModerationSecondLayerEnforceReadiness(cfg *ContentModerationConfig, now time.Time) (bool, string) {
	if cfg == nil {
		return false, "风控配置不可用"
	}
	if !cfg.DeepSeekEnabled && !cfg.YuFengEnabled {
		return false, "至少启用一个审核器后才能启用 Layer 2 Enforce"
	}
	if cfg.DeepSeekEnabled && !s.hasHealthyDeepSeekChannel(cfg, now) {
		return false, "所有启用的 DeepSeek 渠道均须通过最近 15 分钟的双样例健康检查"
	}
	if cfg.YuFengEnabled {
		if len(cfg.enabledYuFengSecondLayerEndpoints()) == 0 {
			return false, "YuFeng 已启用但没有可用审核端点"
		}
		if !s.hasHealthyYuFengEndpoint(cfg, now) {
			return false, "YuFeng 审核器最近 15 分钟内没有成功完成真实审核"
		}
	}
	return true, ""
}
