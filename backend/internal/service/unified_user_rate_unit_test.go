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

func TestUnifiedUserRateGatewayRecordUsage(t *testing.T) {
	for _, tc := range unifiedUserRateCases() {
		var plainCost float64
		for _, vip := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/vip=%t", tc.name, vip), func(t *testing.T) {
				logs := &openAIRecordUsageLogRepoStub{inserted: true}
				users := &openAIRecordUsageUserRepoStub{}
				svc := newGatewayRecordUsageServiceForTest(logs, users, &openAIRecordUsageSubRepoStub{})
				svc.userGroupRateResolver = newUserGroupRateResolver(&openAIUserGroupRateRepoStub{rate: tc.override}, nil, time.Minute, nil, "test")
				user := &User{ID: 2088, IsVIP: vip}
				group := tc.group()
				err := svc.RecordUsage(context.Background(), &RecordUsageInput{
					Result: &ForwardResult{RequestID: "unified-rate", Model: tc.model, Usage: ClaudeUsage{InputTokens: 20, OutputTokens: 5}},
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

func TestUnifiedUserRateInflightEstimate(t *testing.T) {
	for _, tc := range unifiedUserRateCases() {
		// Final billing above fixes PricingAt to cover peak periods. The estimator
		// uses the real clock, so apply the observed peak factor to its base oracle.
		for _, gateway := range []string{"gateway", "openai"} {
			var plainCost float64
			for _, vip := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/vip=%t", tc.name, gateway, vip), func(t *testing.T) {
					svc := newInflightEstimateGateway(t, nil)
					rateRepo := &openAIUserGroupRateRepoStub{rate: tc.override}
					svc.userGroupRateResolver = newUserGroupRateResolver(rateRepo, nil, time.Minute, nil, "test")
					group := tc.group()
					key := &APIKey{User: &User{ID: 1, IsVIP: vip}, GroupID: &group.ID, Group: group}
					estimate := svc.EstimateInflightReservation
					if gateway == "openai" {
						openai := newOpenAIRecordUsageServiceForTest(nil, nil, nil, rateRepo)
						openai.cfg.Billing.InflightReservation = svc.cfg.Billing.InflightReservation
						openai.resolver = svc.resolver
						estimate = openai.EstimateInflightReservation
					}
					peak := group.PeakMultiplierAt(timezone.Now())
					cost, priced := estimate(context.Background(), key, InflightEstimateRequest{Model: tc.model, BodyBytes: 400, MaxTokens: 1000})
					require.True(t, priced)
					pricing, err := svc.billingService.GetModelPricing(tc.model)
					require.NoError(t, err)
					baseRate := tc.wantRate
					if tc.peak {
						baseRate /= 2
					}
					expected := (100*pricing.InputPricePerToken + 1000*pricing.OutputPricePerToken) * baseRate * peak
					require.InDelta(t, expected, cost, 1e-12)
					if vip {
						require.InDelta(t, plainCost, cost, 1e-12)
					} else {
						plainCost = cost
					}
				})
			}
		}
	}
}
