package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeClaude55LegacyThinkingPreservesExplicitIntentAndHistory(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "anthropic/claude-opus-5.5", "claude-sonnet-5-5", "claude-haiku-5-5"} {
		t.Run(model, func(t *testing.T) {
			body := []byte(`{"thinking":{"type":"enabled","budget_tokens":8192,"display":"omitted"},"output_config":{"effort":"max","format":{"type":"json_schema","schema":{"type":"object"}}},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"signed"},{"type":"redacted_thinking","data":"opaque"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"integer":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)
			original := append([]byte(nil), body...)
			out, changed, err := normalizeClaude55LegacyThinkingBody(body, model)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, original, body, "caller body must not be mutated")
			require.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
			require.False(t, gjson.GetBytes(out, "thinking.budget_tokens").Exists())
			require.Equal(t, "omitted", gjson.GetBytes(out, "thinking.display").String())
			require.Equal(t, gjson.GetBytes(body, "messages").Raw, gjson.GetBytes(out, "messages").Raw)
			require.Equal(t, gjson.GetBytes(body, "output_config").Raw, gjson.GetBytes(out, "output_config").Raw)
			require.NoError(t, validateClaude55Request(out, model))
			second, changed, err := normalizeClaude55LegacyThinkingBody(out, model)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, out, second)
		})
	}
}

func TestNormalizeClaude55LegacyThinkingBudgetAndValidationBoundaries(t *testing.T) {
	for _, tc := range []struct{ budget, want string }{
		{"1024", "low"}, {"2048", "medium"}, {"4096", "medium"},
		{"8192", "high"}, {"10240", "high"}, {"16000", "xhigh"},
		{"32000", "xhigh"}, {"32768", "max"}, {"65536", "max"},
	} {
		t.Run(tc.budget, func(t *testing.T) {
			body := []byte(`{"thinking":{"type":"enabled","budget_tokens":` + tc.budget + `}}`)
			out, changed, err := normalizeClaude55LegacyThinkingBody(body, "claude-opus-5-5")
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, tc.want, gjson.GetBytes(out, "output_config.effort").String())
		})
	}
	for _, extra := range []string{
		`"thinking":{"type":"enabled"}`,
		`"thinking":{"type":"enabled","budget_tokens":512}`,
		`"thinking":{"type":"enabled","budget_tokens":"8192"}`,
		`"thinking":{"type":"enabled","budget_tokens":8192.5}`,
		`"thinking":{"type":"enabled","budget_tokens":null}`,
		`"thinking":{"type":"enabled","budget_tokens":8192},"output_config":[]`,
		`"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"none"}`,
		`"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":4}`,
	} {
		_, changed, err := normalizeClaude55LegacyThinkingBody([]byte("{"+extra+"}"), "claude-opus-5-5")
		require.Error(t, err, extra)
		require.False(t, changed)
	}
	out, changed, err := normalizeClaude55LegacyThinkingBody([]byte(`{"thinking":{"type":"enabled"},"output_config":{"effort":"low"}}`), "claude-opus-5-5")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "low", gjson.GetBytes(out, "output_config.effort").String())
}

func TestNormalizeClaude55LegacyThinkingLeavesOtherModesAndModels(t *testing.T) {
	for _, tc := range []struct{ model, body string }{
		{"claude-opus-5-5", `{"thinking":{"type":"disabled"}}`},
		{"claude-sonnet-5-5", `{"thinking":{"type":"between_tools"},"output_config":{"effort":"low"}}`},
		{"claude-haiku-5-5", `{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`},
		{"claude-opus-5-5", `{"thinking":{"type":"adaptive"}}`},
		{"claude-opus-5-5", `{"messages":[]}`},
		{"claude-sonnet-4-6", `{"thinking":{"type":"enabled","budget_tokens":8192}}`},
		{"claude-opus-5-5-preview", `{"thinking":{"type":"enabled","budget_tokens":8192}}`},
		{"deepseek-v4-pro", `{"thinking":{"type":"enabled","budget_tokens":8192}}`},
	} {
		t.Run(tc.model+"/"+tc.body, func(t *testing.T) {
			out, changed, err := normalizeClaude55LegacyThinkingBody([]byte(tc.body), tc.model)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, tc.body, string(out))
		})
	}
}

func TestGatewayForwardNormalizesLegacyThinkingAfterModelMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, count := range []bool{false, true} {
		for _, model := range []string{"claude-opus-5.5", "public-opus"} {
			t.Run(model+map[bool]string{false: "/messages", true: "/count_tokens"}[count], func(t *testing.T) {
				body := []byte(`{"model":"` + model + `","max_tokens":20000,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"xhigh"}}`)
				parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
				require.NoError(t, err)
				account := newAnthropicAPIKeyAccountForTest()
				if model == "public-opus" {
					account.Credentials["model_mapping"] = map[string]any{model: "claude-opus-5-5"}
				}
				response := `{"id":"msg_1","type":"message","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
				if count {
					response = `{"input_tokens":42}`
				}
				upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}}
				cfg := &config.Config{}
				svc := &GatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: &RateLimitService{}, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				path := "/v1/messages"
				if count {
					path += "/count_tokens"
				}
				c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				c.Request.Header.Set("User-Agent", "claude-cli/2.1.318 (external, cli)")
				if count {
					err = svc.ForwardCountTokens(context.Background(), c, account, parsed)
				} else {
					var result *ForwardResult
					result, err = svc.Forward(context.Background(), c, account, parsed)
					require.NotNil(t, result)
				}
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, "adaptive", gjson.GetBytes(upstream.lastBody, "thinking.type").String())
				require.False(t, gjson.GetBytes(upstream.lastBody, "thinking.budget_tokens").Exists())
				require.Equal(t, "xhigh", gjson.GetBytes(upstream.lastBody, "output_config.effort").String())
				require.Equal(t, "xhigh", parsed.OutputEffort)
				require.True(t, parsed.ThinkingEnabled)
				require.Equal(t, "enabled", gjson.GetBytes(body, "thinking.type").String())
			})
		}
	}
}
