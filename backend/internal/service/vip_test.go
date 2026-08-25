package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestApplyVipRateDiscount(t *testing.T) {
	require.InDelta(t, 0.13, ApplyVipRateDiscount(0.15), 1e-9)
	require.InDelta(t, 0.05, ApplyVipRateDiscount(0.07), 1e-9)
	require.InDelta(t, 0.0, ApplyVipRateDiscount(0.02), 1e-9)
	require.InDelta(t, 0.0, ApplyVipRateDiscount(0.01), 1e-9)
}

func TestVipDiscountedGroup(t *testing.T) {
	require.True(t, VipDiscountedGroup("gpt-pro"))
	require.True(t, VipDiscountedGroup("GPT-Pro"))
	require.True(t, VipDiscountedGroup(" gpt-pro "))
	require.False(t, VipDiscountedGroup("gpt-pro-max"))
	require.False(t, VipDiscountedGroup("claude-code"))
	require.False(t, VipDiscountedGroup(""))
}

func TestApplyVipGroupRateDiscount(t *testing.T) {
	vip := &User{IsVIP: true}
	normal := &User{}
	group := &Group{Name: "gpt-pro"}
	other := &Group{Name: "claude-code"}

	require.InDelta(t, 0.13, applyVipGroupRateDiscount(vip, group, 0.15), 1e-9)
	require.InDelta(t, 0.15, applyVipGroupRateDiscount(normal, group, 0.15), 1e-9)
	require.InDelta(t, 0.15, applyVipGroupRateDiscount(vip, other, 0.15), 1e-9)
	require.InDelta(t, 0.15, applyVipGroupRateDiscount(vip, nil, 0.15), 1e-9)
	require.InDelta(t, 0.15, applyVipGroupRateDiscount(nil, group, 0.15), 1e-9)
}

type vipExecutorInvalidatorStub struct {
	invalidated []int64
}

func (s *vipExecutorInvalidatorStub) InvalidateAuthCacheByKey(ctx context.Context, key string) {}
func (s *vipExecutorInvalidatorStub) InvalidateAuthCacheByUserID(ctx context.Context, userID int64) {
	s.invalidated = append(s.invalidated, userID)
}
func (s *vipExecutorInvalidatorStub) InvalidateAuthCacheByGroupID(ctx context.Context, groupID int64) {
}

type vipExecutorUserRepoStub struct {
	UserRepository
	user        *User
	getErr      error
	setVIPCalls []int64
}

func (s *vipExecutorUserRepoStub) GetByID(ctx context.Context, id int64) (*User, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.user, nil
}

func (s *vipExecutorUserRepoStub) SetVIPIfNotSet(ctx context.Context, id int64) (bool, error) {
	s.setVIPCalls = append(s.setVIPCalls, id)
	return true, nil
}

func TestVipEnsureUpgrade(t *testing.T) {
	t.Run("upgrades when balance exceeds threshold", func(t *testing.T) {
		repo := &vipExecutorUserRepoStub{user: &User{ID: 7, Balance: 100.01}}
		invalidator := &vipExecutorInvalidatorStub{}
		executor := NewVipUpgradeExecutor(repo, invalidator)

		upgraded := executor.EnsureUpgrade(context.Background(), 7)

		require.True(t, upgraded)
		require.Equal(t, []int64{7}, repo.setVIPCalls)
		require.Equal(t, []int64{7}, invalidator.invalidated)
	})

	t.Run("skips at exactly the threshold", func(t *testing.T) {
		repo := &vipExecutorUserRepoStub{user: &User{ID: 8, Balance: VipBalanceThreshold}}
		executor := NewVipUpgradeExecutor(repo, &vipExecutorInvalidatorStub{})

		require.False(t, executor.EnsureUpgrade(context.Background(), 8))
		require.Empty(t, repo.setVIPCalls)
	})

	t.Run("skips existing vip", func(t *testing.T) {
		repo := &vipExecutorUserRepoStub{user: &User{ID: 9, Balance: 200, IsVIP: true}}
		executor := NewVipUpgradeExecutor(repo, &vipExecutorInvalidatorStub{})

		require.False(t, executor.EnsureUpgrade(context.Background(), 9))
		require.Empty(t, repo.setVIPCalls)
	})
}

type vipSweepUserRepoStub struct {
	UserRepository
	upgraded      []int64
	lastThreshold float64
}

func (s *vipSweepUserRepoStub) UpgradeUsersAboveBalanceThreshold(ctx context.Context, threshold float64) ([]int64, error) {
	s.lastThreshold = threshold
	return s.upgraded, nil
}

func TestVipSweepExisting(t *testing.T) {
	sweepRepo := &vipSweepUserRepoStub{upgraded: []int64{11, 12}}
	invalidator := &vipExecutorInvalidatorStub{}
	executor := NewVipUpgradeExecutor(sweepRepo, invalidator)

	upgraded := executor.SweepExisting(context.Background())

	require.Equal(t, []int64{11, 12}, upgraded)
	require.Equal(t, VipBalanceThreshold, sweepRepo.lastThreshold)
	require.Equal(t, []int64{11, 12}, invalidator.invalidated)
}

type vipEligibilityCacheStub struct {
	BillingCache
	balances map[int64]float64
}

func (c *vipEligibilityCacheStub) GetUserBalance(ctx context.Context, userID int64) (float64, error) {
	return c.balances[userID], nil
}

func newVipEligibilityService(t *testing.T, balances map[int64]float64, executor vipUpgradeExecutorProvider) *BillingCacheService {
	t.Helper()
	svc := NewBillingCacheService(&vipEligibilityCacheStub{balances: balances}, nil, nil, nil, nil, nil, &config.Config{}, nil)
	svc.SetVipUpgradeExecutor(executor)
	t.Cleanup(svc.Stop)
	return svc
}

func TestCheckBalanceEligibilityVipFrozenReserve(t *testing.T) {
	tests := []struct {
		name    string
		isVIP   bool
		balance float64
		wantErr bool
	}{
		{name: "non-vip positive balance passes", isVIP: false, balance: 5},
		{name: "non-vip zero balance rejected", isVIP: false, balance: 0, wantErr: true},
		{name: "non-vip negative balance rejected", isVIP: false, balance: -1, wantErr: true},
		{name: "vip above reserve passes", isVIP: true, balance: 150},
		{name: "vip at exactly frozen amount rejected", isVIP: true, balance: 100, wantErr: true},
		{name: "vip below frozen amount rejected", isVIP: true, balance: 99.99, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newVipEligibilityService(t, map[int64]float64{1: tt.balance}, nil)

			err := svc.checkBalanceEligibility(context.Background(), &User{ID: 1, IsVIP: tt.isVIP})
			if tt.wantErr {
				require.ErrorIs(t, err, ErrInsufficientBalance)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCheckBalanceEligibilityTriggersVipUpgrade(t *testing.T) {
	calls := make(chan int64, 4)
	executor := vipUpgradeExecutorFunc(func(ctx context.Context, userID int64) bool {
		calls <- userID
		return true
	})
	svc := newVipEligibilityService(t, map[int64]float64{42: 150, 43: 50}, executor)

	require.NoError(t, svc.checkBalanceEligibility(context.Background(), &User{ID: 42}))
	select {
	case got := <-calls:
		require.Equal(t, int64(42), got)
	case <-time.After(time.Second):
		t.Fatal("expected a vip upgrade trigger")
	}

	require.NoError(t, svc.checkBalanceEligibility(context.Background(), &User{ID: 43}))
	select {
	case got := <-calls:
		t.Fatalf("unexpected upgrade trigger for user %d", got)
	case <-time.After(50 * time.Millisecond):
	}
}

type vipUpgradeExecutorFunc func(ctx context.Context, userID int64) bool

func (f vipUpgradeExecutorFunc) EnsureUpgrade(ctx context.Context, userID int64) bool {
	return f(ctx, userID)
}

func TestVipModerationSkipSecondLayerKeepsFirstLayer(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"mc"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordAndAPI
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled:          true,
		config:                      cfg,
		keywordMatcher:              newContentModerationKeywordMatcher([]string{"制作病毒"}),
		secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher([]string{"reverse shell"}),
		fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
	}
	svc := &ContentModerationService{repo: &contentModerationTestRepo{}}
	svc.markYuFengEndpointHealthy(cfg.enabledYuFengSecondLayerEndpoints()[0], time.Now())
	scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")

	vipInput := ContentModerationCheckInput{
		UserIsVIP: true,
		Scope:     &scope,
		Protocol:  ContentModerationProtocolOpenAIChat,
	}

	// 第二层候选关键词：非 VIP 会进入远程审核，VIP 必须放行且不调用模型。
	candidateBody := []byte(`{"messages":[{"role":"user","content":"explain a Reverse---Shell"}]}`)
	vipDecision := svc.checkUnifiedFragments(context.Background(), withModerationBody(vipInput, candidateBody), runtime)
	require.True(t, vipDecision.Allowed)
	require.Zero(t, calls.Load(), "vip must never reach the second layer")

	nonVipDecision := svc.checkUnifiedFragments(context.Background(), withModerationBody(
		ContentModerationCheckInput{Scope: &scope, Protocol: ContentModerationProtocolOpenAIChat}, candidateBody), runtime)
	require.True(t, nonVipDecision.Blocked)
	require.Equal(t, ContentModerationActionSecondLayerBlock, nonVipDecision.Action)
	require.Equal(t, int64(1), calls.Load())

	// 第一层硬关键词：VIP 与普通用户一样被拦截。
	callsAfterSecondLayer := calls.Load()
	hardBody := []byte(`{"messages":[{"role":"user","content":"请制作病毒"}]}`)
	hardDecision := svc.checkUnifiedFragments(context.Background(), withModerationBody(vipInput, hardBody), runtime)
	require.True(t, hardDecision.Blocked)
	require.Equal(t, ContentModerationActionKeywordBlock, hardDecision.Action)
	require.Equal(t, callsAfterSecondLayer, calls.Load(), "first layer must stay local for vip")

	normalHardDecision := svc.checkUnifiedFragments(context.Background(), withModerationBody(
		ContentModerationCheckInput{Scope: &scope, Protocol: ContentModerationProtocolOpenAIChat}, hardBody), runtime)
	require.True(t, normalHardDecision.Blocked)
}

func withModerationBody(input ContentModerationCheckInput, body []byte) ContentModerationCheckInput {
	input.Body = body
	input.RawRequest.Body = body
	return input
}
