package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIAutoResetCreditExtra(t *testing.T) {
	t.Run("历史账号默认关闭", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
		config := ResolveOpenAIAutoResetCreditConfig(account)
		require.False(t, config.Enabled)
		require.Equal(t, 1.0, config.Threshold5h)
		require.Equal(t, 1.0, config.Threshold7d)
	})

	t.Run("开启时补齐两个百分百阈值并剥离运行态", func(t *testing.T) {
		extra, err := normalizeOpenAIAutoResetCreditExtra(PlatformOpenAI, AccountTypeOAuth, map[string]any{
			OpenAIAutoResetCreditEnabledExtraKey: true,
			OpenAIAutoResetCreditStateExtraKey:   map[string]any{"status": "success"},
		})
		require.NoError(t, err)
		require.Equal(t, 1.0, extra[OpenAIAutoResetCredit5hThresholdExtraKey])
		require.Equal(t, 1.0, extra[OpenAIAutoResetCredit7dThresholdExtraKey])
		require.NotContains(t, extra, OpenAIAutoResetCreditStateExtraKey)
	})

	t.Run("阈值和账号类型严格校验", func(t *testing.T) {
		_, err := normalizeOpenAIAutoResetCreditExtra(PlatformOpenAI, AccountTypeOAuth, map[string]any{
			OpenAIAutoResetCreditEnabledExtraKey:     true,
			OpenAIAutoResetCredit5hThresholdExtraKey: 0.0009,
		})
		require.Error(t, err)

		_, err = normalizeOpenAIAutoResetCreditExtra(PlatformOpenAI, AccountTypeAPIKey, map[string]any{
			OpenAIAutoResetCreditEnabledExtraKey: true,
		})
		require.Error(t, err)
	})
}

func TestShouldAutoPauseOpenAIAccountByQuota_AutoResetCreditStates(t *testing.T) {
	now := time.Now().UTC()
	baseExtra := map[string]any{
		OpenAIAutoResetCreditEnabledExtraKey:      true,
		OpenAIAutoResetCreditOptInVersionExtraKey: openAIAutoResetCreditOptInVersion,
		OpenAIAutoResetCredit5hThresholdExtraKey:  1.0,
		OpenAIAutoResetCredit7dThresholdExtraKey:  1.0,
		"auto_pause_5h_threshold":                 0.8,
		"auto_pause_7d_disabled":                  true,
		"codex_5h_used_percent":                   90.0,
		"codex_usage_updated_at":                  now.Format(time.RFC3339),
		"codex_5h_reset_at":                       now.Add(time.Hour).Format(time.RFC3339),
	}

	t.Run("卡状态未知时暂停并触发异步查询", func(t *testing.T) {
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: cloneOpenAIAutoResetExtra(baseExtra)}
		paused, decision := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
		require.True(t, paused)
		require.Equal(t, "quota_auto_reset_credit_check_5h", decision.reason)
	})

	t.Run("明确有卡时允许继续到用卡阈值", func(t *testing.T) {
		extra := cloneOpenAIAutoResetExtra(baseExtra)
		extra[OpenAIAutoResetCreditStateExtraKey] = OpenAIAutoResetCreditState{
			Status: OpenAIAutoResetStatusAvailable, AvailableCount: 1, CheckedAt: now.Format(time.RFC3339),
		}
		account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra}
		paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
		require.False(t, paused)
	})

	t.Run("达到用卡阈值后即使有卡也退出调度", func(t *testing.T) {
		extra := cloneOpenAIAutoResetExtra(baseExtra)
		extra["codex_5h_used_percent"] = 100.0
		extra[OpenAIAutoResetCreditStateExtraKey] = OpenAIAutoResetCreditState{
			Status: OpenAIAutoResetStatusAvailable, AvailableCount: 1, CheckedAt: now.Format(time.RFC3339),
		}
		account := &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra}
		paused, decision := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
		require.True(t, paused)
		require.Equal(t, "quota_auto_reset_pending_5h", decision.reason)
	})

	t.Run("自然窗口重置后清除动态阻塞", func(t *testing.T) {
		extra := cloneOpenAIAutoResetExtra(baseExtra)
		extra["codex_5h_used_percent"] = 100.0
		extra["codex_5h_reset_at"] = now.Add(-time.Second).Format(time.RFC3339)
		extra[OpenAIAutoResetCreditStateExtraKey] = OpenAIAutoResetCreditState{
			Status: OpenAIAutoResetStatusFailed, TriggerWindow: "5h", ErrorCode: "RESET_FAILED",
		}
		account := &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra}
		paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
		require.False(t, paused)
	})
}

func TestSelectOpenAIAutoResetCandidate_FailsClosed(t *testing.T) {
	candidates := []openAIAutoResetCreditCandidate{
		{ID: "later", ExpiresAt: "2099-09-02T00:00:00Z"},
		{ID: "earlier", ExpiresAt: "2099-09-01T00:00:00Z"},
	}
	selected, err := selectOpenAIAutoResetCandidate(candidates, 2, nil, "cycle-a")
	require.NoError(t, err)
	require.Equal(t, "earlier", selected.ID)

	_, err = selectOpenAIAutoResetCandidate([]openAIAutoResetCreditCandidate{
		{ExpiresAt: "2099-09-01T00:00:00Z"},
	}, 1, nil, "cycle-a")
	require.Error(t, err)

	_, err = selectOpenAIAutoResetCandidate(candidates, 2, &OpenAIAutoResetCreditState{
		AttemptCycleHash: "cycle-a", AttemptCreditHash: shortOpenAIAutoResetHash("missing"),
	}, "cycle-a")
	require.Error(t, err, "模糊结果后原卡消失时不得切换下一张卡")
}

func TestOpenAIQuotaAutoResetService_AssessesIndependentWindows(t *testing.T) {
	service := &OpenAIQuotaAutoResetService{}
	account := &Account{Extra: map[string]any{
		"auto_pause_5h_disabled": true,
		"auto_pause_7d_disabled": true,
	}}
	config := OpenAIAutoResetCreditConfig{Enabled: true, Threshold5h: 0.8, Threshold7d: 0.9}
	tests := []struct {
		name       string
		fiveHour   float64
		sevenDay   float64
		wantWindow string
	}{
		{name: "5h", fiveHour: 0.8, sevenDay: 0.2, wantWindow: "5h"},
		{name: "7d", fiveHour: 0.2, sevenDay: 0.9, wantWindow: "7d"},
		{name: "同时触发", fiveHour: 0.95, sevenDay: 0.95, wantWindow: "5h+7d"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assessment := service.buildAssessment(account, config, test.fiveHour, test.sevenDay)
			require.True(t, assessment.resetReached)
			require.Equal(t, test.wantWindow, assessment.triggerWindow)
		})
	}
}

type autoResetTestAccountRepo struct {
	AccountRepository
	mu      sync.Mutex
	account *Account
}

func (r *autoResetTestAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := *r.account
	copy.Extra = cloneOpenAIAutoResetExtra(r.account.Extra)
	return &copy, nil
}

func (r *autoResetTestAccountRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	// Match JSONB storage: callers cannot mutate a previously persisted state.
	encoded, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	var stored map[string]any
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account.Extra == nil {
		r.account.Extra = make(map[string]any)
	}
	for key, value := range stored {
		r.account.Extra[key] = value
	}
	return nil
}

type autoResetTestQuota struct {
	usage        *OpenAIQuotaUsage
	queryCalls   atomic.Int32
	onQuery      func()
	queryErr     error
	resetResult  *OpenAIQuotaResetResult
	resetCalls   atomic.Int32
	resetEntered chan struct{}
	releaseReset chan struct{}
	enterOnce    sync.Once
	mu           sync.Mutex
	resetArgs    [][2]string
	failFirst    bool
}

func (q *autoResetTestQuota) QueryUsage(context.Context, int64) (*OpenAIQuotaUsage, error) {
	q.queryCalls.Add(1)
	if q.onQuery != nil {
		q.onQuery()
	}
	if q.queryErr != nil {
		return nil, q.queryErr
	}
	copy := *q.usage
	return &copy, nil
}

func (q *autoResetTestQuota) CacheResetCreditsSnapshot(context.Context, int64, *OpenAIRateLimitResetCredits) error {
	return nil
}

func (q *autoResetTestQuota) CachePostResetSnapshot(context.Context, int64, *OpenAIQuotaUsage) error {
	return nil
}

func (q *autoResetTestQuota) ResetCreditTargeted(_ context.Context, _ int64, creditID, redeemRequestID string) (*OpenAIQuotaResetResult, error) {
	if creditID == "" || redeemRequestID == "" {
		panic("targeted reset identifiers must be present")
	}
	call := q.resetCalls.Add(1)
	q.mu.Lock()
	q.resetArgs = append(q.resetArgs, [2]string{creditID, redeemRequestID})
	q.mu.Unlock()
	if q.failFirst && call == 1 {
		return nil, context.DeadlineExceeded
	}
	if q.resetEntered != nil {
		q.enterOnce.Do(func() { close(q.resetEntered) })
	}
	if q.releaseReset != nil {
		<-q.releaseReset
	}
	if q.resetResult != nil {
		return q.resetResult, nil
	}
	return &OpenAIQuotaResetResult{Code: "ok", WindowsReset: 2}, nil
}

type autoResetTestRecoverer struct{}

func (autoResetTestRecoverer) RecoverAccountState(context.Context, int64, AccountRecoveryOptions) (*SuccessfulTestRecoveryResult, error) {
	return &SuccessfulTestRecoveryResult{ClearedRateLimit: true}, nil
}

func TestOpenAIQuotaAutoResetService_ConcurrentInstancesConsumeOnce(t *testing.T) {
	now := time.Now().UTC()
	account := &Account{
		ID: 99, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
		Extra: map[string]any{
			OpenAIAutoResetCreditEnabledExtraKey:      true,
			OpenAIAutoResetCreditOptInVersionExtraKey: openAIAutoResetCreditOptInVersion,
			OpenAIAutoResetCredit5hThresholdExtraKey:  1.0,
			OpenAIAutoResetCredit7dThresholdExtraKey:  1.0,
			"codex_5h_used_percent":                   100.0,
			"codex_7d_used_percent":                   10.0,
			"codex_usage_updated_at":                  now.Format(time.RFC3339),
			"codex_5h_reset_at":                       now.Add(time.Hour).Format(time.RFC3339),
			"codex_7d_reset_at":                       now.Add(24 * time.Hour).Format(time.RFC3339),
		},
	}
	repo := &autoResetTestAccountRepo{account: account}
	usage := &OpenAIQuotaUsage{
		FetchedAt: now.Unix(),
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow:   &OpenAIRateLimitWindow{UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 3600, ResetAt: now.Add(time.Hour).Unix()},
			SecondaryWindow: &OpenAIRateLimitWindow{UsedPercent: 10, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 86400, ResetAt: now.Add(24 * time.Hour).Unix()},
		},
		RateLimitResetCredits: &OpenAIRateLimitResetCredits{
			AvailableCount: 1,
			Credits:        []OpenAIRateLimitResetCreditDetail{{ExpiresAt: now.Add(48 * time.Hour).Format(time.RFC3339)}},
		},
		autoResetCandidates: []openAIAutoResetCreditCandidate{{ID: "credit-sensitive-id", ExpiresAt: now.Add(48 * time.Hour).Format(time.RFC3339)}},
	}
	quota := &autoResetTestQuota{usage: usage, resetEntered: make(chan struct{}), releaseReset: make(chan struct{})}
	idempotencyRepo := newInMemoryIdempotencyRepo()
	config := DefaultIdempotencyConfig()
	config.ObserveOnly = false
	config.ProcessingTimeout = time.Second
	serviceA := NewOpenAIQuotaAutoResetService(repo, quota, autoResetTestRecoverer{}, NewIdempotencyCoordinator(idempotencyRepo, config), nil, nil, nil)
	serviceB := NewOpenAIQuotaAutoResetService(repo, quota, autoResetTestRecoverer{}, NewIdempotencyCoordinator(idempotencyRepo, config), nil, nil, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = serviceA.evaluateAccount(context.Background(), account.ID)
	}()
	<-quota.resetEntered
	go func() {
		defer wg.Done()
		_ = serviceB.evaluateAccount(context.Background(), account.ID)
	}()
	time.Sleep(50 * time.Millisecond)
	close(quota.releaseReset)
	wg.Wait()

	require.Equal(t, int32(1), quota.resetCalls.Load())
	repo.mu.Lock()
	state := openAIAutoResetStateFromExtra(repo.account.Extra)
	repo.mu.Unlock()
	require.NotNil(t, state)
	require.Equal(t, OpenAIAutoResetStatusSuccess, state.Status)
	encodedState, err := json.Marshal(state)
	require.NoError(t, err)
	require.NotContains(t, string(encodedState), "credit-sensitive-id")
}

func TestOpenAIQuotaAutoResetService_TimeoutRetryReusesRequestBody(t *testing.T) {
	now := time.Now().UTC()
	account := &Account{
		ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
		Extra: map[string]any{
			OpenAIAutoResetCreditEnabledExtraKey:      true,
			OpenAIAutoResetCreditOptInVersionExtraKey: openAIAutoResetCreditOptInVersion,
			OpenAIAutoResetCredit5hThresholdExtraKey:  1.0,
			OpenAIAutoResetCredit7dThresholdExtraKey:  1.0,
			"codex_5h_used_percent":                   100.0,
			"codex_usage_updated_at":                  now.Format(time.RFC3339),
			"codex_5h_reset_at":                       now.Add(time.Hour).Format(time.RFC3339),
		},
	}
	repo := &autoResetTestAccountRepo{account: account}
	expiresAt := now.Add(48 * time.Hour).Format(time.RFC3339)
	quota := &autoResetTestQuota{
		failFirst: true,
		usage: &OpenAIQuotaUsage{
			FetchedAt: now.Unix(),
			RateLimit: &OpenAIRateLimit{
				PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 3600, ResetAt: now.Add(time.Hour).Unix()},
			},
			RateLimitResetCredits: &OpenAIRateLimitResetCredits{
				AvailableCount: 1,
				Credits:        []OpenAIRateLimitResetCreditDetail{{ExpiresAt: expiresAt}},
			},
			autoResetCandidates: []openAIAutoResetCreditCandidate{{ID: "retry-credit", ExpiresAt: expiresAt}},
		},
	}
	idempotencyConfig := DefaultIdempotencyConfig()
	idempotencyConfig.ObserveOnly = false
	idempotencyConfig.FailedRetryBackoff = 0
	service := NewOpenAIQuotaAutoResetService(
		repo,
		quota,
		autoResetTestRecoverer{},
		NewIdempotencyCoordinator(newInMemoryIdempotencyRepo(), idempotencyConfig),
		nil, nil, nil,
	)

	require.Error(t, service.evaluateAccount(context.Background(), account.ID))
	ageAutoResetCheck(t, repo)
	require.NoError(t, service.evaluateAccount(context.Background(), account.ID))
	quota.mu.Lock()
	args := append([][2]string(nil), quota.resetArgs...)
	quota.mu.Unlock()
	require.Len(t, args, 2)
	require.Equal(t, args[0], args[1], "超时重试必须复用相同 credit_id 与 redeem_request_id")
}

func newAutoResetEvaluationTest() (*OpenAIQuotaAutoResetService, *autoResetTestAccountRepo, *autoResetTestQuota) {
	now := time.Now().UTC()
	account := &Account{
		ID: 101, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{
			OpenAIAutoResetCreditEnabledExtraKey:      true,
			OpenAIAutoResetCreditOptInVersionExtraKey: openAIAutoResetCreditOptInVersion,
			"codex_5h_used_percent":                   100.0,
			"codex_usage_updated_at":                  now.Format(time.RFC3339),
			"codex_5h_reset_at":                       now.Add(time.Hour).Format(time.RFC3339),
		},
	}
	repo := &autoResetTestAccountRepo{account: account}
	expiresAt := now.Add(48 * time.Hour).Format(time.RFC3339)
	quota := &autoResetTestQuota{usage: &OpenAIQuotaUsage{
		FetchedAt: now.Unix(),
		RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{
			UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 3600, ResetAt: now.Add(time.Hour).Unix(),
		}},
		RateLimitResetCredits: &OpenAIRateLimitResetCredits{
			AvailableCount: 1, Credits: []OpenAIRateLimitResetCreditDetail{{ExpiresAt: expiresAt}},
		},
		autoResetCandidates: []openAIAutoResetCreditCandidate{{ID: "credit-a", ExpiresAt: expiresAt}},
	}}
	idempotencyConfig := DefaultIdempotencyConfig()
	idempotencyConfig.ObserveOnly = false
	svc := NewOpenAIQuotaAutoResetService(repo, quota, autoResetTestRecoverer{}, NewIdempotencyCoordinator(newInMemoryIdempotencyRepo(), idempotencyConfig), nil, nil, nil)
	return svc, repo, quota
}

func TestOpenAIQuotaAutoResetService_RechecksEligibilityBeforeConsuming(t *testing.T) {
	changes := []struct {
		name   string
		change func(*Account)
	}{
		{"disabled", func(a *Account) { a.Extra[OpenAIAutoResetCreditEnabledExtraKey] = false }},
		{"paused", func(a *Account) { a.Schedulable = false }},
		{"inactive", func(a *Account) { a.Status = "inactive" }},
		{"expired", func(a *Account) {
			expired := time.Now().Add(-time.Hour)
			a.ExpiresAt = &expired
			a.AutoPauseOnExpired = true
		}},
		{"non OAuth", func(a *Account) { a.Type = AccountTypeAPIKey }},
	}
	for _, change := range changes {
		for _, duringQuery := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/duringQuery=%t", change.name, duringQuery), func(t *testing.T) {
				svc, repo, quota := newAutoResetEvaluationTest()
				defer svc.Stop()
				if duringQuery {
					quota.onQuery = func() { change.change(repo.account) }
				} else {
					change.change(repo.account)
				}
				require.NoError(t, svc.evaluateAccount(context.Background(), repo.account.ID))
				require.Zero(t, quota.resetCalls.Load())
				if !duringQuery {
					require.Zero(t, quota.queryCalls.Load())
				}
			})
		}
	}
}

func TestOpenAIQuotaAutoResetService_UsesLiveUsageBeforeConsuming(t *testing.T) {
	svc, repo, quota := newAutoResetEvaluationTest()
	defer svc.Stop()
	quota.usage.RateLimit.PrimaryWindow.UsedPercent = 20
	require.NoError(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.Zero(t, quota.resetCalls.Load(), "a stale 100%% snapshot must not spend a credit")
	state := openAIAutoResetStateFromExtra(repo.account.Extra)
	require.Equal(t, OpenAIAutoResetStatusAvailable, state.Status)
	require.Equal(t, 20.0, repo.account.Extra["codex_5h_used_percent"])
}

func TestOpenAIQuotaAutoResetService_NoCreditAndFailedReadsDoNotConsume(t *testing.T) {
	tests := []struct {
		name       string
		change     func(*autoResetTestQuota)
		wantError  bool
		wantStatus string
	}{
		{"no credits", func(q *autoResetTestQuota) { q.usage.RateLimitResetCredits.AvailableCount = 0 }, false, OpenAIAutoResetStatusNoCredit},
		{"query failure", func(q *autoResetTestQuota) { q.queryErr = errors.New("upstream unavailable") }, true, OpenAIAutoResetStatusFailed},
		{"missing details", func(q *autoResetTestQuota) { q.usage.autoResetCandidates = nil }, true, OpenAIAutoResetStatusFailed},
		{"missing card ID", func(q *autoResetTestQuota) { q.usage.autoResetCandidates[0].ID = "" }, true, OpenAIAutoResetStatusFailed},
		{"expired card", func(q *autoResetTestQuota) {
			q.usage.autoResetCandidates[0].ExpiresAt = time.Now().Add(-time.Hour).Format(time.RFC3339)
		}, true, OpenAIAutoResetStatusFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc, repo, quota := newAutoResetEvaluationTest()
			defer svc.Stop()
			test.change(quota)
			err := svc.evaluateAccount(context.Background(), repo.account.ID)
			require.Equal(t, test.wantError, err != nil)
			require.Zero(t, quota.resetCalls.Load())
			require.Equal(t, test.wantStatus, openAIAutoResetStateFromExtra(repo.account.Extra).Status)
			paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), repo.account)
			require.True(t, paused)
		})
	}
}

func TestOpenAIQuotaAutoResetService_HonorsAttemptCreatedDuringQuery(t *testing.T) {
	svc, repo, quota := newAutoResetEvaluationTest()
	defer svc.Stop()
	quota.onQuery = func() {
		require.NoError(t, repo.UpdateExtra(context.Background(), repo.account.ID, map[string]any{
			OpenAIAutoResetCreditStateExtraKey: &OpenAIAutoResetCreditState{
				Status:            OpenAIAutoResetStatusResetting,
				AttemptCycleHash:  shortOpenAIAutoResetHash(openAIAutoResetCycleSeed(quota.usage)),
				AttemptCreditHash: shortOpenAIAutoResetHash("original-card-already-consumed"),
			},
		}))
	}
	require.Error(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.Zero(t, quota.resetCalls.Load(), "a late query must not switch to the next card")
	quota.onQuery = nil
	ageAutoResetCheck(t, repo)
	require.Error(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.Zero(t, quota.resetCalls.Load(), "the original attempt must remain protected on subsequent retries")
}

func TestOpenAIQuotaAutoResetService_UpstreamNoCreditDoesNotReportSuccess(t *testing.T) {
	svc, repo, quota := newAutoResetEvaluationTest()
	defer svc.Stop()
	quota.resetResult = &OpenAIQuotaResetResult{Code: "no_credit"}
	require.NoError(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.EqualValues(t, 1, quota.queryCalls.Load(), "no recovery or post-reset refresh without a consumed credit")
	require.Equal(t, OpenAIAutoResetStatusNoCredit, openAIAutoResetStateFromExtra(repo.account.Extra).Status)
}

func TestOpenAIQuotaAutoResetService_DoesNotConsumeAfterNaturalReset(t *testing.T) {
	svc, repo, quota := newAutoResetEvaluationTest()
	defer svc.Stop()
	quota.usage.RateLimit.PrimaryWindow.ResetAt = time.Now().Add(-time.Second).Unix()
	require.NoError(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.Zero(t, quota.resetCalls.Load())
	paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), repo.account)
	require.False(t, paused)
}

func TestSelectOpenAIAutoResetCandidate_StableForEqualExpiries(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).Format(time.RFC3339)
	a := openAIAutoResetCreditCandidate{ID: "a", ExpiresAt: expiresAt}
	b := openAIAutoResetCreditCandidate{ID: "b", ExpiresAt: expiresAt}
	for _, candidates := range [][]openAIAutoResetCreditCandidate{{a, b}, {b, a}} {
		selected, err := selectOpenAIAutoResetCandidate(candidates, 2, nil, "cycle-a")
		require.NoError(t, err)
		require.Equal(t, "a", selected.ID)
	}
}

func TestOpenAIQuotaAutoResetService_UnconfirmedResetDoesNotRecover(t *testing.T) {
	for _, result := range []*OpenAIQuotaResetResult{{Code: "pending"}, {Code: "ok", WindowsReset: 0}} {
		svc, repo, quota := newAutoResetEvaluationTest()
		quota.resetResult = result
		require.Error(t, svc.evaluateAccount(context.Background(), repo.account.ID))
		require.EqualValues(t, 1, quota.queryCalls.Load())
		require.Equal(t, "OPENAI_AUTO_RESET_UNCONFIRMED", openAIAutoResetStateFromExtra(repo.account.Extra).ErrorCode)
		svc.Stop()
	}
}

func ageAutoResetCheck(t *testing.T, repo *autoResetTestAccountRepo) {
	t.Helper()
	account, err := repo.GetByID(context.Background(), repo.account.ID)
	require.NoError(t, err)
	state := openAIAutoResetStateFromExtra(account.Extra)
	require.NotNil(t, state)
	state.CheckedAt = time.Now().Add(-2 * openAIAutoResetScanInterval).UTC().Format(time.RFC3339)
	require.NoError(t, repo.UpdateExtra(context.Background(), account.ID, map[string]any{OpenAIAutoResetCreditStateExtraKey: state}))
}

func TestOpenAIAutoResetCreditConfig_RequiresNewExplicitOptIn(t *testing.T) {
	legacy := map[string]any{OpenAIAutoResetCreditEnabledExtraKey: true}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: legacy}
	require.False(t, ResolveOpenAIAutoResetCreditConfig(account).Enabled, "retired enabled flags must not consume credits on upgrade")

	normalized, err := normalizeOpenAIAutoResetCreditExtra(account.Platform, account.Type, legacy)
	require.NoError(t, err)
	account.Extra = normalized
	require.True(t, ResolveOpenAIAutoResetCreditConfig(account).Enabled)
	require.Equal(t, openAIAutoResetCreditOptInVersion, normalized[OpenAIAutoResetCreditOptInVersionExtraKey])
	require.NotContains(t, legacy, OpenAIAutoResetCreditOptInVersionExtraKey, "normalization must not mutate the old snapshot")

	for _, input := range []map[string]any{
		{OpenAIAutoResetCreditOptInVersionExtraKey: 1},
		{OpenAIAutoResetCreditEnabledExtraKey: false, OpenAIAutoResetCreditOptInVersionExtraKey: 1},
	} {
		normalized, err = normalizeOpenAIAutoResetCreditExtra(account.Platform, account.Type, input)
		require.NoError(t, err)
		require.NotContains(t, normalized, OpenAIAutoResetCreditOptInVersionExtraKey)
	}
	stripped := stripOpenAIAutoResetCreditManagedExtra(map[string]any{
		OpenAIAutoResetCreditEnabledExtraKey:      true,
		OpenAIAutoResetCreditOptInVersionExtraKey: 1,
	}, true)
	require.Empty(t, stripped, "extra-only and bulk admin updates cannot forge the opt-in version")
}

func TestOpenAIQuotaAutoResetService_RateLimitsFailedAndNoCreditChecks(t *testing.T) {
	for _, failQuery := range []bool{false, true} {
		t.Run(fmt.Sprint(failQuery), func(t *testing.T) {
			svc, repo, quota := newAutoResetEvaluationTest()
			defer svc.Stop()
			if failQuery {
				quota.queryErr = errors.New("upstream unavailable")
			} else {
				quota.usage.RateLimitResetCredits.AvailableCount = 0
			}
			_ = svc.evaluateAccount(context.Background(), repo.account.ID)
			for range 10 {
				require.NoError(t, svc.evaluateAccount(context.Background(), repo.account.ID))
			}
			require.EqualValues(t, 1, quota.queryCalls.Load())
			ageAutoResetCheck(t, repo)
			_ = svc.evaluateAccount(context.Background(), repo.account.ID)
			require.EqualValues(t, 2, quota.queryCalls.Load())
			require.Zero(t, quota.resetCalls.Load())
		})
	}
}

func TestOpenAIQuotaAutoResetService_OneCardPerCycleAfterLateStateOverwrite(t *testing.T) {
	svc, repo, quota := newAutoResetEvaluationTest()
	defer svc.Stop()
	require.NoError(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.EqualValues(t, 1, quota.resetCalls.Load())

	// Another instance read "no attempt" before the first consume, and its
	// checking write arrives late. The next card must not gain a different key.
	require.NoError(t, repo.UpdateExtra(context.Background(), repo.account.ID, map[string]any{
		OpenAIAutoResetCreditStateExtraKey: &OpenAIAutoResetCreditState{Status: OpenAIAutoResetStatusChecking},
	}))
	quota.usage.autoResetCandidates[0].ID = "next-card"
	require.ErrorIs(t, svc.evaluateAccount(context.Background(), repo.account.ID), ErrIdempotencyKeyConflict)
	require.EqualValues(t, 1, quota.resetCalls.Load(), "a different card must not bypass the cycle's database claim")
}

func TestOpenAIQuotaAutoResetService_RetainsAttemptsAcrossEmptyOrLowUsageReads(t *testing.T) {
	for _, noCredit := range []bool{false, true} {
		t.Run(fmt.Sprint(noCredit), func(t *testing.T) {
			svc, repo, quota := newAutoResetEvaluationTest()
			defer svc.Stop()
			attempt := &OpenAIAutoResetCreditState{
				Status:            OpenAIAutoResetStatusResetting,
				AttemptCycleHash:  shortOpenAIAutoResetHash(openAIAutoResetCycleSeed(quota.usage)),
				AttemptCreditHash: shortOpenAIAutoResetHash("original-card"),
			}
			require.NoError(t, repo.UpdateExtra(context.Background(), repo.account.ID, map[string]any{OpenAIAutoResetCreditStateExtraKey: attempt}))
			if noCredit {
				quota.usage.RateLimitResetCredits.AvailableCount = 0
			} else {
				quota.usage.RateLimit.PrimaryWindow.UsedPercent = 20
			}
			require.NoError(t, svc.evaluateAccount(context.Background(), repo.account.ID))
			state := openAIAutoResetStateFromExtra(repo.account.Extra)
			require.Equal(t, attempt.AttemptCycleHash, state.AttemptCycleHash)
			require.Equal(t, attempt.AttemptCreditHash, state.AttemptCreditHash)
			require.Zero(t, quota.resetCalls.Load())
		})
	}
}

type retryAutoResetRecoverer struct{ calls int }

func (r *retryAutoResetRecoverer) RecoverAccountState(context.Context, int64, AccountRecoveryOptions) (*SuccessfulTestRecoveryResult, error) {
	r.calls++
	if r.calls == 1 {
		return nil, errors.New("temporary recovery failure")
	}
	return &SuccessfulTestRecoveryResult{ClearedRateLimit: true}, nil
}

func TestOpenAIQuotaAutoResetService_RetriesRecoveryAfterConsumedCardDisappears(t *testing.T) {
	svc, repo, quota := newAutoResetEvaluationTest()
	defer svc.Stop()
	recoverer := &retryAutoResetRecoverer{}
	svc.recoverer = recoverer
	require.Error(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.EqualValues(t, 1, quota.resetCalls.Load())
	require.Equal(t, OpenAIQuotaResetWarningAccountRecoveryFailed, openAIAutoResetStateFromExtra(repo.account.Extra).ErrorCode)

	quota.usage.RateLimit.PrimaryWindow.UsedPercent = 0
	quota.usage.RateLimitResetCredits.AvailableCount = 0
	quota.usage.autoResetCandidates = nil
	ageAutoResetCheck(t, repo)
	require.NoError(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.Equal(t, 2, recoverer.calls)
	require.EqualValues(t, 1, quota.resetCalls.Load(), "recovery replay must never consume another card")
	require.Equal(t, OpenAIAutoResetStatusSuccess, openAIAutoResetStateFromExtra(repo.account.Extra).Status)
}

func TestOpenAIQuotaAutoResetService_RequiresStableUpstreamWindowBoundary(t *testing.T) {
	svc, repo, quota := newAutoResetEvaluationTest()
	defer svc.Stop()
	quota.usage.RateLimit.PrimaryWindow.ResetAt = 0
	require.Error(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.Equal(t, "OPENAI_AUTO_RESET_CYCLE_UNCONFIRMED", openAIAutoResetStateFromExtra(repo.account.Extra).ErrorCode)
	require.Zero(t, quota.resetCalls.Load())
}

func TestOpenAIQuotaAutoResetService_ReplaysStoredSuccessWithoutAnotherConsume(t *testing.T) {
	svc, repo, quota := newAutoResetEvaluationTest()
	defer svc.Stop()
	require.NoError(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.NoError(t, svc.evaluateAccount(context.Background(), repo.account.ID))
	require.EqualValues(t, 1, quota.resetCalls.Load())
	require.Equal(t, OpenAIAutoResetStatusSuccess, openAIAutoResetStateFromExtra(repo.account.Extra).Status)
}

type gatedAutoResetClaimRepo struct {
	IdempotencyRepository
	entered chan struct{}
	release chan struct{}
}

func (r *gatedAutoResetClaimRepo) CreateProcessing(ctx context.Context, record *IdempotencyRecord) (bool, error) {
	owner, err := r.IdempotencyRepository.CreateProcessing(ctx, record)
	if err != nil || !owner {
		return owner, err
	}
	close(r.entered)
	select {
	case <-r.release:
		return owner, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestOpenAIQuotaAutoResetService_RechecksAfterBlockedDatabaseClaim(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*Account)
	}{
		{"disabled", func(a *Account) { a.Extra[OpenAIAutoResetCreditEnabledExtraKey] = false }},
		{"paused", func(a *Account) { a.Schedulable = false }},
		{"raised threshold", func(a *Account) { a.Extra[OpenAIAutoResetCredit5hThresholdExtraKey] = 1.0 }},
	} {
		t.Run(change.name, func(t *testing.T) {
			svc, repo, quota := newAutoResetEvaluationTest()
			defer svc.Stop()
			repo.account.Extra[OpenAIAutoResetCredit5hThresholdExtraKey] = 0.5
			quota.usage.RateLimit.PrimaryWindow.UsedPercent = 75
			claim := &gatedAutoResetClaimRepo{IdempotencyRepository: svc.idempotency.repo, entered: make(chan struct{}), release: make(chan struct{})}
			svc.idempotency.repo = claim
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- svc.evaluateAccount(ctx, repo.account.ID) }()
			select {
			case <-claim.entered:
			case <-ctx.Done():
				t.Fatal("automatic reset did not reach database claim")
			}
			repo.mu.Lock()
			change.apply(repo.account)
			repo.mu.Unlock()
			close(claim.release)
			require.Error(t, <-done)
			require.Zero(t, quota.resetCalls.Load())
			require.Equal(t, "OPENAI_AUTO_RESET_CANCELLED", openAIAutoResetStateFromExtra(repo.account.Extra).ErrorCode)
		})
	}
}
