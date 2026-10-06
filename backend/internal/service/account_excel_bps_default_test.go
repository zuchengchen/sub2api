//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIExcelBPSExtra(t *testing.T) {
	t.Run("oauth missing key persists enabled default", func(t *testing.T) {
		extra, err := normalizeOpenAIExcelBPSExtra(PlatformOpenAI, AccountTypeOAuth, nil, nil, false)
		require.NoError(t, err)
		require.Equal(t, true, extra[OpenAIExcelBPSExtraKey])
	})

	t.Run("oauth explicit false is preserved", func(t *testing.T) {
		extra, err := normalizeOpenAIExcelBPSExtra(PlatformOpenAI, AccountTypeOAuth, nil, map[string]any{OpenAIExcelBPSExtraKey: false}, false)
		require.NoError(t, err)
		require.Equal(t, false, extra[OpenAIExcelBPSExtraKey])
	})

	t.Run("oauth malformed value is rejected", func(t *testing.T) {
		_, err := normalizeOpenAIExcelBPSExtra(PlatformOpenAI, AccountTypeOAuth, nil, map[string]any{OpenAIExcelBPSExtraKey: "true"}, false)
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	})

	t.Run("api key extra is unchanged", func(t *testing.T) {
		extra, err := normalizeOpenAIExcelBPSExtra(PlatformOpenAI, AccountTypeAPIKey, nil, nil, false)
		require.NoError(t, err)
		require.Nil(t, extra)
	})

	t.Run("agent identity extra is unchanged", func(t *testing.T) {
		extra, err := normalizeOpenAIExcelBPSExtra(PlatformOpenAI, AccountTypeOAuth, map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity}, nil, false)
		require.NoError(t, err)
		require.Nil(t, extra)
	})
}

func TestAdminServiceCreateAccountDefaultsOpenAIExcelBPSEnabled(t *testing.T) {
	repo := &longContextBillingRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo}

	account, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "openai-oauth",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeOAuth,
		Credentials:          map[string]any{"access_token": "test"},
		SkipDefaultGroupBind: true,
	})

	require.NoError(t, err)
	require.Same(t, account, repo.createdAccount)
	require.Equal(t, true, account.Extra[OpenAIExcelBPSExtraKey])
}

func TestAdminServiceUpdateAccountPreservesOpenAIExcelBPSOptOutWhenOmitted(t *testing.T) {
	repo := &longContextBillingRepoStub{account: &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{OpenAIExcelBPSExtraKey: false},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	account, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Extra: map[string]any{}})

	require.NoError(t, err)
	require.Equal(t, false, account.Extra[OpenAIExcelBPSExtraKey])
}
