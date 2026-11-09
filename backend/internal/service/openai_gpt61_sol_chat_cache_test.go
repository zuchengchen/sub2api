package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const gpt61ChatCacheBody = `{"model":"gpt-6.1-sol","prompt_cache_key":"synthetic-cache-hint","messages":[{"role":"system","content":"stable system"},{"role":"user","content":"stable user"}]}`

func gpt61ChatCacheAccount(credential string) *Account {
	return &Account{ID: 7007, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "synthetic-unused-token", "chatgpt_account_id": credential},
		Extra: map[string]any{codexFingerprintModeExtraKey: "full", codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
			OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "verdict": "healthy", "recent_samples": []map[string]any{{"verdict": "healthy", "at": "2020-01-01T00:00:00Z"}}}},
	}
}

type gpt61ChatCacheHTTP struct {
	origin *url.URL
	client *http.Client
}

func (u *gpt61ChatCacheHTTP) Do(req *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "chatgpt.com" || req.URL.Path != "/backend-api/codex/responses" || req.Method != http.MethodPost || proxy != "" {
		return nil, errors.New("identity fixture rejected unexpected upstream route")
	}
	copy := req.Clone(req.Context())
	target := *req.URL
	target.Scheme, target.Host = u.origin.Scheme, u.origin.Host
	copy.URL = &target
	copy.Host = u.origin.Host
	return u.client.Do(copy)
}
func (u *gpt61ChatCacheHTTP) DoWithTLS(req *http.Request, proxy string, account int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, account, concurrency)
}

type gpt61ChatCacheCapture struct {
	body    []byte
	headers http.Header
	err     error
}
type gpt61ChatCacheCompletion struct {
	restored bool
	result   *OpenAIForwardResult
	err      error
}

// Real HTTP is used on both sides. The adapter rewrites only the expected Codex
// route; its dialer can connect only to the local fixture, never to an upstream.
func gpt61ChatCacheForward(t *testing.T, body []byte, headers http.Header, keyID, userID int64, credential string, status int) gpt61ChatCacheCapture {
	t.Helper()
	captured := make(chan gpt61ChatCacheCapture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		r.Body.Close()
		if err == nil && r.Header.Get("Content-Encoding") == "zstd" {
			var decoder *zstd.Decoder
			decoder, err = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(8<<20))
			if err == nil {
				raw, err = decoder.DecodeAll(raw, nil)
				decoder.Close()
			}
		}
		captured <- gpt61ChatCacheCapture{body: raw, headers: r.Header.Clone(), err: err}
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"synthetic error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_synthetic","model":"gpt-6.1-sol","status":"completed","output":[{"type":"message","id":"msg_synthetic","role":"assistant","status":"completed","content":[{"type":"output_text","text":"fixture answer","annotations":[]}]}],"usage":{"input_tokens":10000,"input_tokens_details":{"cached_tokens":9500},"output_tokens":12,"total_tokens":10012}}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	origin, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != origin.Host {
			return nil, errors.New("identity fixture blocked nonlocal dial")
		}
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	synth := true
	cfg := &config.Config{}
	cfg.Gateway.CodexFullSynthesis = &synth
	cfg.Gateway.DisableOpenAIClientLocalTimeRewrite = true
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &gpt61ChatCacheHTTP{origin: origin, client: client}, tiboRouteDisabled: true}
	engine, _, _, _ := nativeReplayFixture(t)
	svc.SetNativeReasoningReplayRuntime(nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) {
		return NativeReasoningReplayRuntimeConfig{Mode: NativeReasoningReplayModeReplay, AllUsersModels: []string{"gpt-6.1-sol"}}, nil
	}), engine)
	completed := make(chan gpt61ChatCacheCompletion, 1)
	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		c.Set("api_key", &APIKey{ID: keyID, UserID: userID})
		input, readErr := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if readErr != nil {
			completed <- gpt61ChatCacheCompletion{err: readErr}
			return
		}
		original := r.Header.Clone()
		result, forwardErr := svc.ForwardAsChatCompletions(r.Context(), c, gpt61ChatCacheAccount(credential), input, svc.ExtractSessionID(c, input), "")
		completed <- gpt61ChatCacheCompletion{result: result, err: forwardErr, restored: reflect.DeepEqual(original, r.Header)}
	}))
	defer ingress.Close()
	req, err := http.NewRequest(http.MethodPost, ingress.URL+"/v1/chat/completions", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := ingress.Client().Do(req)
	require.NoError(t, err)
	delivered, err := io.ReadAll(response.Body)
	response.Body.Close()
	require.NoError(t, err)
	select {
	case done := <-completed:
		require.True(t, done.restored, "all inbound header values must be restored after Forward returns")
		if status == http.StatusOK {
			require.NoError(t, done.err)
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.NotNil(t, done.result)
			require.Equal(t, 10000, done.result.Usage.InputTokens)
			require.Equal(t, 9500, done.result.Usage.CacheReadInputTokens)
			require.Equal(t, "fixture answer", gjson.GetBytes(delivered, "choices.0.message.content").String())
		} else {
			require.Error(t, done.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP fixture did not finish")
	}
	select {
	case row := <-captured:
		require.NoError(t, row.err)
		return row
	case <-time.After(time.Second):
		t.Fatal("no local upstream request")
		return gpt61ChatCacheCapture{}
	}
}

func gpt61ChatCacheMeta(t *testing.T, value any) []byte {
	t.Helper()
	body, err := sjson.Set(gpt61ChatCacheBody, "client_metadata", value)
	require.NoError(t, err)
	return []byte(body)
}
func gpt61ChatCacheHeader(name, value string) http.Header {
	h := make(http.Header)
	h.Set(name, value)
	return h
}

func TestGPT61SolChatCacheHTTPExplicitCarriers(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		headers http.Header
	}{}
	for _, name := range clientSessionIDHeaders {
		cases = append(cases, struct {
			name    string
			body    []byte
			headers http.Header
		}{name, []byte(gpt61ChatCacheBody), gpt61ChatCacheHeader(name, "synthetic-stable-session")})
	}
	cases = append(cases,
		struct {
			name    string
			body    []byte
			headers http.Header
		}{"body_flat", gpt61ChatCacheMeta(t, map[string]any{"session_id": "synthetic-stable-session", "thread_id": "synthetic-stable-thread", "turn_id": "synthetic-turn"}), nil},
		struct {
			name    string
			body    []byte
			headers http.Header
		}{"body_embedded", gpt61ChatCacheMeta(t, map[string]any{openAIWSTurnMetadataHeader: `{"session_id":"synthetic-stable-session","thread_id":"synthetic-stable-thread","turn_id":"synthetic-turn"}`}), nil},
		struct {
			name    string
			body    []byte
			headers http.Header
		}{"body_thread_only", gpt61ChatCacheMeta(t, map[string]any{"thread_id": "synthetic-stable-thread"}), nil},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := gpt61ChatCacheForward(t, tc.body, tc.headers, 707, 7007, "credential-one", http.StatusOK)
			b := gpt61ChatCacheForward(t, tc.body, tc.headers, 707, 7007, "credential-one", http.StatusOK)
			require.NotEmpty(t, a.headers.Get("session-id"))
			require.NotEmpty(t, a.headers.Get("thread-id"))
			require.Equal(t, a.headers.Get("session-id"), b.headers.Get("session-id"), "explicit identity must survive separate Chat conversions")
			require.Equal(t, a.headers.Get("thread-id"), b.headers.Get("thread-id"))
			require.Equal(t, a.headers.Get("turn-id"), b.headers.Get("turn-id"))
			require.Equal(t, gjson.GetBytes(a.body, "prompt_cache_key").String(), gjson.GetBytes(b.body, "prompt_cache_key").String())
			require.JSONEq(t, gjson.GetBytes(a.body, "input").Raw, gjson.GetBytes(b.body, "input").Raw)
		})
	}
}

func TestGPT61SolChatCacheHTTPBodyPriorityAndIsolation(t *testing.T) {
	meta := map[string]any{"session_id": "root-session", "thread_id": "child-one", "turn_id": "body-turn", "turn_started_at_unix_ms": 1794182400000, "parent_thread_id": "parent", "forked_from_thread_id": "fork", "root_turn_id": "root-turn", "parent_turn_id": "parent-turn", "private_extra": "must-not-cross"}
	body := gpt61ChatCacheMeta(t, meta)
	headers := gpt61ChatCacheHeader("session-id", "conflicting-header-session")
	headers.Set("thread-id", "conflicting-header-thread")
	headers.Set("X-Conversation-ID", "conflicting-alias")
	headers.Set(openAIWSTurnMetadataHeader, `{"session_id":"header-meta","thread_id":"header-meta-thread","turn_id":"header-turn","turn_started_at_unix_ms":1,"kept":true}`)
	a := gpt61ChatCacheForward(t, body, headers, 707, 7007, "credential-one", http.StatusOK)
	b := gpt61ChatCacheForward(t, body, nil, 707, 7007, "credential-one", http.StatusOK)
	for _, name := range []string{"session-id", "thread-id", "turn-id"} {
		require.Equal(t, b.headers.Get(name), a.headers.Get(name), "body metadata must win %s", name)
	}
	require.NotContains(t, string(a.body), "must-not-cross")
	require.EqualValues(t, 1794182400000, gjson.Get(a.headers.Get(openAIWSTurnMetadataHeader), "turn_started_at_unix_ms").Int())
	require.True(t, gjson.Get(a.headers.Get(openAIWSTurnMetadataHeader), "kept").Bool())
	for _, field := range []string{"parent_thread_id", "forked_from_thread_id", "root_turn_id", "parent_turn_id"} {
		require.NotEmpty(t, gjson.Get(a.headers.Get(openAIWSTurnMetadataHeader), field).String())
		require.NotEqual(t, meta[field], gjson.Get(a.headers.Get(openAIWSTurnMetadataHeader), field).String())
	}
	for _, tc := range []struct {
		name       string
		key, user  int64
		credential string
		child      bool
	}{
		{"other_user", 708, 7008, "credential-one", false}, {"other_key_same_user", 709, 7007, "credential-one", false}, {"other_credential", 707, 7007, "credential-two", false}, {"same_session_different_thread", 707, 7007, "credential-one", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := body
			if tc.child {
				var err error
				next, err = sjson.SetBytes(body, "client_metadata.thread_id", "child-two")
				require.NoError(t, err)
			}
			row := gpt61ChatCacheForward(t, next, nil, tc.key, tc.user, tc.credential, http.StatusOK)
			require.NotEqual(t, b.headers.Get("thread-id"), row.headers.Get("thread-id"))
			if tc.child {
				require.Equal(t, b.headers.Get("session-id"), row.headers.Get("session-id"))
			} else {
				require.NotEqual(t, b.headers.Get("session-id"), row.headers.Get("session-id"))
			}
		})
	}
	t.Run("error_restores_headers", func(t *testing.T) {
		gpt61ChatCacheForward(t, body, headers, 707, 7007, "credential-one", http.StatusBadRequest)
	})
}

func TestGPT61SolChatCacheHTTPAnonymousBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      []byte
		headers   http.Header
		key, user int64
	}{
		{"cache_hint_only", []byte(gpt61ChatCacheBody), nil, 707, 7007},
		{"invalid_alias", []byte(gpt61ChatCacheBody), gpt61ChatCacheHeader("X-Conversation-ID", "bad\tsession"), 707, 7007},
		{"invalid_body_with_valid_header", gpt61ChatCacheMeta(t, map[string]any{"session_id": "bad\tsession"}), gpt61ChatCacheHeader("session-id", "would-be-stable"), 707, 7007},
		{"missing_key", gpt61ChatCacheMeta(t, map[string]any{"session_id": "body-stable"}), nil, 0, 7007},
		{"missing_user", gpt61ChatCacheMeta(t, map[string]any{"session_id": "body-stable"}), nil, 707, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := gpt61ChatCacheForward(t, tc.body, tc.headers, tc.key, tc.user, "credential-one", http.StatusOK)
			b := gpt61ChatCacheForward(t, tc.body, tc.headers, tc.key, tc.user, "credential-one", http.StatusOK)
			require.NotEmpty(t, a.headers.Get("session-id"))
			require.NotEqual(t, a.headers.Get("session-id"), b.headers.Get("session-id"))
			require.NotEqual(t, a.headers.Get("thread-id"), b.headers.Get("thread-id"))
		})
	}
}

func gpt61ChatCacheContext(body []byte, headers http.Header) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header = headers.Clone()
	if c.Request.Header == nil {
		c.Request.Header = make(http.Header)
	}
	c.Set("api_key", &APIKey{ID: 707, UserID: 7007})
	return c
}

func TestGPT61SolChatCacheResolverPriority(t *testing.T) {
	body := gpt61ChatCacheMeta(t, map[string]any{"session_id": "flat", "thread_id": "flat-thread", "turn_id": "flat-turn", openAIWSTurnMetadataHeader: `{"session_id":"embedded","thread_id":"embedded-thread","turn_id":"embedded-turn","turn_started_at_unix_ms":1000}`})
	headers := gpt61ChatCacheHeader("session-id", "canonical")
	headers.Set("session_id", "underscore")
	headers.Set("X-Conversation-ID", "alias")
	headers.Set("thread-id", "header-thread")
	headers.Set("turn-id", "header-turn")
	headers.Set(openAIWSTurnMetadataHeader, `{"session_id":"header-meta","thread_id":"header-meta-thread","turn_id":"header-meta-turn","turn_started_at_unix_ms":2}`)
	for _, tc := range []struct {
		name                          string
		body                          []byte
		headers                       http.Header
		session, thread, turn, source string
	}{
		{"embedded_body_first", body, headers, "embedded", "embedded-thread", "embedded-turn", "body:turn_metadata"},
		{"flat_body_before_headers", gpt61ChatCacheMeta(t, map[string]any{"session_id": "flat", "thread_id": "flat-thread", "turn_id": "flat-turn"}), headers, "flat", "flat-thread", "flat-turn", "body:client_metadata"},
		{"header_metadata_before_canonical", []byte(gpt61ChatCacheBody), headers, "header-meta", "header-meta-thread", "header-meta-turn", "header:turn_metadata"},
		{"canonical_before_alias", []byte(gpt61ChatCacheBody), http.Header{"Session-Id": {"canonical"}, "Session_id": {"underscore"}, "X-Conversation-Id": {"alias"}}, "canonical", "canonical", "", "header:session-id"},
		{"thread_before_alias", []byte(gpt61ChatCacheBody), http.Header{"Thread-Id": {"thread"}, "X-Conversation-Id": {"alias"}}, "thread", "thread", "", "none"},
		{"body_thread_before_alias", gpt61ChatCacheMeta(t, map[string]any{"thread_id": "body-thread"}), gpt61ChatCacheHeader("X-Conversation-ID", "alias"), "body-thread", "body-thread", "", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := gpt61ChatCacheContext(tc.body, tc.headers)
			session, thread, valid := gpt61SolChatExplicitIdentity(c, tc.body)
			require.True(t, valid)
			require.Equal(t, tc.session, session)
			require.Equal(t, tc.thread, thread)
			resolved := resolveGPT61SolChatIdentity(c, tc.body)
			require.Equal(t, tc.turn, resolved.evidence.ClientTurnID)
			require.Equal(t, tc.source, resolved.sessionSource)
			original := c.Request.Header.Clone()
			restore := stageGPT61SolChatClientSession(c, gpt61ChatCacheAccount("credential-one"), tc.body, "gpt-6.1-sol", true)
			converted := collectCodexTurnEvidence(nil, c.Request.Header, gjson.Result{})
			require.Equal(t, resolved.evidence.ClientSessionID, converted.ClientSessionID)
			require.Equal(t, resolved.evidence.ClientThreadID, converted.ClientThreadID)
			require.Equal(t, resolved.evidence.ClientTurnID, converted.ClientTurnID)
			require.Equal(t, resolved.evidence.ClientTurnStartedAtMs, converted.ClientTurnStartedAtMs)
			restore()
			require.Equal(t, original, c.Request.Header)
		})
	}
	t.Run("mismatched_timestamp_dropped", func(t *testing.T) {
		body := gpt61ChatCacheMeta(t, map[string]any{"session_id": "s", "turn_id": "body-turn"})
		c := gpt61ChatCacheContext(body, headers)
		restore := stageGPT61SolChatClientSession(c, gpt61ChatCacheAccount("credential-one"), body, "gpt-6.1-sol", true)
		defer restore()
		require.EqualValues(t, 0, collectCodexTurnEvidence(nil, c.Request.Header, gjson.Result{}).ClientTurnStartedAtMs)
	})
}

func TestGPT61SolChatCacheInvalidIdentityAndAnonymous(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		headers http.Header
	}{
		{"pck_only", []byte(gpt61ChatCacheBody), nil},
		{"control", []byte(gpt61ChatCacheBody), gpt61ChatCacheHeader("X-Conversation-ID", "invalid\tsession")},
		{"edge_control", []byte(gpt61ChatCacheBody), gpt61ChatCacheHeader("session-id", "\t")},
		{"too_long", []byte(gpt61ChatCacheBody), gpt61ChatCacheHeader("session-id", strings.Repeat("x", 256))},
		{"invalid_utf8", []byte(gpt61ChatCacheBody), gpt61ChatCacheHeader("session-id", string([]byte{0xff}))},
		{"repeated_header", []byte(gpt61ChatCacheBody), http.Header{"Session-Id": {"one", "two"}}},
		{"numeric_session", gpt61ChatCacheMeta(t, map[string]any{"session_id": 123}), nil},
		{"body_control", gpt61ChatCacheMeta(t, map[string]any{"session_id": "bad\u0000session"}), nil},
		{"duplicate_metadata", []byte(strings.TrimSuffix(gpt61ChatCacheBody, "}") + `,"client_metadata":{"session_id":"one","session_id":"two"}}`), nil},
		{"duplicate_root", []byte(strings.TrimSuffix(gpt61ChatCacheBody, "}") + `,"client_metadata":{"session_id":"one"},"client_metadata":{"session_id":"two"}}`), nil},
		{"malformed_header_metadata", []byte(gpt61ChatCacheBody), gpt61ChatCacheHeader(openAIWSTurnMetadataHeader, `{"session_id":"one"`)},
		{"metadata_over_limit", gpt61ChatCacheMeta(t, map[string]any{"session_id": "s", "extra": strings.Repeat("x", gpt61SolChatIdentityMaxMetadataBytes)}), nil},
		{"invalid_claude_json", []byte(strings.TrimSuffix(gpt61ChatCacheBody, "}") + `,"metadata":{"user_id":"{\"session_id\":123}"}}`), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := []string{}
			for i := 0; i < 2; i++ {
				c := gpt61ChatCacheContext(tc.body, tc.headers)
				original := c.Request.Header.Clone()
				_, _, valid := gpt61SolChatExplicitIdentity(c, tc.body)
				require.False(t, valid)
				restore := stageGPT61SolChatClientSession(c, gpt61ChatCacheAccount("credential-one"), tc.body, "gpt-6.1-sol", true)
				sessions = append(sessions, c.Request.Header.Get("session-id"))
				require.Empty(t, c.Request.Header.Get(openAIWSTurnMetadataHeader))
				restore()
				require.Equal(t, original, c.Request.Header)
			}
			require.NotEmpty(t, sessions[0])
			require.NotEqual(t, sessions[0], sessions[1], "no invalid identity or PCK may anchor a conversation")
		})
	}
	t.Run("turn_only_is_not_replay_session", func(t *testing.T) {
		body := gpt61ChatCacheMeta(t, map[string]any{"turn_id": "turn-only"})
		c := gpt61ChatCacheContext(body, nil)
		_, _, valid := gpt61SolChatExplicitIdentity(c, body)
		require.False(t, valid)
		restore := stageGPT61SolChatClientSession(c, gpt61ChatCacheAccount("credential-one"), body, "gpt-6.1-sol", true)
		defer restore()
		require.Equal(t, "turn-only", collectCodexTurnEvidence(nil, c.Request.Header, gjson.Result{}).ClientTurnID)
	})
}

func TestGPT61SolChatCacheNilHeaderRestoration(t *testing.T) {
	body := gpt61ChatCacheMeta(t, map[string]any{"session_id": "explicit"})
	c := gpt61ChatCacheContext(body, nil)
	c.Request.Header = nil
	restore := stageGPT61SolChatClientSession(c, gpt61ChatCacheAccount("credential-one"), body, "gpt-6.1-sol", true)
	require.Equal(t, "explicit", c.Request.Header.Get("session-id"))
	restore()
	require.Nil(t, c.Request.Header)
}

func TestGPT61SolChatCacheParentOnlyReplayBoundary(t *testing.T) {
	for _, turn := range []string{"", "child-turn"} {
		body := gpt61ChatCacheMeta(t, map[string]any{"session_id": "root", "parent_thread_id": "parent", "turn_id": turn})
		c := gpt61ChatCacheContext(body, nil)
		session, thread, valid := gpt61SolChatExplicitIdentity(c, body)
		if turn == "" {
			require.False(t, valid)
			require.Empty(t, session)
			require.Empty(t, thread)
		} else {
			require.True(t, valid)
			require.Equal(t, "root", session)
			require.Equal(t, turn, thread)
		}
	}
}

func TestGPT61SolChatCacheGateBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		enabled     bool
		modify      func(*gin.Context, *Account)
		body        []byte
	}{
		{name: "disabled", model: "gpt-6.1-sol"},
		{name: "other_model", model: "gpt-6-astra", enabled: true},
		{name: "native_responses", model: "gpt-6.1-sol", enabled: true, body: []byte(`{"model":"gpt-6.1-sol","input":[]}`)},
		{name: "no_key", model: "gpt-6.1-sol", enabled: true, modify: func(c *gin.Context, _ *Account) { c.Set("api_key", &APIKey{UserID: 7007}) }},
		{name: "no_user", model: "gpt-6.1-sol", enabled: true, modify: func(c *gin.Context, _ *Account) { c.Set("api_key", &APIKey{ID: 707}) }},
		{name: "no_credential_namespace", model: "gpt-6.1-sol", enabled: true, modify: func(_ *gin.Context, a *Account) { a.Credentials = map[string]any{}; a.Extra = map[string]any{} }},
		{name: "setup_token", model: "gpt-6.1-sol", enabled: true, modify: func(_ *gin.Context, a *Account) { a.Type = AccountTypeSetupToken }},
		{name: "personal_access_token", model: "gpt-6.1-sol", enabled: true, modify: func(_ *gin.Context, a *Account) { a.Credentials[openAIAuthModeCredentialKey] = "personalAccessToken" }},
		{name: "agent_identity", model: "gpt-6.1-sol", enabled: true, modify: func(_ *gin.Context, a *Account) {
			a.Credentials[openAIAuthModeCredentialKey] = OpenAIAuthModeAgentIdentity
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if body == nil {
				body = []byte(gpt61ChatCacheBody)
			}
			headers := gpt61ChatCacheHeader("X-Conversation-ID", "same-alias")
			c := gpt61ChatCacheContext(body, headers)
			a := gpt61ChatCacheAccount("credential-one")
			if tc.modify != nil {
				tc.modify(c, a)
			}
			original := c.Request.Header.Clone()
			oldRestore := stageCodexHTTPClientSession(c, a, body)
			legacy := c.Request.Header.Clone()
			oldRestore()
			restore := stageGPT61SolChatClientSession(c, a, body, tc.model, tc.enabled)
			require.Equal(t, legacy, c.Request.Header, "disabled/out-of-scope path must preserve legacy staging")
			require.NotEqual(t, "same-alias", c.Request.Header.Get("session-id"))
			require.Empty(t, c.Request.Header.Get(openAIWSTurnMetadataHeader))
			restore()
			require.Equal(t, original, c.Request.Header)
		})
	}
}

func TestGPT61SolChatCacheIngressDiagnostic(t *testing.T) {
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_USER_ID", "7007")
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_UNTIL", time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339))
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "30")
	for _, name := range clientSessionIDHeaders {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trace.jsonl")
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_PATH", path)
			body := gpt61ChatCacheMeta(t, map[string]any{"session_id": "BODY_SESSION_SECRET", "thread_id": "BODY_THREAD_SECRET", "private_extra": "UNKNOWN_METADATA_SECRET"})
			c := gpt61ChatCacheContext(body, gpt61ChatCacheHeader(name, "HEADER_SESSION_SECRET"))
			require.NotNil(t, beginUserCacheDiagnostic(c, body))
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			for _, secret := range []string{"BODY_SESSION_SECRET", "BODY_THREAD_SECRET", "HEADER_SESSION_SECRET", "UNKNOWN_METADATA_SECRET", "private_extra"} {
				require.NotContains(t, string(raw), secret)
			}
			var event map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(raw), &event))
			var sessionHash string
			require.NoError(t, json.Unmarshal(event["session_sha256"], &sessionHash))
			require.Equal(t, diagnosticSHA256([]byte("HEADER_SESSION_SECRET")), sessionHash)
			var summary gpt61SolChatIdentitySummary
			require.Contains(t, event, "identity")
			require.NoError(t, json.Unmarshal(event["identity"], &summary))
			require.True(t, summary.Valid)
			require.Equal(t, "body:client_metadata", summary.SessionSource)
			candidate := summary.Sources["header:"+strings.ToLower(name)]
			require.True(t, candidate.Present)
			require.True(t, candidate.Valid)
			require.Equal(t, diagnosticHeaderValueDigest("HEADER_SESSION_SECRET"), candidate.HMAC)
			require.Equal(t, diagnosticHeaderValueDigest("BODY_SESSION_SECRET"), summary.Session.HMAC)
			require.LessOrEqual(t, len(summary.Sources), 25)
		})
	}
	t.Run("invalid_presence_and_body_only", func(t *testing.T) {
		body := gpt61ChatCacheMeta(t, map[string]any{"session_id": "BODY_ONLY_SECRET"})
		c := gpt61ChatCacheContext(body, nil)
		summary := summarizeGPT61SolChatIdentity(c, body)
		require.True(t, summary.Session.Valid)
		require.Equal(t, "body:client_metadata", summary.SessionSource)
		require.Empty(t, ExtractClientSessionID(c))
		c.Request.Header.Set("X-Conversation-ID", "bad\tsession")
		summary = summarizeGPT61SolChatIdentity(c, body)
		require.False(t, summary.Valid)
		candidate := summary.Sources["header:x-conversation-id"]
		require.True(t, candidate.Present)
		require.False(t, candidate.Valid)
		require.Empty(t, candidate.HMAC)
	})
	t.Run("invalid_body_presence_is_not_absence", func(t *testing.T) {
		body := gpt61ChatCacheMeta(t, map[string]any{"session_id": 123})
		summary := summarizeGPT61SolChatIdentity(gpt61ChatCacheContext(body, nil), body)
		candidate := summary.Sources["body:client_metadata:session_id"]
		require.False(t, summary.Valid)
		require.True(t, candidate.Examined)
		require.True(t, candidate.Present)
		require.False(t, candidate.Valid)
		require.Empty(t, candidate.HMAC)
		body = gpt61ChatCacheMeta(t, map[string]any{"session_id": "s", "extra": strings.Repeat("x", gpt61SolChatIdentityMaxMetadataBytes)})
		summary = summarizeGPT61SolChatIdentity(gpt61ChatCacheContext(body, nil), body)
		require.True(t, summary.Sources["body:client_metadata"].Present)
		require.False(t, summary.Sources["body:client_metadata:session_id"].Examined)
	})

	t.Run("admission_required", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "not-created.jsonl")
		t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_PATH", path)
		c := gpt61ChatCacheContext([]byte(gpt61ChatCacheBody), nil)
		c.Set("api_key", &APIKey{ID: 708, UserID: 7008})
		require.Nil(t, beginUserCacheDiagnostic(c, []byte(gpt61ChatCacheBody)))
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err))
	})
}
