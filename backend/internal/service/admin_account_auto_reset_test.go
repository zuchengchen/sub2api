//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateAccountAutoReset_PreservesManagedAttempt(t *testing.T) {
	state := &OpenAIAutoResetCreditState{
		Status:            OpenAIAutoResetStatusFailed,
		AttemptCreditHash: "saved-credit-hash", AttemptCycleHash: "saved-cycle-hash",
	}
	repo := &updateAccountOveragesRepoStub{account: &Account{
		ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Extra: map[string]any{OpenAIAutoResetCreditStateExtraKey: state},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.UpdateAccount(context.Background(), 42, &UpdateAccountInput{
		Extra: map[string]any{
			OpenAIAutoResetCreditEnabledExtraKey:     true,
			OpenAIAutoResetCredit5hThresholdExtraKey: 0.755,
			OpenAIAutoResetCredit7dThresholdExtraKey: 0.92,
			OpenAIAutoResetCreditStateExtraKey:       map[string]any{"status": "success"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, OpenAIAutoResetCreditConfig{Enabled: true, Threshold5h: 0.755, Threshold7d: 0.92}, ResolveOpenAIAutoResetCreditConfig(updated))
	require.Equal(t, state, openAIAutoResetStateFromExtra(updated.Extra), "an account edit must not replace the in-flight attempt")

	updated, err = svc.UpdateAccount(context.Background(), 42, &UpdateAccountInput{
		Extra: map[string]any{OpenAIAutoResetCreditEnabledExtraKey: false},
	})
	require.NoError(t, err)
	require.False(t, ResolveOpenAIAutoResetCreditConfig(updated).Enabled)
	require.Equal(t, state, openAIAutoResetStateFromExtra(updated.Extra))
}

func TestUpdateAccountAutoReset_RejectsInvalidConfiguration(t *testing.T) {
	repo := &updateAccountOveragesRepoStub{account: &Account{
		ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	_, err := svc.UpdateAccount(context.Background(), 42, &UpdateAccountInput{
		Extra: map[string]any{
			OpenAIAutoResetCreditEnabledExtraKey:     true,
			OpenAIAutoResetCredit7dThresholdExtraKey: 1.01,
		},
	})
	require.Error(t, err)
	require.Zero(t, repo.updateCalls)
}
