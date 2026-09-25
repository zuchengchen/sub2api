package service

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestExcelBPSLive posts a real /v1/responses request through Forward using an
// in-memory OAuth account with BPS extra. It never writes the production
// database. Tokens are not logged.
func TestExcelBPSLive(t *testing.T) {
	file := os.Getenv("SUB2API_COOKIE_WS_LIVE_FIXTURE")
	if file == "" {
		t.Skip("set SUB2API_COOKIE_WS_LIVE_FIXTURE to opt into billed upstream verification")
	}
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var fixture cookieWSLiveFixture
	require.NoError(t, json.Unmarshal(data, &fixture))
	require.Equal(t, "brendon_quifgy@mail.com", fixture.Email)
	require.NotEmpty(t, fixture.AccessToken)

	gin.SetMode(gin.TestMode)
	upstream := &excelBPSLiveHTTP{harvest: strings.TrimSpace(fixture.HarvestProxyURL)}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := ticketTestAccount(fixture.ID)
	account.Name = fixture.Email
	account.Credentials = map[string]any{"access_token": fixture.AccessToken, "chatgpt_account_id": fixture.ChatGPTAccountID}
	account.Extra = map[string]any{"openai_excel_bps": true}
	account.Concurrency = 4

	body := []byte(`{"model":"gpt-6-astra","stream":true,"store":false,"reasoning":{"effort":"low"},"input":[{"role":"user","content":[{"type":"input_text","text":"Reply with exactly 21"}]}]}`)
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	c.Set("api_key", &APIKey{ID: 910001, UserID: 910001})

	result, err := svc.Forward(ctx, c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	req := upstream.last.Load()
	require.NotNil(t, req)
	require.Equal(t, "bps.openai.com", req.URL.Host)
	require.Equal(t, "/basispoints/api/responses", req.URL.Path)
	require.Equal(t, basispoints.ResponsesURL, req.URL.String())
	require.Equal(t, "text/event-stream", req.Header.Get("Accept"))
	require.Empty(t, req.Header.Get("x-codex-turn-state"))
	require.False(t, result.OpenAIWSMode)
	require.Contains(t, rec.Body.String(), "response.completed")
	require.NotContains(t, rec.Body.String(), fixture.AccessToken)
}

type excelBPSLiveHTTP struct {
	harvest string
	last    atomic.Pointer[http.Request]
}

func (h *excelBPSLiveHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if req == nil || req.URL == nil || req.URL.Host != "bps.openai.com" || req.URL.Path != "/basispoints/api/responses" {
		return nil, errors.New("live BPS test refuses non-BPS HTTP")
	}
	h.last.Store(req.Clone(req.Context()))
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout: 20 * time.Second,
	}
	if harvest := strings.TrimSpace(h.harvest); harvest != "" {
		u, err := url.Parse(harvest)
		if err != nil {
			return nil, errors.New("invalid private harvest proxy")
		}
		if err := proxyutil.ConfigureTransportProxy(transport, u); err != nil {
			return nil, errors.New("proxy transport configuration failed")
		}
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Minute}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("private BPS HTTP transport failed")
	}
	return response, nil
}

func (h *excelBPSLiveHTTP) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(req, proxy, accountID, concurrency)
}

func TestExcelBPSImageRelayOffDoesNotRewriteDataURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	wire := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_off\",\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[]}}\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}}
	svc := openAIClientToolsTestService(upstream)
	body := []byte(`{"model":"gpt-6-astra","stream":false,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := svc.Forward(context.Background(), c, excelAccount(), body)
	require.NoError(t, err)
	require.Equal(t, "/basispoints/api/responses", upstream.lastReq.URL.Path)
	require.False(t, gjson.GetBytes(upstream.lastBody, "input").Raw == "")
}
