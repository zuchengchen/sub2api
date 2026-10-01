package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// kiro-rs routes cache TTL per breakpoint (default 5m, 1h only when asked or a
// 1h rule matches) and reports the split in usage.cache_creation. Sub2API must
// forward the client's TTL untouched and bill the 5m/1h buckets separately.
const (
	kiroMixedStart   = `{"type":"message_start","message":{"usage":{"input_tokens":20,"output_tokens":1,"cache_creation_input_tokens":80,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":50,"ephemeral_1h_input_tokens":30}}}}`
	kiroMixedDelta   = `{"type":"message_delta","usage":{"input_tokens":20,"output_tokens":9,"cache_creation_input_tokens":80,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":50,"ephemeral_1h_input_tokens":30},"usage_final":true}}`
	kiroDefaultDelta = `{"type":"message_delta","usage":{"input_tokens":20,"output_tokens":9,"cache_creation_input_tokens":80,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":80,"ephemeral_1h_input_tokens":0},"usage_final":true}}`
)

func TestKiroCacheTTL_StreamParsersKeepMixedSplit(t *testing.T) {
	svc := &GatewayService{}
	parsed := &ClaudeUsage{}
	svc.parseSSEUsage(kiroMixedStart, parsed)
	svc.parseSSEUsage(kiroMixedDelta, parsed)

	passthrough := &ClaudeUsage{}
	parseSSEUsagePassthrough(kiroMixedStart, passthrough)
	parseSSEUsagePassthrough(kiroMixedDelta, passthrough)

	for name, u := range map[string]*ClaudeUsage{"gateway": parsed, "passthrough": passthrough} {
		require.Equal(t, 80, u.CacheCreationInputTokens, name)
		require.Equal(t, 50, u.CacheCreation5mTokens, name)
		require.Equal(t, 30, u.CacheCreation1hTokens, name)
	}

	defaulted := &ClaudeUsage{}
	parseSSEUsagePassthrough(kiroDefaultDelta, defaulted)
	require.Equal(t, 80, defaulted.CacheCreation5mTokens)
	require.Zero(t, defaulted.CacheCreation1hTokens)
}

func TestKiroCacheTTL_NonStreamBodyKeepsMixedSplit(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":20,"output_tokens":9,"cache_creation_input_tokens":80,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":50,"ephemeral_1h_input_tokens":30}}}`)
	u := parseClaudeUsageFromResponseBody(body)
	require.Equal(t, 50, u.CacheCreation5mTokens)
	require.Equal(t, 30, u.CacheCreation1hTokens)
}

func TestKiroCacheTTL_BillingChargesEachBucketAtItsOwnPrice(t *testing.T) {
	svc := &BillingService{
		cfg: &config.Config{},
		fallbackPrices: map[string]*ModelPricing{
			"claude-opus-5": {
				InputPricePerToken:     5e-6,
				OutputPricePerToken:    25e-6,
				CacheReadPricePerToken: 0.5e-6,
				SupportsCacheBreakdown: true,
				CacheCreation5mPrice:   6.25e-6,
				CacheCreation1hPrice:   10e-6,
			},
		},
	}
	cost, err := svc.CalculateCost("claude-opus-5", UsageTokens{
		InputTokens:           20,
		OutputTokens:          9,
		CacheCreationTokens:   80,
		CacheCreation5mTokens: 50,
		CacheCreation1hTokens: 30,
	}, 1.0)
	require.NoError(t, err)
	require.InDelta(t, 50*6.25e-6+30*10e-6, cost.CacheCreationCost, 1e-12)

	// Aggregate-only usage (older upstreams) falls back to the 5m price.
	legacy, err := svc.CalculateCost("claude-opus-5", UsageTokens{CacheCreationTokens: 80}, 1.0)
	require.NoError(t, err)
	require.InDelta(t, 80*6.25e-6, legacy.CacheCreationCost, 1e-12)
}

func TestKiroCacheTTL_APIKeyRewriteKeepsClientTTLAndMixedOrder(t *testing.T) {
	// Explicit 1h system + 5m messages (valid order) survive the API-key
	// rewrite; the moved message breakpoints keep the client's longest TTL.
	body := []byte(`{
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"q1","cache_control":{"type":"ephemeral","ttl":"5m"}}]},
			{"role":"assistant","content":[{"type":"text","text":"a1"}]}
		]}`)
	out := rewriteMessageCacheControlBody(body)
	require.Equal(t, "1h", gjson.GetBytes(out, "system.0.cache_control.ttl").String())
	require.Equal(t, "5m", gjson.GetBytes(out, "messages.1.content.0.cache_control.ttl").String())

	// enforceCacheControlLimit only trims breakpoint count; it never rewrites TTLs.
	limited := enforceCacheControlLimit(out)
	require.Equal(t, "1h", gjson.GetBytes(limited, "system.0.cache_control.ttl").String())
	require.Equal(t, "5m", gjson.GetBytes(limited, "messages.1.content.0.cache_control.ttl").String())

	// Unspecified everywhere stays unspecified: no ttl field is invented.
	plain := rewriteMessageCacheControlBody([]byte(`{
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"hi"}]}`))
	require.False(t, gjson.GetBytes(plain, "system.0.cache_control.ttl").Exists())
	require.Equal(t, "ephemeral", gjson.GetBytes(plain, "messages.0.content.0.cache_control.type").String())
	require.False(t, gjson.GetBytes(plain, "messages.0.content.0.cache_control.ttl").Exists())
}

func TestKiroCacheTTL_NoTTLOverrideForAPIKeyAccounts(t *testing.T) {
	// Kiro relays are Anthropic API-key accounts. The OAuth-only 1h request
	// injection and usage TTL override must never apply to them, even if the
	// account carries override extras.
	account := &Account{ID: 22173, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{
		"cache_ttl_override_enabled": true,
		"cache_ttl_override_target":  "1h",
	}}
	svc := &GatewayService{}
	require.False(t, account.IsCacheTTLOverrideEnabled())
	_, ok := svc.resolveCacheTTLUsageOverrideTarget(t.Context(), account)
	require.False(t, ok)
	require.False(t, svc.shouldInjectAnthropicCacheTTL1h(t.Context(), account))
}
