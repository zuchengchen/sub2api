package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	usagePolicyFlagPhrase           = "flagged as potentially violating our usage policy"
	defaultUsagePolicyBanThreshold  = 1
	usagePolicySkipReasonAdmin      = "admin"
	usagePolicySkipReasonDisabled   = "already_disabled"
	usagePolicySkipReasonThreshold  = "below_threshold"
	usagePolicySkipReasonAutoBanOff = "auto_ban_disabled"
	usagePolicySkipReasonNoUser     = "missing_user"
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

type UsagePolicyUserStat struct {
	UserID     int64     `json:"user_id"`
	Email      string    `json:"email"`
	Username   string    `json:"username"`
	Role       string    `json:"role"`
	Status     string    `json:"status"`
	Count      int       `json:"count"`
	AutoBanned bool      `json:"auto_banned"`
	LastAt     time.Time `json:"last_at"`
}

type UsagePolicyStats struct {
	Total           int                   `json:"total"`
	UniqueUsers     int                   `json:"unique_users"`
	DisabledUsers   int                   `json:"disabled_users"`
	AutoBannedUsers int                   `json:"auto_banned_users"`
	Users           []UsagePolicyUserStat `json:"users"`
	Config          *UsagePolicyConfig    `json:"config"`
}

type UsagePolicyRepository interface {
	InsertViolation(ctx context.Context, v *UsagePolicyViolation) (id int64, inserted bool, count int, err error)
	UpdateViolationDisposition(ctx context.Context, id int64, autoBanned bool, skipReason string) error
	ListUserStats(ctx context.Context) ([]UsagePolicyUserStat, error)
	DisableUserIfActive(ctx context.Context, userID int64) (bool, error)
}

type UsagePolicyUserReader interface {
	GetByID(ctx context.Context, id int64) (*User, error)
}

type UsagePolicyService struct {
	repo                 UsagePolicyRepository
	settingRepo          SettingRepository
	userRepo             UsagePolicyUserReader
	authCacheInvalidator APIKeyAuthCacheInvalidator
}

func NewUsagePolicyService(
	repo UsagePolicyRepository,
	settingRepo SettingRepository,
	userRepo UsagePolicyUserReader,
	authCacheInvalidator APIKeyAuthCacheInvalidator,
) *UsagePolicyService {
	return &UsagePolicyService{
		repo:                 repo,
		settingRepo:          settingRepo,
		userRepo:             userRepo,
		authCacheInvalidator: authCacheInvalidator,
	}
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
	stats := &UsagePolicyStats{Config: cfg, Users: []UsagePolicyUserStat{}}
	if s.repo == nil {
		return stats, nil
	}
	users, err := s.repo.ListUserStats(ctx)
	if err != nil {
		return nil, err
	}
	if users == nil {
		users = []UsagePolicyUserStat{}
	}
	stats.Users = users
	seen := make(map[int64]struct{}, len(users))
	for _, row := range users {
		stats.Total += row.Count
		if _, ok := seen[row.UserID]; !ok {
			seen[row.UserID] = struct{}{}
			stats.UniqueUsers++
			if row.Status == StatusDisabled {
				stats.DisabledUsers++
			}
			if row.AutoBanned {
				stats.AutoBannedUsers++
			}
		}
	}
	return stats, nil
}

// ObserveErrorLogs records matching upstream usage-policy failures and may disable the user.
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
	skipReason, autoBanned := s.applyBan(ctx, cfg, *entry.UserID, count)
	if skipReason != "" || autoBanned {
		if err := s.repo.UpdateViolationDisposition(ctx, id, autoBanned, skipReason); err != nil {
			slog.Error("usage_policy.update_disposition_failed", "id", id, "user_id", *entry.UserID, "error", err)
		}
	}
	if autoBanned {
		slog.Warn("usage_policy.user_disabled",
			"user_id", *entry.UserID,
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
	if s.userRepo != nil {
		user, err := s.userRepo.GetByID(ctx, userID)
		if err != nil {
			slog.Warn("usage_policy.load_user_failed", "user_id", userID, "error", err)
			return usagePolicySkipReasonNoUser, false
		}
		if user.Role == RoleAdmin {
			return usagePolicySkipReasonAdmin, false
		}
		if user.Status == StatusDisabled {
			return usagePolicySkipReasonDisabled, false
		}
	}
	transitioned, err := s.repo.DisableUserIfActive(ctx, userID)
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
