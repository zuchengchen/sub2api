package service

import (
	"context"
	"maps"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Account protection stores a named OpenAI identity/TLS strategy on extra.
// Applying a strategy cannot prove upstream model quality; it only keeps
// fingerprint, TLS template, and concurrency stable across ordinary saves.

const (
	AntiDegradeMarkerExtraKey   = "anti_degrade"
	AntiDegradationExtraKey     = "anti_degradation"
	ProtectionScopeExtraKey     = "protection_scope"
	AntiDegradeConcurrencyCap   = 16
	tlsFingerprintBuiltinKey    = "tls_fingerprint_builtin"
	tlsFingerprintEnabledKey    = "enable_tls_fingerprint"
	tlsFingerprintProfileIDKey  = "tls_fingerprint_profile_id"
	accountProxyModeExtraKey    = "proxy_mode"
	mode1PolicyVersion          = 3
)

type AntiDegradeMode string

const (
	AntiDegradeModeLegacy AntiDegradeMode = "legacy"
	AntiDegradeMode1      AntiDegradeMode = "mode1"
	DefaultAntiDegradeMode                = AntiDegradeModeLegacy
)

var (
	ErrUnknownAntiDegradeMode   = infraerrors.BadRequest("UNKNOWN_PROTECTION_STRATEGY", "未知的账号保护策略")
	ErrProtectionNotEligible    = infraerrors.BadRequest("PROTECTION_NOT_ELIGIBLE", "该账号不能应用固定出口保护策略")
	ErrProtectionConflict       = infraerrors.Conflict("PROTECTION_CONFLICT", "账号保护配置已被其他操作更新，请刷新后重试")
	ErrProtectedProxyModeChange = infraerrors.BadRequest("PROTECTED_PROXY_MODE", "随机代理与固定出口保护策略冲突")
)

var protectionManagedExtraKeys = []string{
	AntiDegradationExtraKey,
	ProtectionScopeExtraKey,
	AntiDegradeMarkerExtraKey,
	codexFingerprintModeExtraKey,
	tlsFingerprintEnabledKey,
	tlsFingerprintBuiltinKey,
	tlsFingerprintProfileIDKey,
	accountProxyModeExtraKey,
}

type protectionStrategy struct {
	ID           AntiDegradeMode
	Name         string
	IdentityMode codexFingerprintMode
	TLSProfile   string // "standard" or builtin template name
	Scope        string
}

var protectionStrategies = map[AntiDegradeMode]protectionStrategy{
	AntiDegradeModeLegacy: {
		ID: AntiDegradeModeLegacy, Name: "初代兼容",
		IdentityMode: codexFingerprintSession, TLSProfile: "nodejs24", Scope: "legacy",
	},
	AntiDegradeMode1: {
		ID: AntiDegradeMode1, Name: "兼容架构 v3",
		IdentityMode: codexFingerprintDevice, TLSProfile: "standard", Scope: "codex_v3",
	},
}

func normalizeAntiDegradeMode(mode AntiDegradeMode) AntiDegradeMode {
	if mode == "" {
		return DefaultAntiDegradeMode
	}
	return mode
}

func registeredProtectionStrategy(mode AntiDegradeMode) (protectionStrategy, bool) {
	profile, ok := protectionStrategies[normalizeAntiDegradeMode(mode)]
	return profile, ok && profile.ID != ""
}

func RegisteredProtectionModes() []string {
	return []string{string(AntiDegradeModeLegacy), string(AntiDegradeMode1)}
}

// ListAntiDegradeStrategyProfiles is the admin-visible registry. New accounts
// are not auto-assigned a strategy; operators pick legacy or mode1 explicitly.
func ListAntiDegradeStrategyProfiles() []map[string]any {
	out := make([]map[string]any, 0, len(protectionStrategies))
	for _, id := range RegisteredProtectionModes() {
		profile, ok := registeredProtectionStrategy(AntiDegradeMode(id))
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"id":          string(profile.ID),
			"name":        profile.Name,
			"tls_profile": profile.TLSProfile,
			"scope":       profile.Scope,
		})
	}
	return out
}

func mode1Int(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func isMode1ProtectionEnabled(a *Account) bool {
	if a == nil || !a.AntiDegradationEnabled() {
		return false
	}
	marker := protectionMarker(a)
	version := 0
	switch n := marker["policy_version"].(type) {
	case int:
		version = n
	case int64:
		version = int(n)
	case float64:
		version = int(n)
	}
	return marker["mode"] == string(AntiDegradeMode1) && (version == 2 || version == mode1PolicyVersion)
}

func isLegacyProtectionEnabled(a *Account) bool {
	return a != nil && a.AntiDegradationEnabled() && antiDegradeMode(a) == AntiDegradeModeLegacy
}

// Mode1EffectiveConcurrency is the editable account ceiling. Protection does
// not replace the 429 near-limit selector; it only reports the stored cap.
func (a *Account) Mode1EffectiveConcurrency() int {
	if a == nil {
		return 0
	}
	return a.Concurrency
}

type protectionManagedWriteKey struct{}
type protectionWriteExpectationKey struct{}

type protectionWriteExpectation struct {
	AccountID int64
	UpdatedAt time.Time
}

func withProtectionManagedWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, protectionManagedWriteKey{}, true)
}

func ProtectionManagedWrite(ctx context.Context) bool {
	return ctx != nil && ctx.Value(protectionManagedWriteKey{}) == true
}

func withProtectionWriteExpectation(ctx context.Context, account *Account) context.Context {
	if account == nil {
		return ctx
	}
	return context.WithValue(ctx, protectionWriteExpectationKey{}, protectionWriteExpectation{AccountID: account.ID, UpdatedAt: account.UpdatedAt})
}

func GetProtectionWriteExpectation(ctx context.Context) (protectionWriteExpectation, bool) {
	if ctx == nil {
		return protectionWriteExpectation{}, false
	}
	value, ok := ctx.Value(protectionWriteExpectationKey{}).(protectionWriteExpectation)
	return value, ok && value.AccountID > 0
}

func (a *Account) AntiDegradationEnabled() bool {
	if a == nil || a.Extra == nil {
		return false
	}
	if enabled, ok := a.Extra[AntiDegradationExtraKey].(bool); ok {
		return enabled
	}
	return antiDegradeEnabled(a)
}

func antiDegradeEnabled(a *Account) bool {
	marker := protectionMarker(a)
	enabled, _ := marker["enabled"].(bool)
	return enabled
}

func protectionMarker(a *Account) map[string]any {
	if a == nil || a.Extra == nil {
		return nil
	}
	marker, _ := a.Extra[AntiDegradeMarkerExtraKey].(map[string]any)
	return marker
}

func antiDegradeMode(a *Account) AntiDegradeMode {
	if marker := protectionMarker(a); marker != nil {
		if raw, ok := marker["mode"].(string); ok && raw != "" {
			return AntiDegradeMode(raw)
		}
	}
	return DefaultAntiDegradeMode
}

func (a *Account) ProtectionMode() string {
	if a == nil || !a.AntiDegradationEnabled() {
		return "disabled"
	}
	return string(antiDegradeMode(a))
}

func (a *Account) ProtectionScope() string {
	if a == nil || !a.AntiDegradationEnabled() {
		return "disabled"
	}
	if scope, ok := a.Extra[ProtectionScopeExtraKey].(string); ok && scope != "" {
		return scope
	}
	if isMode1ProtectionEnabled(a) {
		return "codex_v3"
	}
	return "legacy"
}

func (a *Account) IsRandomProxy() bool {
	if a == nil || a.Extra == nil {
		return false
	}
	mode, _ := a.Extra[accountProxyModeExtraKey].(string)
	return mode == "random"
}

func ProtectionManagedKeys(a *Account) []string {
	keys := []string{AntiDegradationExtraKey, ProtectionScopeExtraKey, AntiDegradeMarkerExtraKey}
	if a != nil && a.AntiDegradationEnabled() {
		keys = append(keys, protectionManagedExtraKeys[3:]...)
	}
	return keys
}

func PreserveAccountProtection(ctx context.Context, current *Account, incoming map[string]any) map[string]any {
	if ProtectionManagedWrite(ctx) {
		return incoming
	}
	result := maps.Clone(incoming)
	if result == nil {
		result = map[string]any{}
	}
	if current == nil {
		return result
	}
	for _, key := range ProtectionManagedKeys(current) {
		if value, exists := current.Extra[key]; exists {
			result[key] = value
		} else {
			delete(result, key)
		}
	}
	if seed, ok := codexFingerprintSeed(current.Extra); ok {
		result[codexFingerprintSeedExtraKey] = seed
	}
	return result
}

func StripProtectionManagedExtraUpdates(ctx context.Context, updates map[string]any) map[string]any {
	if ProtectionManagedWrite(ctx) || updates == nil {
		return updates
	}
	stripped := maps.Clone(updates)
	for _, key := range protectionManagedExtraKeys {
		delete(stripped, key)
	}
	return stripped
}

func ProtectedProxyModeConflict(current *Account, incoming map[string]any) bool {
	if current == nil || !current.AntiDegradationEnabled() || incoming == nil {
		return false
	}
	mode, ok := incoming[accountProxyModeExtraKey].(string)
	return ok && mode == "random"
}

type AntiDegradeStore interface {
	GetAccount(ctx context.Context, id int64) (*Account, error)
	UpdateAccount(ctx context.Context, id int64, input *UpdateAccountInput) (*Account, error)
}

type AntiDegradeService struct {
	admin AntiDegradeStore
}

func NewAntiDegradeService(admin AntiDegradeStore) *AntiDegradeService {
	return &AntiDegradeService{admin: admin}
}

func ProvideAntiDegradeService(admin AdminService) *AntiDegradeService {
	return NewAntiDegradeService(admin)
}

type AntiDegradeChange struct {
	Key  string `json:"key"`
	From any    `json:"from,omitempty"`
	To   any    `json:"to"`
	Note string `json:"note,omitempty"`
}

type AntiDegradePreview struct {
	AccountID  int64               `json:"account_id"`
	ActiveMode string              `json:"active_mode"`
	Enabled    bool                `json:"enabled"`
	Eligible   bool                `json:"eligible"`
	Reason     string              `json:"reason,omitempty"`
	TLSProfile string              `json:"tls_profile"`
	Changes    []AntiDegradeChange `json:"changes"`
}

func (s *AntiDegradeService) Preview(ctx context.Context, id int64, mode AntiDegradeMode) (AntiDegradePreview, error) {
	if s == nil || s.admin == nil {
		return AntiDegradePreview{}, infraerrors.InternalServer("PROTECTION_UNAVAILABLE", "account protection service unavailable")
	}
	account, err := s.admin.GetAccount(ctx, id)
	if err != nil {
		return AntiDegradePreview{}, err
	}
	return previewProtection(account, mode), nil
}

func (s *AntiDegradeService) Apply(ctx context.Context, id int64, mode AntiDegradeMode) (*Account, error) {
	return s.mutate(ctx, id, func(account *Account) error {
		return applyProtectionMode(account, mode)
	})
}

func (s *AntiDegradeService) Revert(ctx context.Context, id int64) (*Account, error) {
	return s.mutate(ctx, id, revertProtection)
}

func (s *AntiDegradeService) SetProtection(ctx context.Context, id int64, enabled, confirmDisable bool) (*Account, error) {
	if !enabled {
		if !confirmDisable {
			return nil, infraerrors.BadRequest("PROTECTION_CONFIRM_REQUIRED", "关闭账号保护需要管理员明确确认")
		}
		return s.Revert(ctx, id)
	}
	return s.Apply(ctx, id, DefaultAntiDegradeMode)
}

func (s *AntiDegradeService) mutate(ctx context.Context, id int64, apply func(*Account) error) (*Account, error) {
	if s == nil || s.admin == nil {
		return nil, infraerrors.InternalServer("PROTECTION_UNAVAILABLE", "account protection service unavailable")
	}
	account, err := s.admin.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	draft := cloneAccountForProtection(account)
	if err := apply(draft); err != nil {
		return nil, err
	}
	writeCtx := withProtectionWriteExpectation(withProtectionManagedWrite(ctx), account)
	return s.admin.UpdateAccount(writeCtx, id, &UpdateAccountInput{
		Extra:       draft.Extra,
		Concurrency: concurrencyPtrIfChanged(account, draft),
	})
}

func cloneAccountForProtection(account *Account) *Account {
	if account == nil {
		return nil
	}
	clone := *account
	clone.Extra = maps.Clone(account.Extra)
	if clone.Extra == nil {
		clone.Extra = map[string]any{}
	}
	return &clone
}

func concurrencyPtrIfChanged(before, after *Account) *int {
	if before == nil || after == nil || before.Concurrency == after.Concurrency {
		return nil
	}
	value := after.Concurrency
	return &value
}

func previewProtection(account *Account, mode AntiDegradeMode) AntiDegradePreview {
	mode = normalizeAntiDegradeMode(mode)
	out := AntiDegradePreview{Changes: []AntiDegradeChange{}}
	if account == nil {
		out.Reason = "account not found"
		return out
	}
	out.AccountID = account.ID
	out.Enabled = account.AntiDegradationEnabled()
	if out.Enabled {
		out.ActiveMode = account.ProtectionMode()
	}
	profile, ok := registeredProtectionStrategy(mode)
	if !ok {
		out.Reason = ErrUnknownAntiDegradeMode.Error()
		return out
	}
	out.TLSProfile = profile.TLSProfile
	if reason := protectionEligibilityIssue(account, mode); reason != "" {
		out.Reason = reason
		return out
	}
	out.Eligible = true
	if out.Enabled && antiDegradeMode(account) == mode {
		out.Reason = "already applied"
		return out
	}
	out.Changes = append(out.Changes, AntiDegradeChange{
		Key: "strategy", From: out.ActiveMode, To: string(mode), Note: profile.Name,
	})
	return out
}

func protectionEligibilityIssue(account *Account, mode AntiDegradeMode) string {
	if _, ok := registeredProtectionStrategy(mode); !ok {
		return ErrUnknownAntiDegradeMode.Message
	}
	if account == nil || !account.IsOpenAIOAuthLike() || account.IsShadow() {
		return "仅支持独立的 OpenAI OAuth / Setup Token 账号"
	}
	if account.IsRandomProxy() {
		return "随机代理与固定出口保护策略冲突，请先改为固定代理或直连"
	}
	return ""
}

func applyProtectionMode(account *Account, mode AntiDegradeMode) error {
	mode = normalizeAntiDegradeMode(mode)
	profile, ok := registeredProtectionStrategy(mode)
	if !ok {
		return ErrUnknownAntiDegradeMode
	}
	if reason := protectionEligibilityIssue(account, mode); reason != "" {
		return infraerrors.BadRequest("PROTECTION_NOT_ELIGIBLE", reason)
	}
	if account.AntiDegradationEnabled() && antiDegradeMode(account) == mode {
		return nil
	}
	prev := snapshotProtectionConfig(account)
	if account.AntiDegradationEnabled() && antiDegradeMode(account) != mode {
		if err := restoreProtectionSnapshot(account, protectionMarker(account)["prev"]); err != nil {
			return err
		}
		prev = snapshotProtectionConfig(account)
	}
	extra := account.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	extra[codexFingerprintModeExtraKey] = string(profile.IdentityMode)
	if profile.TLSProfile == "standard" {
		extra[tlsFingerprintEnabledKey] = false
		delete(extra, tlsFingerprintBuiltinKey)
		delete(extra, tlsFingerprintProfileIDKey)
	} else {
		extra[tlsFingerprintEnabledKey] = true
		extra[tlsFingerprintBuiltinKey] = profile.TLSProfile
		delete(extra, tlsFingerprintProfileIDKey)
	}
	cap := account.Concurrency
	if cap <= 0 {
		cap = AntiDegradeConcurrencyCap
		account.Concurrency = cap
	}
	marker := map[string]any{
		"enabled":         true,
		"mode":            string(mode),
		"max_concurrency": cap,
		"applied_at":      time.Now().UTC().Format(time.RFC3339),
		"prev":            prev,
	}
	if mode == AntiDegradeMode1 {
		marker["policy_version"] = mode1PolicyVersion
	}
	extra[AntiDegradeMarkerExtraKey] = marker
	extra[AntiDegradationExtraKey] = true
	extra[ProtectionScopeExtraKey] = profile.Scope
	account.Extra = prepareCodexFingerprintExtraForUpdate(account, extra)
	return nil
}

func revertProtection(account *Account) error {
	if account == nil || !account.AntiDegradationEnabled() {
		return nil
	}
	if err := restoreProtectionSnapshot(account, protectionMarker(account)["prev"]); err != nil {
		return err
	}
	extra := account.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	extra[AntiDegradationExtraKey] = false
	extra[ProtectionScopeExtraKey] = "disabled"
	delete(extra, AntiDegradeMarkerExtraKey)
	account.Extra = extra
	return nil
}

func snapshotProtectionConfig(account *Account) map[string]any {
	prev := map[string]any{"concurrency": account.Concurrency}
	for _, key := range protectionManagedExtraKeys {
		if key == AntiDegradeMarkerExtraKey || key == AntiDegradationExtraKey || key == ProtectionScopeExtraKey {
			continue
		}
		if account.Extra == nil {
			prev[key] = nil
			continue
		}
		if value, exists := account.Extra[key]; exists {
			prev[key] = value
		} else {
			prev[key] = nil
		}
	}
	return prev
}

func restoreProtectionSnapshot(account *Account, raw any) error {
	prev, _ := raw.(map[string]any)
	if prev == nil {
		prev = map[string]any{}
	}
	extra := maps.Clone(account.Extra)
	if extra == nil {
		extra = map[string]any{}
	}
	for _, key := range protectionManagedExtraKeys {
		if key == AntiDegradeMarkerExtraKey || key == AntiDegradationExtraKey || key == ProtectionScopeExtraKey {
			continue
		}
		if value, exists := prev[key]; exists {
			if value == nil {
				delete(extra, key)
			} else {
				extra[key] = value
			}
		}
	}
	account.Extra = extra
	return nil
}

func enforceProtectionWriteExpectation(ctx context.Context, current *Account) error {
	expected, ok := GetProtectionWriteExpectation(ctx)
	if !ok || current == nil {
		return nil
	}
	if expected.AccountID != current.ID || !expected.UpdatedAt.Equal(current.UpdatedAt) {
		return ErrProtectionConflict
	}
	return nil
}

func mergeAccountProtectionForSave(ctx context.Context, current *Account, incoming map[string]any) (map[string]any, error) {
	if ProtectedProxyModeConflict(current, incoming) && !ProtectionManagedWrite(ctx) {
		return nil, ErrProtectedProxyModeChange
	}
	merged := PreserveAccountProtection(ctx, current, incoming)
	if !ProtectionManagedWrite(ctx) {
		if _, err := ParseAccountTrafficPolicy(merged); err != nil {
			return nil, err
		}
		if err := validateRequestIntegrityExtra(merged); err != nil {
			return nil, err
		}
	}
	return merged, nil
}
