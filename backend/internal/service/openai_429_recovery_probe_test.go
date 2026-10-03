package service

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openai429RecoveryRepo struct {
	mockAccountRepoForTest
	mu                        sync.Mutex
	clearRateLimitIDs         []int64
	setRateLimitedIDs         []int64
	lastRateLimitReset        time.Time
	clearModelRateLimitIDs    []int64
	clearTempUnschedulableIDs []int64
	extraUpdates              []map[string]any
}

func (r *openai429RecoveryRepo) store(account *Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accountsByID == nil {
		r.accountsByID = map[int64]*Account{}
	}
	r.accountsByID[account.ID] = account
}

func (r *openai429RecoveryRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	account, ok := r.accountsByID[id]
	if !ok {
		return nil, fmt.Errorf("account not found")
	}
	return account, nil
}

func (r *openai429RecoveryRepo) ClearRateLimit(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearRateLimitIDs = append(r.clearRateLimitIDs, id)
	if account := r.accountsByID[id]; account != nil {
		account.RateLimitedAt = nil
		account.RateLimitResetAt = nil
	}
	return nil
}

func (r *openai429RecoveryRepo) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setRateLimitedIDs = append(r.setRateLimitedIDs, id)
	r.lastRateLimitReset = resetAt
	if account := r.accountsByID[id]; account != nil {
		now := time.Now()
		account.RateLimitedAt = &now
		reset := resetAt
		account.RateLimitResetAt = &reset
	}
	return nil
}

func (r *openai429RecoveryRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make(map[string]any, len(updates))
	for k, v := range updates {
		copied[k] = v
	}
	r.extraUpdates = append(r.extraUpdates, copied)
	account := r.accountsByID[id]
	if account == nil {
		return nil
	}
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	for k, v := range updates {
		account.Extra[k] = v
	}
	return nil
}

func (r *openai429RecoveryRepo) ClearModelRateLimits(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearModelRateLimitIDs = append(r.clearModelRateLimitIDs, id)
	return nil
}

func (r *openai429RecoveryRepo) ClearTempUnschedulable(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearTempUnschedulableIDs = append(r.clearTempUnschedulableIDs, id)
	return nil
}

type openai429RecoveryBlocker struct {
	mu         sync.Mutex
	clearedIDs []int64
}

func (b *openai429RecoveryBlocker) BlockAccountScheduling(*Account, time.Time, string) {}

func (b *openai429RecoveryBlocker) ClearAccountSchedulingBlock(accountID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clearedIDs = append(b.clearedIDs, accountID)
}

type openai429RecoveryAfterCall struct {
	delay time.Duration
	fn    func()
}

type considerCallRecorder struct {
	mu         sync.Mutex
	accountIDs []int64
}

func (c *considerCallRecorder) ConsiderOpenAI429Recovery(account *Account) {
	if account == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accountIDs = append(c.accountIDs, account.ID)
}

func openai429RecoveryRemainingHeaders() http.Header {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "30")
	h.Set("x-codex-primary-reset-after-seconds", "500000")
	h.Set("x-codex-primary-window-minutes", "10080")
	h.Set("x-codex-secondary-used-percent", "20")
	h.Set("x-codex-secondary-reset-after-seconds", "14400")
	h.Set("x-codex-secondary-window-minutes", "300")
	return h
}

func openai429UsageLimitReachedBody(resetAt time.Time) []byte {
	return []byte(fmt.Sprintf(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":%d}}`, resetAt.Unix()))
}

func openai429RecoveryEligibleExtra(now time.Time) map[string]any {
	return map[string]any{
		"codex_usage_updated_at": now.Format(time.RFC3339),
		"codex_5h_used_percent":  20.0,
		"codex_5h_reset_at":      now.Add(4 * time.Hour).Format(time.RFC3339),
		"codex_7d_used_percent":  30.0,
		"codex_7d_reset_at":      now.Add(72 * time.Hour).Format(time.RFC3339),
	}
}

func openai429RecoveryEligibleAccount(id int64, now time.Time) *Account {
	resetAt := now.Add(2 * time.Hour)
	limitedAt := now.Add(-time.Minute)
	return &Account{
		ID:               id,
		Platform:         PlatformOpenAI,
		Type:             AccountTypeOAuth,
		Status:           StatusActive,
		Schedulable:      true,
		RateLimitedAt:    &limitedAt,
		RateLimitResetAt: &resetAt,
		Extra:            openai429RecoveryEligibleExtra(now),
	}
}

func dummyOpenAI429RecoveryTimer() *time.Timer {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	return timer
}

func installOpenAI429RecoveryAfterRecorder(t *testing.T, svc *RateLimitService) *[]openai429RecoveryAfterCall {
	t.Helper()
	var calls []openai429RecoveryAfterCall
	svc.openai429RecoveryAfter = func(d time.Duration, fn func()) *time.Timer {
		calls = append(calls, openai429RecoveryAfterCall{delay: d, fn: fn})
		return dummyOpenAI429RecoveryTimer()
	}
	t.Cleanup(func() {
		svc.openai429RecoveryMu.Lock()
		ids := make([]int64, 0, len(svc.openai429RecoveryStates))
		for id := range svc.openai429RecoveryStates {
			ids = append(ids, id)
		}
		svc.openai429RecoveryMu.Unlock()
		for _, id := range ids {
			svc.cancelOpenAI429RecoveryProbe(id)
		}
	})
	return &calls
}

func fireLastOpenAI429RecoveryAfter(t *testing.T, calls *[]openai429RecoveryAfterCall) {
	t.Helper()
	require.NotEmpty(t, *calls)
	fn := (*calls)[len(*calls)-1].fn
	require.NotNil(t, fn)
	fn()
}

func TestOpenAI429RecoveryDelayBackoff(t *testing.T) {
	require.Equal(t, 30*time.Second, openAI429RecoveryDelay(0))
	require.Equal(t, time.Minute, openAI429RecoveryDelay(1))
	require.Equal(t, 2*time.Minute, openAI429RecoveryDelay(2))
	require.Equal(t, 5*time.Minute, openAI429RecoveryDelay(3))
	require.Equal(t, 15*time.Minute, openAI429RecoveryDelay(4))
	require.Equal(t, 15*time.Minute, openAI429RecoveryDelay(9))
	require.Equal(t, 30*time.Second, openAI429RecoveryDelay(-1))
}

func TestOpenAI429RecoveryProbeModelDefault(t *testing.T) {
	require.Equal(t, openai.DefaultTestModel, openai429RecoveryProbeModel(nil, ""))
	require.Equal(t, "gpt-5.3", openai429RecoveryProbeModel(nil, "gpt-5.3"))
}

func TestOpenAIAccountEligibleFor429RecoveryProbe(t *testing.T) {
	now := time.Now()
	future := now.Add(2 * time.Hour)
	short := now.Add(20 * time.Second)
	expired := now.Add(-time.Minute)
	parentID := int64(1)
	freshExtra := openai429RecoveryEligibleExtra(now)
	staleExtra := openai429RecoveryEligibleExtra(now)
	staleExtra["codex_usage_updated_at"] = now.Add(-3 * time.Hour).Format(time.RFC3339)
	exhausted5h := openai429RecoveryEligibleExtra(now)
	exhausted5h["codex_5h_used_percent"] = 100.0
	exhausted7d := openai429RecoveryEligibleExtra(now)
	exhausted7d["codex_7d_used_percent"] = 100.0
	missing7d := openai429RecoveryEligibleExtra(now)
	delete(missing7d, "codex_7d_used_percent")
	delete(missing7d, "codex_7d_reset_at")
	reset5h := openai429RecoveryEligibleExtra(now)
	reset5h["codex_5h_used_percent"] = 100.0
	reset5h["codex_5h_reset_at"] = now.Add(-time.Minute).Format(time.RFC3339)
	missingUpdatedAt := openai429RecoveryEligibleExtra(now)
	delete(missingUpdatedAt, "codex_usage_updated_at")

	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "eligible oauth remaining", account: openai429RecoveryEligibleAccount(1, now), want: true},
		{name: "nil", account: nil, want: false},
		{name: "anthropic", account: &Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeOAuth, RateLimitResetAt: &future, Extra: freshExtra}, want: false},
		{name: "shadow", account: &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID, RateLimitResetAt: &future, Extra: freshExtra}, want: false},
		{name: "pool mode without custom codes", account: &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true}, RateLimitResetAt: &future, Extra: freshExtra}, want: false},
		{name: "expired cooldown", account: &Account{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitResetAt: &expired, Extra: freshExtra}, want: false},
		{name: "short remaining", account: &Account{ID: 6, Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitResetAt: &short, Extra: freshExtra}, want: false},
		{name: "stale snapshot", account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitResetAt: &future, Extra: staleExtra}, want: false},
		{name: "exhausted 5h", account: &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitResetAt: &future, Extra: exhausted5h}, want: false},
		{name: "exhausted 7d", account: &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitResetAt: &future, Extra: exhausted7d}, want: false},
		{name: "missing 7d window", account: &Account{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitResetAt: &future, Extra: missing7d}, want: false},
		{name: "missing snapshot time", account: &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitResetAt: &future, Extra: missingUpdatedAt}, want: false},
		{name: "5h already reset", account: &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitResetAt: &future, Extra: reset5h}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, openAIAccountEligibleFor429RecoveryProbe(tt.account, now))
		})
	}
}

func TestHandleUpstreamError_OpenAI429SoftRemainingDoesNotScheduleRecovery(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &openai429RecoveryRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	calls := installOpenAI429RecoveryAfterRecorder(t, svc)
	account := &Account{ID: 9101, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	repo.store(account)

	for i := 1; i <= 29; i++ {
		shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429RecoveryRemainingHeaders(), openai429UsageLimitReachedBody(time.Now().Add(time.Hour)), "gpt-5.4")
		require.False(t, shouldDisable, "count %d", i)
		require.Empty(t, repo.setRateLimitedIDs, "soft 429 %d", i)
		require.Empty(t, *calls, "soft 429 %d must not schedule recovery", i)
		require.Equal(t, i, openAI429Count(account.ID))
	}
}

func TestHandleUpstreamError_OpenAI429FormalCooldownWithRemainingSchedules30s(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &openai429RecoveryRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	calls := installOpenAI429RecoveryAfterRecorder(t, svc)
	account := &Account{ID: 9102, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	repo.store(account)
	resetAt := time.Now().Add(time.Hour)
	body := openai429UsageLimitReachedBody(resetAt)

	for i := 0; i < 29; i++ {
		svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429RecoveryRemainingHeaders(), body, "gpt-5.4")
	}
	require.Empty(t, *calls)
	require.Empty(t, repo.setRateLimitedIDs)

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429RecoveryRemainingHeaders(), body, "gpt-5.4")
	require.False(t, shouldDisable)
	require.Equal(t, []int64{account.ID}, repo.setRateLimitedIDs)
	require.Len(t, *calls, 1)
	require.Equal(t, 30*time.Second, (*calls)[0].delay)
	require.Equal(t, quota429CooldownThreshold, openAI429Count(account.ID))

	svc.openai429RecoveryMu.Lock()
	state := svc.openai429RecoveryStates[account.ID]
	svc.openai429RecoveryMu.Unlock()
	require.NotNil(t, state)
	require.Equal(t, "gpt-5.4", state.model)
	require.Equal(t, 0, state.attempts)
}

func TestHandleUpstreamError_OpenAI429ExhaustedHeadersDoNotScheduleRecovery(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &openai429RecoveryRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	calls := installOpenAI429RecoveryAfterRecorder(t, svc)
	account := &Account{ID: 9103, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	repo.store(account)

	for i := 0; i < quota429CooldownThreshold; i++ {
		svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429Headers(), nil, "gpt-5.4")
	}
	require.Equal(t, []int64{account.ID}, repo.setRateLimitedIDs)
	require.Empty(t, *calls)
	require.Nil(t, svc.openai429RecoveryStates[account.ID])
}

func TestConsiderOpenAI429Recovery_DoesNotPostponePendingTimer(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &openai429RecoveryRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	calls := installOpenAI429RecoveryAfterRecorder(t, svc)
	account := openai429RecoveryEligibleAccount(9104, time.Now())
	repo.store(account)

	svc.markOpenAIFormalCooldown(account, *account.RateLimitResetAt, "gpt-5.4")
	require.Len(t, *calls, 1)
	require.Equal(t, 30*time.Second, (*calls)[0].delay)

	svc.ConsiderOpenAI429Recovery(account)
	require.Len(t, *calls, 1)

	svc.openai429RecoveryMu.Lock()
	state := svc.openai429RecoveryStates[account.ID]
	svc.openai429RecoveryMu.Unlock()
	require.NotNil(t, state)
	require.Equal(t, 0, state.attempts)
}

func TestOpenAI429RecoveryProbe2xxClearsAccountLevelOnly(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &openai429RecoveryRepo{}
	blocker := &openai429RecoveryBlocker{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	svc.SetAccountRuntimeBlocker(blocker)
	calls := installOpenAI429RecoveryAfterRecorder(t, svc)
	now := time.Now()
	account := openai429RecoveryEligibleAccount(9105, now)
	account.Extra["model_rate_limits"] = map[string]any{
		"gpt-5.4": map[string]any{"rate_limit_reset_at": now.Add(time.Hour).Format(time.RFC3339)},
		"gpt-5.3": map[string]any{"rate_limit_reset_at": now.Add(2 * time.Hour).Format(time.RFC3339)},
	}
	repo.store(account)
	quota429Mu.Lock()
	quota429Counts[account.ID] = 12
	quota429Mu.Unlock()

	var probedModel string
	svc.openai429RecoveryProbe = func(_ context.Context, _ *Account, model string) openai429RecoveryProbeResult {
		probedModel = model
		return openai429RecoveryProbeResult{Status: http.StatusOK, Headers: openai429RecoveryRemainingHeaders()}
	}

	svc.markOpenAIFormalCooldown(account, *account.RateLimitResetAt, "gpt-5.4")
	require.Len(t, *calls, 1)
	fireLastOpenAI429RecoveryAfter(t, calls)

	require.Equal(t, "gpt-5.4", probedModel)
	require.Equal(t, []int64{account.ID}, repo.clearRateLimitIDs)
	require.Empty(t, repo.clearModelRateLimitIDs)
	require.Empty(t, repo.clearTempUnschedulableIDs)
	require.Equal(t, 0, openAI429Count(account.ID))
	require.Equal(t, []int64{account.ID}, blocker.clearedIDs)
	require.Nil(t, svc.openai429RecoveryStates[account.ID])

	limits, ok := account.Extra[modelRateLimitsKey].(map[string]any)
	require.True(t, ok)
	_, hasProbed := limits["gpt-5.4"]
	_, hasOther := limits["gpt-5.3"]
	require.False(t, hasProbed)
	require.True(t, hasOther)
}

func TestOpenAI429RecoveryProbe429DoesNotBumpConsecutiveOrSetRateLimited(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &openai429RecoveryRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	calls := installOpenAI429RecoveryAfterRecorder(t, svc)
	account := openai429RecoveryEligibleAccount(9106, time.Now())
	repo.store(account)
	quota429Mu.Lock()
	quota429Counts[account.ID] = 8
	quota429Mu.Unlock()

	svc.openai429RecoveryProbe = func(_ context.Context, _ *Account, _ string) openai429RecoveryProbeResult {
		return openai429RecoveryProbeResult{Status: http.StatusTooManyRequests, Headers: openai429RecoveryRemainingHeaders()}
	}

	svc.markOpenAIFormalCooldown(account, *account.RateLimitResetAt, "gpt-5.4")
	require.Len(t, *calls, 1)
	fireLastOpenAI429RecoveryAfter(t, calls)

	require.Empty(t, repo.setRateLimitedIDs)
	require.Empty(t, repo.clearRateLimitIDs)
	require.Equal(t, 8, openAI429Count(account.ID))
	require.Len(t, *calls, 2)
	require.Equal(t, time.Minute, (*calls)[1].delay)

	svc.openai429RecoveryMu.Lock()
	state := svc.openai429RecoveryStates[account.ID]
	svc.openai429RecoveryMu.Unlock()
	require.NotNil(t, state)
	require.Equal(t, 1, state.attempts)
	require.False(t, state.inFlight)
}

func TestOpenAI429RecoveryProbe429ExhaustedWindowsStops(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &openai429RecoveryRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	calls := installOpenAI429RecoveryAfterRecorder(t, svc)
	account := openai429RecoveryEligibleAccount(9107, time.Now())
	repo.store(account)

	svc.openai429RecoveryProbe = func(_ context.Context, _ *Account, _ string) openai429RecoveryProbeResult {
		return openai429RecoveryProbeResult{Status: http.StatusTooManyRequests, Headers: openai429Headers()}
	}

	svc.markOpenAIFormalCooldown(account, *account.RateLimitResetAt, "gpt-5.4")
	require.Len(t, *calls, 1)
	fireLastOpenAI429RecoveryAfter(t, calls)

	require.Empty(t, repo.setRateLimitedIDs)
	require.Equal(t, 0, openAI429Count(account.ID))
	require.Len(t, *calls, 1)
	require.Nil(t, svc.openai429RecoveryStates[account.ID])
}

func TestOpenAI429RecoveryProbeAuthStops(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &openai429RecoveryRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	calls := installOpenAI429RecoveryAfterRecorder(t, svc)
	account := openai429RecoveryEligibleAccount(9108, time.Now())
	repo.store(account)

	svc.openai429RecoveryProbe = func(_ context.Context, _ *Account, _ string) openai429RecoveryProbeResult {
		return openai429RecoveryProbeResult{Status: http.StatusUnauthorized}
	}

	svc.markOpenAIFormalCooldown(account, *account.RateLimitResetAt, "gpt-5.4")
	fireLastOpenAI429RecoveryAfter(t, calls)
	require.Nil(t, svc.openai429RecoveryStates[account.ID])
	require.Empty(t, repo.clearRateLimitIDs)
}

func TestOpenAI429RecoveryProbeStopsWhenRemainingAtOrBelowDelay(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &openai429RecoveryRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	calls := installOpenAI429RecoveryAfterRecorder(t, svc)
	now := time.Now()
	resetAt := now.Add(50 * time.Second)
	limitedAt := now.Add(-time.Minute)
	account := &Account{
		ID:               9109,
		Platform:         PlatformOpenAI,
		Type:             AccountTypeOAuth,
		RateLimitedAt:    &limitedAt,
		RateLimitResetAt: &resetAt,
		Extra:            openai429RecoveryEligibleExtra(now),
	}
	repo.store(account)

	svc.openai429RecoveryProbe = func(_ context.Context, _ *Account, _ string) openai429RecoveryProbeResult {
		return openai429RecoveryProbeResult{Status: http.StatusTooManyRequests, Headers: openai429RecoveryRemainingHeaders()}
	}

	svc.markOpenAIFormalCooldown(account, resetAt, "gpt-5.4")
	require.Len(t, *calls, 1)
	require.Equal(t, 30*time.Second, (*calls)[0].delay)
	fireLastOpenAI429RecoveryAfter(t, calls)
	require.Len(t, *calls, 1)
	require.Nil(t, svc.openai429RecoveryStates[account.ID])
}

func TestOpenAI429RecoveryShadowSkipped(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &openai429RecoveryRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	calls := installOpenAI429RecoveryAfterRecorder(t, svc)
	parent := int64(1)
	now := time.Now()
	resetAt := now.Add(2 * time.Hour)
	shadow := &Account{
		ID:               9110,
		Platform:         PlatformOpenAI,
		Type:             AccountTypeOAuth,
		ParentAccountID:  &parent,
		RateLimitResetAt: &resetAt,
		Extra:            openai429RecoveryEligibleExtra(now),
	}
	repo.store(shadow)

	svc.HandleUpstreamError(context.Background(), shadow, http.StatusTooManyRequests, openai429RecoveryRemainingHeaders(), openai429UsageLimitReachedBody(resetAt), "gpt-5.4")
	svc.markOpenAIFormalCooldown(shadow, resetAt, "gpt-5.4")
	svc.ConsiderOpenAI429Recovery(shadow)
	require.Empty(t, *calls)
	require.Empty(t, repo.setRateLimitedIDs)
	require.Equal(t, 0, openAI429Count(shadow.ID))
}

func TestGetOpenAIUsage_ConsiderOpenAI429RecoverySkipsShadow(t *testing.T) {
	recorder := &considerCallRecorder{}
	svc := NewAccountUsageService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	svc.SetOpenAI429RecoveryScheduler(recorder)
	now := time.Now()
	resetAt := now.Add(2 * time.Hour)
	account := openai429RecoveryEligibleAccount(9111, now)
	_, err := svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []int64{account.ID}, recorder.accountIDs)

	parent := int64(1)
	shadow := &Account{
		ID:               9112,
		Platform:         PlatformOpenAI,
		Type:             AccountTypeOAuth,
		ParentAccountID:  &parent,
		RateLimitResetAt: &resetAt,
		Extra:            openai429RecoveryEligibleExtra(now),
	}
	_, err = svc.GetUsageForAccount(context.Background(), shadow)
	require.NoError(t, err)
	require.Equal(t, []int64{account.ID}, recorder.accountIDs)
}
