package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"

	"github.com/tidwall/gjson"
)

const (
	usagePolicyFlagPhrase           = "flagged as potentially violating our usage policy"
	defaultUsagePolicyBanThreshold  = 1
	usagePolicyUserBanDuration      = 5 * time.Minute
	usagePolicyUnbanPollInterval    = 5 * time.Second
	usagePolicyUnbanBatchSize       = 100
	usagePolicySkipReasonDisabled   = "already_disabled"
	usagePolicySkipReasonThreshold  = "below_threshold"
	usagePolicySkipReasonAutoBanOff = "auto_ban_disabled"
	usagePolicySkipReasonNoUser     = "missing_user"

	UsagePolicyClientErrorType = "invalid_prompt"
	UsagePolicyClientErrorCode = "invalid_prompt"
	UsagePolicyClientStatus    = http.StatusBadRequest
	UsagePolicyClientNotice    = "已违反 OpenAI 使用政策，请立即停止本次会话。账户已禁用，冷却 5 分钟后自动解禁。"
)

// UsagePolicyConfig is stored as settings.usage_policy_config JSON.
type UsagePolicyConfig struct {
	Enabled        bool `json:"enabled"`
	AutoBanEnabled bool `json:"auto_ban_enabled"`
	BanThreshold   int  `json:"ban_threshold"`
}

type UpdateUsagePolicyConfigInput struct {
	Enabled        *bool `json:"enabled"`
	AutoBanEnabled *bool `json:"auto_ban_enabled"`
	BanThreshold   *int  `json:"ban_threshold"`
}

type UsagePolicyViolation struct {
	UserID             int64
	RequestID          string
	ClientRequestID    string
	APIKeyID           *int64
	AccountID          *int64
	GroupID            *int64
	Platform           string
	Model              string
	InboundEndpoint    string
	StatusCode         *int
	UpstreamStatusCode *int
	ErrorMessage       string
	AutoBanned         bool
	SkipReason         string
	CreatedAt          time.Time
}

type UsagePolicyKeyStat struct {
	UserID       int64     `json:"user_id"`
	Email        string    `json:"email"`
	Username     string    `json:"username"`
	Role         string    `json:"role"`
	APIKeyID     *int64    `json:"api_key_id,omitempty"`
	APIKeyName   string    `json:"api_key_name"`
	APIKeyStatus string    `json:"api_key_status"`
	Count        int       `json:"count"`
	AutoBanned   bool      `json:"auto_banned"`
	LastAt       time.Time `json:"last_at"`
}

type UsagePolicyStats struct {
	Total          int                  `json:"total"`
	UniqueUsers    int                  `json:"unique_users"`
	UniqueKeys     int                  `json:"unique_keys"`
	DisabledKeys   int                  `json:"disabled_keys"`
	AutoBannedKeys int                  `json:"auto_banned_keys"`
	Keys           []UsagePolicyKeyStat `json:"keys"`
	Config         *UsagePolicyConfig   `json:"config"`
}

type UsagePolicyRepository interface {
	InsertViolation(ctx context.Context, v *UsagePolicyViolation) (id int64, inserted bool, count int, err error)
	UpdateViolationDisposition(ctx context.Context, id int64, autoBanned bool, skipReason string) error
	ListKeyStats(ctx context.Context) ([]UsagePolicyKeyStat, error)
	DisableUserForUsagePolicy(ctx context.Context, userID int64, until time.Time) (transitioned bool, err error)
	UnbanDueUsers(ctx context.Context, limit int) ([]int64, error)
}

type UsagePolicyConversationArchiver interface {
	RecordUsagePolicyConversation(ctx context.Context, in CyberPolicyRecordInput)
}

type UsagePolicyService struct {
	repo                 UsagePolicyRepository
	settingRepo          SettingRepository
	authCacheInvalidator APIKeyAuthCacheInvalidator
	archiver             UsagePolicyConversationArchiver

	start  sync.Once
	stop   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewUsagePolicyService(
	repo UsagePolicyRepository,
	settingRepo SettingRepository,
	authCacheInvalidator APIKeyAuthCacheInvalidator,
) *UsagePolicyService {
	return &UsagePolicyService{
		repo:                 repo,
		settingRepo:          settingRepo,
		authCacheInvalidator: authCacheInvalidator,
	}
}

func (s *UsagePolicyService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.start.Do(func() {
		s.ctx, s.cancel = context.WithCancel(context.Background())
		s.wg.Add(1)
		go s.unbanLoop()
	})
}

func (s *UsagePolicyService) Stop() {
	if s == nil {
		return
	}
	s.stop.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()
	})
}

func (s *UsagePolicyService) SetConversationArchiver(archiver UsagePolicyConversationArchiver) {
	if s == nil {
		return
	}
	s.archiver = archiver
}

func (s *UsagePolicyService) archiveFromEntry(ctx context.Context, entry *OpsInsertErrorLogInput) {
	if entry == nil {
		return
	}
	input := ContentModerationCheckInput{}
	if entry.ConversationInput != nil {
		input = *entry.ConversationInput
	}
	s.ArchiveConversation(ctx, entry, input)
}

func (s *UsagePolicyService) ArchiveConversation(ctx context.Context, entry *OpsInsertErrorLogInput, input ContentModerationCheckInput) {
	if s == nil || s.archiver == nil || entry == nil || !IsUsagePolicyViolation(entry) {
		return
	}
	in := CyberPolicyRecordInput{
		RequestID:       entry.RequestID,
		UserEmail:       input.UserEmail,
		APIKeyName:      input.APIKeyName,
		GroupID:         input.GroupID,
		GroupName:       input.GroupName,
		Endpoint:        firstNonEmpty(entry.InboundEndpoint, input.Endpoint),
		Model:           firstNonEmpty(entry.RequestedModel, entry.Model, input.Model),
		UpstreamMessage: usagePolicyMessage(entry),
		UpstreamBody:    entry.ErrorBody,
		Protocol:        input.Protocol,
		RawRequest:      input.RawRequest,
		UserRole:        input.UserRole,
	}
	if entry.UserID != nil {
		in.UserID = *entry.UserID
	} else if input.UserID > 0 {
		in.UserID = input.UserID
	}
	if entry.APIKeyID != nil {
		in.APIKeyID = *entry.APIKeyID
	} else if input.APIKeyID > 0 {
		in.APIKeyID = input.APIKeyID
	}
	if entry.UpstreamStatusCode != nil {
		in.UpstreamStatus = *entry.UpstreamStatusCode
	}
	s.archiver.RecordUsagePolicyConversation(ctx, in)
}

func defaultUsagePolicyConfig() *UsagePolicyConfig {
	return &UsagePolicyConfig{
		Enabled:        true,
		AutoBanEnabled: true,
		BanThreshold:   defaultUsagePolicyBanThreshold,
	}
}

func (cfg *UsagePolicyConfig) normalize() {
	if cfg == nil {
		return
	}
	if cfg.BanThreshold <= 0 {
		cfg.BanThreshold = defaultUsagePolicyBanThreshold
	}
}

func parseUsagePolicyConfig(raw string) (*UsagePolicyConfig, error) {
	cfg := defaultUsagePolicyConfig()
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(trimmed), cfg); err != nil {
		return nil, infraerrors.BadRequest("INVALID_USAGE_POLICY_CONFIG", "usage policy 配置不是有效 JSON")
	}
	cfg.normalize()
	return cfg, nil
}

func (s *UsagePolicyService) GetConfig(ctx context.Context) (*UsagePolicyConfig, error) {
	if s == nil || s.settingRepo == nil {
		return defaultUsagePolicyConfig(), nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyUsagePolicyConfig)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaultUsagePolicyConfig(), nil
		}
		return nil, err
	}
	return parseUsagePolicyConfig(raw)
}

func (s *UsagePolicyService) UpdateConfig(ctx context.Context, input UpdateUsagePolicyConfigInput) (*UsagePolicyConfig, error) {
	if s == nil || s.settingRepo == nil {
		return nil, infraerrors.InternalServer("USAGE_POLICY_SETTINGS_UNAVAILABLE", "usage policy settings are unavailable")
	}
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	if input.Enabled != nil {
		cfg.Enabled = *input.Enabled
	}
	if input.AutoBanEnabled != nil {
		cfg.AutoBanEnabled = *input.AutoBanEnabled
	}
	if input.BanThreshold != nil {
		cfg.BanThreshold = *input.BanThreshold
	}
	cfg.normalize()
	payload, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := s.settingRepo.Set(ctx, SettingKeyUsagePolicyConfig, string(payload)); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (s *UsagePolicyService) GetStats(ctx context.Context) (*UsagePolicyStats, error) {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	stats := &UsagePolicyStats{Config: cfg, Keys: []UsagePolicyKeyStat{}}
	if s.repo == nil {
		return stats, nil
	}
	keys, err := s.repo.ListKeyStats(ctx)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		keys = []UsagePolicyKeyStat{}
	}
	stats.Keys = keys
	seenUsers := make(map[int64]struct{}, len(keys))
	seenKeys := make(map[int64]struct{}, len(keys))
	for _, row := range keys {
		stats.Total += row.Count
		if _, ok := seenUsers[row.UserID]; !ok {
			seenUsers[row.UserID] = struct{}{}
			stats.UniqueUsers++
		}
		if row.APIKeyID != nil && *row.APIKeyID > 0 {
			if _, ok := seenKeys[*row.APIKeyID]; !ok {
				seenKeys[*row.APIKeyID] = struct{}{}
				stats.UniqueKeys++
				if row.APIKeyStatus == StatusDisabled || row.APIKeyStatus == StatusAPIKeyDisabled {
					stats.DisabledKeys++
				}
			}
		}
		if row.AutoBanned {
			stats.AutoBannedKeys++
		}
	}
	return stats, nil
}

// ObserveErrorLogs records matching upstream usage-policy failures and may disable the user for 5 minutes.
func (s *UsagePolicyService) ObserveErrorLogs(ctx context.Context, entries []*OpsInsertErrorLogInput) {
	if s == nil || len(entries) == 0 {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("usage_policy.observe_panic", "panic", rec)
		}
	}()
	if !s.isRiskControlEnabled(ctx) {
		return
	}
	cfg, err := s.GetConfig(ctx)
	if err != nil || cfg == nil || !cfg.Enabled {
		return
	}
	for _, entry := range entries {
		s.handleEntry(ctx, cfg, entry)
	}
}

func (s *UsagePolicyService) handleEntry(ctx context.Context, cfg *UsagePolicyConfig, entry *OpsInsertErrorLogInput) {
	if !IsUsagePolicyViolation(entry) {
		return
	}
	if entry.UserID == nil || *entry.UserID <= 0 {
		return
	}
	if s.repo == nil {
		return
	}
	model := strings.TrimSpace(entry.RequestedModel)
	if model == "" {
		model = strings.TrimSpace(entry.Model)
	}
	violation := &UsagePolicyViolation{
		UserID:          *entry.UserID,
		RequestID:       strings.TrimSpace(entry.RequestID),
		ClientRequestID: strings.TrimSpace(entry.ClientRequestID),
		APIKeyID:        entry.APIKeyID,
		AccountID:       entry.AccountID,
		GroupID:         entry.GroupID,
		Platform:        strings.TrimSpace(entry.Platform),
		Model:           model,
		InboundEndpoint: strings.TrimSpace(entry.InboundEndpoint),
		ErrorMessage:    usagePolicyMessage(entry),
		CreatedAt:       entry.CreatedAt,
	}
	if entry.StatusCode != 0 {
		status := entry.StatusCode
		violation.StatusCode = &status
	}
	violation.UpstreamStatusCode = entry.UpstreamStatusCode
	if violation.CreatedAt.IsZero() {
		violation.CreatedAt = time.Now()
	}

	id, inserted, count, err := s.repo.InsertViolation(ctx, violation)
	if err != nil {
		slog.Error("usage_policy.insert_failed", "user_id", *entry.UserID, "error", err)
		return
	}
	if !inserted {
		return
	}
	s.archiveFromEntry(ctx, entry)
	skipReason, autoBanned := s.applyBan(ctx, cfg, *entry.UserID, count)
	if skipReason != "" || autoBanned {
		if err := s.repo.UpdateViolationDisposition(ctx, id, autoBanned, skipReason); err != nil {
			slog.Error("usage_policy.update_disposition_failed", "id", id, "user_id", *entry.UserID, "error", err)
		}
	}
	if autoBanned {
		slog.Warn("usage_policy.user_disabled",
			"user_id", *entry.UserID,
			"duration", usagePolicyUserBanDuration.String(),
			"count", count,
			"threshold", cfg.BanThreshold,
			"request_id", violation.RequestID,
		)
	}
}

func (s *UsagePolicyService) applyBan(ctx context.Context, cfg *UsagePolicyConfig, userID int64, count int) (skipReason string, autoBanned bool) {
	if cfg == nil || !cfg.AutoBanEnabled {
		return usagePolicySkipReasonAutoBanOff, false
	}
	if cfg.BanThreshold <= 0 || count < cfg.BanThreshold {
		return usagePolicySkipReasonThreshold, false
	}
	if userID <= 0 {
		return usagePolicySkipReasonNoUser, false
	}
	until := time.Now().UTC().Add(usagePolicyUserBanDuration)
	transitioned, err := s.repo.DisableUserForUsagePolicy(ctx, userID, until)
	if err != nil {
		slog.Error("usage_policy.disable_user_failed", "user_id", userID, "error", err)
		return "", false
	}
	if !transitioned {
		return usagePolicySkipReasonDisabled, false
	}
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, userID)
	}
	return "", true
}

func (s *UsagePolicyService) unbanLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(usagePolicyUnbanPollInterval)
	defer ticker.Stop()
	s.unbanDue(s.ctx)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.unbanDue(s.ctx)
		}
	}
}

func (s *UsagePolicyService) unbanDue(ctx context.Context) {
	if s == nil || s.repo == nil || ctx.Err() != nil {
		return
	}
	ids, err := s.repo.UnbanDueUsers(ctx, usagePolicyUnbanBatchSize)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("usage_policy.unban_due_failed", "error", err)
		}
		return
	}
	if s.authCacheInvalidator == nil {
		return
	}
	for _, id := range ids {
		s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, id)
	}
}

func (s *UsagePolicyService) isRiskControlEnabled(ctx context.Context) bool {
	if s == nil || s.settingRepo == nil {
		return false
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyRiskControlEnabled)
	if err != nil {
		return false
	}
	return raw == "true"
}

func IsUsagePolicyViolation(entry *OpsInsertErrorLogInput) bool {
	if entry == nil {
		return false
	}
	if containsUsagePolicyPhrase(entry.ErrorMessage) || containsUsagePolicyPhrase(entry.ErrorBody) {
		return true
	}
	if entry.UpstreamErrorMessage != nil && containsUsagePolicyPhrase(*entry.UpstreamErrorMessage) {
		return true
	}
	if entry.UpstreamErrorDetail != nil && containsUsagePolicyPhrase(*entry.UpstreamErrorDetail) {
		return true
	}
	for _, event := range entry.UpstreamErrors {
		if event == nil {
			continue
		}
		if containsUsagePolicyPhrase(event.Message) || containsUsagePolicyPhrase(event.Detail) || containsUsagePolicyPhrase(event.UpstreamResponseBody) {
			return true
		}
	}
	return false
}

func containsUsagePolicyPhrase(value string) bool {
	return strings.Contains(strings.ToLower(value), usagePolicyFlagPhrase)
}

func usagePolicyMessage(entry *OpsInsertErrorLogInput) string {
	if entry == nil {
		return ""
	}
	if entry.UpstreamErrorMessage != nil && containsUsagePolicyPhrase(*entry.UpstreamErrorMessage) {
		return strings.TrimSpace(*entry.UpstreamErrorMessage)
	}
	if containsUsagePolicyPhrase(entry.ErrorMessage) {
		return strings.TrimSpace(entry.ErrorMessage)
	}
	if containsUsagePolicyPhrase(entry.ErrorBody) {
		return strings.TrimSpace(entry.ErrorBody)
	}
	if entry.UpstreamErrorDetail != nil && containsUsagePolicyPhrase(*entry.UpstreamErrorDetail) {
		return strings.TrimSpace(*entry.UpstreamErrorDetail)
	}
	for _, event := range entry.UpstreamErrors {
		if event == nil {
			continue
		}
		if containsUsagePolicyPhrase(event.Message) {
			return strings.TrimSpace(event.Message)
		}
		if containsUsagePolicyPhrase(event.Detail) {
			return strings.TrimSpace(event.Detail)
		}
		if containsUsagePolicyPhrase(event.UpstreamResponseBody) {
			return strings.TrimSpace(event.UpstreamResponseBody)
		}
	}
	return strings.TrimSpace(entry.ErrorMessage)
}

// UsagePolicyClientError returns the upstream usage-policy message that should
// be sent to the caller. ok is false when the payload is not a usage-policy hit.
func UsagePolicyClientError(body []byte, extra ...string) (string, bool) {
	candidates := make([]string, 0, 8+len(extra))
	if msg := strings.TrimSpace(extractUpstreamErrorMessage(body)); msg != "" {
		candidates = append(candidates, msg)
	}
	if msg := strings.TrimSpace(gjson.GetBytes(body, "response.error.message").String()); msg != "" {
		candidates = append(candidates, msg)
	}
	if msg := strings.TrimSpace(gjson.GetBytes(body, "error.message").String()); msg != "" {
		candidates = append(candidates, msg)
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if msg := strings.TrimSpace(gjson.GetBytes(payload, "response.error.message").String()); msg != "" {
			candidates = append(candidates, msg)
		}
		if msg := strings.TrimSpace(gjson.GetBytes(payload, "error.message").String()); msg != "" {
			candidates = append(candidates, msg)
		}
	}
	for _, extraMsg := range extra {
		if msg := strings.TrimSpace(extraMsg); msg != "" {
			candidates = append(candidates, msg)
		}
	}
	for _, msg := range candidates {
		if containsUsagePolicyPhrase(msg) {
			return appendUsagePolicyClientNotice(sanitizeUpstreamErrorMessage(msg)), true
		}
	}
	if containsUsagePolicyPhrase(string(body)) {
		return appendUsagePolicyClientNotice("Invalid prompt: your prompt was flagged as potentially violating our usage policy."), true
	}
	return "", false
}

func appendUsagePolicyClientNotice(msg string) string {
	msg = strings.TrimSpace(msg)
	if strings.Contains(msg, UsagePolicyClientNotice) {
		return msg
	}
	if msg == "" {
		return UsagePolicyClientNotice
	}
	return msg + " " + UsagePolicyClientNotice
}

func isOpenAIUsagePolicyError(upstreamMsg string, upstreamBody []byte) bool {
	_, ok := UsagePolicyClientError(upstreamBody, upstreamMsg)
	return ok
}
