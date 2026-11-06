//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

func TestLunaResponseBillingFloor(t *testing.T) {
	for _, gateway := range []string{"openai", "gateway"} {
		for _, luna := range []string{"gpt-5.6-luna", "gpt-6-luna"} {
			for _, vip := range []bool{false, true} {
				for _, tc := range []struct {
					name, source, requested string
					rate, peak, wantRate    float64
					subscription, conflict  bool
					wantLuna, freeFast      bool
				}{
					{name: "standard group", source: BillingModelSourceResponse, rate: 0.1, wantRate: 0.2, wantLuna: true},
					{name: "subscription group", source: BillingModelSourceResponse, rate: 0.1, peak: 1, subscription: true, wantRate: 0.2, wantLuna: true},
					{name: "above floor", source: BillingModelSourceResponse, rate: 0.3, wantRate: 0.3, wantLuna: true},
					{name: "peak surcharge", source: BillingModelSourceResponse, rate: 0.1, peak: 2, subscription: true, wantRate: 0.4, wantLuna: true},
					{name: "peak discount", source: BillingModelSourceResponse, rate: 0.1, peak: 0.5, subscription: true, wantRate: 0.2, wantLuna: true},
					{name: "free fast", source: BillingModelSourceResponse, rate: 0.1, wantRate: 0.2, wantLuna: true, freeFast: true},
					{name: "disabled response billing", source: BillingModelSourceChannelMapped, rate: 0.1, wantRate: 0.1},
					{name: "conflicting declaration", source: BillingModelSourceResponse, rate: 0.1, conflict: true, wantRate: 0.1},
					{name: "would raise actual charge", source: BillingModelSourceResponse, requested: "gpt-5.4-mini", rate: 0.1, wantRate: 0.1},
					{name: "free baseline", source: BillingModelSourceResponse, rate: 0, wantRate: 0},
				} {
					if tc.freeFast && gateway != "openai" {
						continue // Free Fast is an OpenAI-gateway billing policy.
					}
					t.Run(fmt.Sprintf("%s/%s/%s/vip=%t", gateway, luna, tc.name, vip), func(t *testing.T) {
						requested := tc.requested
						if requested == "" {
							requested = "gpt-5.5"
						}
						group := &Group{ID: 88, Name: "custom-group", Platform: PlatformOpenAI, RateMultiplier: tc.rate}
						var tier *string
						if tc.freeFast {
							group.FreeOpenAIFast = true
							priority := "priority"
							tier = &priority
						}
						var subscription *UserSubscription
						if tc.subscription {
							group.SubscriptionType = SubscriptionTypeSubscription
							group.PeakRateEnabled, group.PeakStart, group.PeakEnd, group.PeakRateMultiplier = true, "00:00", "23:59", tc.peak
							subscription = &UserSubscription{ID: 4088}
						}
						user := &User{ID: 2088, IsVIP: vip}
						key := &APIKey{ID: 1088, UserID: user.ID, User: user, GroupID: &group.ID, Group: group}
						account := &Account{ID: 3088, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
						fields := ChannelUsageFields{OriginalModel: requested, ChannelMappedModel: requested, BillingModelSource: tc.source}
						pricingAt := time.Date(2026, 11, 6, 12, 0, 0, 0, timezone.Location())
						logs, users, subs := &openAIRecordUsageLogRepoStub{inserted: true}, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}
						var billing *BillingService
						if gateway == "openai" {
							svc := newOpenAIRecordUsageServiceForTest(logs, users, subs, nil)
							billing = svc.billingService
							require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
								Result: &OpenAIForwardResult{
									RequestID: "luna-response-floor", Model: requested, UpstreamModel: requested,
									UpstreamResponseModel: luna, UpstreamResponseModelConflict: tc.conflict,
									Usage: OpenAIUsage{InputTokens: 100}, ServiceTier: tier,
								},
								APIKey: key, User: user, Account: account, Subscription: subscription,
								PricingAt: pricingAt, ChannelUsageFields: fields,
							}))
						} else {
							svc := newGatewayRecordUsageServiceForTest(logs, users, subs)
							billing = svc.billingService
							require.NoError(t, svc.RecordUsage(context.Background(), &RecordUsageInput{
								Result: &ForwardResult{
									RequestID: "luna-response-floor", Model: requested, UpstreamModel: requested,
									UpstreamResponseModel: luna, UpstreamResponseModelConflict: tc.conflict,
									Usage: ClaudeUsage{InputTokens: 100},
								},
								APIKey: key, User: user, Account: account, Subscription: subscription,
								PricingAt: pricingAt, ChannelUsageFields: fields,
							}))
						}
						wantModel := requested
						if tc.wantLuna {
							wantModel = luna
						}
						want, err := billing.CalculateCost(wantModel, UsageTokens{InputTokens: 100}, tc.wantRate)
						require.NoError(t, err)
						if tc.freeFast {
							priorityCost, err := billing.CalculateCostWithServiceTier(wantModel, UsageTokens{InputTokens: 100}, tc.wantRate, "priority")
							require.NoError(t, err)
							want.TotalCost = priorityCost.TotalCost
						}
						require.NotNil(t, logs.lastLog)
						require.InDelta(t, tc.wantRate, logs.lastLog.RateMultiplier, 1e-12)
						require.InDelta(t, want.TotalCost, logs.lastLog.TotalCost, 1e-12)
						require.InDelta(t, want.ActualCost, logs.lastLog.ActualCost, 1e-12)
						require.Equal(t, requested, logs.lastLog.RequestedModel)
						require.Equal(t, luna, *logs.lastLog.UpstreamResponseModel)
						if subscription != nil {
							require.InDelta(t, want.ActualCost, subs.lastCost, 1e-12)
						} else {
							require.InDelta(t, want.ActualCost, users.lastAmount, 1e-12)
						}
					})
				}
			}
		}
	}
}
