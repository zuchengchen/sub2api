//go:build linux

package main

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fixtureProfileCapture struct {
	calls   int
	profile *tlsfingerprint.Profile
	body    string
}

func (f *fixtureProfileCapture) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return f.DoWithTLS(req, "", 0, 0, nil)
}
func (f *fixtureProfileCapture) DoWithTLS(req *http.Request, _ string, _ int64, _ int, p *tlsfingerprint.Profile) (*http.Response, error) {
	f.calls++
	f.profile = p
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	f.body = string(raw)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture_policy\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))}, nil
}
func TestGatewayFixtureUsesProductionModelPolicyAndTLSSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cert := certServer.Certificate()
	certServer.Close()
	caPath := filepath.Join(t.TempDir(), "fixture.pem")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600))
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "custom_ca"}[custom], func(t *testing.T) {
			t.Setenv("CODEX_CA_CERTIFICATE", "")
			t.Setenv("SSL_CERT_FILE", "")
			if custom {
				t.Setenv("CODEX_CA_CERTIFICATE", caPath)
			}
			for _, eligible := range []bool{false, true} {
				t.Run(map[bool]string{false: "missing_health", true: "explicit_health_snapshot"}[eligible], func(t *testing.T) {
					capture := &fixtureProfileCapture{}
					cfg := gatewayScenarioConfig()
					require.False(t, cfg.Gateway.OpenAIWS.CodexHTTPH2Enabled)
					account := &service.Account{ID: 901, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"access_token": "synthetic", "chatgpt_account_id": "acct-fixture"}, Extra: map[string]any{"openai_passthrough": true}}
					if eligible {
						account.Extra = gatewaySyntheticModelPolicyExtra(account.Extra)
					}
					svc := service.NewOpenAIGatewayService(&gatewayAccounts{account: account}, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, capture, nil, nil, nil, nil, nil, nil, nil, nil)
					svc.StopOpenAICodexTicketHarvester()
					defer svc.CloseOpenAIWSPool()
					require.True(t, account.AllowsOpenAIOAuthModelPolicy("gpt-6-astra"))
					body := []byte(`{"model":"gpt-6-astra","instructions":"Reply briefly.","input":[{"role":"user","content":"ok"}],"stream":true,"store":false}`)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
					c.Request.Header.Set("User-Agent", "codex_cli_rs/0.166.0")
					_, err := svc.Forward(context.Background(), c, account, body)
					require.NoError(t, err)
					require.Equal(t, 1, capture.calls)
					require.NotNil(t, capture.profile)
					if custom {
						require.Equal(t, tlsfingerprint.BuiltinProfile("rustls-h2").Name, capture.profile.Name)
						require.True(t, capture.profile.H2)
					} else {
						require.Equal(t, tlsfingerprint.BuiltinProfile("openssl36").Name, capture.profile.Name)
						require.True(t, capture.profile.H1)
					}
					require.Equal(t, "gpt-6-astra", account.GetMappedModel("gpt-6-astra"))
				})
			}
		})
	}
}
