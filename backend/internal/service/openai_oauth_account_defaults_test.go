//go:build unit

package service

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthCreateDefaultsPersistAndRespectOverrides(t *testing.T) {
	tests := []struct {
		name, platform, accountType string
		concurrency                 int
		provided                    bool
		extra                       map[string]any
		wantConcurrency             int
		wantBridge                  *bool
		wantPolicy                  string
	}{
		{name: "omitted", wantConcurrency: 10, wantBridge: boolOverridePtr(true)},
		{name: "explicit concurrency", concurrency: 7, wantConcurrency: 7, wantBridge: boolOverridePtr(true)},
		{name: "explicit unlimited", provided: true, wantBridge: boolOverridePtr(true)},
		{name: "disabled", extra: map[string]any{featureKeyCodexImageGenerationBridge: false}, wantConcurrency: 10, wantBridge: boolOverridePtr(false)},
		{name: "inherit", extra: map[string]any{featureKeyCodexImageGenerationBridge: nil}, wantConcurrency: 10},
		{name: "legacy disabled", extra: map[string]any{"codex_image_generation_bridge_enabled": false}, wantConcurrency: 10, wantBridge: boolOverridePtr(false)},
		{name: "nested disabled", extra: map[string]any{PlatformOpenAI: map[string]any{featureKeyCodexImageGenerationBridge: false}}, wantConcurrency: 10, wantBridge: boolOverridePtr(false)},
		{name: "nested legacy disabled", extra: map[string]any{PlatformOpenAI: map[string]any{"codex_image_generation_bridge_enabled": false}}, wantConcurrency: 10, wantBridge: boolOverridePtr(false)},
		{name: "nested inherit", extra: map[string]any{PlatformOpenAI: map[string]any{featureKeyCodexImageGenerationBridge: nil}}, wantConcurrency: 10},
		{name: "strip", extra: map[string]any{featureKeyCodexImageGenerationExplicitToolPolicy: "strip"}, wantConcurrency: 10, wantPolicy: "strip"},
		{name: "nested strip", extra: map[string]any{PlatformOpenAI: map[string]any{featureKeyCodexImageGenerationExplicitToolPolicy: "strip"}}, wantConcurrency: 10, wantPolicy: "strip"},
		{name: "OpenAI API key unchanged", accountType: AccountTypeAPIKey},
		{name: "OpenAI setup token unchanged", accountType: AccountTypeSetupToken},
		{name: "Claude OAuth unchanged", platform: PlatformAnthropic},
		{name: "Grok OAuth keeps its default", platform: PlatformGrok, wantConcurrency: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			platform, accountType := tc.platform, tc.accountType
			if platform == "" {
				platform = PlatformOpenAI
			}
			if accountType == "" {
				accountType = AccountTypeOAuth
			}
			original := maps.Clone(tc.extra)
			repo := &longContextBillingRepoStub{}
			svc := &adminServiceImpl{accountRepo: repo}
			created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
				Name: "defaults", Platform: platform, Type: accountType,
				Concurrency: tc.concurrency, ConcurrencyProvided: tc.provided,
				Extra: tc.extra, SkipDefaultGroupBind: true,
			})
			require.NoError(t, err)
			require.Same(t, created, repo.createdAccount)
			require.Equal(t, tc.wantConcurrency, created.Concurrency)
			require.Equal(t, tc.wantBridge, created.CodexImageGenerationBridgeOverride())
			if tc.wantPolicy != "" {
				require.Equal(t, tc.wantPolicy, created.CodexImageGenerationExplicitToolPolicy())
			}
			if tc.name == "inherit" {
				require.Contains(t, created.Extra, featureKeyCodexImageGenerationBridge)
				require.Nil(t, created.Extra[featureKeyCodexImageGenerationBridge])
			}
			require.Equal(t, original, tc.extra, "creation must not mutate caller-owned overrides")
		})
	}
}

func TestOpenAIOAuthCreateDefaultsAuthenticationModesAndLegacyService(t *testing.T) {
	for _, mode := range []string{"", OpenAIAuthModePersonalAccessToken, OpenAIAuthModeAgentIdentity} {
		t.Run(mode, func(t *testing.T) {
			repo := &upstreamBillingProbeAccountRepo{accounts: make(map[int64]*Account)}
			created, err := NewAccountService(repo, nil).Create(context.Background(), CreateAccountRequest{
				Name: "legacy-defaults", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Credentials: map[string]any{"auth_mode": mode},
			})
			require.NoError(t, err)
			require.Equal(t, 10, created.Concurrency)
			require.Equal(t, boolOverridePtr(true), created.CodexImageGenerationBridgeOverride())
			zero := 0
			explicit, err := NewAccountService(repo, nil).Create(context.Background(), CreateAccountRequest{
				Name: "explicit", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Concurrency: &zero, Extra: map[string]any{featureKeyCodexImageGenerationBridge: false},
			})
			require.NoError(t, err)
			require.Zero(t, explicit.Concurrency)
			require.Equal(t, boolOverridePtr(false), explicit.CodexImageGenerationBridgeOverride())
		})
	}
}

func TestOpenAIOAuthCreateDefaultsDoNotReapplyOnUpdate(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 9,
		Extra: map[string]any{featureKeyCodexImageGenerationBridge: false}}
	repo := &longContextBillingRepoStub{account: account}
	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), 1, &UpdateAccountInput{Name: "renamed"})
	require.NoError(t, err)
	require.Equal(t, 9, updated.Concurrency)
	require.Equal(t, boolOverridePtr(false), updated.CodexImageGenerationBridgeOverride())
}

func TestOpenAIOAuthCreateDefaultsCRSCreateAndUpdate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		existing   bool
		extra      map[string]any
		wantBridge *bool
	}{
		{name: "new OAuth", wantBridge: boolOverridePtr(true)},
		{name: "explicit opt out", extra: map[string]any{featureKeyCodexImageGenerationBridge: false}, wantBridge: boolOverridePtr(false)},
		{name: "explicit inherit", extra: map[string]any{featureKeyCodexImageGenerationBridge: nil}},
		{name: "preserve existing hosted", existing: true, wantBridge: boolOverridePtr(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var existing *Account
			if tc.existing {
				existing = &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 8,
					Extra: map[string]any{"crs_account_id": "crs-openai-1", featureKeyCodexImageGenerationBridge: true, featureKeyCodexImageGenerationExplicitToolPolicy: "allow"}}
			}
			repo := newCRSLongContextAccountRepo(existing)
			result := runCRSOpenAILongContextSync(t, repo, crsOpenAILongContextSource{
				collection: "openaiOAuthAccounts", credentials: map[string]any{"access_token": "fixture"}, extra: tc.extra,
			})
			require.Zero(t, result.Failed)
			account := repo.accounts["crs-openai-1"]
			if tc.existing {
				require.Equal(t, 8, account.Concurrency)
			} else {
				require.Equal(t, 10, account.Concurrency)
			}
			require.Equal(t, tc.wantBridge, account.CodexImageGenerationBridgeOverride())
		})
	}
}
