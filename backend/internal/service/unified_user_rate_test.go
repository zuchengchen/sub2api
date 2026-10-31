package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

type unifiedUserRateCase struct {
	name, model, groupName string
	groupRate, wantRate    float64
	override               *float64
	peak                   bool
}

func unifiedUserRateCases() []unifiedUserRateCase {
	zero, low, high := 0.0, 0.07, 0.3
	return []unifiedUserRateCase{
		{name: "ordinary", model: "gpt-5.1", groupName: "gpt-pro", groupRate: 0.1, wantRate: 0.1},
		{name: "case insensitive group", model: "gpt-5.1", groupName: " GPT-PRO ", groupRate: 0.1, wantRate: 0.1},
		{name: "other group", model: "gpt-5.1", groupName: "other", groupRate: 0.1, wantRate: 0.1},
		{name: "zero group", model: "gpt-5.1", groupName: "gpt-pro", groupRate: 0, wantRate: 0},
		{name: "former discount boundary", model: "gpt-5.1", groupName: "gpt-pro", groupRate: 0.05, wantRate: 0.05},
		{name: "manual override", model: "gpt-5.1", groupName: "gpt-pro", groupRate: 0.1, override: &low, wantRate: 0.07},
		{name: "zero override", model: "gpt-5.1", groupName: "gpt-pro", groupRate: 0.1, override: &zero, wantRate: 0},
		{name: "luna zero", model: "gpt-5.6-luna", groupName: "gpt-pro", groupRate: 0, wantRate: 0.2},
		{name: "luna below floor", model: "gpt-5.6-luna", groupName: "gpt-pro", groupRate: 0.1, wantRate: 0.2},
		{name: "luna at floor", model: "gpt-5.6-luna", groupName: "gpt-pro", groupRate: 0.2, wantRate: 0.2},
		{name: "luna above floor", model: "gpt-5.6-luna", groupName: "gpt-pro", groupRate: 0.3, wantRate: 0.3},
		{name: "luna low override", model: "gpt-5.6-luna", groupName: "gpt-pro", groupRate: 0.3, override: &low, wantRate: 0.2},
		{name: "luna high override", model: "gpt-5.6-luna", groupName: "gpt-pro", groupRate: 0.1, override: &high, wantRate: 0.3},
		{name: "luna other group", model: "gpt-5.6-luna", groupName: "other", groupRate: 0.1, wantRate: 0.2},
		{name: "luna6 below floor", model: "gpt-6-luna", groupName: "gpt-pro", groupRate: 0.1, wantRate: 0.2},
		{name: "luna6 at floor", model: "gpt-6-luna", groupName: "gpt-pro", groupRate: 0.2, wantRate: 0.2},
		{name: "luna6 above floor", model: "gpt-6-luna", groupName: "gpt-pro", groupRate: 0.3, wantRate: 0.3},
		{name: "ordinary peak", model: "gpt-5.1", groupName: "gpt-pro", groupRate: 0.1, peak: true, wantRate: 0.2},
		{name: "luna peak", model: "gpt-6-luna", groupName: "gpt-pro", groupRate: 0.1, peak: true, wantRate: 0.4},
	}
}

func (tc unifiedUserRateCase) group() *Group {
	return &Group{
		ID: 88, Name: tc.groupName, Platform: PlatformOpenAI, RateMultiplier: tc.groupRate,
		SubscriptionType: SubscriptionTypeSubscription,
		PeakRateEnabled:  tc.peak, PeakStart: "00:00", PeakEnd: "23:59", PeakRateMultiplier: 2,
	}
}

func TestUnifiedUserRateOpenAIRecordUsage(t *testing.T) {
	for _, tc := range unifiedUserRateCases() {
		var plainCost float64
		for _, vip := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/vip=%t", tc.name, vip), func(t *testing.T) {
				logs := &openAIRecordUsageLogRepoStub{inserted: true}
				users := &openAIRecordUsageUserRepoStub{}
				rates := &openAIUserGroupRateRepoStub{rate: tc.override}
				svc := newOpenAIRecordUsageServiceForTest(logs, users, &openAIRecordUsageSubRepoStub{}, rates)
				user := &User{ID: 2088, IsVIP: vip}
				group := tc.group()
				err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
					Result: &OpenAIForwardResult{RequestID: "unified-rate", Model: tc.model, Usage: OpenAIUsage{InputTokens: 20, OutputTokens: 5}},
					APIKey: &APIKey{ID: 1088, UserID: user.ID, User: user, GroupID: &group.ID, Group: group},
					User:   user, Account: &Account{ID: 3088, Platform: PlatformOpenAI},
					PricingAt: time.Date(2026, 10, 31, 12, 0, 0, 0, timezone.Location()),
				})
				require.NoError(t, err)
				require.Equal(t, 1, logs.calls)
				require.NotNil(t, logs.lastLog)
				require.Positive(t, logs.lastLog.TotalCost)
				require.InDelta(t, tc.wantRate, logs.lastLog.RateMultiplier, 1e-12)
				require.InDelta(t, logs.lastLog.TotalCost*tc.wantRate, logs.lastLog.ActualCost, 1e-12)
				pricingModel := tc.model
				if pricingModel == "gpt-6-luna" {
					pricingModel = "gpt-5.6-luna"
				}
				expected := expectedOpenAICost(t, svc, pricingModel, OpenAIUsage{InputTokens: 20, OutputTokens: 5}, tc.wantRate)
				require.InDelta(t, expected.ActualCost, logs.lastLog.ActualCost, 1e-12)
				require.InDelta(t, logs.lastLog.ActualCost, users.lastAmount, 1e-12)
				if tc.wantRate > 0 {
					require.Equal(t, 1, users.deductCalls)
				}
				if vip {
					require.InDelta(t, plainCost, logs.lastLog.ActualCost, 1e-12)
				} else {
					plainCost = logs.lastLog.ActualCost
				}
			})
		}
	}
}

type unifiedDisplayRateRepo struct {
	UserGroupRateRepository
	rates map[int64]float64
	err   error
}

func (r *unifiedDisplayRateRepo) GetByUserID(context.Context, int64) (map[int64]float64, error) {
	return r.rates, r.err
}

type unifiedDisplayGroupRepo struct{ GroupRepository }

func (unifiedDisplayGroupRepo) ListActive(context.Context) ([]Group, error) {
	return []Group{{ID: 88, Name: "gpt-pro", RateMultiplier: 0.1}}, nil
}

func TestUnifiedUserRateDisplayUserGroupRates(t *testing.T) {
	for _, rates := range []map[int64]float64{nil, {}, {88: 0.07}, {88: 0}, {88: 0.1}} {
		for _, vip := range []bool{false, true} {
			t.Run(fmt.Sprintf("rates=%v/vip=%t", rates, vip), func(t *testing.T) {
				repo := &unifiedDisplayRateRepo{rates: maps.Clone(rates)}
				svc := &APIKeyService{
					userGroupRateRepo: repo,
					userRepo:          &vipExecutorUserRepoStub{user: &User{ID: 1, IsVIP: vip}},
					groupRepo:         unifiedDisplayGroupRepo{},
				}
				got, err := svc.GetDisplayUserGroupRates(context.Background(), 1)
				require.NoError(t, err)
				require.Equal(t, rates, got)
				require.Equal(t, rates, repo.rates, "display must not mutate stored overrides")
			})
		}
	}
	t.Run("repository error", func(t *testing.T) {
		want := errors.New("rates unavailable")
		svc := &APIKeyService{userGroupRateRepo: &unifiedDisplayRateRepo{err: want}}
		_, err := svc.GetDisplayUserGroupRates(context.Background(), 1)
		require.ErrorIs(t, err, want)
	})
	t.Run("no override repository", func(t *testing.T) {
		got, err := (&APIKeyService{}).GetDisplayUserGroupRates(context.Background(), 1)
		require.NoError(t, err)
		require.Nil(t, got)
	})
}
