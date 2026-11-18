package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func gpt62PricingCatalog(t *testing.T) *PricingService {
	t.Helper()
	body, err := os.ReadFile("../../resources/model-pricing/model_prices_and_context_window.json")
	require.NoError(t, err)
	return newStubPricingServiceFromJSON(t, string(body))
}

func TestGPT62PricingCatalogAndFallbacks(t *testing.T) {
	catalog := gpt62PricingCatalog(t)
	for _, tc := range []struct {
		model                      string
		input, output, write, read float64
	}{
		{"gpt-6.2-astra", 8e-6, 40e-6, 10e-6, 0.4e-6},
		{"gpt-6.2-sol", 2e-6, 10e-6, 2.5e-6, 0.1e-6},
	} {
		for _, source := range []*PricingService{catalog, {}} {
			for _, model := range []string{tc.model, "openai/" + tc.model + "-max", tc.model + "-openai-compact"} {
				p := source.GetModelPricing(model)
				require.NotNil(t, p, model)
				require.InDelta(t, tc.input, p.InputCostPerToken, 1e-12, model)
				require.InDelta(t, tc.output, p.OutputCostPerToken, 1e-12)
				require.InDelta(t, tc.write, p.CacheCreationInputTokenCost, 1e-12)
				require.InDelta(t, tc.read, p.CacheReadInputTokenCost, 1e-12)
				require.Equal(t, 272000, p.LongContextInputTokenThreshold)
			}
		}
		require.Same(t, catalog.pricingData[tc.model], catalog.GetIdentifiedModelPricing("openai/"+tc.model+"-max"))
	}
	// Exact catalog entries for an effort alias must retain priority over its
	// base model, including intentional zero-valued local override entries.
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		explicit := &LiteLLMModelPricing{}
		catalog.pricingData[model+"-max"] = explicit
		require.Same(t, explicit, catalog.GetIdentifiedModelPricing("openai/"+model+"-max"))
		require.Same(t, explicit, catalog.GetModelPricing("openai/"+model+"-max"))
	}
}

func TestGPT62BillingAcrossSourcesTiersAndContextBoundary(t *testing.T) {
	sources := map[string]*BillingService{
		"billing fallback": NewBillingService(&config.Config{}, nil),
		"stale catalog": NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
			"gpt-6-astra": {InputCostPerToken: 10e-6, OutputCostPerToken: 50e-6},
			"gpt-6-luna":  {InputCostPerToken: 0.1e-6, OutputCostPerToken: 0.5e-6},
		}}),
		"bundled catalog": NewBillingService(&config.Config{}, gpt62PricingCatalog(t)),
	}
	for source, svc := range sources {
		for _, tc := range []struct {
			model                      string
			input, output, write, read float64
		}{
			{"gpt-6.2-astra", 12e-6, 60e-6, 15e-6, 0.6e-6},
			{"gpt-6.2-sol", 4e-6, 20e-6, 5e-6, 0.2e-6},
		} {
			for _, model := range []string{tc.model, "openai/" + tc.model + "-max", tc.model + "-openai-compact"} {
				t.Run(source+"/"+model, func(t *testing.T) {
					for _, prompt := range []int{271999, 272000, 272001} {
						for tier, multiplier := range map[string]float64{"": 1, "fast": 2, "priority": 2, "ultrafast": 3} {
							tokens := UsageTokens{InputTokens: prompt - 3000, CacheReadTokens: 2000, CacheCreationTokens: 1000, OutputTokens: 500}
							cost, err := svc.CalculateCostUnified(CostInput{
								Ctx: context.Background(), Model: model, Tokens: tokens, RateMultiplier: 0.05,
								ServiceTier: tier, PricingAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC),
								Resolver: NewModelPricingResolver(nil, svc), Group: &Group{LongContextPricingEnabled: true},
							})
							require.NoError(t, err)
							inputMultiplier, outputMultiplier := 1.0, 1.0
							if prompt > 272000 {
								inputMultiplier, outputMultiplier = 2, 1.5
							}
							require.InDelta(t, float64(tokens.InputTokens)*tc.input*inputMultiplier*multiplier, cost.InputCost, 1e-10, tier)
							require.InDelta(t, 500*tc.output*outputMultiplier*multiplier, cost.OutputCost, 1e-10, tier)
							require.InDelta(t, 1000*tc.write*inputMultiplier*multiplier, cost.CacheCreationCost, 1e-10, tier)
							require.InDelta(t, 2000*tc.read*inputMultiplier*multiplier, cost.CacheReadCost, 1e-10, tier)
							require.InDelta(t, cost.TotalCost*0.05, cost.ActualCost, 1e-10)
							require.Equal(t, prompt > 272000, cost.LongContextBillingApplied)
						}
					}
				})
			}
		}
	}
}

func TestGPT62UltrafastUsesRequestTimeAtPacificPromotionBoundary(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		price := 1e-6
		custom := ChannelModelPricing{Models: []string{model}, InputPrice: &price, OutputPrice: &price, CacheWritePrice: &price, CacheReadPrice: &price}
		resolver := NewModelPricingResolver(nil, svc)
		channel := resolver.resolveConfiguredPricing(&custom, model, PricingSourceChannel)
		// The channel base was resolved before the promotion boundary. The
		// request's timestamp must still decide its Ultrafast multiplier.
		for _, source := range []struct {
			resolver *ModelPricingResolver
			group    *Group
			resolved *ResolvedPricing
		}{
			{},
			{resolver: resolver},
			{resolver: resolver, group: &Group{ModelPricing: []ChannelModelPricing{custom}}},
			{resolver: resolver, resolved: channel},
		} {
			for _, tc := range []struct {
				at   time.Time
				mult float64
			}{
				{time.Date(2027, 3, 1, 7, 59, 59, 0, time.UTC), 3},
				{time.Date(2027, 3, 1, 8, 0, 0, 0, time.UTC), 6},
			} {
				input := CostInput{Ctx: context.Background(), Model: model, Tokens: UsageTokens{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 1000, CacheCreationTokens: 1000}, RateMultiplier: 1, PricingAt: tc.at, Resolver: source.resolver, Group: source.group, Resolved: source.resolved}
				standard, err := svc.CalculateCostUnified(input)
				require.NoError(t, err)
				input.ServiceTier = "ultrafast"
				fast, err := svc.CalculateCostUnified(input)
				require.NoError(t, err)
				require.InDelta(t, standard.TotalCost*tc.mult, fast.TotalCost, 1e-12)
			}
		}
		tokens := UsageTokens{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 1000, CacheCreationTokens: 1000}
		standard, err := svc.CalculateCost(model, tokens, 1)
		require.NoError(t, err)
		legacy, err := svc.CalculateCostWithServiceTier(model, tokens, 1, "ultrafast")
		require.NoError(t, err)
		require.InDelta(t, standard.TotalCost*gpt62UltrafastMultiplierAt(time.Now()), legacy.TotalCost, 1e-12)
	}
}

func TestGPT62PricingPreservesGroupAndChannelOverrides(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		for _, price := range []float64{0, 1e-6} {
			fast := 2.5
			custom := ChannelModelPricing{
				Models: []string{model}, InputPrice: &price, OutputPrice: &price,
				CacheWritePrice: &price, CacheReadPrice: &price, FastMultiplier: &fast,
			}
			channelPrice, err := svc.GetModelPricingWithChannel(model, &custom)
			require.NoError(t, err)
			tokens := UsageTokens{InputTokens: 300000, OutputTokens: 1000, CacheCreationTokens: 1000, CacheReadTokens: 1000}
			// Channel overrides preserve their own prices and speed multiplier.
			channelCost := svc.computeTokenBreakdown(channelPrice, tokens, 1, "priority", false)
			require.InDelta(t, 303000*price*fast, channelCost.TotalCost, 1e-12)
			for _, longContext := range []bool{false, true} {
				group := &Group{LongContextPricingEnabled: longContext, ModelPricing: []ChannelModelPricing{custom}}
				for tier, mult := range map[string]float64{"": 1, "priority": fast, "ultrafast": 3} {
					cost, err := svc.CalculateCostUnified(CostInput{
						Ctx: context.Background(), Model: model, Group: group, Tokens: tokens, RateMultiplier: 0.05,
						ServiceTier: tier, PricingAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), Resolver: NewModelPricingResolver(nil, svc),
					})
					require.NoError(t, err)
					want := 303000 * price * mult
					if longContext {
						want = (302000*2 + 1000*1.5) * price * mult
					}
					require.InDelta(t, want, cost.TotalCost, 1e-12, "%s long=%t tier=%s", model, longContext, tier)
					require.InDelta(t, want*0.05, cost.ActualCost, 1e-12)
				}
			}
		}
		fresh, err := svc.GetModelPricing(model)
		require.NoError(t, err)
		require.Positive(t, fresh.CacheReadPricePerToken, "custom zero prices must not mutate shared defaults")
	}
}

func TestGPT62GatewayFallbacksPreserveRequestPricingTime(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	gateway := &OpenAIGatewayService{billingService: svc}
	ctx := context.Background()
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		for _, tc := range []struct {
			at   time.Time
			mult float64
		}{
			{time.Date(2027, 3, 1, 7, 59, 59, 0, time.UTC), 3},
			{time.Date(2027, 3, 1, 8, 0, 0, 0, time.UTC), 6},
		} {
			tokens := UsageTokens{InputTokens: 300000, OutputTokens: 500, CacheReadTokens: 1000, CacheCreationTokens: 1000}
			standard, err := svc.CalculateCost(model, tokens, 0.05)
			require.NoError(t, err)
			generic, err := svc.CalculateTokenCostForRequest(TokenCostRequest{
				Ctx: ctx, Model: model, Tokens: tokens, RateMultiplier: 0.05,
				PricingAt: tc.at, ServiceTier: "ultrafast",
			})
			require.NoError(t, err)
			require.InDelta(t, standard.ActualCost*tc.mult, generic.ActualCost, 1e-12)
			openaiCost, err := gateway.calculateOpenAIRecordUsageTokenCost(ctx, &APIKey{}, model, 0.05, tc.at, tokens, "ultrafast", "", nil)
			require.NoError(t, err)
			require.InDelta(t, generic.ActualCost, openaiCost.ActualCost, 1e-12)
			longContext := false
			withoutLongContext, err := gateway.calculateOpenAIRecordUsageTokenCost(ctx, &APIKey{}, model, 0.05, tc.at, tokens, "ultrafast", "", &longContext)
			require.NoError(t, err)
			require.False(t, withoutLongContext.LongContextBillingApplied)
			require.InDelta(t, openaiCost.InputCost/2, withoutLongContext.InputCost, 1e-12)
			require.InDelta(t, openaiCost.OutputCost/1.5, withoutLongContext.OutputCost, 1e-12)
		}
	}
}

func TestGPT62PricingDoesNotCaptureOtherModels(t *testing.T) {
	catalog := gpt62PricingCatalog(t)
	svc := NewBillingService(&config.Config{}, nil)
	for _, model := range []string{"gpt-6.2", "gpt-6.2-astra-preview", "gpt-6.2-astra-ultra", "gpt-6.2-sol-ultra", "gpt-6.2-astral", "gpt-6.2-solitude", "gpt-6.20-sol"} {
		require.Nil(t, catalog.GetIdentifiedModelPricing(model), model)
		_, err := svc.GetModelPricing(model)
		require.ErrorIs(t, err, ErrModelPricingUnavailable, model)
	}
	for _, tc := range []struct {
		model string
		input float64
	}{
		{"gpt-6-astra", 15e-6},
		{"gpt-6.1-sol", 4e-6},
	} {
		p, err := svc.GetModelPricing(tc.model)
		require.NoError(t, err)
		require.InDelta(t, tc.input, p.InputPricePerToken, 1e-12)
	}
}
