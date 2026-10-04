package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrismBrowserResponsesURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{name: "v1 base", base: "http://127.0.0.1:8319/v1", want: "http://127.0.0.1:8319/v1/responses"},
		{name: "responses suffix", base: "http://127.0.0.1:8319/v1/responses/", want: "http://127.0.0.1:8319/v1/responses"},
		{name: "empty", base: " ", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, prismBrowserResponsesURL(tt.base))
		})
	}
}

func TestPrismBrowserForwardErrorKeepsKnownCodeWithoutReflectingSecrets(t *testing.T) {
	err := prismBrowserForwardError(422, []byte(`{"error":{"type":"tools_disabled","message":"private input fixture"}}`))
	require.EqualError(t, err, "prism adapter returned HTTP 422 (tools_disabled)")
	err = prismBrowserForwardError(422, []byte(`{"error":{"type":"private input fixture","message":"private input fixture"}}`))
	require.EqualError(t, err, "prism adapter returned HTTP 422")
}

func TestPrismBrowserSessionIDUsesExistingCodexIdentity(t *testing.T) {
	identity := func(keyID, accountID int64, headers map[string]string, body string) (string, error) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Set("api_key", &APIKey{ID: keyID})
		for k, v := range headers {
			c.Request.Header.Set(k, v)
		}
		return prismBrowserSessionID(c, accountID, []byte(body))
	}
	first, err := identity(7, 42, map[string]string{"session-id": "fixture"}, "{}")
	require.NoError(t, err)
	require.Len(t, first, 64)
	for _, tc := range []struct {
		name             string
		keyID, accountID int64
		headers          map[string]string
		body             string
		same             bool
	}{
		{"same session header alias", 7, 42, map[string]string{"session_id": "fixture"}, "{}", true},
		{"same session body", 7, 42, nil, `{"client_metadata":{"session_id":"fixture"}}`, true},
		{"private header cannot override", 7, 42, map[string]string{"session-id": "fixture", "X-Prism-Session-ID": "forged"}, "{}", true},
		{"other key", 8, 42, map[string]string{"session-id": "fixture"}, "{}", false},
		{"other account", 7, 43, map[string]string{"session-id": "fixture"}, "{}", false},
		{"other session", 7, 42, map[string]string{"session-id": "different"}, "{}", false},
		{"thread wins over shared session", 7, 42, map[string]string{"session-id": "fixture"}, `{"client_metadata":{"thread_id":"thread-a"}}`, false},
		{"thread namespace differs", 7, 42, map[string]string{"conversation_id": "fixture"}, "{}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := identity(tc.keyID, tc.accountID, tc.headers, tc.body)
			require.NoError(t, err)
			require.NotEmpty(t, got)
			require.Equal(t, tc.same, first == got)
		})
	}
	_, err = identity(0, 42, map[string]string{"session-id": "fixture"}, "{}")
	require.Error(t, err)
}

func TestPrismBrowserSessionIDRejectsAmbiguousIdentity(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 7})
	c.Request.Header.Add("session_id", "one")
	c.Request.Header.Add("session_id", "two")
	_, err := prismBrowserSessionID(c, 42, nil)
	require.Error(t, err)
	c.Request.Header.Del("session_id")
	c.Request.Header.Set("session-id", "one")
	c.Request.Header.Set("session_id", "two")
	_, err = prismBrowserSessionID(c, 42, nil)
	require.Error(t, err)
	c.Request.Header.Del("session_id")
	_, err = prismBrowserSessionID(c, 42, []byte(`{"client_metadata":{"session_id":"conflict"}}`))
	require.Error(t, err)
}

func prismTestService(endpoint string) (*OpenAIGatewayService, *Account) {
	cfg := &config.Config{}
	cfg.Gateway.PrismBrowser = config.GatewayPrismBrowserConfig{Enabled: true, BaseURL: endpoint + "/v1", APIKey: "fixture-bridge-key"}
	return &OpenAIGatewayService{cfg: cfg}, &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "fixture-oauth"}}
}

const prismTerminal = `{"id":"resp_fixture","status":"completed","model":"gpt-6.1-sol","usage":null,"output":[{"type":"message","content":[{"type":"output_text","text":"21"}]}]}`

func TestPrismBrowserSessionForwardAndStatelessAdminTest(t *testing.T) {
	const body = `{"model":"gpt-6.1-sol","input":"fixture"}`
	requests := make(chan string, 2)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		calls++
		if gjson.GetBytes(got, "model").String() != "gpt-6.1-sol" {
			t.Errorf("model = %s", gjson.GetBytes(got, "model").String())
		}
		if calls == 1 && gjson.GetBytes(got, "reasoning.effort").String() != "medium" {
			t.Errorf("effort = %s", gjson.GetBytes(got, "reasoning.effort").String())
		}
		requests <- r.Header.Get("X-Prism-Session-ID")
		_, _ = io.WriteString(w, prismTerminal)
	}))
	defer server.Close()
	s, account := prismTestService(server.URL)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("session-id", "fixture-session")
	c.Request.Header.Set("X-Prism-Session-ID", "forged-adapter-key")
	c.Set("api_key", &APIKey{ID: 7})
	expected, err := prismBrowserSessionID(c, account.ID, []byte(body))
	require.NoError(t, err)
	_, err = s.forwardPrismBrowser(context.Background(), c, account, []byte(body), time.Now())
	require.NoError(t, err)
	require.Equal(t, expected, <-requests)
	require.NotEqual(t, "fixture-session", expected)
	_, _, _, err = s.callPrismBrowser(context.Background(), account, []byte(body))
	require.NoError(t, err)
	require.Empty(t, <-requests)
}

func TestPrismBrowserInvalidSessionDoesNotDispatch(t *testing.T) {
	s, account := prismTestService("http://127.0.0.1:1")
	for _, value := range []string{"with\tcontrol", "with\ncontrol", strings.Repeat("x", 256)} {
		t.Run(value, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("session-id", value)
			c.Set("api_key", &APIKey{ID: 7})
			_, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(`{"model":"gpt-6.1-sol","input":"fixture"}`), time.Now())
			require.Error(t, err)
			require.False(t, errors.Is(err, errPrismBrowserHTTPFallback))
			require.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestPrismBrowserExplicitMappingReachesAdapter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "gpt-6.1-sol", body["model"])
		_, _ = io.WriteString(w, prismTerminal)
	}))
	defer server.Close()
	s, account := prismTestService(server.URL)
	account.Credentials["model_mapping"] = map[string]any{"my-sol": "gpt-6.1-sol"}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	result, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(`{"model":"my-sol","input":"hi"}`), time.Now())
	require.NoError(t, err)
	require.Equal(t, "my-sol", result.Model)
	require.Equal(t, "gpt-6.1-sol", result.UpstreamModel)
	require.Equal(t, prismBrowserUpstreamEndpoint, result.UpstreamEndpoint)
}

func TestPrismBrowserCallProtectsCredentialBoundary(t *testing.T) {
	var count int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer fixture-bridge-key" ||
			r.Header.Get("X-Prism-OAuth-Token") != "fixture-oauth" || r.Header.Get("X-Prism-Account-ID") != "42" {
			t.Error("unexpected adapter request")
		}
		w.Header().Set("Location", "http://127.0.0.1:1/credential-leak")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	s, account := prismTestService(server.URL)
	if _, _, _, err := s.callPrismBrowser(context.Background(), account, []byte(`{"input":"test"}`)); err == nil {
		t.Fatal("redirect must be rejected")
	}
	require.Equal(t, 1, count)
	s.cfg.Gateway.PrismBrowser.Enabled = false
	require.True(t, accountHasPrismBrowser(account))
	if _, _, _, err := s.callPrismBrowser(context.Background(), account, nil); err == nil {
		t.Fatal("disabled adapter must fail closed")
	}
	require.Equal(t, 1, count)
}

func TestPrismBrowserForwardTerminalAndEstimatedUsage(t *testing.T) {
	emptyModel := `{"id":"resp_fixture","status":"completed","usage":null,"output":[{"type":"message","content":[{"type":"output_text","text":"21"}]}]}`
	for _, tc := range []struct {
		name     string
		stream   bool
		terminal string
	}{
		{name: "json", terminal: prismTerminal},
		{name: "empty model", terminal: emptyModel},
		{name: "stream", stream: true, terminal: prismTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.stream {
					_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+tc.terminal+"}\n\n")
				} else {
					_, _ = io.WriteString(w, tc.terminal)
				}
			}))
			defer server.Close()
			s, account := prismTestService(server.URL)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			body := `{"model":"gpt-6.1-sol","input":"candy","stream":false}`
			if tc.stream {
				body = strings.Replace(body, "false", "true", 1)
			}
			result, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(body), time.Now())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, w.Code)
			require.NotNil(t, result)
			require.Equal(t, "resp_fixture", result.ResponseID)
			require.Greater(t, result.Usage.InputTokens, 0)
			require.Greater(t, result.Usage.OutputTokens, 0)
			require.Equal(t, "estimated", w.Header().Get("X-Prism-Usage"))
			require.True(t, IsPrismBrowserAttempt(c, account.ID))
		})
	}
	if _, err := prismBrowserTerminal([]byte(prismTerminal), "gpt-5.6-sol", false); err == nil {
		t.Fatal("model substitution accepted")
	}
	if _, err := prismBrowserTerminal([]byte(`{"id":"resp_fixture","status":"failed","model":"gpt-6.1-sol","output":[{"content":[{"text":"21"}]}]}`), "gpt-6.1-sol", false); err == nil {
		t.Fatal("invalid terminal accepted")
	}
}

func TestPrismBrowserAdapterURLStaysOnLoopback(t *testing.T) {
	for _, input := range []string{"http://127.0.0.1:8319/v1", "http://[::1]:8319/v1/responses"} {
		if _, err := prismBrowserAdapterURL(input); err != nil {
			t.Fatalf("valid adapter %q rejected: %v", input, err)
		}
	}
	for _, input := range []string{
		"https://127.0.0.1:8319/v1", "http://localhost:8319/v1",
		"http://adapter.example:8319/v1", "http://127.0.0.2:8319/v1",
		"http://127.0.0.1:8319/v1?next=evil", "http://user@127.0.0.1:8319/v1",
		"http://127.0.0.1:8319/other", "http://127.0.0.1/v1",
	} {
		if _, err := prismBrowserAdapterURL(input); err == nil {
			t.Fatalf("unsafe adapter %q accepted", input)
		}
	}
}

func TestAccountUsesPrismBrowserDefaultsOnForOAuth(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.PrismBrowser.Enabled = true
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.True(t, accountUsesPrismBrowser(account, cfg))
	account.Extra = map[string]any{"openai_prism_browser": false}
	require.False(t, accountUsesPrismBrowser(account, cfg))
	account.Extra["openai_prism_browser"] = "yes"
	require.False(t, accountUsesPrismBrowser(account, cfg))
	account.Extra = nil
	cfg.Gateway.PrismBrowser.Enabled = false
	require.False(t, accountUsesPrismBrowser(account, cfg))
	cfg.Gateway.PrismBrowser.Enabled = true
	account.Type = AccountTypeAPIKey
	require.False(t, accountUsesPrismBrowser(account, cfg))
	shadow := int64(9)
	account = &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &shadow}
	require.False(t, accountUsesPrismBrowser(account, cfg))
}

func TestPrismBrowserInfrastructureFallsBackWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "busy", status: http.StatusTooManyRequests, body: `{"error":{"type":"prism_busy","message":"full"}}`},
		{name: "down", status: http.StatusBadGateway, body: `{"error":{"type":"bad_gateway"}}`},
		{name: "unavailable", status: http.StatusServiceUnavailable, body: `{}`},
		{name: "timeout", status: http.StatusGatewayTimeout, body: `{}`},
		{name: "bridge key", status: http.StatusUnauthorized, body: `{"error":{"type":"unauthorized"}}`},
		{name: "path", status: http.StatusNotFound, body: `{"error":{"type":"not_found"}}`},
		{name: "model entitlement", status: http.StatusUnprocessableEntity, body: `{"error":{"type":"model_unavailable","message":"no sol"}}`},
		{name: "encrypted history", status: http.StatusUnprocessableEntity, body: `{"error":{"type":"unsupported_reasoning_history","message":"Encrypted reasoning cannot be replayed into Prism"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			s, account := prismTestService(server.URL)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			_, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(`{"model":"gpt-6.1-sol","input":"hi"}`), time.Now())
			require.ErrorIs(t, err, errPrismBrowserHTTPFallback)
			require.Empty(t, w.Body.String())
			require.False(t, IsPrismBrowserAttempt(c, account.ID))
		})
	}
	s, account := prismTestService("http://127.0.0.1:1")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(`{"model":"gpt-6.1-sol","input":"hi"}`), time.Now())
	require.ErrorIs(t, err, errPrismBrowserHTTPFallback)
	require.Empty(t, w.Body.String())
}

func TestPrismBrowserEncryptedReasoningFallsBackWithoutCallingAdapter(t *testing.T) {
	serverHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverHits++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, prismTerminal)
	}))
	defer server.Close()
	s, account := prismTestService(server.URL)
	cases := []string{
		`{"model":"gpt-6.1-sol","input":[{"type":"reasoning","encrypted_content":"opaque"},{"role":"user","content":"go"}]}`,
		`{"model":"gpt-6.1-sol","input":{"type":"reasoning","encrypted_content":"opaque"}}`,
		`{"model":"gpt-6.1-sol","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":[{"type":"reasoning","encrypted_content":"opaque"},{"role":"user","content":"go"}]}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			before := serverHits
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			_, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(body), time.Now())
			require.ErrorIs(t, err, errPrismBrowserHTTPFallback)
			require.Empty(t, w.Body.String())
			require.False(t, IsPrismBrowserAttempt(c, account.ID))
			require.Equal(t, before, serverHits)
		})
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	result, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(`{"model":"gpt-6.1-sol","include":["reasoning.encrypted_content"],"input":"hi"}`), time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, serverHits)
}

func TestPrismBrowserStructuredOutputFallsBackWithoutCallingAdapter(t *testing.T) {
	serverHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverHits++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, prismTerminal)
	}))
	defer server.Close()
	s, account := prismTestService(server.URL)
	cases := []string{
		`{"model":"gpt-6.1-sol","input":"hi","text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}}`,
		`{"model":"gpt-6.1-sol","input":"hi","text":{"format":{"type":"json_object"}}}`,
		`{"model":"gpt-6.1-sol","input":"hi","response_format":{"type":"json_schema"}}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			before := serverHits
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			_, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(body), time.Now())
			require.ErrorIs(t, err, errPrismBrowserHTTPFallback)
			require.Empty(t, w.Body.String())
			require.False(t, IsPrismBrowserAttempt(c, account.ID))
			require.Equal(t, before, serverHits)
		})
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	result, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(`{"model":"gpt-6.1-sol","input":"hi","text":{"verbosity":"low","format":{"type":"text"}}}`), time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, serverHits)
}

func TestPrismBrowserRequestHasStructuredOutput(t *testing.T) {
	require.True(t, prismBrowserRequestHasStructuredOutput([]byte(`{"text":{"format":{"type":"json_schema"}}}`)))
	require.True(t, prismBrowserRequestHasStructuredOutput([]byte(`{"text":{"format":{"type":"json_object"}}}`)))
	require.True(t, prismBrowserRequestHasStructuredOutput([]byte(`{"response_format":{"type":"json_schema"}}`)))
	require.False(t, prismBrowserRequestHasStructuredOutput([]byte(`{"text":{"verbosity":"low"}}`)))
	require.False(t, prismBrowserRequestHasStructuredOutput([]byte(`{"text":{"format":{"type":"text"}}}`)))
	require.False(t, prismBrowserRequestHasStructuredOutput([]byte(`{"text":{"format":null}}`)))
	require.False(t, prismBrowserRequestHasStructuredOutput([]byte(`{"input":"hi"}`)))
}

func TestPrismBrowserRequestHasEncryptedReasoning(t *testing.T) {
	require.True(t, prismBrowserRequestHasEncryptedReasoning([]byte(`{"input":[{"type":"reasoning","encrypted_content":"gAAA"}]}`)))
	require.True(t, prismBrowserRequestHasEncryptedReasoning([]byte(`{"input":{"type":"reasoning","encrypted_content":"gAAA"}}`)))
	require.False(t, prismBrowserRequestHasEncryptedReasoning([]byte(`{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}]}]}`)))
	require.False(t, prismBrowserRequestHasEncryptedReasoning([]byte(`{"input":[{"type":"reasoning","encrypted_content":""}]}`)))
	require.False(t, prismBrowserRequestHasEncryptedReasoning([]byte(`{"include":["reasoning.encrypted_content"],"input":"hi"}`)))
}

func TestPrismBrowserClientErrorsDoNotFallBack(t *testing.T) {
	serverHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverHits++
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"type":"unsupported_request","message":"fixture"}}`)
	}))
	defer server.Close()
	s, account := prismTestService(server.URL)
	cases := []struct {
		name string
		path string
		body string
	}{
		{name: "adapter refusal", body: `{"model":"gpt-6.1-sol","input":"hi"}`},
		{name: "compact path", path: "/v1/responses/compact", body: `{"model":"gpt-6.1-sol","input":"hi"}`},
		{name: "compact spelling", body: `{"model":"gpt-6.1-sol-openai-compact","input":"hi"}`},
		{name: "image", body: `{"model":"gpt-6.1-sol","input":[{"type":"message","content":[{"type":"input_image","image_url":"data:image/png;base64,aa"}]}]}`},
		{name: "previous response", body: `{"model":"gpt-6.1-sol","input":"hi","previous_response_id":"resp_old"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := serverHits
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			path := tc.path
			if path == "" {
				path = "/v1/responses"
			}
			c.Request = httptest.NewRequest(http.MethodPost, path, nil)
			_, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(tc.body), time.Now())
			require.Error(t, err)
			require.False(t, errors.Is(err, errPrismBrowserHTTPFallback))
			require.Equal(t, http.StatusUnprocessableEntity, w.Code)
			require.Contains(t, w.Body.String(), "unsupported_request")
			require.True(t, IsPrismBrowserAttempt(c, account.ID))
			if tc.name == "adapter refusal" {
				require.Equal(t, before+1, serverHits)
			} else {
				require.Equal(t, before, serverHits)
			}
		})
	}
}

func TestPrismBrowserInvalidTerminalDoesNotFallBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"resp_fixture","status":"failed","model":"gpt-6.1-sol"}`)
	}))
	defer server.Close()
	s, account := prismTestService(server.URL)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(`{"model":"gpt-6.1-sol","input":"hi"}`), time.Now())
	require.Error(t, err)
	require.False(t, errors.Is(err, errPrismBrowserHTTPFallback))
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.True(t, IsPrismBrowserAttempt(c, account.ID))
}

func TestPrismBrowserMaxFoldsToXhigh(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-6.1-sol","input":"hi","reasoning":{"effort":"max"}}`,
		`{"model":"gpt-6.1-sol-max","input":"hi"}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(raw, "model").String())
				require.Equal(t, "xhigh", gjson.GetBytes(raw, "reasoning.effort").String())
				_, _ = io.WriteString(w, prismTerminal)
			}))
			defer server.Close()
			s, account := prismTestService(server.URL)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			result, err := s.forwardPrismBrowser(context.Background(), c, account, []byte(body), time.Now())
			require.NoError(t, err)
			require.NotNil(t, result.ReasoningEffort)
			require.Equal(t, "xhigh", *result.ReasoningEffort)
		})
	}
}

func TestShouldAttemptPrismBrowserSkipsNonHTTP61Sol(t *testing.T) {
	s, account := prismTestService("http://127.0.0.1:8319")
	httpCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	httpCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(httpCtx, OpenAIClientTransportHTTP)
	require.True(t, shouldAttemptPrismBrowser(s, httpCtx, account, "gpt-6.1-sol"))
	require.True(t, shouldAttemptPrismBrowser(s, httpCtx, account, "gpt-6.1-sol-max"))
	require.False(t, shouldAttemptPrismBrowser(s, httpCtx, account, "gpt-5.6-sol"))
	wsCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	wsCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(wsCtx, OpenAIClientTransportWS)
	require.False(t, shouldAttemptPrismBrowser(s, wsCtx, account, "gpt-6.1-sol"))
	apiKey := *account
	apiKey.Type = AccountTypeAPIKey
	require.False(t, shouldAttemptPrismBrowser(s, httpCtx, &apiKey, "gpt-6.1-sol"))
}

func TestPrismBrowserDoesNotExcludeWebSocketScheduling(t *testing.T) {
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.PrismBrowser.Enabled = true
	base := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"openai_oauth_responses_websockets_v2_enabled": true,
	}}
	prism := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"openai_oauth_responses_websockets_v2_enabled": true,
		"openai_prism_browser":                         true,
	}}
	svc := &OpenAIGatewayService{cfg: cfg}
	for _, model := range []string{"gpt-6.1-sol", "gpt-5.6-sol"} {
		require.Equal(t,
			svc.isOpenAIAccountTransportCompatible(base, OpenAIUpstreamTransportResponsesWebsocketV2Ingress, model),
			svc.isOpenAIAccountTransportCompatible(prism, OpenAIUpstreamTransportResponsesWebsocketV2Ingress, model),
			model,
		)
	}
}

func TestPrismBrowserAccountTestExplainsAdapterRefusal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"type":"pending_turn","message":"Previous Prism turn outcome is unknown; inspect it before a new request"}}`)
	}))
	defer server.Close()
	gateway, account := prismTestService(server.URL)
	svc := &AccountTestService{openaiGatewayService: gateway, cfg: gateway.cfg}
	for _, tc := range []struct {
		name    string
		enabled bool
		want    string
	}{
		{name: "adapter refusal", enabled: true, want: "Prism adapter returned HTTP 409 (pending_turn): Previous Prism turn outcome is unknown"},
		{name: "gateway switch off", enabled: false, want: "Prism adapter request failed: prism adapter is disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway.cfg.Gateway.PrismBrowser.Enabled = tc.enabled
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/42/test", nil)
			require.Error(t, svc.testPrismBrowserConnection(c, account, "gpt-6.1-sol-max", "hi"))
			require.Contains(t, rec.Body.String(), tc.want)
		})
	}
}
