package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestExcelBPSModelSelection(t *testing.T) {
	a := excelAccount()
	require.True(t, a.IsExcelBPSEnabledForModel("gpt-6-sol"), "legacy all-model setting")
	a.Extra["openai_excel_bps_models"] = []any{"gpt-6-astra"}
	require.True(t, a.IsExcelBPSEnabledForModel("gpt-6-astra"))
	require.False(t, a.IsExcelBPSEnabledForModel("gpt-6-sol"))
	require.False(t, a.IsExcelBPSEnabledForModel("gpt-6-astra-other"))
	a.Credentials["model_mapping"] = map[string]any{"alias": "gpt-6-astra", "gpt-6-astra": "gpt-6-sol"}
	require.True(t, a.IsExcelBPSEnabledForModel("alias"))
	require.False(t, a.IsExcelBPSEnabledForModel("gpt-6-astra"), "selection matches mapped upstream")
	require.True(t, a.isExcelBPSUpstreamModelEnabled("gpt-6-astra"), "already mapped names must not map again")
	for _, models := range []any{[]any{}, []string{}, nil, "gpt-6-astra", []any{42, false}} {
		a.Extra["openai_excel_bps_models"] = models
		require.False(t, a.IsExcelBPSEnabledForModel("alias"))
		require.False(t, a.isExcelBPSAllModelsEnabled())
	}
	a.Extra["openai_excel_bps_models"] = []string{" gpt-6-astra "}
	require.True(t, a.IsExcelBPSEnabledForModel("alias"))
	a.Extra["openai_excel_bps"] = false
	require.False(t, a.IsExcelBPSEnabledForModel("alias"))
	var absent *Account
	require.False(t, absent.IsExcelBPSEnabledForModel("gpt-6-astra"))
}

func TestExcelBPSSelectedModelForwarding(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6-sol"} {
		t.Run(model, func(t *testing.T) {
			wire := fmt.Sprintf("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"model\":%q,\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", model)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}}
			svc := openAIClientToolsTestService(upstream)
			a := excelAccount()
			a.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			result, err := svc.Forward(context.Background(), c, a, []byte(fmt.Sprintf(`{"model":%q,"stream":true,"input":"test"}`, model)))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, upstream.lastReq)
			if model == "gpt-6-astra" {
				require.Equal(t, "bps.openai.com", upstream.lastReq.URL.Host)
				require.Equal(t, "/basispoints/api/responses", upstream.lastReq.URL.Path)
			} else {
				require.Equal(t, "chatgpt.com", upstream.lastReq.URL.Host)
				require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)
			}
		})
	}
}

func TestExcelBPSSelectedModelsPreserveCodexTransportAndTickets(t *testing.T) {
	a := excelAccount()
	a.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
	a.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
	a.Extra["openai_oauth_responses_websockets_v2_enabled"] = true
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	svc := &OpenAIGatewayService{cfg: cfg}
	require.False(t, a.IsOpenAIWSForceHTTPEnabled())
	require.True(t, a.IsOpenAIResponsesWebSocketV2Enabled())
	require.Equal(t, OpenAIWSIngressModeCtxPool, a.ResolveOpenAIResponsesWebSocketV2Mode("off"))
	for _, transport := range []OpenAIUpstreamTransport{OpenAIUpstreamTransportResponsesWebsocketV2, OpenAIUpstreamTransportResponsesWebsocketV2Ingress} {
		require.False(t, svc.isOpenAIAccountTransportCompatible(a, transport, "gpt-6-astra"))
		require.True(t, svc.isOpenAIAccountTransportCompatible(a, transport, "gpt-6-sol"))
	}
	require.True(t, svc.isOpenAIAccountTransportCompatible(a, OpenAIUpstreamTransportHTTPSSE, "gpt-6-astra"))
	require.True(t, isOpenAICodexTicketAccount(a))
	cfg.Gateway.OpenAICodexTicket.Enabled = true
	cfg.Gateway.OpenAICodexTicket.FailClosed = true
	cfg.Gateway.OpenAICodexTicket.Models = []string{"gpt-6-astra", "gpt-6-sol"}
	require.False(t, svc.openAICodexTicketBlocksAccount(a, svc.openAICodexTicketOutboundModel(a, "gpt-6-astra", false)))
	require.True(t, svc.openAICodexTicketBlocksAccount(a, svc.openAICodexTicketOutboundModel(a, "gpt-6-sol", false)))
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), a, "gpt-6-astra", http.Header{}))
	require.Error(t, svc.applyOpenAICodexTicket(context.Background(), a, "gpt-6-sol", http.Header{}))
	statuses := OpenAICodexTicketStatuses(a, cfg.Gateway.OpenAICodexTicket, time.Now())
	require.NotEmpty(t, statuses)
}

func TestExcelBPSSelectedCompactKeepsExcelModel(t *testing.T) {
	a := excelAccount()
	a.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
	a.Credentials["model_mapping"] = map[string]any{"alias": "gpt-6-astra"}
	a.Credentials["compact_model_mapping"] = map[string]any{"alias": "gpt-6-sol"}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	require.Equal(t, "gpt-6-astra", svc.openAICodexTicketOutboundModel(a, "alias", true))
	require.Equal(t, "gpt-6-astra", resolveOpenAIAccountUpstreamModelForRequest(a, "alias", true))
}
