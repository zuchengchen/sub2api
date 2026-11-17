package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const oauthReliabilityModelError = `{"type":"invalid_request_error","code":"unsupported_model","param":"model","message":"synthetic upstream detail"}`
const oauthReliabilityModelUsage = `{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":4}}`

func TestOAuthReliabilityModelUnavailableTerminalUsageEvidence(t *testing.T) {
	for _, body := range []string{
		`{"error":` + oauthReliabilityModelError + `,"usage":` + oauthReliabilityModelUsage + `}`,
		`{"type":"response.failed","response":{"status":"failed","error":` + oauthReliabilityModelError + `,"usage":` + oauthReliabilityModelUsage + `}}`,
	} {
		for _, status := range []int{400, 502} {
			usage := openAIModelUnavailableUsage(status, []byte(body))
			require.NotNil(t, usage)
			require.Equal(t, OpenAIUsage{InputTokens: 11, OutputTokens: 3, CacheReadInputTokens: 4}, *usage)
		}
	}
	for _, body := range []string{
		`{"error":` + oauthReliabilityModelError + `,"usage":{"input_tokens":0,"output_tokens":0}}`,
		`{"error":` + oauthReliabilityModelError + `,"usage":{"input_tokens":11},"usage":{"input_tokens":3}}`,
		`{"output":"quoted model_not_found","usage":` + oauthReliabilityModelUsage + `}`,
		`{"error":{"type":"invalid_request_error","param":"input","code":"unsupported_model"},"usage":` + oauthReliabilityModelUsage + `}`,
	} {
		require.Nil(t, openAIModelUnavailableUsage(502, []byte(body)))
	}
}

func TestOAuthReliabilityModelUnavailableTerminalHTTPWriters(t *testing.T) {
	for _, protocol := range []string{openAIModelErrorResponses, openAIModelErrorChat, openAIModelErrorMessages} {
		for _, status := range []int{400, 502} {
			for _, committed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/committed_%t", protocol, status, committed), func(t *testing.T) {
					path := map[string]string{openAIModelErrorResponses: "/v1/responses", openAIModelErrorChat: "/v1/chat/completions", openAIModelErrorMessages: "/v1/messages"}[protocol]
					c, rec := newOpenAIUpstreamErrorTestContext(t)
					c.Request = httptest.NewRequest(http.MethodPost, path, nil)
					if committed {
						c.Writer.Header().Set("Content-Type", "text/event-stream")
						c.Writer.WriteHeaderNow()
						MarkResponseCommitted(c)
					}
					body := `{"error":` + oauthReliabilityModelError + `,"usage":` + oauthReliabilityModelUsage + `}`
					resp := newOpenAIUpstreamErrorResponse(status, body)
					svc := &OpenAIGatewayService{}
					var result *OpenAIForwardResult
					var err error
					if protocol == openAIModelErrorResponses {
						result, err = svc.handleErrorResponse(context.Background(), resp, c, newOpenAIUpstreamErrorTestAccount(), nil, "gpt-5.2")
					} else if protocol == openAIModelErrorChat {
						result, err = svc.handleChatCompletionsErrorResponse(resp, c, newOpenAIUpstreamErrorTestAccount(), "gpt-5.2")
					} else {
						result, err = svc.handleAnthropicErrorResponse(resp, c, newOpenAIUpstreamErrorTestAccount(), "gpt-5.2")
					}
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover))
					require.NotNil(t, result)
					require.Equal(t, 11, result.Usage.InputTokens)
					require.Equal(t, 4, result.Usage.CacheReadInputTokens)
					require.Equal(t, 3, result.Usage.OutputTokens)
					require.Equal(t, status, c.GetInt(OpsUpstreamStatusCodeKey))
					require.Contains(t, rec.Body.String(), `"code":"model_not_found"`)
					require.Contains(t, rec.Body.String(), `"param":"model"`)
					require.NotContains(t, rec.Body.String(), "synthetic upstream detail")
					if committed {
						require.Equal(t, 200, rec.Code)
						stream, ok := GetOpsStreamError(c)
						require.True(t, ok)
						require.Equal(t, 400, stream.IntendedStatus)
					} else {
						require.Equal(t, 400, rec.Code)
					}
					before := rec.Body.String()
					writeOpenAIModelUnavailableTerminal(c, status, protocol)
					require.Equal(t, before, rec.Body.String(), "one terminal per turn")
				})
			}
		}
	}
}

func TestOAuthReliabilityModelUnavailableTerminalPublicForward(t *testing.T) {
	for _, protocol := range []string{openAIModelErrorResponses, openAIModelErrorChat, openAIModelErrorMessages} {
		for _, status := range []int{400, 502} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/stream_%t", protocol, status, stream), func(t *testing.T) {
					path := map[string]string{openAIModelErrorResponses: "/v1/responses", openAIModelErrorChat: "/v1/chat/completions", openAIModelErrorMessages: "/v1/messages"}[protocol]
					request := fmt.Sprintf(`{"model":"gpt-5.2","stream":%t,"input":"hi","messages":[{"role":"user","content":"hi"}],"max_tokens":32}`, stream)
					body := `{"type":"response.failed","response":{"status":"failed","id":"resp_partial","error":` + oauthReliabilityModelError + `,"usage":` + oauthReliabilityModelUsage + `}}`
					upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(status, body)}}
					svc := newOpenAIRejectedFieldTestService(upstream)
					account := newOpenAIOAuthNamespaceTestAccount()
					account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeOff
					c := newOpenAIRejectedFieldTestContext([]byte(request))
					c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(request))
					c.Request.Header.Set("Content-Type", "application/json")
					var result *OpenAIForwardResult
					var err error
					if protocol == openAIModelErrorResponses {
						result, err = svc.Forward(context.Background(), c, account, []byte(request))
					} else if protocol == openAIModelErrorChat {
						result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(request), "", "")
					} else {
						result, err = svc.ForwardAsAnthropic(context.Background(), c, account, []byte(request), "", "")
					}
					require.Error(t, err)
					require.NotNil(t, result)
					require.Len(t, upstream.bodies, 1)
					require.Equal(t, 11, result.Usage.InputTokens)
					require.Equal(t, 4, result.Usage.CacheReadInputTokens)
					require.Equal(t, 3, result.Usage.OutputTokens)
					require.Equal(t, "gpt-5.2", result.Model)
					require.Equal(t, stream, result.Stream)
					require.Equal(t, 400, c.Writer.Status())
					require.Equal(t, status, c.GetInt(OpsUpstreamStatusCodeKey))
				})
			}
		}
	}
}

func TestOAuthReliabilityModelUnavailableTerminalWSBridgeMetered(t *testing.T) {
	for _, status := range []int{400, 502} {
		body := `{"error":` + oauthReliabilityModelError + `,"usage":` + oauthReliabilityModelUsage + `}`
		upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(status, body)}}
		svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		c, _ := newOpenAIUpstreamErrorTestContext(t)
		payload := []byte(`{"type":"response.create","model":"gpt-5.2","instructions":"test","input":"hi"}`)
		var writes [][]byte
		result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, newOpenAIOAuthNamespaceTestAccount(), "synthetic", payload, len(payload), "gpt-5.2", "", "", "", "", 1, func(body []byte) error { writes = append(writes, bytes.Clone(body)); return nil })
		require.Error(t, err)
		var failover *UpstreamFailoverError
		require.False(t, errors.As(err, &failover))
		require.NotNil(t, result)
		require.Equal(t, 11, result.Usage.InputTokens)
		require.Equal(t, 4, result.Usage.CacheReadInputTokens)
		require.Equal(t, 3, result.Usage.OutputTokens)
		require.Len(t, upstream.bodies, 1)
		require.Len(t, writes, 1)
		require.Equal(t, "model", gjson.GetBytes(writes[0], "response.error.param").String())
		require.Equal(t, "model_not_found", gjson.GetBytes(writes[0], "response.error.code").String())
		require.Equal(t, status, c.GetInt(OpsUpstreamStatusCodeKey))
	}
}

func TestOAuthReliabilityModelUnavailableTerminalNativeWSMetered(t *testing.T) {
	for _, stream := range []bool{false, true} {
		cfg := newOpenAIWSExecutionScopeTestConfig()
		cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
		failed := []byte(`{"type":"response.failed","response":{"id":"resp_partial","status":"failed","output":[],"error":` + oauthReliabilityModelError + `,"usage":` + oauthReliabilityModelUsage + `}}`)
		capture := &openAIWSCaptureConn{events: [][]byte{failed}}
		pool := newOpenAIWSConnPool(cfg)
		pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: capture})
		defer pool.Close()
		upstream := &httpUpstreamRecorder{}
		svc := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool, httpUpstream: upstream, toolCorrector: NewCodexToolCorrector(), tiboRouteDisabled: true}
		account := newOpenAIOAuthNamespaceTestAccount()
		account.Extra["responses_websockets_v2_enabled"] = true
		account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.2","stream":%t,"input":"hi"}`, stream))
		c := newOpenAIWSExecutionScopeTestContext(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		result, err := svc.Forward(ctx, c, account, body)
		cancel()
		require.Error(t, err)
		var failover *UpstreamFailoverError
		require.False(t, errors.As(err, &failover))
		require.NotNil(t, result)
		require.Equal(t, 11, result.Usage.InputTokens)
		require.Equal(t, 4, result.Usage.CacheReadInputTokens)
		require.Equal(t, 3, result.Usage.OutputTokens)
		require.Empty(t, upstream.bodies)
		capture.mu.Lock()
		require.Len(t, capture.writes, 1)
		capture.mu.Unlock()
		if !stream {
			require.Equal(t, 400, c.Writer.Status())
		}
		streamErr, ok := GetOpsStreamError(c)
		require.True(t, ok)
		require.Equal(t, 400, streamErr.IntendedStatus)
	}
}

func TestOAuthReliabilityModelUnavailableTerminalOutboundMetadata(t *testing.T) {
	for _, protocol := range []string{openAIModelErrorResponses, openAIModelErrorChat, openAIModelErrorMessages} {
		t.Run(protocol, func(t *testing.T) {
			request := []byte(`{"model":"gpt-5.2","stream":false,"input":"hi","messages":[{"role":"user","content":"hi"}],"max_tokens":32,"service_tier":"priority","reasoning":{"effort":"high"},"reasoning_effort":"high"}`)
			body := `{"type":"response.failed","response":{"id":"resp_partial","model":"gpt-5.2","service_tier":"default","status":"failed","error":` + oauthReliabilityModelError + `,"usage":` + oauthReliabilityModelUsage + `}}`
			upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(502, body)}}
			svc := newOpenAIRejectedFieldTestService(upstream)
			account := newOpenAIRejectedFieldTestAccount()
			c := newOpenAIRejectedFieldTestContext(request)
			var result *OpenAIForwardResult
			var err error
			switch protocol {
			case openAIModelErrorResponses:
				result, err = svc.Forward(context.Background(), c, account, request)
			case openAIModelErrorChat:
				c.Request.URL.Path = "/v1/chat/completions"
				result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, request, "", "")
			case openAIModelErrorMessages:
				c.Request.URL.Path = "/v1/messages"
				result, err = svc.ForwardAsAnthropic(context.Background(), c, account, request, "", "")
			}
			require.Error(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, "gpt-5.2", result.UpstreamResponseModel)
			require.Equal(t, "default", result.UpstreamResponseServiceTier)
			tier := gjson.GetBytes(upstream.lastBody, "service_tier")
			if tier.Type == gjson.String && tier.String() != "" {
				require.NotNil(t, result.ServiceTier)
				require.Equal(t, tier.String(), *result.ServiceTier)
			} else {
				require.Nil(t, result.ServiceTier)
			}
			effort := gjson.GetBytes(upstream.lastBody, "reasoning.effort")
			if effort.Type == gjson.String && effort.String() != "" {
				require.NotNil(t, result.ReasoningEffort)
				require.Equal(t, effort.String(), *result.ReasoningEffort)
			}
		})
	}
}

func TestOAuthReliabilityModelUnavailableTerminalBridgeBareErrorUsage(t *testing.T) {
	wire := "data: " + `{"type":"error","error":` + oauthReliabilityModelError + `,"usage":` + oauthReliabilityModelUsage + `}` + "\n\n"
	response := newOpenAIRejectedFieldTestResponse(200, wire)
	response.Header.Set("Content-Type", "text/event-stream")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, _ := newOpenAIUpstreamErrorTestContext(t)
	payload := []byte(`{"type":"response.create","model":"gpt-5.2","instructions":"test","input":"hi"}`)
	var writes [][]byte
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, newOpenAIOAuthNamespaceTestAccount(), "synthetic", payload, len(payload), "gpt-5.2", "", "", "", "", 1, func(body []byte) error {
		writes = append(writes, bytes.Clone(body))
		return nil
	})
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.NotNil(t, result)
	require.Equal(t, 11, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Len(t, upstream.bodies, 1)
	require.Len(t, writes, 1)
	require.Equal(t, "response.failed", gjson.GetBytes(writes[0], "type").String())
	require.Equal(t, "model", gjson.GetBytes(writes[0], "response.error.param").String())
	require.Equal(t, "model_not_found", gjson.GetBytes(writes[0], "response.error.code").String())
	require.NotContains(t, string(writes[0]), "synthetic upstream detail")
	require.Equal(t, 200, c.GetInt(OpsUpstreamStatusCodeKey))
	streamErr, ok := GetOpsStreamError(c)
	require.True(t, ok)
	require.Equal(t, 400, streamErr.IntendedStatus)
}
