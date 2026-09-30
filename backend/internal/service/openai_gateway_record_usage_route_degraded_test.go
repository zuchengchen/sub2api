package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// RouteDegraded is tri-state on both OpenAIForwardResult and UsageLog:
//   - nil:   Tibo route selection did not apply (non Cookie-WS accounts, other
//     platforms); the usage row keeps route_degraded NULL.
//   - false: Tibo routing applied and a healthy/unknown route served the request.
//   - true:  every route was degraded and the plain HTTP fallback served it.
func TestOpenAIGatewayServiceRecordUsage_CopiesRouteDegraded(t *testing.T) {
	healthy, degraded := false, true
	for _, tc := range []struct {
		name  string
		value *bool
	}{
		{name: "nil_tibo_not_applied", value: nil},
		{name: "false_healthy_route", value: &healthy},
		{name: "true_http_fallback", value: &degraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)

			err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				Result: &OpenAIForwardResult{
					RequestID:     "resp_route_degraded_" + tc.name,
					Usage:         OpenAIUsage{InputTokens: 12, OutputTokens: 3},
					Model:         "gpt-5.1",
					Duration:      time.Second,
					RouteDegraded: tc.value,
				},
				APIKey:  &APIKey{ID: 1301, Group: &Group{RateMultiplier: 1}},
				User:    &User{ID: 2301},
				Account: &Account{ID: 3301, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			})

			require.NoError(t, err)
			require.Equal(t, 1, usageRepo.calls)
			require.NotNil(t, usageRepo.lastLog)
			if tc.value == nil {
				require.Nil(t, usageRepo.lastLog.RouteDegraded, "nil must stay nil (SQL NULL), not false")
				return
			}
			require.NotNil(t, usageRepo.lastLog.RouteDegraded)
			require.Equal(t, *tc.value, *usageRepo.lastLog.RouteDegraded)
		})
	}
}
