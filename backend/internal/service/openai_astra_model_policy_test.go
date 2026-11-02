package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func astraPolicyTestHistory(verdict string, at time.Time) map[string]any {
	return map[string]any{"model": "gpt-6-astra", "verdict": "healthy", "recent_samples": []map[string]any{{"verdict": verdict, "at": at.Format(time.RFC3339Nano)}}}
}

func astraPolicyTestAccount(verdict string) *Account {
	return &Account{ID: 500, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 2,
		Extra:       map[string]any{OpenAIAstraPolicyExtraKey: astraPolicyTestHistory(verdict, time.Now().Add(-time.Hour))},
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-6-astra": "gpt-6-luna"}, "compact_model_mapping": map[string]any{"gpt-6-luna": "gpt-6-astra"}}}
}

func TestAstraOAuthModelPolicyStatesAndOriginalConfiguration(t *testing.T) {
	for _, verdict := range []string{"healthy", "degraded", "unknown"} {
		t.Run(verdict, func(t *testing.T) {
			account := astraPolicyTestAccount(verdict)
			require.Equal(t, verdict, account.OpenAIOAuthModelPolicy().Verdict)
			require.Nil(t, account.GetModelMapping())
			require.Nil(t, account.GetCompactModelMapping())
			require.Len(t, account.Credentials, 2)
			for _, model := range []string{"gpt-6-luna", "gpt-5.6-luna", "provider/gpt6luna-high", "gpt-5.6-luna-2026-09-01"} {
				require.Equal(t, verdict != "healthy", account.IsModelSupported(model), model)
			}
			for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6-sol", "gpt-6-lunatic"} {
				require.Equal(t, verdict == "healthy", account.IsModelSupported(model), model)
			}
			for _, model := range []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
				require.True(t, account.IsModelSupported(model), model)
			}
			require.False(t, account.IsModelSupported("gpt-5.5"))
			require.Equal(t, "gpt-6-astra", account.GetMappedModel("gpt-6-astra"))
			account.Extra["openai_passthrough"] = true
			require.Equal(t, verdict == "healthy", account.IsModelSupported("gpt-6-astra"))
		})
	}
	for _, kind := range []string{AccountTypeAPIKey, AccountTypeSetupToken} {
		account := astraPolicyTestAccount("degraded")
		account.Type = kind
		require.Nil(t, account.OpenAIOAuthModelPolicy())
		require.NotEmpty(t, account.GetModelMapping())
		require.True(t, account.IsModelSupported("gpt-6-astra"))
	}
}

func TestAstraOAuthModelPolicyLatestDefinitiveSample(t *testing.T) {
	account := astraPolicyTestAccount("healthy")
	at := time.Now().Add(-24 * time.Hour)
	raw := astraPolicyTestHistory("degraded", at)
	raw["recent_samples"] = []any{
		map[string]any{"verdict": "healthy", "at": at.Add(time.Minute).Format(time.RFC3339Nano)},
		map[string]any{"verdict": "degraded", "at": at.Format(time.RFC3339Nano)},
		map[string]any{"verdict": "unknown", "at": at.Add(2 * time.Minute).Format(time.RFC3339Nano)},
		map[string]any{"verdict": "degraded", "at": "broken"},
	}
	raw["verdict"] = "degraded"
	account.Extra[OpenAIAstraPolicyExtraKey] = raw
	require.Equal(t, "healthy", account.OpenAIOAuthModelPolicy().Verdict)
	require.True(t, account.IsModelSupported("gpt-6-astra"))
	for _, model := range []string{"gpt-5.6-sol", "", "gpt-6-luna"} {
		raw["model"] = model
		require.Equal(t, "unknown", account.OpenAIOAuthModelPolicy().Verdict)
	}
}

func TestAstraOAuthModelPolicyForwardAndCompactCannotBypass(t *testing.T) {
	account := astraPolicyTestAccount("degraded")
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	_, err := svc.Forward(context.Background(), nil, account, []byte(`{"model":"gpt-6-astra","tools":[{"type":"image_generation"}]}`))
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.False(t, failover.RetryableOnSameAccount)
	svc.cfg.Gateway.OpenAICompactModel = "gpt-6-astra"
	require.Empty(t, svc.resolveOpenAICompactFallbackModel(account, "gpt-6-luna"))
	account = astraPolicyTestAccount("healthy")
	svc.cfg.Gateway.OpenAICompactModel = "gpt-6-luna"
	require.Empty(t, svc.resolveOpenAICompactFallbackModel(account, "gpt-6-astra"))
	require.NoError(t, validateOpenAIAstraModelPolicy(account, "gpt-image-2"))
}

// Transport and catalog fixtures must explicitly be eligible for non-Luna text.
// Policy-specific tests use astraPolicyTestAccount and cover restricted states.
func astraHealthyTestExtra(extra map[string]any) map[string]any {
	result := make(map[string]any, len(extra)+1)
	result[OpenAIAstraPolicyExtraKey] = astraPolicyTestHistory("healthy", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	for key, value := range extra {
		result[key] = value
	}
	return result
}
