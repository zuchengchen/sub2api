//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIGatewayServiceRecordUsage_LunaFreeFastUsesFinalChargeForResponseModel(t *testing.T) {
	const requestedModel = "gpt-5.6-sol"
	const inputTokens = 1000
	for _, luna := range []string{"gpt-5.6-luna", "gpt-6-luna"} {
		for _, tc := range []struct {
			name                          string
			lunaPrice, lunaFast           float64
			wantTotal, wantPaid, wantRate float64
		}{
			{name: "rejects higher standard charge", lunaPrice: 0.75, lunaFast: 1, wantTotal: 0.003, wantPaid: 0.0001, wantRate: 0.1},
			{name: "adopts cheaper standard charge with floor", lunaPrice: 0.4, lunaFast: 1, wantTotal: 0.0004, wantPaid: 0.00008, wantRate: 0.2},
			{name: "adopts equal standard charge with floor", lunaPrice: 0.5, lunaFast: 1, wantTotal: 0.0005, wantPaid: 0.0001, wantRate: 0.2},
			{name: "preserves priority total guard", lunaPrice: 0.4, lunaFast: 10, wantTotal: 0.003, wantPaid: 0.0001, wantRate: 0.1},
		} {
			t.Run(luna+"/"+tc.name, func(t *testing.T) {
				logs, users := &openAIRecordUsageLogRepoStub{inserted: true}, &openAIRecordUsageUserRepoStub{}
				svc := newOpenAIRecordUsageServiceForTest(logs, users, &openAIRecordUsageSubRepoStub{}, nil)
				group := &Group{ID: 88, Platform: PlatformOpenAI, RateMultiplier: 0.1, FreeOpenAIFast: true}
				requestedPricing := tokenPricingForModels([]string{requestedModel}, 1)
				requestedPricing.FastMultiplier = float64Ptr(3)
				lunaPricing := tokenPricingForModels([]string{luna}, tc.lunaPrice)
				lunaPricing.FastMultiplier = &tc.lunaFast
				svc.channelService = newChannelServiceWithPricings(group.ID, []ChannelModelPricing{requestedPricing, lunaPricing})
				svc.resolver = NewModelPricingResolver(svc.channelService, svc.billingService)
				tier := "priority"
				err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
					Result: &OpenAIForwardResult{
						RequestID: "luna-free-fast-final-charge", Model: requestedModel, UpstreamModel: requestedModel,
						UpstreamResponseModel: luna, ServiceTier: &tier, Usage: OpenAIUsage{InputTokens: inputTokens},
					},
					APIKey:  &APIKey{ID: 1088, GroupID: &group.ID, Group: group},
					User:    &User{ID: 2088},
					Account: &Account{ID: 3088, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
					ChannelUsageFields: ChannelUsageFields{
						ChannelID: 1, OriginalModel: requestedModel, ChannelMappedModel: requestedModel, BillingModelSource: BillingModelSourceResponse,
					},
				})
				require.NoError(t, err)
				require.NotNil(t, logs.lastLog)
				require.InDelta(t, tc.wantRate, logs.lastLog.RateMultiplier, 1e-12)
				require.InDelta(t, tc.wantTotal, logs.lastLog.TotalCost, 1e-12, "retain Priority cost for the selected model")
				require.InDelta(t, tc.wantPaid, logs.lastLog.ActualCost, 1e-12, "compare Standard charges before selecting the response model")
				require.InDelta(t, tc.wantPaid, users.lastAmount, 1e-12)
				require.Equal(t, tier, *logs.lastLog.ServiceTier)
			})
		}
	}
}
