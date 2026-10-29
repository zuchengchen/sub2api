package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// Account management implementations
func (s *adminServiceImpl) ListAccounts(ctx context.Context, page, pageSize int, platform, accountType, status, search string, groupID int64, privacyMode string, sortBy, sortOrder string) ([]Account, int64, error) {
	if groupID > 0 {
		if err := s.ValidateAccountGroupBindings(ctx, []int64{groupID}); err != nil {
			return nil, 0, err
		}
	}
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	accounts, result, err := s.accountRepo.ListWithFilters(ctx, params, platform, accountType, status, search, groupID, privacyMode)
	if err != nil {
		return nil, 0, err
	}
	return accounts, result.Total, nil
}

func (s *adminServiceImpl) ListAccountsForSchedulerScoreFilter(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	return s.accountRepo.ListAllWithFilters(ctx, platform, accountType, status, search, groupID, privacyMode)
}

func (s *adminServiceImpl) ListOpenAISchedulableAccountsForSchedulerScore(ctx context.Context, groupID *int64) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	if groupID != nil {
		return s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformOpenAI)
	}
	return s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, PlatformOpenAI)
}

func (s *adminServiceImpl) GetAccount(ctx context.Context, id int64) (*Account, error) {
	return s.accountRepo.GetByID(ctx, id)
}

func (s *adminServiceImpl) GetAccountsByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	if len(ids) == 0 {
		return []*Account{}, nil
	}

	accounts, err := s.accountRepo.GetByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("failed to get accounts by IDs: %w", err)
	}

	return accounts, nil
}

const maxAccountNameRunes = 100
const duplicateAccountOperationIDExtraKey = "duplicate_operation_id"

func duplicateAccountName(sourceName string) string {
	const suffix = " (Copy)"
	nameRunes := []rune(strings.TrimSpace(sourceName))
	maxBaseRunes := maxAccountNameRunes - len([]rune(suffix))
	if len(nameRunes) > maxBaseRunes {
		nameRunes = nameRunes[:maxBaseRunes]
	}
	return string(nameRunes) + suffix
}

func cloneAccountJSONMap(value map[string]any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	cloned := make(map[string]any, len(value))
	if err := json.Unmarshal(payload, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

var duplicateAccountDiscardedExtraKeys = map[string]struct{}{
	// A retry identity belongs to the operation that created one copy, not to later copies.
	duplicateAccountOperationIDExtraKey: {},
	// External sync identity belongs to one local account only.
	"crs_account_id": {},
	"crs_kind":       {},
	"crs_synced_at":  {},
	// Local quota usage and derived window timestamps must start fresh.
	"quota_used":            {},
	"quota_daily_used":      {},
	"quota_weekly_used":     {},
	"quota_daily_start":     {},
	"quota_weekly_start":    {},
	"quota_daily_reset_at":  {},
	"quota_weekly_reset_at": {},
	// Provider observations, capability probes, and transient scheduling state.
	"model_rate_limits":               {},
	"session_window_utilization":      {},
	"passive_usage_7d_utilization":    {},
	"passive_usage_7d_reset":          {},
	"passive_usage_7d_oi_utilization": {},
	"passive_usage_7d_oi_reset":       {},
	"passive_usage_sampled_at":        {},
	"grok_usage_snapshot":             {},
	"grok_billing_snapshot":           {},
	"openai_responses_supported":      {},
	"openai_compact_supported":        {},
	"openai_compact_checked_at":       {},
	"openai_compact_last_status":      {},
	"openai_compact_last_error":       {},
	"drive_storage_limit":             {},
	"drive_storage_usage":             {},
	"drive_tier_updated_at":           {},
	// Codex fingerprint convergence uses a per-account random seed, never copied from another account.
	codexFingerprintSeedExtraKey:           {},
	"codex_primary_used_percent":           {},
	"codex_primary_reset_after_seconds":    {},
	"codex_primary_window_minutes":         {},
	"codex_secondary_used_percent":         {},
	"codex_secondary_reset_after_seconds":  {},
	"codex_secondary_window_minutes":       {},
	"codex_primary_over_secondary_percent": {},
	"codex_usage_updated_at":               {},
	"codex_5h_used_percent":                {},
	"codex_5h_reset_after_seconds":         {},
	"codex_5h_window_minutes":              {},
	"codex_5h_reset_at":                    {},
	"codex_7d_used_percent":                {},
	"codex_7d_reset_after_seconds":         {},
	"codex_7d_window_minutes":              {},
	"codex_7d_reset_at":                    {},
}

func duplicateAccountExtra(value map[string]any) (map[string]any, error) {
	cloned, err := cloneAccountJSONMap(value)
	if err != nil {
		return nil, err
	}
	for key := range duplicateAccountDiscardedExtraKeys {
		delete(cloned, key)
	}
	return cloned, nil
}

func canDuplicateAccountType(accountType string) bool {
	switch accountType {
	case AccountTypeAPIKey, AccountTypeUpstream, AccountTypeBedrock, AccountTypeServiceAccount:
		return true
	default:
		return false
	}
}

func duplicateAccountGroups(source *Account) ([]AccountGroup, []int64) {
	if len(source.AccountGroups) > 0 {
		groups := make([]AccountGroup, 0, len(source.AccountGroups))
		groupIDs := make([]int64, 0, len(source.AccountGroups))
		for _, sourceGroup := range source.AccountGroups {
			groups = append(groups, AccountGroup{GroupID: sourceGroup.GroupID, Priority: sourceGroup.Priority})
			groupIDs = append(groupIDs, sourceGroup.GroupID)
		}
		return groups, groupIDs
	}

	groups := make([]AccountGroup, 0, len(source.GroupIDs))
	groupIDs := append([]int64(nil), source.GroupIDs...)
	for i, groupID := range groupIDs {
		groups = append(groups, AccountGroup{GroupID: groupID, Priority: i + 1})
	}
	return groups, groupIDs
}

func duplicateAccountOperationID(sourceID int64, actorScope, operationKey string) string {
	operationKey = strings.TrimSpace(operationKey)
	if operationKey == "" {
		return ""
	}
	actorScope = strings.TrimSpace(actorScope)
	if actorScope == "" {
		actorScope = "admin:0"
	}
	payload := "admin.accounts.duplicate\x00" + actorScope + "\x00" + strconv.FormatInt(sourceID, 10) + "\x00" + operationKey
	digest := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", digest)
}

func (s *adminServiceImpl) findDuplicateByOperationID(ctx context.Context, operationID string) (*Account, error) {
	if operationID == "" {
		return nil, nil
	}
	accounts, err := s.accountRepo.FindByExtraField(ctx, duplicateAccountOperationIDExtraKey, operationID)
	if err != nil {
		return nil, fmt.Errorf("find duplicate account operation: %w", err)
	}
	if len(accounts) == 0 {
		return nil, nil
	}
	account := accounts[0]
	return &account, nil
}

// RecoverDuplicateAccount performs a read-only lookup for an already committed duplicate.
// It is used when the idempotency coordinator cannot confirm whether response persistence
// succeeded, and deliberately never repeats the create side effect.
func (s *adminServiceImpl) RecoverDuplicateAccount(ctx context.Context, id int64, actorScope, operationKey string) (*Account, error) {
	return s.findDuplicateByOperationID(ctx, duplicateAccountOperationID(id, actorScope, operationKey))
}

func cloneAccountValuePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// DuplicateAccount creates a paused account from source configuration without carrying first-class
// runtime state. Credentials and extra configuration are deep-copied so normalization of the new
// account cannot mutate the in-memory source.
func (s *adminServiceImpl) DuplicateAccount(ctx context.Context, id int64, actorScope, operationKey string) (*Account, error) {
	operationID := duplicateAccountOperationID(id, actorScope, operationKey)
	existing, err := s.RecoverDuplicateAccount(ctx, id, actorScope, operationKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	source, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !canDuplicateAccountType(source.Type) {
		return nil, infraerrors.BadRequest(
			"ACCOUNT_DUPLICATE_CREDENTIAL_TYPE_UNSUPPORTED",
			"accounts with rotating or unsupported credential types cannot be duplicated",
		)
	}

	credentials, err := cloneAccountJSONMap(source.Credentials)
	if err != nil {
		return nil, fmt.Errorf("clone account credentials: %w", err)
	}
	extra, err := duplicateAccountExtra(source.Extra)
	if err != nil {
		return nil, fmt.Errorf("clone account extra configuration: %w", err)
	}
	if operationID != "" {
		if extra == nil {
			extra = make(map[string]any, 1)
		}
		extra[duplicateAccountOperationIDExtraKey] = operationID
	}

	var expiresAt *int64
	if source.ExpiresAt != nil {
		unix := source.ExpiresAt.Unix()
		expiresAt = &unix
	}
	autoPauseOnExpired := source.AutoPauseOnExpired
	groups, groupIDs := duplicateAccountGroups(source)
	if err := s.ValidateAccountGroupBindings(ctx, groupIDs); err != nil {
		return nil, err
	}
	proxyID := source.ProxyID
	if source.ProxyFallbackOriginID != nil {
		// Proxy fallback is transient runtime state; duplicate the configured origin.
		proxyID = source.ProxyFallbackOriginID
	}
	input := &CreateAccountInput{
		Name:                  duplicateAccountName(source.Name),
		Notes:                 cloneAccountValuePointer(source.Notes),
		Platform:              source.Platform,
		Type:                  source.Type,
		Credentials:           credentials,
		Extra:                 extra,
		ProxyID:               cloneAccountValuePointer(proxyID),
		Concurrency:           source.Concurrency,
		Priority:              source.Priority,
		RateMultiplier:        cloneAccountValuePointer(source.RateMultiplier),
		LoadFactor:            cloneAccountValuePointer(source.LoadFactor),
		GroupIDs:              groupIDs,
		ExpiresAt:             expiresAt,
		AutoPauseOnExpired:    &autoPauseOnExpired,
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
	}
	accountExtra, err := normalizeOpenAILongContextBillingExtra(input.Platform, input.Extra)
	if err != nil {
		return nil, fmt.Errorf("normalize duplicate account extra: %w", err)
	}
	if err := NormalizeHeaderOverrideCredentials(input.Credentials); err != nil {
		return nil, err
	}
	input.Credentials, accountExtra, err = normalizeOpenAICompatibleProviderConfig(
		input.Platform,
		input.Type,
		input.Credentials,
		accountExtra,
	)
	if err != nil {
		return nil, fmt.Errorf("normalize duplicate provider preset: %w", err)
	}
	if err := NormalizeProtocolRulesCredentials(input.Credentials); err != nil {
		return nil, err
	}
	duplicate, err := buildAccountForCreate(input, accountExtra)
	if err != nil {
		return nil, err
	}
	// A copied credential must be reviewed before it can share live traffic with its source.
	duplicate.Schedulable = false
	if s.accountDuplicateRepo == nil {
		return nil, errors.New("account duplicate repository is not configured")
	}
	if err := s.accountDuplicateRepo.CreateWithAccountGroups(ctx, duplicate, groups); err != nil {
		return nil, fmt.Errorf("create duplicate account: %w", err)
	}
	for i := range groups {
		groups[i].AccountID = duplicate.ID
	}
	duplicate.AccountGroups = groups
	duplicate.GroupIDs = groupIDs
	return duplicate, nil
}

func normalizeAccountConcurrency(platform, accountType string, concurrency int) int {
	if platform == PlatformGrok && accountType == AccountTypeOAuth {
		if concurrency <= 0 {
			return 1
		}
	}
	return concurrency
}

// ValidateOpenAILongContextBillingExtra validates the OpenAI account billing flag when present.
func ValidateOpenAILongContextBillingExtra(platform string, extra map[string]any) error {
	if platform != PlatformOpenAI {
		return nil
	}
	raw, exists := extra[openAILongContextBillingEnabledKey]
	if !exists {
		return nil
	}
	if _, ok := raw.(bool); !ok {
		return infraerrors.BadRequest(
			"OPENAI_LONG_CONTEXT_BILLING_INVALID",
			"openai_long_context_billing_enabled must be a boolean",
		)
	}
	return nil
}

func normalizeOpenAILongContextBillingExtra(platform string, extra map[string]any) (map[string]any, error) {
	if platform != PlatformOpenAI {
		return extra, nil
	}
	if err := ValidateOpenAILongContextBillingExtra(platform, extra); err != nil {
		return nil, err
	}

	normalized := maps.Clone(extra)
	if normalized == nil {
		normalized = make(map[string]any, 1)
	}
	_, exists := normalized[openAILongContextBillingEnabledKey]
	if !exists {
		normalized[openAILongContextBillingEnabledKey] = true
	}
	return normalized, nil
}

func normalizeOpenAILongContextBillingUpdateExtra(account *Account, input *UpdateAccountInput) (map[string]any, error) {
	normalized, err := normalizeOpenAILongContextBillingExtra(account.Platform, input.Extra)
	if err != nil || account.Platform != PlatformOpenAI {
		return normalized, err
	}

	_, provided := input.Extra[openAILongContextBillingEnabledKey]
	current, hasCurrent := account.Extra[openAILongContextBillingEnabledKey].(bool)
	if !provided {
		if hasCurrent {
			normalized[openAILongContextBillingEnabledKey] = current
		}
	}
	return normalized, nil
}

// Grok media eligibility helpers live in account_grok_media_eligibility.go.

func buildAccountForCreate(input *CreateAccountInput, accountExtra map[string]any) (*Account, error) {
	// 兜住所有建号入口（含批量创建、数据导入、复制账号）：平台必须有对应转发器。
	if !isConcreteRequestPlatform(strings.TrimSpace(input.Platform)) {
		return nil, ErrAccountUnsupportedPlatform
	}
	accountExtra = MergeOpenAICodexTicketExtra(accountExtra, nil)
	// Probe/session state is system-managed. New accounts always start with automatic refresh disabled.
	delete(accountExtra, UpstreamBillingProbeEnabledExtraKey)
	delete(accountExtra, UpstreamBillingRateSyncEnabledExtraKey)
	delete(accountExtra, UpstreamBillingProbeExtraKey)
	delete(accountExtra, OllamaCloudUsageSessionExtraKey)
	delete(accountExtra, OllamaCloudUsageAutoRefreshExtraKey)
	delete(accountExtra, OllamaCloudUsageSnapshotExtraKey)
	delete(accountExtra, OpenCodeGoUsageAutoRefreshExtraKey)
	delete(accountExtra, OpenCodeGoUsageSnapshotExtraKey)
	accountExtra = prepareCodexFingerprintExtraForCreate(input.Platform, input.Type, accountExtra)
	account := &Account{
		Name:        input.Name,
		Notes:       normalizeAccountNotes(input.Notes),
		Platform:    input.Platform,
		Type:        input.Type,
		Credentials: input.Credentials,
		Extra:       accountExtra,
		ProxyID:     input.ProxyID,
		Concurrency: normalizeAccountConcurrency(input.Platform, input.Type, input.Concurrency),
		Priority:    input.Priority,
		Status:      StatusActive,
		Schedulable: true,
	}
	if input.ProbeEnabled != nil && *input.ProbeEnabled {
		if !isUpstreamBillingProbeAccount(account) {
			return nil, ErrUpstreamBillingProbeAccountInvalid
		}
		if account.Extra == nil {
			account.Extra = make(map[string]any)
		}
		account.Extra[UpstreamBillingProbeEnabledExtraKey] = true
	}
	// 预计算固定时间重置的下次重置时间
	if account.Extra != nil {
		if err := ValidateQuotaResetConfig(account.Extra); err != nil {
			return nil, err
		}
		ComputeQuotaResetAt(account.Extra)
		NormalizeFixedQuotaWindows(account.Extra)
	}
	if input.ExpiresAt != nil && *input.ExpiresAt > 0 {
		expiresAt := time.Unix(*input.ExpiresAt, 0)
		account.ExpiresAt = &expiresAt
	}
	if input.AutoPauseOnExpired != nil {
		account.AutoPauseOnExpired = *input.AutoPauseOnExpired
	} else {
		account.AutoPauseOnExpired = true
	}
	if input.RateMultiplier != nil {
		if *input.RateMultiplier < 0 {
			return nil, errors.New("rate_multiplier must be >= 0")
		}
		account.RateMultiplier = input.RateMultiplier
	}
	if input.LoadFactor != nil && *input.LoadFactor > 0 {
		if *input.LoadFactor > 10000 {
			return nil, errors.New("load_factor must be <= 10000")
		}
		account.LoadFactor = input.LoadFactor
	}
	return account, nil
}

func (s *adminServiceImpl) CreateAccount(ctx context.Context, input *CreateAccountInput) (*Account, error) {
	accountExtra, err := normalizeOpenAILongContextBillingExtra(input.Platform, input.Extra)
	if err != nil {
		return nil, err
	}
	accountExtra, err = normalizeGrokMediaEligibilityExtra(input.Platform, accountExtra)
	if err != nil {
		return nil, err
	}
	accountExtra, err = normalizeOpenAIAutoResetCreditExtra(input.Platform, input.Type, false, accountExtra)
	if err != nil {
		return nil, err
	}
	if err := ValidateUpstreamRequestIDHeaderExtra(accountExtra); err != nil {
		return nil, err
	}

	// 绑定分组
	groupIDs := input.GroupIDs
	// 如果没有指定分组,自动绑定对应平台的默认分组
	if len(groupIDs) == 0 && !input.SkipDefaultGroupBind {
		defaultGroupName := input.Platform + "-default"
		groups, err := s.groupRepo.ListActiveByPlatform(ctx, input.Platform)
		if err == nil {
			for _, g := range groups {
				if g.Name == defaultGroupName {
					groupIDs = []int64{g.ID}
					break
				}
			}
		}
	}

	// 检查混合渠道风险（除非用户已确认）
	if len(groupIDs) > 0 && !input.SkipMixedChannelCheck {
		if err := s.checkMixedChannelRisk(ctx, 0, input.Platform, groupIDs); err != nil {
			return nil, err
		}
	}

	// 校验并规范化请求头覆写配置（header 名小写化、格式检查）
	if err := NormalizeHeaderOverrideCredentials(input.Credentials); err != nil {
		return nil, err
	}
	if err := NormalizeProtocolRulesCredentials(input.Credentials); err != nil {
		return nil, err
	}
	// Never persist ephemeral SSO/password secrets after OAuth conversion.
	input.Credentials = SanitizeStoredCredentials(input.Platform, input.Credentials)
	input.Credentials, accountExtra, err = normalizeOpenAICompatibleProviderConfig(
		input.Platform,
		input.Type,
		input.Credentials,
		accountExtra,
	)
	if err != nil {
		return nil, err
	}

	account, err := buildAccountForCreate(input, accountExtra)
	if err != nil {
		return nil, err
	}
	if err := s.ValidateAccountGroupBindings(ctx, groupIDs); err != nil {
		return nil, err
	}
	if err := s.accountRepo.Create(ctx, account); err != nil {
		return nil, err
	}

	// 绑定分组
	if len(groupIDs) > 0 {
		if err := s.accountRepo.BindGroups(ctx, account.ID, groupIDs); err != nil {
			return nil, err
		}
	}

	AfterAccountCommit(ctx, func(_ context.Context) {
		// OAuth 账号：创建后异步设置隐私。
		// 使用 Ensure（幂等）而非 Force：新建账号 Extra 为空时效果相同，但更安全。
		if account.Type == AccountTypeOAuth {
			switch account.Platform {
			case PlatformOpenAI:
				go func() {
					defer func() {
						if r := recover(); r != nil {
							slog.Error("create_account_openai_privacy_panic", "account_id", account.ID, "recover", r)
						}
					}()
					s.EnsureOpenAIPrivacy(context.Background(), account)
				}()
			}
		}
	})

	return account, nil
}

func (s *adminServiceImpl) UpdateAccount(ctx context.Context, id int64, input *UpdateAccountInput) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	var normalizedExtra map[string]any
	if input.Extra != nil {
		normalizedExtra, err = normalizeOpenAILongContextBillingUpdateExtra(account, input)
		if err != nil {
			return nil, err
		}
		normalizedExtra, err = normalizeGrokMediaEligibilityUpdateExtra(account, input, normalizedExtra)
		if err != nil {
			return nil, err
		}
		effectiveType := account.Type
		if input.Type != "" {
			effectiveType = input.Type
		}
		normalizedExtra, err = normalizeOpenAIAutoResetCreditExtra(account.Platform, effectiveType, false, normalizedExtra)
		if err != nil {
			return nil, err
		}
		if err := ValidateUpstreamRequestIDHeaderExtra(normalizedExtra); err != nil {
			return nil, err
		}
	}
	previousProbeIdentity := upstreamBillingProbeIdentity(account)
	previousOllamaUsageIdentity := ollamaCloudUsageIdentity(account)
	previousOpenCodeUsageIdentity := openCodeGoUsageIdentity(account)
	if input.Name != "" {
		account.Name = input.Name
	}
	if input.Type != "" {
		account.Type = input.Type
	}
	if input.Notes != nil {
		account.Notes = normalizeAccountNotes(input.Notes)
	}
	if len(input.Credentials) > 0 {
		// 敏感子键采用"incoming 没提供就保留"的合并语义：前端响应已脱敏，
		// 全对象 PUT 编辑时不会再带回 token，避免覆盖时清空已有凭证。
		account.Credentials = MergePreservingSensitiveCreds(account.Credentials, input.Credentials)
		// 校验并规范化请求头覆写配置（header 名小写化、格式检查）
		if err := NormalizeHeaderOverrideCredentials(account.Credentials); err != nil {
			return nil, err
		}
		if err := NormalizeProtocolRulesCredentials(account.Credentials); err != nil {
			return nil, err
		}
		// Strip SSO/password residue that must never sit next to OAuth tokens.
		account.Credentials = SanitizeStoredCredentials(account.Platform, account.Credentials)
	}
	// Extra 使用 map：需要区分“未提供(nil)”与“显式清空({})”。
	// 关闭配额限制时前端会删除 quota_* 键并提交 extra:{}，此时也必须落库。
	requestedProbeEnabledUpdate := input.ProbeEnabled
	requestedRateSyncEnabledUpdate := input.RateSyncEnabled
	if input.Extra != nil {
		requestedProbeEnabled, hasRequestedProbeEnabled := normalizedExtra[UpstreamBillingProbeEnabledExtraKey]
		if hasRequestedProbeEnabled {
			enabled, ok := requestedProbeEnabled.(bool)
			if !ok {
				return nil, infraerrors.BadRequest("INVALID_UPSTREAM_BILLING_PROBE_ENABLED", "upstream_billing_probe_enabled must be a boolean")
			}
			if requestedProbeEnabledUpdate != nil && *requestedProbeEnabledUpdate != enabled {
				return nil, infraerrors.BadRequest("CONFLICTING_UPSTREAM_BILLING_PROBE_ENABLED", "conflicting upstream_billing_probe_enabled values")
			}
			requestedProbeEnabledUpdate = &enabled
		}
		delete(normalizedExtra, UpstreamBillingProbeEnabledExtraKey)
		delete(normalizedExtra, UpstreamBillingRateSyncEnabledExtraKey)
		delete(normalizedExtra, UpstreamBillingProbeExtraKey)
		delete(normalizedExtra, OllamaCloudUsageSessionExtraKey)
		delete(normalizedExtra, OllamaCloudUsageAutoRefreshExtraKey)
		delete(normalizedExtra, OllamaCloudUsageSnapshotExtraKey)
		delete(normalizedExtra, OpenCodeGoUsageAutoRefreshExtraKey)
		delete(normalizedExtra, OpenCodeGoUsageSnapshotExtraKey)
		// 目录观察和能力 metadata 只由同步写入。表单里的旧值或注入值不能进入本次更新。
		delete(normalizedExtra, grokObservedModelsExtraKey)
		delete(normalizedExtra, UpstreamModelMetadataExtraKey)
		// 保留配额用量和专用服务受管字段，防止普通账号编辑意外覆盖。
		for _, key := range []string{
			"quota_used",
			"quota_daily_used",
			"quota_daily_start",
			"quota_weekly_used",
			"quota_weekly_start",
			grokBillingExtraKey,
			grokObservedModelsExtraKey,
			UpstreamModelMetadataExtraKey,
			UpstreamBillingProbeEnabledExtraKey,
			UpstreamBillingRateSyncEnabledExtraKey,
			UpstreamBillingProbeExtraKey,
			OllamaCloudUsageSessionExtraKey,
			OllamaCloudUsageAutoRefreshExtraKey,
			OllamaCloudUsageSnapshotExtraKey,
			OpenCodeGoUsageAutoRefreshExtraKey,
			OpenCodeGoUsageSnapshotExtraKey,
		} {
			if v, ok := account.Extra[key]; ok {
				normalizedExtra[key] = v
			}
		}
		normalizedExtra = MergeOpenAICodexTicketExtra(normalizedExtra, account.Extra)
		normalizedExtra = prepareCodexFingerprintExtraForUpdate(account, normalizedExtra)
		normalizedExtra, err = mergeAccountProtectionForSave(ctx, account, normalizedExtra)
		if err != nil {
			return nil, err
		}
		account.Extra = normalizedExtra
		// 校验并预计算固定时间重置的下次重置时间
		if err := ValidateQuotaResetConfig(account.Extra); err != nil {
			return nil, err
		}
		ComputeQuotaResetAt(account.Extra)
		NormalizeFixedQuotaWindows(account.Extra)
	}
	if input.Extra == nil {
		account.Extra = prepareCodexFingerprintExtraForUpdate(account, account.Extra)
	}
	if requestedRateSyncEnabledUpdate != nil && *requestedRateSyncEnabledUpdate {
		if requestedProbeEnabledUpdate != nil && !*requestedProbeEnabledUpdate {
			return nil, infraerrors.BadRequest(
				"UPSTREAM_BILLING_RATE_SYNC_REQUIRES_PROBE",
				"upstream billing rate sync requires upstream billing probe",
			)
		}
		enabled := true
		requestedProbeEnabledUpdate = &enabled
	}
	if requestedProbeEnabledUpdate != nil && !*requestedProbeEnabledUpdate {
		disabled := false
		requestedRateSyncEnabledUpdate = &disabled
	}
	if (requestedProbeEnabledUpdate != nil && *requestedProbeEnabledUpdate) ||
		(requestedRateSyncEnabledUpdate != nil && *requestedRateSyncEnabledUpdate) {
		if !isUpstreamBillingProbeAccount(account) {
			return nil, ErrUpstreamBillingProbeAccountInvalid
		}
	}
	if account.Extra == nil && (requestedProbeEnabledUpdate != nil || requestedRateSyncEnabledUpdate != nil) {
		account.Extra = make(map[string]any)
	}
	if requestedProbeEnabledUpdate != nil {
		account.Extra[UpstreamBillingProbeEnabledExtraKey] = *requestedProbeEnabledUpdate
	}
	if requestedRateSyncEnabledUpdate != nil {
		account.Extra[UpstreamBillingRateSyncEnabledExtraKey] = *requestedRateSyncEnabledUpdate
	}
	if input.ProxyID != nil {
		// 0 表示清除代理（前端发送 0 而不是 null 来表达清除意图）
		if *input.ProxyID == 0 {
			account.ProxyID = nil
		} else {
			account.ProxyID = input.ProxyID
		}
		account.Proxy = nil // 清除关联对象，防止 GORM Save 时根据 Proxy.ID 覆盖 ProxyID
	}
	if !reflect.DeepEqual(previousProbeIdentity, upstreamBillingProbeIdentity(account)) && account.Extra != nil {
		delete(account.Extra, UpstreamBillingProbeExtraKey)
		if !isUpstreamBillingProbeAccount(account) {
			delete(account.Extra, UpstreamBillingProbeEnabledExtraKey)
			delete(account.Extra, UpstreamBillingRateSyncEnabledExtraKey)
		}
	}
	if account.Extra != nil {
		if !IsOllamaCloudUsageAccount(account) {
			delete(account.Extra, OllamaCloudUsageSessionExtraKey)
			delete(account.Extra, OllamaCloudUsageAutoRefreshExtraKey)
			delete(account.Extra, OllamaCloudUsageSnapshotExtraKey)
		} else if !reflect.DeepEqual(previousOllamaUsageIdentity, ollamaCloudUsageIdentity(account)) {
			delete(account.Extra, OllamaCloudUsageSessionExtraKey)
			delete(account.Extra, OllamaCloudUsageAutoRefreshExtraKey)
			delete(account.Extra, OllamaCloudUsageSnapshotExtraKey)
		}
	}
	// OpenCode Go 受管键：身份改变或不再 eligible 时随本次写入清除，防止跨组污染。
	// （代理变化只失效快照而保留开关，由 repository 合并层在锁定的 DB 行上裁决。）
	if account.Extra != nil {
		if !IsOpenCodeGoUsageAccount(account) {
			delete(account.Extra, OpenCodeGoUsageAutoRefreshExtraKey)
			delete(account.Extra, OpenCodeGoUsageSnapshotExtraKey)
		} else if !reflect.DeepEqual(previousOpenCodeUsageIdentity, openCodeGoUsageIdentity(account)) {
			delete(account.Extra, OpenCodeGoUsageAutoRefreshExtraKey)
			delete(account.Extra, OpenCodeGoUsageSnapshotExtraKey)
		}
	}
	// 只在指针非 nil 时更新 Concurrency（支持设置为 0）
	if input.Concurrency != nil {
		account.Concurrency = normalizeAccountConcurrency(account.Platform, account.Type, *input.Concurrency)
	}
	// 只在指针非 nil 时更新 Priority（支持设置为 0）
	if input.Priority != nil {
		account.Priority = *input.Priority
	}
	if input.RateMultiplier != nil {
		if *input.RateMultiplier < 0 {
			return nil, errors.New("rate_multiplier must be >= 0")
		}
		// 同步开启时倍率归上游所有，手工值活不过下一次成功探测（表现为"改了又自己
		// 变回去"），与批量路径一样直接拒绝。判断的是本次请求生效后的状态：上面
		// 已把请求携带的两个开关落进 account.Extra，所以"同一请求关闭同步 + 改倍率"
		// （用户显式收回所有权）会走到这里时读到 false，正常放行。
		if upstreamBillingRateSyncEnabled(account) {
			return nil, ErrUpstreamBillingRateSyncConflict
		}
		account.RateMultiplier = input.RateMultiplier
	}
	if input.LoadFactor != nil {
		if *input.LoadFactor <= 0 {
			account.LoadFactor = nil // 0 或负数表示清除
		} else if *input.LoadFactor > 10000 {
			return nil, errors.New("load_factor must be <= 10000")
		} else {
			account.LoadFactor = input.LoadFactor
		}
	}
	if input.Status != "" {
		account.Status = input.Status
	}
	account.Credentials, account.Extra, err = normalizeOpenAICompatibleProviderConfig(
		account.Platform,
		account.Type,
		account.Credentials,
		account.Extra,
	)
	if err != nil {
		return nil, err
	}
	if input.ExpiresAt != nil {
		if *input.ExpiresAt <= 0 {
			account.ExpiresAt = nil
		} else {
			expiresAt := time.Unix(*input.ExpiresAt, 0)
			account.ExpiresAt = &expiresAt
		}
	}
	if input.AutoPauseOnExpired != nil {
		account.AutoPauseOnExpired = *input.AutoPauseOnExpired
	}

	// 先验证分组是否存在（在任何写操作之前）
	if input.GroupIDs != nil {
		if err := s.validateGroupIDsExist(ctx, *input.GroupIDs); err != nil {
			return nil, err
		}
		if err := s.ValidateAccountGroupBindings(ctx, *input.GroupIDs); err != nil {
			return nil, err
		}

		// 检查混合渠道风险（除非用户已确认）
		if !input.SkipMixedChannelCheck {
			if err := s.checkMixedChannelRisk(ctx, account.ID, account.Platform, *input.GroupIDs); err != nil {
				return nil, err
			}
		}
	}

	billingSettingsAppliedAtomically := false
	updater := s.accountBillingRepo
	if updater == nil {
		// Unit tests and narrow internal callers may construct adminServiceImpl
		// directly; production wiring requires this capability through
		// AdminAccountRepository.
		updater, _ = s.accountRepo.(AccountBillingSettingsRepository)
	}
	if updater != nil {
		if err := updater.UpdateWithAccountBillingSettings(
			ctx,
			account,
			requestedProbeEnabledUpdate,
			requestedRateSyncEnabledUpdate,
			input.RateMultiplier,
		); err != nil {
			return nil, err
		}
		billingSettingsAppliedAtomically = true
	}
	if !billingSettingsAppliedAtomically {
		if err := s.accountRepo.Update(ctx, account); err != nil {
			return nil, err
		}
		if (requestedProbeEnabledUpdate != nil || requestedRateSyncEnabledUpdate != nil) &&
			isUpstreamBillingProbeAccount(account) {
			settings := make(map[string]any, 2)
			if requestedProbeEnabledUpdate != nil {
				settings[UpstreamBillingProbeEnabledExtraKey] = *requestedProbeEnabledUpdate
			}
			if requestedRateSyncEnabledUpdate != nil {
				settings[UpstreamBillingRateSyncEnabledExtraKey] = *requestedRateSyncEnabledUpdate
			}
			if err := s.accountRepo.UpdateExtra(ctx, account.ID, settings); err != nil {
				return nil, err
			}
		}
	}

	// 绑定分组
	if input.GroupIDs != nil {
		if err := s.accountRepo.BindGroups(ctx, account.ID, *input.GroupIDs); err != nil {
			return nil, err
		}
	}

	// 重新查询以确保返回完整数据（包括正确的 Proxy 关联对象）
	updated, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// UpdateAccountExtra 仅对 Extra JSONB 做 key 级合并，避免覆盖其它运行态键
// （如 model_rate_limits / passive_usage_* 等）。
func (s *adminServiceImpl) UpdateAccountExtra(ctx context.Context, id int64, updates map[string]any) error {
	updates = MergeOpenAICodexTicketExtra(updates, nil)
	updates = sanitizedCodexFingerprintExtraUpdates(updates)
	// Managed provider presets must be changed through UpdateAccount so the
	// provider marker, base URL, model mapping, transport, and runtime policy
	// remain one atomic contract.
	delete(updates, openai_compat.ExtraKeyCompatibleProvider)
	updates = stripOpenAIAutoResetCreditManagedExtra(updates, true)
	delete(updates, UpstreamBillingProbeEnabledExtraKey)
	delete(updates, UpstreamBillingRateSyncEnabledExtraKey)
	delete(updates, UpstreamBillingProbeExtraKey)
	delete(updates, OllamaCloudUsageSessionExtraKey)
	delete(updates, OllamaCloudUsageAutoRefreshExtraKey)
	delete(updates, OllamaCloudUsageSnapshotExtraKey)
	delete(updates, OpenCodeGoUsageAutoRefreshExtraKey)
	delete(updates, OpenCodeGoUsageSnapshotExtraKey)
	if _, exists := updates[openAILongContextBillingEnabledKey]; exists {
		account, err := s.accountRepo.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if err := ValidateOpenAILongContextBillingExtra(account.Platform, updates); err != nil {
			return err
		}
	}
	if len(updates) == 0 {
		return nil
	}
	return s.accountRepo.UpdateExtra(ctx, id, updates)
}

// BulkUpdateAccounts updates multiple accounts in one request.
// It merges credentials/extra keys instead of overwriting the whole object.
func (s *adminServiceImpl) BulkUpdateAccounts(ctx context.Context, input *BulkUpdateAccountsInput) (*BulkUpdateAccountsResult, error) {
	// Managed probe/session state may only enter through dedicated typed endpoints.
	input.Extra = MergeOpenAICodexTicketExtra(input.Extra, nil)
	input.Extra = sanitizedCodexFingerprintExtraUpdates(input.Extra)
	// A bulk marker update cannot safely apply provider-specific credentials to
	// heterogeneous accounts. Require the single-account managed preset path.
	delete(input.Extra, openai_compat.ExtraKeyCompatibleProvider)
	// Managed probe/session state may only enter through dedicated typed endpoints.
	input.Extra = stripOpenAIAutoResetCreditManagedExtra(input.Extra, true)
	delete(input.Extra, UpstreamBillingProbeEnabledExtraKey)
	delete(input.Extra, UpstreamBillingRateSyncEnabledExtraKey)
	delete(input.Extra, UpstreamBillingProbeExtraKey)
	delete(input.Extra, OllamaCloudUsageSessionExtraKey)
	delete(input.Extra, OllamaCloudUsageAutoRefreshExtraKey)
	delete(input.Extra, OllamaCloudUsageSnapshotExtraKey)
	delete(input.Extra, OpenCodeGoUsageAutoRefreshExtraKey)
	delete(input.Extra, OpenCodeGoUsageSnapshotExtraKey)

	if len(input.AccountIDs) == 0 && input.Filters != nil {
		accountIDs, err := s.resolveBulkUpdateTargetIDs(ctx, input.Filters)
		if err != nil {
			return nil, err
		}
		input.AccountIDs = accountIDs
	}

	result := &BulkUpdateAccountsResult{
		SuccessIDs: make([]int64, 0, len(input.AccountIDs)),
		FailedIDs:  make([]int64, 0, len(input.AccountIDs)),
		Results:    make([]BulkUpdateAccountResult, 0, len(input.AccountIDs)),
	}

	if len(input.AccountIDs) == 0 {
		return result, nil
	}
	if input.GroupIDs != nil {
		if err := s.validateGroupIDsExist(ctx, *input.GroupIDs); err != nil {
			return nil, err
		}
		if err := s.ValidateAccountGroupBindings(ctx, *input.GroupIDs); err != nil {
			return nil, err
		}
	}
	openAISettings, err := normalizeBulkOpenAISettings(input)
	if err != nil {
		return nil, err
	}

	needMixedChannelCheck := input.GroupIDs != nil && !input.SkipMixedChannelCheck

	// 预取目标账号供批量设置验证和混合渠道检查使用。
	var cachedTargets []*Account
	if len(input.Credentials) > 0 || input.ProxyID != nil || needMixedChannelCheck || openAISettings.any() || input.ProbeEnabled != nil || input.RateMultiplier != nil {
		loaded, err := s.accountRepo.GetByIDs(ctx, input.AccountIDs)
		if err != nil {
			return nil, err
		}
		cachedTargets = loaded
	}
	targetsByID := make(map[int64]*Account, len(cachedTargets))
	for _, account := range cachedTargets {
		if account != nil {
			targetsByID[account.ID] = account
		}
	}
	if openAISettings.any() {
		if err := validateBulkOpenAISettingsTargets(input, openAISettings, targetsByID); err != nil {
			return nil, err
		}
	}
	if input.ProbeEnabled != nil {
		for _, accountID := range input.AccountIDs {
			account, ok := targetsByID[accountID]
			if !ok {
				return nil, ErrAccountNotFound
			}
			if !isUpstreamBillingProbeAccount(account) {
				return nil, ErrUpstreamBillingProbeAccountInvalid
			}
		}
	}

	// 预加载账号平台信息（混合渠道检查需要）。
	platformByID := map[int64]string{}
	if needMixedChannelCheck {
		for _, account := range cachedTargets {
			if account != nil {
				platformByID[account.ID] = account.Platform
			}
		}
	}

	// 预检查混合渠道风险：在任何写操作之前，若发现风险立即返回错误。
	if needMixedChannelCheck {
		for _, accountID := range input.AccountIDs {
			platform := platformByID[accountID]
			if platform == "" {
				continue
			}
			if err := s.checkMixedChannelRisk(ctx, accountID, platform, *input.GroupIDs); err != nil {
				return nil, err
			}
		}
	}

	if input.RateMultiplier != nil {
		if *input.RateMultiplier < 0 {
			return nil, errors.New("rate_multiplier must be >= 0")
		}
		syncEnabledCount := 0
		for _, account := range cachedTargets {
			if account == nil || account.Extra == nil {
				continue
			}
			enabled, _ := account.Extra[UpstreamBillingRateSyncEnabledExtraKey].(bool)
			if enabled {
				syncEnabledCount++
			}
		}
		if syncEnabledCount > 0 {
			return nil, ErrUpstreamBillingRateSyncBulkConflict.WithMetadata(map[string]string{
				"count": strconv.Itoa(syncEnabledCount),
			})
		}
	}

	// 校验并规范化请求头覆写配置（批量路径为 JSONB 顶层 key 合并，直接校验增量即可）
	if err := NormalizeHeaderOverrideCredentials(input.Credentials); err != nil {
		return nil, err
	}
	if err := NormalizeProtocolRulesCredentials(input.Credentials); err != nil {
		return nil, err
	}
	// Bulk may mix platforms; always drop ephemeral SSO/password keys (cookie
	// only when platform is known Grok — empty platform still strips password/*).
	if input.Credentials != nil {
		input.Credentials = SanitizeStoredCredentials("", input.Credentials)
	}

	// Prepare bulk updates for columns and JSONB fields.
	repoUpdates := AccountBulkUpdate{
		Credentials:                input.Credentials,
		Extra:                      input.Extra,
		ProbeEnabled:               input.ProbeEnabled,
		EnsureCodexFingerprintSeed: ShouldEnsureCodexFingerprintSeedForExtraUpdates(input.Extra),
	}
	if input.ProbeEnabled != nil {
		if repoUpdates.Extra == nil {
			repoUpdates.Extra = make(map[string]any)
		}
		repoUpdates.Extra[UpstreamBillingProbeEnabledExtraKey] = *input.ProbeEnabled
		if !*input.ProbeEnabled {
			repoUpdates.Extra[UpstreamBillingRateSyncEnabledExtraKey] = false
		}
	}
	if updatesUpstreamBillingProbeIdentity(input.Credentials) || input.ProxyID != nil {
		if repoUpdates.Extra == nil {
			repoUpdates.Extra = make(map[string]any)
		}
		// JSON null makes every reader treat the old snapshot as absent and lets the
		// next enabled runner cycle probe the new upstream identity immediately.
		repoUpdates.Extra[UpstreamBillingProbeExtraKey] = nil
	}
	if input.Name != "" {
		repoUpdates.Name = &input.Name
	}
	if input.ProxyID != nil {
		repoUpdates.ProxyID = input.ProxyID
	}
	if input.Concurrency != nil {
		repoUpdates.Concurrency = input.Concurrency
	}
	if input.Priority != nil {
		repoUpdates.Priority = input.Priority
	}
	if input.RateMultiplier != nil {
		repoUpdates.RateMultiplier = input.RateMultiplier
	}
	if input.LoadFactor != nil {
		if *input.LoadFactor <= 0 {
			repoUpdates.LoadFactor = nil // 0 或负数表示清除
		} else if *input.LoadFactor > 10000 {
			return nil, errors.New("load_factor must be <= 10000")
		} else {
			repoUpdates.LoadFactor = input.LoadFactor
		}
	}
	if input.Status != "" {
		repoUpdates.Status = &input.Status
	}
	if input.Schedulable != nil {
		repoUpdates.Schedulable = input.Schedulable
	}

	// Run bulk update for column/jsonb fields first.
	if _, err := s.accountRepo.BulkUpdate(ctx, input.AccountIDs, repoUpdates); err != nil {
		return nil, err
	}

	// Handle group bindings per account (requires individual operations).
	for _, accountID := range input.AccountIDs {
		entry := BulkUpdateAccountResult{AccountID: accountID}

		if input.GroupIDs != nil {
			if err := s.accountRepo.BindGroups(ctx, accountID, *input.GroupIDs); err != nil {
				entry.Success = false
				entry.Error = err.Error()
				result.Failed++
				result.FailedIDs = append(result.FailedIDs, accountID)
				result.Results = append(result.Results, entry)
				continue
			}
		}

		entry.Success = true
		result.Success++
		result.SuccessIDs = append(result.SuccessIDs, accountID)
		result.Results = append(result.Results, entry)
	}

	return result, nil
}

func updatesUpstreamBillingProbeIdentity(credentials map[string]any) bool {
	for _, key := range []string{"api_key", "base_url", credKeyHeaderOverrideEnabled, credKeyHeaderOverrides} {
		if _, ok := credentials[key]; ok {
			return true
		}
	}
	return false
}

func upstreamBillingProbeIdentity(account *Account) map[string]any {
	if account == nil {
		return nil
	}
	identity := map[string]any{"platform": account.Platform, "type": account.Type, "proxy_id": nil}
	if account.ProxyID != nil {
		identity["proxy_id"] = *account.ProxyID
	}
	for _, key := range []string{"api_key", "base_url", credKeyHeaderOverrideEnabled, credKeyHeaderOverrides} {
		if value, ok := account.Credentials[key]; ok {
			identity[key] = value
		}
	}
	return identity
}

func (s *adminServiceImpl) resolveBulkUpdateTargetIDs(ctx context.Context, filters *BulkUpdateAccountFilters) ([]int64, error) {
	if filters == nil {
		return nil, nil
	}

	groupID := int64(0)
	switch strings.TrimSpace(filters.Group) {
	case "":
	case "ungrouped":
		groupID = AccountListGroupUngrouped
	default:
		parsedGroupID, err := strconv.ParseInt(strings.TrimSpace(filters.Group), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid group filter: %w", err)
		}
		groupID = parsedGroupID
	}

	const pageSize = 500
	page := 1
	accountIDs := make([]int64, 0, pageSize)

	for {
		accounts, total, err := s.ListAccounts(
			ctx,
			page,
			pageSize,
			filters.Platform,
			filters.Type,
			filters.Status,
			filters.Search,
			groupID,
			filters.PrivacyMode,
			"",
			"",
		)
		if err != nil {
			return nil, err
		}
		for _, account := range accounts {
			accountIDs = append(accountIDs, account.ID)
		}
		if int64(len(accountIDs)) >= total || len(accounts) == 0 {
			return accountIDs, nil
		}
		page++
	}
}

func (s *adminServiceImpl) DeleteAccount(ctx context.Context, id int64) error {
	if err := s.accountRepo.Delete(ctx, id); err != nil {
		return err
	}
	return nil
}

func (s *adminServiceImpl) RefreshAccountCredentials(ctx context.Context, id int64) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// TODO: Implement refresh logic
	return account, nil
}

func (s *adminServiceImpl) ClearAccountError(ctx context.Context, id int64) (*Account, error) {
	if err := s.accountRepo.ClearError(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearRateLimit(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearModelRateLimits(ctx, id); err != nil {
		return nil, err
	}
	if err := s.accountRepo.ClearTempUnschedulable(ctx, id); err != nil {
		return nil, err
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	AfterAccountCommit(ctx, func(context.Context) {
		if s.runtimeBlocker != nil {
			s.runtimeBlocker.ClearAccountSchedulingBlock(id)
		}
		ResetOpenAI429Counter(id)
	})
	return account, nil
}

func (s *adminServiceImpl) SetAccountError(ctx context.Context, id int64, errorMsg string) error {
	return s.accountRepo.SetError(ctx, id, errorMsg)
}

// ErrOpenAICreditsToggleUnsupported 表示账号不是可切换点数的 OpenAI OAuth/SetupToken 账号。
var ErrOpenAICreditsToggleUnsupported = infraerrors.New(http.StatusBadRequest, "OPENAI_CREDITS_TOGGLE_UNSUPPORTED",
	"credits toggle only applies to OpenAI OAuth or setup-token accounts")

func (s *adminServiceImpl) SetOpenAICreditsEnabled(ctx context.Context, id int64, enabled bool) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !account.IsOpenAIOAuthLike() {
		return nil, ErrOpenAICreditsToggleUnsupported
	}
	if err := s.accountRepo.UpdateExtra(ctx, id, map[string]any{OpenAICreditsEnabledExtraKey: enabled}); err != nil {
		return nil, err
	}
	// 管理员手动确认启用点数：解除当前限流（多为额度用尽所致），账号立即恢复调度。
	// 若上游点数也已耗尽，下一次请求的 429 会按正常路径重新限流。
	if enabled && account.RateLimitResetAt != nil && time.Now().Before(*account.RateLimitResetAt) {
		if err := s.accountRepo.ClearRateLimit(ctx, id); err != nil {
			return nil, err
		}
		if s.runtimeBlocker != nil {
			s.runtimeBlocker.ClearAccountSchedulingBlock(id)
		}
		ResetOpenAI429Counter(id)
	}
	return s.accountRepo.GetByID(ctx, id)
}

func (s *adminServiceImpl) SetAccountSchedulable(ctx context.Context, id int64, schedulable bool) (*Account, error) {
	if err := s.accountRepo.SetSchedulable(ctx, id, schedulable); err != nil {
		return nil, err
	}
	updated, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *adminServiceImpl) RevertAccountProxyFallback(ctx context.Context, id int64) error {
	return s.accountRepo.RevertProxyFallback(ctx, id)
}

// checkMixedChannelRisk 检查分组中是否存在混合渠道
// 如果存在混合，返回错误提示用户确认
func (s *adminServiceImpl) checkMixedChannelRisk(ctx context.Context, currentAccountID int64, currentAccountPlatform string, groupIDs []int64) error {
	// 判断当前账号的渠道类型（基于 platform 字段，而不是 type 字段）
	currentPlatform := getAccountPlatform(currentAccountPlatform)
	if currentPlatform == "" {
		// 不参与混合渠道检查的平台，直接放行
		return nil
	}

	// 检查每个分组中的其他账号
	for _, groupID := range groupIDs {
		accounts, err := s.accountRepo.ListByGroup(ctx, groupID)
		if err != nil {
			return fmt.Errorf("get accounts in group %d: %w", groupID, err)
		}

		// 检查是否存在不同渠道的账号
		for _, account := range accounts {
			if currentAccountID > 0 && account.ID == currentAccountID {
				continue // 跳过当前账号
			}

			otherPlatform := getAccountPlatform(account.Platform)
			if otherPlatform == "" {
				continue // 不参与混合渠道检查的平台，跳过
			}

			// 检测混合渠道
			if currentPlatform != otherPlatform {
				group, _ := s.groupRepo.GetByID(ctx, groupID)
				groupName := fmt.Sprintf("Group %d", groupID)
				if group != nil {
					groupName = group.Name
				}

				return &MixedChannelError{
					GroupID:         groupID,
					GroupName:       groupName,
					CurrentPlatform: currentPlatform,
					OtherPlatform:   otherPlatform,
				}
			}
		}
	}

	return nil
}

func (s *adminServiceImpl) validateGroupIDsExist(ctx context.Context, groupIDs []int64) error {
	if len(groupIDs) == 0 {
		return nil
	}
	if s.groupRepo == nil {
		return errors.New("group repository not configured")
	}

	if batchReader, ok := s.groupRepo.(groupExistenceBatchReader); ok {
		existsByID, err := batchReader.ExistsByIDs(ctx, groupIDs)
		if err != nil {
			return fmt.Errorf("check groups exists: %w", err)
		}
		for _, groupID := range groupIDs {
			if groupID <= 0 || !existsByID[groupID] {
				return fmt.Errorf("get group: %w", ErrGroupNotFound)
			}
		}
		return nil
	}

	for _, groupID := range groupIDs {
		if _, err := s.groupRepo.GetByID(ctx, groupID); err != nil {
			return fmt.Errorf("get group: %w", err)
		}
	}
	return nil
}

// ValidateAccountGroupBindings is the shared fail-closed policy boundary for
// every account path that accepts explicit group bindings.
func (s *adminServiceImpl) ValidateAccountGroupBindings(ctx context.Context, groupIDs []int64) error {
	if len(groupIDs) == 0 || s.cfg == nil || s.cfg.RunMode != config.RunModeSimple {
		return nil
	}
	if s.groupRepo == nil {
		return errors.New("group repository not configured")
	}
	seen := make(map[int64]struct{}, len(groupIDs))
	for _, groupID := range groupIDs {
		if groupID <= 0 {
			return fmt.Errorf("get group: %w", ErrGroupNotFound)
		}
		if _, ok := seen[groupID]; ok {
			continue
		}
		seen[groupID] = struct{}{}
		group, err := s.groupRepo.GetByIDLite(ctx, groupID)
		if err != nil {
			return fmt.Errorf("get group: %w", err)
		}
		if !IsGroupBindableInSimpleMode(group) {
			return infraerrors.BadRequest("SIMPLE_MODE_GROUP_NOT_BINDABLE", "composite groups cannot be bound in simple mode")
		}
	}
	return nil
}

// CheckMixedChannelRisk checks whether target groups contain mixed channels for the current account platform.
func (s *adminServiceImpl) CheckMixedChannelRisk(ctx context.Context, currentAccountID int64, currentAccountPlatform string, groupIDs []int64) error {
	return s.checkMixedChannelRisk(ctx, currentAccountID, currentAccountPlatform, groupIDs)
}

// getAccountPlatform 根据账号 platform 判断混合渠道检查用的平台标识
func getAccountPlatform(accountPlatform string) string {
	switch strings.ToLower(strings.TrimSpace(accountPlatform)) {
	case PlatformAnthropic, "claude":
		return "Anthropic"
	default:
		return ""
	}
}

// MixedChannelError 混合渠道错误
type MixedChannelError struct {
	GroupID         int64
	GroupName       string
	CurrentPlatform string
	OtherPlatform   string
}

func (e *MixedChannelError) Error() string {
	return fmt.Sprintf("mixed_channel_warning: Group '%s' contains both %s and %s accounts. Using mixed channels in the same context may cause thinking block signature validation issues, which will fallback to non-thinking mode for historical messages.",
		e.GroupName, e.CurrentPlatform, e.OtherPlatform)
}

func (s *adminServiceImpl) ResetAccountQuota(ctx context.Context, id int64) error {
	if _, err := s.accountRepo.GetByID(ctx, id); err != nil {
		return err
	}
	return s.accountRepo.ResetQuotaUsedAndClearRateLimitCooldown(ctx, id)
}

// EnsureOpenAIPrivacy 检查 OpenAI OAuth 账号是否已设置 privacy_mode，
// 未设置则调用 disableOpenAITraining 并持久化到 Extra，返回设置的 mode 值。
func (s *adminServiceImpl) EnsureOpenAIPrivacy(ctx context.Context, account *Account) string {
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return ""
	}
	if s.privacyClientFactory == nil {
		return ""
	}
	if shouldSkipOpenAIPrivacyEnsure(account.Extra) {
		return ""
	}

	token, _ := account.Credentials["access_token"].(string)
	if token == "" {
		return ""
	}

	var proxyURL string
	if account.ProxyID != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && p != nil {
			proxyURL = p.URL()
		}
	}

	mode := disableOpenAITraining(ctx, s.privacyClientFactory, token, proxyURL)
	if mode == "" {
		return ""
	}

	_ = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"privacy_mode": mode})
	return mode
}

// ForceOpenAIPrivacy 强制重新设置 OpenAI OAuth 账号隐私，无论当前状态。
func (s *adminServiceImpl) ForceOpenAIPrivacy(ctx context.Context, account *Account) string {
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return ""
	}
	if s.privacyClientFactory == nil {
		return ""
	}

	token, _ := account.Credentials["access_token"].(string)
	if token == "" {
		return ""
	}

	var proxyURL string
	if account.ProxyID != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && p != nil {
			proxyURL = p.URL()
		}
	}

	mode := disableOpenAITraining(ctx, s.privacyClientFactory, token, proxyURL)
	if mode == "" {
		return ""
	}

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"privacy_mode": mode}); err != nil {
		logger.LegacyPrintf("service.admin", "force_update_openai_privacy_mode_failed: account_id=%d err=%v", account.ID, err)
		return mode
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra["privacy_mode"] = mode
	return mode
}
