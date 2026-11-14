package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeCodexModelCompactGPT56Aliases(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"gpt-5.6sol", "gpt-5.6-sol"},
		{"openai/GPT5.6SOL", "gpt-5.6-sol"},
		{"gpt-5.6sol-high", "gpt-5.6-sol"},
		{"gpt-5.6terra", "gpt-5.6-terra"},
		{"gpt-5.6luna-max", "gpt-5.6-luna"},
		{"gpt-5.6-sol-2026-07-09", "gpt-5.6-sol"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			require.Equal(t, tc.want, normalizeCodexModel(tc.model))
			require.True(t, isOpenAIGPT56Model(tc.model))
		})
	}
}

func TestNormalizeCodexModelPreservesUnknownFamiliesAndCustomNames(t *testing.T) {
	for _, model := range []string{
		"gpt-5.7", "gpt-5.60", "gpt-5.6solar", "gpt-5.6-solitude",
		"gpt-5.6-sol-private", "gpt-5.5-custom", "gpt-5.5-proxy",
		"gpt-5.4-mini-custom", "gpt-5.4-custom", "gpt-5.3-custom",
		"private-codex-model", "vendor/gpt-5.7", "vendor/GPT-5.6-SOL-PRIVATE",
	} {
		t.Run(model, func(t *testing.T) {
			require.Empty(t, normalizeKnownOpenAICodexModel(model))
			require.Equal(t, model, normalizeCodexModel(model))
		})
	}
}

func TestOpenAIForwardCompactModelAliasAndExplicitMapping(t *testing.T) {
	for _, tc := range []struct {
		name, requested, mapped, want string
	}{
		{"observed typo", "gpt-5.6sol", "", "gpt-5.6-sol"},
		{"future family", "gpt-5.7", "", "gpt-5.7"},
		{"explicit model mapping wins", "gpt-5.6sol", "gpt-5.6-terra", "gpt-5.6-terra"},
		{"explicit private model survives", "public-alias", "gpt-5.6-sol-private", "gpt-5.6-sol-private"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tc.requested + `","stream":false,"instructions":"test","input":"hello"}`)
			account := newOpenAIOAuthNamespaceTestAccount()
			if tc.mapped != "" {
				// Stored mappings are intentionally disabled for native OAuth;
				// API-key accounts own the explicit account-mapping contract.
				account = newOpenAIRejectedFieldTestAccount()
				account.Credentials["model_mapping"] = map[string]any{tc.requested: tc.mapped}
			}
			upstream := &httpUpstreamRecorder{responses: []*http.Response{openAICompatSSECompletedResponse("resp_model_alias", tc.want)}}
			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, tc.want, gjson.GetBytes(upstream.bodies[0], "model").String())
		})
	}
}
