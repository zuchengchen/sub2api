package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func newImagesModelSchedulerTestService(advanced string, accounts []Account) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(advanced),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
}

func TestOpenAIImagesSchedulerSkipsUnsupportedCodexModels(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
			for _, capability := range []OpenAIImagesCapability{OpenAIImagesCapabilityBasic, OpenAIImagesCapabilityNative} {
				t.Run(fmt.Sprintf("advanced=%s/%s/%s", advanced, accountType, capability), func(t *testing.T) {
					unsupported := Account{
						ID: 38101, Platform: PlatformOpenAI, Type: accountType,
						Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
					}
					provider := Account{
						ID: 38102, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
						Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 10,
						Credentials: map[string]any{"model_mapping": map[string]any{"gpt-image-custom": "gpt-image-provider-custom"}},
					}
					groupID := int64(10121)
					svc := newImagesModelSchedulerTestService(advanced, []Account{unsupported, provider})
					selection, _, err := svc.SelectAccountWithSchedulerForImages(context.Background(), &groupID, "", "gpt-image-custom", nil, capability)
					require.NoError(t, err)
					require.NotNil(t, selection)
					require.Equal(t, provider.ID, selection.Account.ID, "Codex must not intercept provider-defined image models")
					if selection.ReleaseFunc != nil {
						selection.ReleaseFunc()
					}

					svc = newImagesModelSchedulerTestService(advanced, []Account{unsupported})
					selection, _, err = svc.SelectAccountWithSchedulerForImages(context.Background(), &groupID, "", "gpt-image-custom", nil, capability)
					require.Error(t, err)
					require.Nil(t, selection, "an OAuth-only pool must not select an unsupported image model")
				})
			}
		}
	}
}

func TestOpenAIImagesSchedulerKeepsKnownModelsAndExplicitMappings(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		for _, tc := range []struct {
			name, model, accountType string
			mapping                  map[string]any
			forwardModel             string
		}{
			{name: "known", model: "gpt-image-2", accountType: AccountTypeOAuth},
			{name: "alias25", model: "gpt-image-2.5", accountType: AccountTypeOAuth},
			{name: "alias26", model: "gpt-image-2.6", accountType: AccountTypeOAuth},
			{name: "setup_mapping", model: "gpt-image-custom", accountType: AccountTypeSetupToken, mapping: map[string]any{"gpt-image-custom": "gpt-image-2.5-flare"}},
			{name: "setup_alias_mapping", model: "gpt-image-custom", accountType: AccountTypeSetupToken, mapping: map[string]any{"gpt-image-custom": "gpt-image-2.6"}},
			{name: "channel_mapping", model: "gpt-image-custom", accountType: AccountTypeOAuth, forwardModel: "gpt-image-2.5-flare"},
		} {
			t.Run("advanced="+advanced+"/"+tc.name, func(t *testing.T) {
				account := Account{
					ID: 38103, Platform: PlatformOpenAI, Type: tc.accountType,
					Status: StatusActive, Schedulable: true, Concurrency: 1,
					Credentials: map[string]any{"model_mapping": tc.mapping},
				}
				svc := newImagesModelSchedulerTestService(advanced, []Account{account})
				groupID := int64(10121)
				ctx := context.Background()
				if tc.forwardModel != "" {
					ctx = WithOpenAIForwardModel(ctx, tc.forwardModel, false)
				}
				selection, _, err := svc.SelectAccountWithSchedulerForImages(ctx, &groupID, "", tc.model, nil, OpenAIImagesCapabilityNative)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, account.ID, selection.Account.ID)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			})
		}
	}

	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.True(t, accountSupportsOpenAICapabilities(context.Background(), account, "", "", "gpt-image-custom"),
		"the Codex image model fence must not change non-Images scheduling")
}
