package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountTrafficPolicyDefaultOff(t *testing.T) {
	p, err := ParseAccountTrafficPolicy(nil)
	require.NoError(t, err)
	require.False(t, p.Enabled())
	require.False(t, p.Enforces())
	require.False(t, p.StrictRPMEnabled)
	require.False(t, p.AdaptiveEnabled)
}

func TestAccountTrafficStrictRPMIndependentOfNearLimitExtra(t *testing.T) {
	account := &Account{
		ID:          7,
		Concurrency: 8,
		Platform:    PlatformOpenAI,
		Extra: map[string]any{
			"auto_pause_5h_threshold": 0.95,
			"auto_pause_7d_disabled":  true,
			AccountTrafficPolicyKey:   DefaultAccountTrafficPolicy(),
		},
	}
	plan, err := AccountTrafficPlanFor(account)
	require.NoError(t, err)
	require.False(t, plan.Policy.Enabled(), "default traffic policy must not change the 429 near-limit path")
	require.Equal(t, 0.95, account.Extra["auto_pause_5h_threshold"])
}

func TestAccountTrafficGrokRealtimeRejectsHardRPM(t *testing.T) {
	account := &Account{
		ID: 3, Concurrency: 4, Platform: PlatformGrok,
		Extra: map[string]any{
			AccountTrafficPolicyKey: AccountTrafficPolicy{
				StrictRPMEnabled: true, RPM: 10, Burst: 2, AdaptiveMode: "observe",
				MinConcurrency: 1, FailureThreshold: 3, FailureWindowSeconds: 60, RecoverySeconds: 60,
			},
		},
	}
	err := validateGrokRealtimeTrafficPolicy(account)
	require.Error(t, err)
	var limited *AccountTrafficLimitError
	require.ErrorAs(t, err, &limited)
	require.Equal(t, 400, limited.Status)

	account.Extra[AccountTrafficPolicyKey] = DefaultAccountTrafficPolicy()
	require.NoError(t, validateGrokRealtimeTrafficPolicy(account))
}
