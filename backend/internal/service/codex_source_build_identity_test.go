package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const codexSourceTestBootstrapUA = "codex_cli_rs/0.0.0 (Ubuntu 26.4.0; x86_64) xterm-256color"
const codexSourceTestUA = codexSourceTestBootstrapUA + " (codex_cli_rs; 0.0.0)"

func codexSourceTestState(t *testing.T) {
	t.Helper()
	codexCanonicalUAMu.RLock()
	previousResolver := codexCanonicalUAResolver
	codexCanonicalUAMu.RUnlock()
	previousEnforcement := codexIdentityEnforcement.Load()
	codexSessionIdentities.Lock()
	previousEntries := codexSessionIdentities.entries
	codexSessionIdentities.entries = make(map[string]codexSessionIdentitySnapshot)
	codexSessionIdentities.Unlock()
	SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent("0.166.0") })
	SetCodexIdentityEnforcementEnabled(true)
	t.Cleanup(func() {
		SetCodexCanonicalUserAgentResolver(previousResolver)
		SetCodexIdentityEnforcementEnabled(previousEnforcement)
		codexSessionIdentities.Lock()
		codexSessionIdentities.entries = previousEntries
		codexSessionIdentities.Unlock()
	})
}

func codexSourceTestContext(ua, originator, version string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", ua)
	c.Request.Header.Set("Originator", originator)
	if version != "" {
		c.Request.Header.Set("Version", version)
	}
	c.Request.Header.Set("Session-Id", "source-build-session")
	c.Request.Header.Set("Thread-Id", "source-build-thread")
	c.Set("api_key", &APIKey{ID: 6789, UserID: 6788})
	return c
}

func TestCodexSourceBuildIdentityRequiresCompleteOfficialShape(t *testing.T) {
	for _, tc := range []struct {
		name, ua, origin string
		want             bool
	}{
		{"bootstrap", codexSourceTestBootstrapUA, "codex_cli_rs", true},
		{"initialized", codexSourceTestUA, "codex_cli_rs", true},
		{"official_tui_trailer", codexSourceTestBootstrapUA + " (codex-tui; 0.0.0)", "codex_cli_rs", true},
		{"missing_platform", "codex_cli_rs/0.0.0", "codex_cli_rs", false},
		{"origin_only", "curl/0.0.0", "codex_cli_rs", false},
		{"version_only", "other/0.0.0 (Ubuntu 26.4.0; x86_64) xterm", "other", false},
		{"embedded_codex", "Mozilla " + codexSourceTestUA, "codex_cli_rs", false},
		{"missing_origin", codexSourceTestUA, "", false},
		{"mismatched_origin", codexSourceTestUA, "codex_exec", false},
		{"unofficial_trailer", codexSourceTestBootstrapUA + " (other; 0.0.0)", "codex_cli_rs", false},
		{"mismatched_trailer_version", codexSourceTestBootstrapUA + " (codex_cli_rs; 0.166.0)", "codex_cli_rs", false},
		{"old_stable", strings.ReplaceAll(codexSourceTestUA, "0.0.0", "0.137.0"), "codex_cli_rs", false},
		{"other_zero_version", strings.ReplaceAll(codexSourceTestUA, "0.0.0", "0.0.1"), "codex_cli_rs", false},
		{"injection", codexSourceTestUA + "\r\nX: 1", "codex_cli_rs", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity, ok := codexSourceBuildIdentity(tc.ua, tc.origin)
			require.Equal(t, tc.want, ok)
			if ok {
				require.Equal(t, tc.ua, identity.userAgent)
				require.Equal(t, tc.origin, identity.originator)
				require.Equal(t, "0.0.0", identity.version)
			}
		})
	}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	headers := codexSourceTestContext(codexSourceTestUA, "codex_cli_rs", "0.0.0").Request.Header
	_, ok := codexSourceBuildIdentityFromHeaders(headers, account)
	require.True(t, ok)
	for _, other := range []*Account{nil, {Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, {Platform: PlatformOpenAI, Type: AccountTypeSetupToken}, {Platform: PlatformGrok, Type: AccountTypeOAuth}} {
		_, ok = codexSourceBuildIdentityFromHeaders(headers, other)
		require.False(t, ok)
	}
	headers.Add("Version", "0.0.0")
	_, ok = codexSourceBuildIdentityFromHeaders(headers, account)
	require.False(t, ok, "duplicate version cannot establish a single identity")
	headers.Set("Version", "0.166.0")
	_, ok = codexSourceBuildIdentityFromHeaders(headers, account)
	require.False(t, ok, "a conflicting version header is not the observed source identity")
}

func TestCodexSourceBuildSessionDoesNotChangeCanonicalOrStablePolicy(t *testing.T) {
	codexSourceTestState(t)
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 678, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "synthetic-source-account"}}
	ordinary := codexSourceTestContext("codex_cli_rs/0.137.0 (Ubuntu 26.4.0; x86_64) xterm-256color", "codex_cli_rs", "0.137.0")
	require.Equal(t, "0.166.0", svc.codexSessionClientVersion(ordinary, account, nil))
	source := codexSourceTestContext(codexSourceTestUA, "codex_cli_rs", "0.0.0")
	require.Equal(t, "0.0.0", svc.codexSessionClientVersion(source, account, nil))
	profile, _ := svc.codexWireProfileForRequest(source, account, nil)
	wantProfile, _ := resolveCodexWireProfile("0.166.0", codexWireProfileModeAuto)
	require.Equal(t, wantProfile.Name, profile.Name, "source version must not silently downgrade the configured wire profile")
	require.Equal(t, "0.166.0", svc.codexSessionClientVersion(codexSourceTestContext("curl/0.0.0", "codex_cli_rs", "0.0.0"), account, nil))
	require.Equal(t, "0.166.0", svc.codexSessionClientVersion(codexSourceTestContext("codex_cli_rs/0.137.0 (Ubuntu 26.4.0; x86_64) xterm-256color", "codex_cli_rs", "0.137.0"), account, nil))
	require.Equal(t, "0.166.0", CodexCanonicalClientVersion())
	require.Equal(t, codexCLIVersion, codexClientVersionFromUA(codexSourceTestUA), "global minimum still treats unbound zero version as invalid")
	SetCodexCanonicalUserAgentResolver(func() string { return codexSourceTestUA })
	require.Equal(t, codexCLIVersion, CodexCanonicalClientVersion(), "no global source-build exception")
}

func TestCodexSourceBuildModelsHTTPAndWSKeepActualIdentity(t *testing.T) {
	codexSourceTestState(t)
	type observed struct {
		path, query, method string
		headers             http.Header
	}
	observedRequests := make(chan observed, 12)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedRequests <- observed{r.URL.Path, r.URL.RawQuery, r.Method, r.Header.Clone()}
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			connection, err := coderws.Accept(w, r, nil)
			if err == nil {
				_ = connection.CloseNow()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6-astra"}]}`)
	}))
	defer server.Close()
	previousURL := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL + "/backend-api/codex/models"
	t.Cleanup(func() { chatgptCodexModelsURL = previousURL })
	svc := &OpenAIGatewayService{cfg: codexProbeConfig(nil), tiboRouteDisabled: true}
	account := &Account{ID: 679, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "synthetic-access", "chatgpt_account_id": "synthetic-account"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	check := func(wantUA string) observed {
		t.Helper()
		select {
		case value := <-observedRequests:
			require.Equal(t, wantUA, value.headers.Get("User-Agent"))
			require.Equal(t, "codex_cli_rs", value.headers.Get("Originator"))
			require.Equal(t, "0.0.0", value.headers.Get("Version"))
			return value
		case <-ctx.Done():
			t.Fatal("synthetic local request was not observed")
			return observed{}
		}
	}
	for _, ua := range []string{codexSourceTestBootstrapUA, codexSourceTestUA} {
		modelsCtx := WithCodexModelsClientContext(ctx, ua, "codex_cli_rs")
		_, err := svc.FetchCodexModelsManifest(modelsCtx, account, "0.0.0", "")
		require.NoError(t, err)
		value := check(ua)
		require.Equal(t, "client_version=0.0.0", value.query)
	}
	baseURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	body := []byte(`{"model":"gpt-6-astra","stream":true,"input":[{"role":"user","content":"synthetic"}]}`)
	send := func(req *http.Request) {
		t.Helper()
		local := req.Clone(ctx)
		copyURL := *req.URL
		copyURL.Scheme, copyURL.Host = baseURL.Scheme, baseURL.Host
		local.URL = &copyURL
		response, err := http.DefaultClient.Do(local)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		check(codexSourceTestUA)
	}
	for _, enforcement := range []bool{true, false} {
		SetCodexIdentityEnforcementEnabled(enforcement)
		c := codexSourceTestContext(codexSourceTestUA, "codex_cli_rs", "0.0.0")
		req, err := svc.buildUpstreamRequest(ctx, c, account, bytes.Clone(body), "synthetic-access", true, "", true)
		require.NoError(t, err)
		send(req)
		req, err = svc.buildUpstreamRequestOpenAIPassthrough(ctx, codexSourceTestContext(codexSourceTestUA, "codex_cli_rs", "0.0.0"), account, bytes.Clone(body), "synthetic-access")
		require.NoError(t, err)
		send(req)
		headers, _, err := svc.buildOpenAIWSHeaders(ctx, codexSourceTestContext(codexSourceTestUA, "codex_cli_rs", "0.0.0"), account,
			"synthetic-access", OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, true, "", "", "", "gpt-6-astra", "")
		require.NoError(t, err)
		connection, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/backend-api/codex/responses", &coderws.DialOptions{HTTPHeader: headers})
		require.NoError(t, err)
		require.NoError(t, connection.CloseNow())
		value := check(codexSourceTestUA)
		require.Equal(t, "websocket", value.headers.Get("Upgrade"))
	}
}

func TestCodexSourceBuildModelsRejectUnboundZeroAndKeepStableMinimum(t *testing.T) {
	codexSourceTestState(t)
	for _, tc := range []struct{ name, ua, origin, query string }{
		{"version_only", "curl/0.0.0", "codex_cli_rs", "0.0.0"},
		{"missing_origin", codexSourceTestUA, "", "0.0.0"},
		{"conflicting_query", codexSourceTestUA, "codex_cli_rs", "0.137.0"},
		{"old_stable", strings.ReplaceAll(codexSourceTestUA, "0.0.0", "0.137.0"), "codex_cli_rs", "0.137.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"gpt-6-astra"}]}`))}}
			svc := &OpenAIGatewayService{cfg: codexProbeConfig(nil), httpUpstream: upstream}
			_, err := svc.FetchCodexModelsManifest(WithCodexModelsClientContext(context.Background(), tc.ua, tc.origin), directImagesTestAccount(), tc.query, "")
			require.NoError(t, err)
			require.Equal(t, "0.166.0", upstream.lastReq.Header.Get("Version"))
			require.NotContains(t, upstream.lastReq.Header.Get("User-Agent"), "/0.0.0")
			require.Equal(t, tc.query, upstream.lastReq.URL.Query().Get("client_version"), "content negotiation is still passed through")
		})
	}
}
