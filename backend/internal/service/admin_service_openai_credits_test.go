//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type accountRepoStubForOpenAICredits struct {
	accountRepoStubForClearAccountError
	extraUpdates []map[string]any
}

func (r *accountRepoStubForOpenAICredits) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.extraUpdates = append(r.extraUpdates, updates)
	if r.account.Extra == nil {
		r.account.Extra = map[string]any{}
	}
	for k, v := range updates {
		r.account.Extra[k] = v
	}
	return nil
}

func newOpenAICreditsRepo(account *Account) *accountRepoStubForOpenAICredits {
	return &accountRepoStubForOpenAICredits{accountRepoStubForClearAccountError: accountRepoStubForClearAccountError{account: account}}
}

func TestAdminService_SetOpenAICreditsEnabled_EnableClearsRateLimit(t *testing.T) {
	resetAt := time.Now().Add(3 * time.Hour)
	repo := newOpenAICreditsRepo(&Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, RateLimitResetAt: &resetAt})
	blocker := &runtimeBlockRecorder{}
	svc := &adminServiceImpl{accountRepo: repo, runtimeBlocker: blocker}

	updated, err := svc.SetOpenAICreditsEnabled(context.Background(), 41, true)
	require.NoError(t, err)
	require.True(t, updated.IsOpenAICreditsEnabled())
	require.Equal(t, []map[string]any{{OpenAICreditsEnabledExtraKey: true}}, repo.extraUpdates)
	require.Equal(t, 1, repo.clearRateLimitCalls)
	require.Nil(t, updated.RateLimitResetAt)
	require.Equal(t, []int64{41}, blocker.clearedIDs)
}

func TestAdminService_SetOpenAICreditsEnabled_DisableKeepsRateLimit(t *testing.T) {
	resetAt := time.Now().Add(3 * time.Hour)
	repo := newOpenAICreditsRepo(&Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Status: StatusActive,
		RateLimitResetAt: &resetAt, Extra: map[string]any{OpenAICreditsEnabledExtraKey: true}})
	svc := &adminServiceImpl{accountRepo: repo}

	updated, err := svc.SetOpenAICreditsEnabled(context.Background(), 42, false)
	require.NoError(t, err)
	require.False(t, updated.IsOpenAICreditsEnabled())
	require.Equal(t, 0, repo.clearRateLimitCalls)
	require.NotNil(t, updated.RateLimitResetAt)
}

func TestAdminService_SetOpenAICreditsEnabled_RejectsUnsupportedAccounts(t *testing.T) {
	parentID := int64(1)
	for name, account := range map[string]*Account{
		"api key":   {ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		"anthropic": {ID: 44, Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		"shadow":    {ID: 45, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newOpenAICreditsRepo(account)
			svc := &adminServiceImpl{accountRepo: repo}
			_, err := svc.SetOpenAICreditsEnabled(context.Background(), account.ID, true)
			require.ErrorIs(t, err, ErrOpenAICreditsToggleUnsupported)
			require.Empty(t, repo.extraUpdates)
		})
	}
}
