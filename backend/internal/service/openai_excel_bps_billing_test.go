package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccount_IsExcelBPSCacheCreationAsInputEnabled(t *testing.T) {
	newAccount := func() *Account {
		return &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
			"openai_excel_bps": true, "openai_excel_bps_cache_creation_as_input": true,
		}}
	}
	require.False(t, (*Account)(nil).IsExcelBPSCacheCreationAsInputEnabled())
	require.True(t, newAccount().IsExcelBPSCacheCreationAsInputEnabled())
	for name, modify := range map[string]func(*Account){
		"missing option":  func(a *Account) { delete(a.Extra, "openai_excel_bps_cache_creation_as_input") },
		"disabled option": func(a *Account) { a.Extra["openai_excel_bps_cache_creation_as_input"] = false },
		"invalid option":  func(a *Account) { a.Extra["openai_excel_bps_cache_creation_as_input"] = "true" },
		"BPS disabled":    func(a *Account) { a.Extra["openai_excel_bps"] = false },
		"no extra":        func(a *Account) { a.Extra = nil },
		"API key":         func(a *Account) { a.Type = AccountTypeAPIKey },
		"other platform":  func(a *Account) { a.Platform = PlatformAnthropic },
		"shadow account":  func(a *Account) { a.ParentAccountID = i64p(1) },
		"agent identity":  func(a *Account) { a.Credentials = map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity} },
	} {
		t.Run(name, func(t *testing.T) {
			account := newAccount()
			modify(account)
			require.False(t, account.IsExcelBPSCacheCreationAsInputEnabled())
		})
	}
}

func TestUpdateAccountExcelBPSCacheCreationAsInput(t *testing.T) {
	credentials := map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
	account := &Account{ID: 51, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: credentials, Extra: map[string]any{"openai_excel_bps": true, "unrelated": "keep"}}
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	svc := &adminServiceImpl{accountRepo: repo}
	for _, enabled := range []bool{true, false} {
		extra := map[string]any{"openai_excel_bps": true, "unrelated": "keep"}
		if enabled {
			extra["openai_excel_bps_cache_creation_as_input"] = true
		}
		updated, err := svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Extra: extra})
		require.NoError(t, err)
		require.Equal(t, enabled, updated.IsExcelBPSCacheCreationAsInputEnabled())
		require.Equal(t, enabled, repo.accounts[account.ID].IsExcelBPSCacheCreationAsInputEnabled())
		require.True(t, updated.IsExcelBPSEnabled())
		require.Equal(t, "keep", updated.Extra["unrelated"])
		require.Equal(t, credentials, updated.Credentials)
	}
}

func TestExcelBPSCacheCreationAsInputBilling(t *testing.T) {
	for _, tt := range []struct {
		name                          string
		bps                           bool
		option                        any
		input, creation, read, output int
		wantInput, wantCreation       int
	}{
		{"default", true, nil, 1000, 200, 100, 50, 700, 200},
		{"disabled", true, false, 1000, 200, 100, 50, 700, 200},
		{"enabled", true, true, 1000, 200, 100, 50, 900, 0},
		{"BPS disabled", false, true, 1000, 200, 100, 50, 700, 200},
		{"no cache creation", true, true, 1000, 0, 100, 50, 900, 0},
		{"all input cached", true, true, 1000, 800, 200, 50, 800, 0},
		{"empty usage", true, true, 0, 0, 0, 0, 0, 0},
	} {
		for _, stream := range []bool{false, true} {
			for _, subscription := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/subscription=%t", tt.name, stream, subscription), func(t *testing.T) {
					usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
					billingRepo := &openAIRecordUsageBillingRepoStub{}
					svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
					svc.billingService = NewBillingService(svc.cfg, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
						"gpt-6-astra": {
							InputCostPerToken: 5e-6, OutputCostPerToken: 30e-6,
							CacheCreationInputTokenCost: 6.25e-6, CacheCreationInputTokenCostExplicit: true,
							CacheReadInputTokenCost: 0.5e-6,
						},
					}})
					extra := map[string]any{"openai_excel_bps": tt.bps}
					if tt.option != nil {
						extra["openai_excel_bps_cache_creation_as_input"] = tt.option
					}
					original := OpenAIUsage{InputTokens: tt.input, CacheCreationInputTokens: tt.creation, CacheReadInputTokens: tt.read, OutputTokens: tt.output}
					result := &OpenAIForwardResult{RequestID: "resp_bps_billing", UpstreamEndpoint: "/basispoints/api/responses", Usage: original, Model: "gpt-6-astra", Stream: stream, Duration: time.Second}
					input := &OpenAIRecordUsageInput{
						Result: result, APIKey: &APIKey{ID: 1001}, User: &User{ID: 2001},
						Account: &Account{ID: 3001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra},
					}
					if subscription {
						input.Subscription = &UserSubscription{ID: 4001}
						input.APIKey.Group = &Group{SubscriptionType: "subscription"}
					}
					require.NoError(t, svc.RecordUsage(context.Background(), input))
					require.Equal(t, original, result.Usage, "billing must not rewrite upstream response usage")
					require.NotNil(t, usageRepo.lastLog)
					log := usageRepo.lastLog
					require.Equal(t, tt.wantInput, log.InputTokens)
					require.Equal(t, tt.wantCreation, log.CacheCreationTokens)
					require.Equal(t, tt.read, log.CacheReadTokens)
					require.Equal(t, tt.output, log.OutputTokens)
					require.Equal(t, tt.input+tt.output, log.TotalTokens())
					require.Equal(t, stream, log.Stream)
					if tt.wantCreation == 0 {
						require.Zero(t, log.CacheCreationCost)
					} else {
						require.Greater(t, log.CacheCreationCost, 0.0)
					}
					require.Equal(t, 1, billingRepo.calls)
					cmd := billingRepo.lastCmd
					require.Equal(t, tt.wantInput, cmd.InputTokens)
					require.Equal(t, tt.wantCreation, cmd.CacheCreationTokens)
					require.Equal(t, tt.read, cmd.CacheReadTokens)
					if tt.input+tt.output+tt.creation+tt.read == 0 {
						require.Zero(t, cmd.BalanceCost)
						require.Zero(t, cmd.SubscriptionCost)
					} else if subscription {
						require.Zero(t, cmd.BalanceCost)
						require.Greater(t, cmd.SubscriptionCost, 0.0)
					} else {
						require.Zero(t, cmd.SubscriptionCost)
						require.Greater(t, cmd.BalanceCost, 0.0)
					}
				})
			}
		}
	}
}

func TestExcelBPSSelectedModelBillingPreservesCodexCacheCreation(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	svc.billingService = NewBillingService(svc.cfg, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"gpt-6-sol": {InputCostPerToken: 5e-6, OutputCostPerToken: 30e-6, CacheCreationInputTokenCost: 6.25e-6, CacheCreationInputTokenCostExplicit: true, CacheReadInputTokenCost: 0.5e-6},
	}})
	account := excelAccount()
	account.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
	account.Extra["openai_excel_bps_cache_creation_as_input"] = true
	require.False(t, account.IsExcelBPSEnabledForModel("gpt-6-sol"))
	original := OpenAIUsage{InputTokens: 1000, CacheCreationInputTokens: 200, CacheReadInputTokens: 100, OutputTokens: 50}
	result := &OpenAIForwardResult{RequestID: "resp_codex_billing", Model: "gpt-6-sol", UpstreamEndpoint: "/v1/responses", Usage: original}
	require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: result, APIKey: &APIKey{ID: 1001}, User: &User{ID: 2001}, Account: account,
	}))
	require.Equal(t, original, result.Usage)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, 700, usageRepo.lastLog.InputTokens)
	require.Equal(t, 200, usageRepo.lastLog.CacheCreationTokens)
	require.Equal(t, 100, usageRepo.lastLog.CacheReadTokens)
	require.Greater(t, usageRepo.lastLog.CacheCreationCost, 0.0)
	require.Equal(t, 200, billingRepo.lastCmd.CacheCreationTokens)
}
