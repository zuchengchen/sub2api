package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func decodeCodexProbeTestBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	rc, err := req.GetBody()
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	require.NoError(t, err)
	if req.Header.Get("Content-Encoding") == "zstd" {
		decoder, err := zstd.NewReader(nil)
		require.NoError(t, err)
		defer decoder.Close()
		raw, err = decoder.DecodeAll(raw, nil)
		require.NoError(t, err)
	}
	require.True(t, json.Valid(raw))
	return raw
}

type codexProbeUpstream struct {
	httpUpstreamRecorder
	profiles []*tlsfingerprint.Profile
	plain    int
}

func (u *codexProbeUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	u.plain++
	return u.httpUpstreamRecorder.Do(req, proxy, id, concurrency)
}

func (u *codexProbeUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.profiles = append(u.profiles, profile)
	return u.httpUpstreamRecorder.Do(req, proxy, id, concurrency)
}

func newCodexProbeTestAccount() *Account {
	return &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "probe-account"},
		Extra:       map[string]any{codexFingerprintSeedExtraKey: testCodexFingerprintSeed, codexClientFamilyExtraKey: "exec"},
	}
}

func requireCodexProbeWire(t *testing.T, upstream *codexProbeUpstream) []byte {
	t.Helper()
	req := upstream.lastReq
	require.NotNil(t, req)
	require.Zero(t, upstream.plain, "OAuth diagnostics must use the Codex transport")
	require.NotEmpty(t, upstream.profiles)
	profile := upstream.profiles[len(upstream.profiles)-1]
	require.NotNil(t, profile)
	require.True(t, profile.H1)
	require.Contains(t, profile.Name, "OpenSSL")
	require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
	require.Equal(t, int64(len(upstream.lastBody)), req.ContentLength)
	replay, err := req.GetBody()
	require.NoError(t, err)
	raw, err := io.ReadAll(replay)
	require.NoError(t, replay.Close())
	require.NoError(t, err)
	require.Equal(t, upstream.lastBody, raw, "replay must preserve the exact compressed wire body")
	require.True(t, strings.HasPrefix(req.UserAgent(), "codex_exec/"))
	require.Equal(t, "codex_exec", req.Header.Get("originator"))
	for _, header := range []string{"OpenAI-Beta", "session_id", "conversation_id", "x-codex-installation-id", "accept-language"} {
		require.Empty(t, req.Header.Get(header), header)
	}
	body := decodeCodexProbeTestBody(t, req)
	require.False(t, gjson.GetBytes(body, "instructions").Exists())
	require.True(t, gjson.GetBytes(body, "stream").Bool())
	require.False(t, gjson.GetBytes(body, "store").Bool())
	require.Equal(t, "developer", gjson.GetBytes(body, "input.0.role").String())
	require.True(t, codexInputCarriesBaseInstructions(body))
	session := req.Header.Get("session-id")
	parsed, err := uuid.Parse(session)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsed.Version())
	require.Equal(t, session, req.Header.Get("thread-id"))
	require.Equal(t, session, req.Header.Get("x-client-request-id"))
	require.Equal(t, session, gjson.GetBytes(body, "client_metadata.session_id").String())
	require.Equal(t, session, gjson.GetBytes(body, "prompt_cache_key").String())
	require.NotEmpty(t, gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String())
	require.False(t, gjson.GetBytes(body, "client_metadata.x-codex-ws-stream-request-start-ms").Exists())
	return body
}

func TestCodexProbeTiboUsesSharedHTTPWire(t *testing.T) {
	account := newCodexProbeTestAccount()
	policy := DefaultAccountTrafficPolicy()
	policy.StrictRPMEnabled = true
	policy.RPM = 1
	policy.Burst = 1
	account.Extra[AccountTrafficPolicyKey] = policy
	upstream := &codexProbeUpstream{httpUpstreamRecorder: httpUpstreamRecorder{responses: []*http.Response{cookieWSHTTPResponse("True"), cookieWSHTTPResponse("False")}}}
	svc := &OpenAIGatewayService{cfg: codexProbeConfig(nil), httpUpstream: upstream}
	require.True(t, svc.openAITiboRouteApplies(account))
	require.Equal(t, openAITiboHealthy, svc.probeOpenAITiboHTTP(context.Background(), account).verdict)
	first := requireCodexProbeWire(t, upstream)
	require.Nil(t, upstream.lastReq.Context().Value(accountTrafficPlanKey{}), "background health samples retain their existing admission policy")
	require.Contains(t, string(first), openAITiboProbePrompt)
	require.Equal(t, "low", gjson.GetBytes(first, "reasoning.effort").String())
	require.Equal(t, openAITiboDegraded, svc.probeOpenAITiboHTTP(context.Background(), account).verdict)
	second := requireCodexProbeWire(t, upstream)
	require.Equal(t, gjson.GetBytes(first, "client_metadata.x-codex-installation-id").String(), gjson.GetBytes(second, "client_metadata.x-codex-installation-id").String())
	require.NotEqual(t, gjson.GetBytes(first, "client_metadata.session_id").String(), gjson.GetBytes(second, "client_metadata.session_id").String())
	require.NotEqual(t, gjson.GetBytes(first, "client_metadata.turn_id").String(), gjson.GetBytes(second, "client_metadata.turn_id").String())
}

func TestCodexProbeAdminModesUseSharedHTTPWire(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"text", "compact", "image"} {
		t.Run(mode, func(t *testing.T) {
			upstream := &codexProbeUpstream{httpUpstreamRecorder: httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(compactProbeSSESuccessBody)),
			}}}
			svc := &AccountTestService{httpUpstream: upstream}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/admin/test", nil)
			account := newCodexProbeTestAccount()
			policy := DefaultAccountTrafficPolicy()
			policy.StrictRPMEnabled = true
			policy.RPM = 1
			policy.Burst = 1
			account.Extra[AccountTrafficPolicyKey] = policy
			switch mode {
			case "compact":
				require.NoError(t, svc.testOpenAICompactConnection(c, account, "gpt-5.5"))
			case "image":
				upstream.resp.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aGVsbG8=\"}]}}\n\n"))
				require.NoError(t, svc.testOpenAIImageOAuth(c, context.Background(), account, "gpt-image-1", "draw a cat"))
			default:
				require.NoError(t, svc.testOpenAIAccountConnection(c, account, "gpt-5.5", "", ""))
			}
			body := requireCodexProbeWire(t, upstream)
			require.NotNil(t, upstream.lastReq.Context().Value(accountTrafficPlanKey{}), "manual tests retain their traffic admission policy")
			if mode == "compact" {
				items := gjson.GetBytes(body, "input").Array()
				require.Equal(t, "compaction_trigger", items[len(items)-1].Get("type").String())
				require.Equal(t, "compaction", gjson.Get(upstream.lastReq.Header.Get(openAIWSTurnMetadataHeader), "request_kind").String())
			}
			if mode == "image" {
				require.True(t, openAIRequestBodyHasImageGenerationDeclaration(body))
			}
		})
	}
}

func TestCodexProbeRecoveryAndUsageUseSharedHTTPWire(t *testing.T) {
	for _, kind := range []string{"recovery", "usage"} {
		t.Run(kind, func(t *testing.T) {
			account := newCodexProbeTestAccount()
			upstream := &codexProbeUpstream{httpUpstreamRecorder: httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}}}
			gateway := &OpenAIGatewayService{cfg: codexProbeConfig(nil), httpUpstream: upstream}
			if kind == "recovery" {
				svc := &RateLimitService{codexProbeSender: gateway}
				result := svc.probeOpenAI429RecoveryHTTP(context.Background(), account, "gpt-5.5")
				require.NoError(t, result.Err)
				require.Equal(t, http.StatusOK, result.Status)
			} else {
				svc := &AccountUsageService{codexProbeSender: gateway}
				_, err := svc.probeOpenAICodexSnapshotLegacy(context.Background(), account)
				require.NoError(t, err)
			}
			requireCodexProbeWire(t, upstream)
		})
	}
}

func TestCodexProbeRetryKeepsPreparedBodyAndIdentity(t *testing.T) {
	account := newCodexProbeTestAccount()
	upstream := &codexProbeUpstream{httpUpstreamRecorder: httpUpstreamRecorder{responses: []*http.Response{cookieWSHTTPResponse("True"), cookieWSHTTPResponse("True")}}}
	svc := &OpenAIGatewayService{cfg: codexProbeConfig(nil), httpUpstream: upstream}
	raw, err := json.Marshal(openAITiboProbePayload())
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, bytes.NewReader(raw))
	require.NoError(t, err)
	resp, err := svc.doOpenAICodexProbe(req, "", account)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	first := append([]byte(nil), upstream.lastBody...)
	retry := req.Clone(req.Context())
	retry.Body, err = req.GetBody()
	require.NoError(t, err)
	resp, err = svc.doOpenAICodexProbe(retry, "", account)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, first, upstream.lastBody)
	require.Equal(t, req.Header.Get("session-id"), retry.Header.Get("session-id"))
}

func TestCodexProbeUsesCredentialOwnerTLSWithoutChangingAdmission(t *testing.T) {
	parent := newCodexProbeTestAccount()
	parent.Extra["codex_tls_profile"] = "rustls-h2"
	shadow := &Account{ID: 43, ParentAccountID: &parent.ID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1}
	repo := &stubOpenAIAccountRepo{accounts: []Account{*parent, *shadow}}
	upstream := &codexProbeUpstream{httpUpstreamRecorder: httpUpstreamRecorder{resp: cookieWSHTTPResponse("True")}}
	svc := &OpenAIGatewayService{cfg: codexProbeConfig(nil), accountRepo: repo, httpUpstream: upstream}
	req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, strings.NewReader(`{"model":"gpt-5.3-codex-spark","input":[]}`))
	require.NoError(t, err)
	resp, err := svc.doOpenAICodexProbe(req, "", shadow)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Len(t, upstream.profiles, 1)
	require.True(t, upstream.profiles[0].H2)
	body := decodeCodexProbeTestBody(t, req)
	require.False(t, gjson.GetBytes(body, "instructions").Exists())
	require.Contains(t, string(body), codexSparkImageUnsupportedMarker)
	require.Nil(t, req.Context().Value(accountTrafficPlanKey{}))
}

func TestCodexProbeStandaloneSenderDoesNotFallback(t *testing.T) {
	account := newCodexProbeTestAccount()
	result := (&RateLimitService{}).probeOpenAI429RecoveryHTTP(context.Background(), account, "gpt-5.5")
	require.ErrorContains(t, result.Err, "transport is unavailable")
	require.True(t, result.Stop)
	_, err := (&AccountUsageService{}).probeOpenAICodexSnapshotLegacy(context.Background(), account)
	require.ErrorContains(t, err, "transport is unavailable")
}

func TestCodexProbeAPIKeyDoesNotReceiveOAuthContract(t *testing.T) {
	account := &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}
	raw := `{"model":"model","input":"hello","stream":true}`
	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(raw))
	require.NoError(t, err)
	upstream := &codexProbeUpstream{httpUpstreamRecorder: httpUpstreamRecorder{resp: cookieWSHTTPResponse("True")}}
	svc := &OpenAIGatewayService{cfg: codexProbeConfig(nil), httpUpstream: upstream}
	resp, err := svc.doOpenAICodexProbe(req, "", account)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, raw, string(upstream.lastBody))
	require.Empty(t, req.Header.Get("Content-Encoding"))
	require.Empty(t, req.Header.Get("chatgpt-account-id"))
	require.Equal(t, 1, upstream.plain)
}

func TestCodexProbeDirectImageRetainsEndpointContract(t *testing.T) {
	account := newCodexProbeTestAccount()
	raw := `{"model":"gpt-image-2.5-sunburst","prompt":"draw a cat"}`
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/images/generations", strings.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json")
	upstream := &codexProbeUpstream{httpUpstreamRecorder: httpUpstreamRecorder{resp: cookieWSHTTPResponse("True")}}
	svc := &OpenAIGatewayService{cfg: codexProbeConfig(nil), httpUpstream: upstream}
	resp, err := svc.doOpenAICodexProbe(req, "", account)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, raw, string(upstream.lastBody))
	require.Equal(t, "application/json", req.Header.Get("Accept"))
	require.Empty(t, req.Header.Get("Content-Encoding"))
	require.Equal(t, "codex_exec", req.Header.Get("originator"))
	require.Len(t, upstream.profiles, 1)
	require.True(t, upstream.profiles[0].H1)
}

func TestCodexProbePreservesIntelligentCaptureAndExplicitTransportConfig(t *testing.T) {
	account := newCodexProbeTestAccount()
	upstream := &codexProbeUpstream{httpUpstreamRecorder: httpUpstreamRecorder{resp: cookieWSHTTPResponse("True")}}
	capture := &intelligentCapture{}
	svc := &AccountTestService{
		cfg:          &config.Config{}, // An explicit rollback setting is distinct from missing configuration.
		httpUpstream: &intelligentHTTPUpstream{inner: upstream, capture: capture},
	}
	req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, strings.NewReader(`{"model":"gpt-5.5","input":[{"role":"user","content":"keep this test prompt"}]}`))
	require.NoError(t, err)
	req.Header.Set("conversation_id", "legacy-intelligent-session")
	resp, err := svc.doOpenAIAccountTestUpstream(req, "", account, true)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, upstream.plain)
	require.Empty(t, req.Header.Get("Content-Encoding"))
	require.Empty(t, req.Header.Get("conversation_id"))
	require.Contains(t, string(decodeCodexProbeTestBody(t, req)), "keep this test prompt")
	require.Equal(t, http.StatusOK, capture.status)
	require.Equal(t, "True", strings.TrimSpace(capture.outputText()))
}
