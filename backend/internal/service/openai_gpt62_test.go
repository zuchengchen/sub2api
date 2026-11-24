//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestGPT62OfflineCodexCatalogAndAccountTiers(t *testing.T) {
	for _, tc := range []struct {
		model            string
		window, priority int
	}{{"gpt-6.2-sol", 500000, 1}, {"gpt-6.2-astra", 272000, 2}} {
		var raw map[string]any
		require.NoError(t, json.Unmarshal(newConfiguredCodexModelDescriptor(tc.model).officialMetadata, &raw))
		require.EqualValues(t, tc.priority, raw["priority"])
		require.EqualValues(t, tc.window, raw["context_window"])
		require.EqualValues(t, 872000, raw["max_context_window"])
		require.Equal(t, "low", raw["default_reasoning_level"])
		require.Equal(t, []string{"low", "medium", "high", "xhigh", "max", "ultra"}, effortsFromManifestModel(t, raw))
		require.Equal(t, "xhigh", raw["multi_agent_reasoning_effort"])
		require.Equal(t, "v2", raw["multi_agent_version"])
		require.Equal(t, true, raw["use_responses_lite"])
		require.Equal(t, "code_mode_only", raw["tool_mode"])
		require.NotEmpty(t, openai.CodexBaseInstructionsForModel(tc.model))
		descriptor := newConfiguredCodexModelDescriptor(tc.model)
		require.EqualValues(t, tc.window, descriptor.ContextWindow)
		require.Equal(t, "low", *descriptor.DefaultReasoningLevel)
		for _, kind := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			account := &Account{Platform: PlatformOpenAI, Type: kind}
			caps := accountCodexToolCapabilities(account, tc.model)
			if kind == AccountTypeAPIKey {
				require.Contains(t, string(caps["service_tiers"]), "ultrafast")
				require.Contains(t, string(caps["service_tiers"]), "Ultra-low latency processing.")
			} else {
				require.NotContains(t, string(caps["service_tiers"]), "ultrafast")
				group := &Group{ID: 1, Status: StatusActive, Hydrated: true, Platform: PlatformOpenAI, ForceOpenAIUltrafast: true}
				ctx := context.WithValue(context.Background(), ctxkey.Group, group)
				require.Empty(t, openAIGroupForcedServiceTier(ctx, account, tc.model))
				group.ForceOpenAIFast = true
				account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{tc.model: {ID: tc.model, CodexToolCapabilities: map[string]json.RawMessage{"service_tiers": json.RawMessage(`[{"id":"priority"}]`)}}}})
				require.Equal(t, "priority", openAIGroupForcedServiceTier(ctx, account, tc.model))
				// An account which actually advertises Ultrafast keeps that capability.
				account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{tc.model: {ID: tc.model, CodexToolCapabilities: map[string]json.RawMessage{"service_tiers": json.RawMessage(`[{"id":"ultrafast","name":"Ultrafast"}]`)}}}})
				require.Equal(t, "ultrafast", openAIGroupForcedServiceTier(ctx, account, tc.model+"-max"))
				require.Contains(t, string(accountCodexToolCapabilities(account, tc.model)["service_tiers"]), "ultrafast")
			}
		}
	}
}

func TestGPT62SubscriptionUltrafastInManifestAndForcedRequests(t *testing.T) {
	const fastOnly = `[{"id":"priority","name":"Fast","description":"account Fast"}]`
	const both = `[{"id":"priority","name":"Fast"},{"id":"ultrafast","name":"Ultrafast","description":"workspace grant"}]`
	for _, tc := range []struct {
		plan, model, snapshot string
		want                  bool
	}{
		{"pro", "gpt-6.2-sol", fastOnly, false},
		{"pro", "gpt-6.2-astra", fastOnly, false},
		{"chatgpt_pro", "gpt-6.2-sol", "", false},
		{"promax", "gpt-6.2-astra", fastOnly, false},
		{"promax", "gpt-6.2-sol", "", false},
		{"prolite", "gpt-6.2-sol", fastOnly, false},
		{"enterprise", "gpt-6.2-astra", both, true},
		{"edu", "gpt-6.2-sol", fastOnly, false},
		{"pro", "gpt-6.2-sol", both, true},
	} {
		t.Run(tc.plan+"/"+tc.model+"/"+tc.snapshot, func(t *testing.T) {
			account := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"plan_type": tc.plan}}
			if tc.snapshot != "" {
				account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{tc.model: {ID: tc.model, CodexToolCapabilities: map[string]json.RawMessage{"service_tiers": json.RawMessage(tc.snapshot)}}}})
			}
			group := &Group{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, ForceOpenAIFast: true, ForceOpenAIUltrafast: true}
			gateway := &GatewayService{accountRepo: codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{1: {account}}}}
			body, err := gateway.BuildCodexModelsManifestForGroup(context.Background(), group, "", []string{tc.model})
			require.NoError(t, err)
			models := decodeCodexManifestModels(t, body)
			require.Len(t, models, 1)
			tiers, ok := models[0]["service_tiers"].([]any)
			require.True(t, ok)
			wantTiers := 1
			if tc.want {
				wantTiers++
			}
			require.Len(t, tiers, wantTiers)
			require.Equal(t, "priority", tiers[0].(map[string]any)["id"])
			if tc.snapshot == fastOnly {
				require.Equal(t, "account Fast", tiers[0].(map[string]any)["description"])
			}
			ctx := context.WithValue(context.Background(), ctxkey.Group, group)
			forced := openAIGroupForcedServiceTier(ctx, &account, tc.model+"-max")
			if tc.want {
				require.Equal(t, "ultrafast", tiers[1].(map[string]any)["id"])
				require.Equal(t, OpenAIFastTierUltrafast, forced)
				if tc.snapshot == both {
					require.Equal(t, "workspace grant", tiers[1].(map[string]any)["description"])
				}
			} else {
				require.Equal(t, OpenAIFastTierPriority, forced)
			}
		})
	}
}
