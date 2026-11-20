package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const responsesDiagnosticTestModel = "gpt-5.6-sol"

func responsesDiagnosticFixture(t *testing.T, limit int) (*gin.Context, *APIKey, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	path := filepath.Join(dir, "native.jsonl")
	for name, value := range map[string]string{
		"USER_ID": "556", "API_KEY_ID": "73", "PATH": path, "MODELS": responsesDiagnosticTestModel + ",gpt-6.2-sol",
		"UNTIL": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339), "MAX_REQUESTS": strconv.Itoa(limit),
	} {
		t.Setenv(responsesCacheDiagnosticEnv+name, value)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	key := &APIKey{ID: 73, UserID: 556}
	c.Set("api_key", key)
	return c, key, path
}

func responsesDiagnosticRecords(t *testing.T, path string) []gjson.Result {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []gjson.Result
	for line := range bytes.SplitSeq(bytes.TrimSpace(raw), []byte{'\n'}) {
		require.True(t, gjson.ValidBytes(line))
		out = append(out, gjson.ParseBytes(line))
	}
	return out
}

func TestResponsesCacheDiagnosticWindowPrefixAndSources(t *testing.T) {
	c, key, path := responsesDiagnosticFixture(t, 4)
	c.Request.Header.Set("Authorization", "private-bearer")
	c.Request.Header.Set("session-id", "private-session")
	c.Request.Header.Set(openAIWSTurnStateHeader, "private-turn-state")
	c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"session_id":"private-session","thread_id":"private-thread","other":"private-header-data"}`)
	observer := NewResponsesCacheDiagnosticObserver(c, key)
	require.NotNil(t, observer)
	// The gateway can synthesize new identity headers while proxying. Later
	// frames on the same socket still carry the original handshake evidence.
	c.Request.Header.Set("session-id", "synthetic-gateway-session")
	c.Request.Header.Set("thread-id", "synthetic-gateway-thread")

	body := []byte(`{"type":"response.create","response":{"model":"private-public-alias","instructions":"private-instruction","tools":[{"type":"function","name":"private-tool"}],"input":[{"type":"message","role":"user","content":"private-user"},{"type":"message","role":"developer","content":"private-policy"}],"client_metadata":{"session_id":"private-session","x-codex-turn-state":"private-turn-state","x-codex-turn-metadata":"{\"thread_id\":\"private-thread\"}"},"metadata":{"user_id":"private-person"},"prompt_cache_key":"private-routing-key"}}`)
	first := observer.Begin(body, responsesDiagnosticTestModel, "ws")
	require.NotNil(t, first)
	first.RecordAttempt(10, responsesDiagnosticTestModel)
	first.RecordAttempt(11, responsesDiagnosticTestModel)
	first.RecordAttempt(11, responsesDiagnosticTestModel)
	first.Finish(11, &OpenAIForwardResult{Model: "private-public-alias", UpstreamModel: responsesDiagnosticTestModel, UpstreamResponseModel: "private-build", ResponseID: "private-response-id", UpstreamTerminalEvent: "response.completed", Usage: OpenAIUsage{InputTokens: 300, CacheReadInputTokens: 240, OutputTokens: 12}}, nil)
	first.RecordAttempt(99, responsesDiagnosticTestModel)
	first.Finish(99, nil, errors.New("private-error"))

	next := []byte(`{"type":"response.create","instructions":"private-instruction-2","tools":[{"name":"private-tool","type":"function"}],"input":[{"content":"private-user","role":"user","type":"message"},{"type":"message","role":"developer","content":"private-policy-change"},{"role":"user","content":"private-next"}],"previous_response_id":"private-response-id","conversation":{"id":"private-session"}}`)
	second := observer.Begin(next, responsesDiagnosticTestModel, "ws")
	require.NotNil(t, second)
	second.Finish(11, &OpenAIForwardResult{Model: responsesDiagnosticTestModel, UpstreamResponseModel: responsesDiagnosticTestModel, UpstreamTerminalEvent: "response.incomplete"}, nil)

	events := responsesDiagnosticRecords(t, path)
	require.Len(t, events, 2)
	a, b := events[0], events[1]
	for _, e := range events {
		require.NotContains(t, e.Raw, "private-")
		require.NotContains(t, e.Raw, "synthetic-gateway")
		require.False(t, e.Get("identity.header:thread-id").Exists())
	}
	require.Equal(t, a.Get("window_id").Str, b.Get("window_id").Str)
	require.NotEmpty(t, a.Get("window_id").Str)
	require.Equal(t, a.Get("ws_connection_sample").Int(), b.Get("ws_connection_sample").Int())
	require.Positive(t, a.Get("ws_connection_sample").Int())
	require.Equal(t, "body", a.Get("request_model_source").Str)
	require.False(t, a.Get("request_model_match").Bool())
	require.NotEmpty(t, a.Get("request_model_hmac").Str)
	require.Equal(t, "ws_inherited", b.Get("request_model_source").Str)
	require.Equal(t, responsesDiagnosticTestModel, a.Get("selected_model").Str)
	require.Equal(t, responsesDiagnosticTestModel, a.Get("sent_model").Str)
	require.Equal(t, "different", a.Get("reported_model_status").Str)
	require.False(t, a.Get("reported_model").Exists())
	require.Equal(t, "response.incomplete", b.Get("terminal_event").Str)
	require.Equal(t, "forward_returned", b.Get("status").Str)
	require.Equal(t, "zero_or_unreported", b.Get("usage_state").Str)
	require.EqualValues(t, 300, a.Get("usage.input_tokens").Int())
	require.EqualValues(t, 240, a.Get("usage.cached_tokens").Int())
	require.EqualValues(t, 3, a.Get("attempts").Int())
	require.EqualValues(t, 1, a.Get("account_switches").Int())
	require.JSONEq(t, "[10,11,11]", a.Get("attempt_accounts").Raw)
	require.EqualValues(t, 11, a.Get("account_id").Int())
	session := a.Get("identity.header:session-id.hmac").Str
	require.NotEmpty(t, session)
	require.Equal(t, session, a.Get("identity.body:client_metadata:session_id.hmac").Str)
	require.Equal(t, session, b.Get("identity.header:session-id.hmac").Str)
	require.Equal(t, session, b.Get("identity.body:conversation.hmac").Str)
	state := a.Get("identity.header:x-codex-turn-state.hmac").Str
	require.NotEmpty(t, state)
	require.Equal(t, state, a.Get("identity.body:client_metadata:x-codex-turn-state.hmac").Str)
	require.NotEmpty(t, a.Get("identity.header:turn_metadata:thread_id.hmac").Str)
	require.Equal(t, a.Get("identity.header:turn_metadata:thread_id.hmac").Str, a.Get("identity.body:client_metadata:turn_metadata:thread_id.hmac").Str)
	require.Equal(t, "no_verified_session", a.Get("identity.body:claude_metadata_session.state").Str)
	require.False(t, a.Get("identity.body:claude_metadata_session.hmac").Exists())
	require.NotEmpty(t, a.Get("response_id_hmac").Str)
	require.Equal(t, a.Get("response_id_hmac").Str, b.Get("identity.body:previous_response_id.hmac").Str)
	require.Len(t, b.Get("input.items").Array(), 3)
	require.NotEqual(t, a.Get("input.items.0.hmac").Str, b.Get("input.items.0.hmac").Str)
	require.NotEmpty(t, a.Get("input.items.0.canonical_hmac").Str)
	require.Equal(t, a.Get("input.items.0.canonical_hmac").Str, b.Get("input.items.0.canonical_hmac").Str)
	require.NotEqual(t, a.Get("input.items.1.canonical_hmac").Str, b.Get("input.items.1.canonical_hmac").Str)
	require.NotEqual(t, a.Get("fields.instructions.canonical_hmac").Str, b.Get("fields.instructions.canonical_hmac").Str)
	require.Equal(t, a.Get("fields.tools.canonical_hmac").Str, b.Get("fields.tools.canonical_hmac").Str)
	require.NotEqual(t, a.Get("fields.tools.hmac").Str, b.Get("fields.tools.hmac").Str)
}

func TestResponsesCacheDiagnosticScopeQuotaAndMissingResult(t *testing.T) {
	c, key, path := responsesDiagnosticFixture(t, 6)
	observer := NewResponsesCacheDiagnosticObserver(c, key)
	require.NotNil(t, observer)
	require.Nil(t, NewResponsesCacheDiagnosticObserver(c, &APIKey{UserID: key.UserID, ID: key.ID + 1}))
	require.Nil(t, NewResponsesCacheDiagnosticObserver(c, &APIKey{UserID: key.UserID + 1, ID: key.ID}))
	for _, tc := range []struct{ body, model, transport string }{
		{`{"model":"gpt-6.2-astra","input":"elsewhere"}`, "gpt-6.2-astra", "http"},
		{`{"model":"gpt-5.6-sol","input":[]}`, "claude-opus-4-7", "ws"},
		{`{"model":"gpt-5.6-sol","model":"secret"}`, responsesDiagnosticTestModel, "http"},
		{`{"model":"gpt-5.6-sol","Model":"secret"}`, responsesDiagnosticTestModel, "ws"},
		{`{"model":"gpt-5.6-sol","Instructions":"untracked prefix"}`, responsesDiagnosticTestModel, "http"},
		{`{"type":"response.cancel","model":"gpt-5.6-sol"}`, responsesDiagnosticTestModel, "ws"},
		{`{"response":{"model":"gpt-5.6-sol"},"model":"unknown"}`, responsesDiagnosticTestModel, "ws"},
		{`{"model":"gpt-5.6-sol","generate":false}`, responsesDiagnosticTestModel, "ws"},
		{`{"model":"gpt-5.6-sol","input":[{"type":"compaction_trigger"}]}`, responsesDiagnosticTestModel, "ws"},
		{`{"model":"gpt-5.6-sol","tool_choice":{"type":"image_generation"}}`, responsesDiagnosticTestModel, "http"},
		{`{"input":"alone"}`, responsesDiagnosticTestModel, "http"},
		{`{"model":"gpt-5.6-sol"}`, responsesDiagnosticTestModel, "unknown"},
		{`{"model":"gpt-5.6-sol"`, responsesDiagnosticTestModel, "ws"},
	} {
		require.Nil(t, observer.Begin([]byte(tc.body), tc.model, tc.transport), tc.body)
	}
	normal := []byte(`{"model":"gpt-5.6-sol","input":"private question","tools":[{"type":"image_generation"}]}`)
	for _, rejected := range []*OpenAIForwardResult{
		{Model: responsesDiagnosticTestModel, UpstreamModel: "unapproved-secret"},
		{Model: responsesDiagnosticTestModel, UpstreamModel: " " + responsesDiagnosticTestModel},
		{Model: responsesDiagnosticTestModel, ImageCount: 1},
		{Model: responsesDiagnosticTestModel, Usage: OpenAIUsage{ImageOutputTokens: 4}},
	} {
		sample := observer.Begin(normal, responsesDiagnosticTestModel, "http")
		require.NotNil(t, sample)
		sample.Finish(99, rejected, nil)
	}
	sample := observer.Begin(normal, responsesDiagnosticTestModel, "http")
	require.NotNil(t, sample)
	sample.RecordAttempt(15, responsesDiagnosticTestModel)
	sample.Finish(15, nil, fmt.Errorf("do not publish this error: %w", context.DeadlineExceeded))
	sample = observer.Begin(normal, responsesDiagnosticTestModel, "http")
	require.NotNil(t, sample)
	sample.Finish(16, &OpenAIForwardResult{Model: responsesDiagnosticTestModel, Usage: OpenAIUsage{InputTokens: 85}}, nil)
	require.Nil(t, observer.Begin(normal, responsesDiagnosticTestModel, "ws"))
	events := responsesDiagnosticRecords(t, path)
	require.Len(t, events, 2)
	require.Equal(t, "deadline", events[0].Get("status").Str)
	require.Equal(t, "selected_only", events[0].Get("model_evidence").Str)
	require.False(t, events[0].Get("sent_model").Exists())
	require.False(t, events[0].Get("usage").Exists())
	require.False(t, events[1].Get("ws_connection_sample").Exists())
	require.Empty(t, events[1].Get("identity").Map(), "anonymous, with no invented session ID")
	for _, e := range events {
		require.NotContains(t, e.Raw, "private question")
		require.NotContains(t, e.Raw, "do not publish")
	}
}

func TestResponsesCacheDiagnosticRequiresDedicatedConfigurationAndAuthentication(t *testing.T) {
	c, key, _ := responsesDiagnosticFixture(t, 1)
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_USER_ID", strconv.FormatInt(key.UserID, 10))
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MODEL", responsesDiagnosticTestModel)
	for _, tc := range []struct{ setting, value string }{
		{"USER_ID", ""}, {"USER_ID", "555"}, {"API_KEY_ID", ""}, {"API_KEY_ID", "99"},
		{"MODELS", ""}, {"MODELS", "*"}, {"MODELS", "claude-opus-4-7"}, {"MODELS", "gpt-image-2"},
		{"MODELS", "provider/" + responsesDiagnosticTestModel}, {"MODELS", responsesDiagnosticTestModel + "," + responsesDiagnosticTestModel},
		{"UNTIL", ""}, {"UNTIL", time.Now().Add(-time.Minute).Format(time.RFC3339)},
		{"UNTIL", time.Now().Add(61 * time.Minute).Format(time.RFC3339)}, {"PATH", "relative.jsonl"},
		{"MAX_REQUESTS", "-1"}, {"MAX_REQUESTS", "101"}, {"MAX_REQUESTS", "unlimited"},
	} {
		t.Run(tc.setting+"_"+tc.value, func(t *testing.T) {
			t.Setenv(responsesCacheDiagnosticEnv+tc.setting, tc.value)
			require.Nil(t, NewResponsesCacheDiagnosticObserver(c, key))
		})
	}
	require.NotNil(t, NewResponsesCacheDiagnosticObserver(c, key))
	c.Request.URL.Path = "/v1/responses/compact"
	require.Nil(t, NewResponsesCacheDiagnosticObserver(c, key))
	c.Request.URL.Path = "/v1/responses"
	c.Set("api_key", "unauthenticated")
	require.Nil(t, NewResponsesCacheDiagnosticObserver(c, key))
	c.Set("api_key", &APIKey{UserID: key.UserID, ID: key.ID + 1})
	require.Nil(t, NewResponsesCacheDiagnosticObserver(c, key))
}

func TestResponsesCacheDiagnosticFinalFailureUsesLastAccountRoute(t *testing.T) {
	c, key, path := responsesDiagnosticFixture(t, 5)
	o := NewResponsesCacheDiagnosticObserver(c, key)
	body := []byte(`{"model":"gpt-5.6-sol","input":"private question"}`)
	for _, tc := range []struct {
		lastModel string
		account   int64
		result    *OpenAIForwardResult
	}{
		{"", 2, nil},
		{"unapproved-private-model", 2, nil},
		{"gpt-6.2-sol", 3, nil}, // account at Finish did not start the observed attempt
		{"gpt-6.2-sol", 2, nil},
		{"", 2, &OpenAIForwardResult{Model: responsesDiagnosticTestModel, UpstreamModel: "gpt-6.2-sol"}},
	} {
		d := o.Begin(body, responsesDiagnosticTestModel, "ws")
		require.NotNil(t, d)
		d.RecordAttempt(1, responsesDiagnosticTestModel)
		d.RecordAttempt(2, tc.lastModel)
		d.Finish(tc.account, tc.result, errors.New("private upstream failure"))
	}
	events := responsesDiagnosticRecords(t, path)
	require.Len(t, events, 2)
	require.Equal(t, "selected_only", events[0].Get("model_evidence").Str)
	require.Equal(t, responsesDiagnosticTestModel, events[0].Get("selected_model").Str)
	require.Equal(t, "gpt-6.2-sol", events[0].Get("final_selected_model").Str)
	require.False(t, events[0].Get("sent_model").Exists())
	require.Equal(t, "forward_result", events[1].Get("model_evidence").Str)
	require.Equal(t, "gpt-6.2-sol", events[1].Get("sent_model").Str)
	require.False(t, events[1].Get("final_selected_model").Exists())
	for _, event := range events {
		require.NotContains(t, event.Raw, "private")
	}
}

func TestResponsesCacheDiagnosticConnectionAndClaudeCarriers(t *testing.T) {
	c, key, path := responsesDiagnosticFixture(t, 5)
	const id = "2d68811e-723a-4b28-a273-2a2ee65c164d"
	c.Request.Header.Set(claudeCodeSessionHeader, id)
	body := []byte(`{"model":"gpt-5.6-sol","metadata":{"user_id":"{\"session_id\":\"` + id + `\",\"account_uuid\":\"private-account\"}"},"input":"private-content"}`)
	send := func(o *ResponsesCacheDiagnosticObserver, input []byte, transport string) {
		sample := o.Begin(input, responsesDiagnosticTestModel, transport)
		require.NotNil(t, sample)
		sample.Finish(1, &OpenAIForwardResult{Model: responsesDiagnosticTestModel}, nil)
	}
	first := NewResponsesCacheDiagnosticObserver(c, key)
	send(first, body, "ws")
	send(first, body, "ws")
	reconnected := NewResponsesCacheDiagnosticObserver(c, key)
	send(reconnected, body, "ws")
	send(NewResponsesCacheDiagnosticObserver(c, key), body, "http")
	c.Request.Header.Add(claudeCodeSessionHeader, "second-private-value")
	ambiguous := []byte(`{"model":"gpt-5.6-sol","metadata":{"user_id":"{\"session_id\":\"not-canonical\"}"},"conversation":{"id":"partial-id","ID":"other-id"}}`)
	send(NewResponsesCacheDiagnosticObserver(c, key), ambiguous, "http")
	ev := responsesDiagnosticRecords(t, path)
	require.Len(t, ev, 5)
	for _, event := range ev {
		require.NotContains(t, event.Raw, id)
		require.NotContains(t, event.Raw, "private-")
	}
	for _, event := range ev[:4] {
		header := event.Get("identity.header:x-claude-code-session-id.hmac").Str
		require.NotEmpty(t, header)
		require.Equal(t, header, event.Get("identity.body:claude_metadata_session.hmac").Str)
	}
	require.Equal(t, ev[0].Get("ws_connection_sample").Int(), ev[1].Get("ws_connection_sample").Int())
	require.NotEqual(t, ev[1].Get("ws_connection_sample").Int(), ev[2].Get("ws_connection_sample").Int())
	require.False(t, ev[3].Get("ws_connection_sample").Exists())
	for _, source := range []string{"header:x-claude-code-session-id", "body:claude_metadata_session", "body:conversation"} {
		require.Equal(t, "invalid", ev[4].Get("identity."+source+".state").Str)
		require.False(t, ev[4].Get("identity."+source+".hmac").Exists())
	}
}

func TestResponsesCacheDiagnosticWSFrameResponseID(t *testing.T) {
	c, key, path := responsesDiagnosticFixture(t, 8)
	o := NewResponsesCacheDiagnosticObserver(c, key)
	observed := "resp_private_" + strings.Repeat("a", 248) // longest protocol response ID
	body := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"` + observed + `"}`)
	for _, tc := range []struct {
		transport             string
		wsMode                bool
		requestID, responseID string
	}{
		{"ws", true, observed, ""},
		{"ws", true, "req_private_edge", ""},
		{"ws", true, "msg_private_item", ""},
		{"ws", false, observed, ""},
		{"http", true, observed, ""},
		{"http", false, observed, ""},
		{"ws", true, "resp_other", observed},
		{"http", true, "req_private_edge", observed},
	} {
		sample := o.Begin(body, responsesDiagnosticTestModel, tc.transport)
		require.NotNil(t, sample)
		sample.Finish(1, &OpenAIForwardResult{Model: responsesDiagnosticTestModel,
			RequestID: tc.requestID, ResponseID: tc.responseID, OpenAIWSMode: tc.wsMode}, nil)
	}
	events := responsesDiagnosticRecords(t, path)
	require.Len(t, events, 8)
	want := events[0].Get("identity.body:previous_response_id.hmac").Str
	require.NotEmpty(t, want)
	for i, event := range events {
		require.NotContains(t, event.Raw, "private")
		value := event.Get("response_id_hmac")
		if i == 0 || i >= 6 {
			require.Equal(t, want, value.Str)
		} else {
			require.False(t, value.Exists())
		}
	}
}

func TestResponsesCacheDiagnosticWindowsAndFileBounds(t *testing.T) {
	t.Run("rotation", func(t *testing.T) {
		c, key, path := responsesDiagnosticFixture(t, 15)
		c.Request.Header.Set("session-id", "same-session")
		body := []byte(`{"model":"gpt-5.6-sol","input":"same-prefix"}`)
		old := NewResponsesCacheDiagnosticObserver(c, key)
		old.Begin(body, responsesDiagnosticTestModel, "ws").Finish(1, &OpenAIForwardResult{Model: responsesDiagnosticTestModel}, nil)
		late := old.Begin(body, responsesDiagnosticTestModel, "ws")
		before := responsesDiagnosticRecords(t, path)[0]
		t.Setenv(responsesCacheDiagnosticEnv+"UNTIL", time.Now().Add(15*time.Minute).UTC().Format(time.RFC3339))
		late.Finish(1, nil, errors.New("obsolete"))
		require.Nil(t, old.Begin(body, responsesDiagnosticTestModel, "ws"))
		now := NewResponsesCacheDiagnosticObserver(c, key)
		sample := now.Begin(body, responsesDiagnosticTestModel, "ws")
		require.NotNil(t, sample)
		sample.Finish(1, &OpenAIForwardResult{Model: responsesDiagnosticTestModel}, nil)
		events := responsesDiagnosticRecords(t, path)
		require.Len(t, events, 2)
		require.NotEqual(t, before.Get("window_id").Str, events[1].Get("window_id").Str)
		require.NotEqual(t, before.Get("identity.header:session-id.hmac").Str, events[1].Get("identity.header:session-id.hmac").Str)
		require.NotEqual(t, before.Get("input.items.0.hmac").Str, events[1].Get("input.items.0.hmac").Str)
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	})
	t.Run("expires while in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c, key, path := responsesDiagnosticFixture(t, 2)
			observer := NewResponsesCacheDiagnosticObserver(c, key)
			body := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
			sample := observer.Begin(body, responsesDiagnosticTestModel, "http")
			require.NotNil(t, sample)
			time.Sleep(10 * time.Minute)
			sample.Finish(3, nil, errors.New("expired"))
			require.Nil(t, observer.Begin(body, responsesDiagnosticTestModel, "http"))
			require.NoFileExists(t, path)
		})
	})
	for _, kind := range []string{"public-file", "symlink", "file-full", "public-parent"} {
		t.Run(kind, func(t *testing.T) {
			c, key, path := responsesDiagnosticFixture(t, 2)
			const untouched = "do not modify"
			target := path
			switch kind {
			case "symlink":
				target = filepath.Join(filepath.Dir(path), "real")
				require.NoError(t, os.WriteFile(target, []byte(untouched), 0600))
				require.NoError(t, os.Symlink(target, path))
			case "public-file":
				require.NoError(t, os.WriteFile(path, []byte(untouched), 0600))
				require.NoError(t, os.Chmod(path, 0644))
			case "file-full":
				require.NoError(t, os.WriteFile(path, nil, 0600))
				require.NoError(t, os.Truncate(path, userCacheDiagnosticMaxFile))
			case "public-parent":
				require.NoError(t, os.Chmod(filepath.Dir(path), 0755))
			}
			o := NewResponsesCacheDiagnosticObserver(c, key)
			o.Begin([]byte(`{"model":"gpt-5.6-sol"}`), responsesDiagnosticTestModel, "http").Finish(1, nil, errors.New("unknown"))
			switch kind {
			case "file-full":
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.EqualValues(t, userCacheDiagnosticMaxFile, info.Size())
			case "public-parent":
				require.NoFileExists(t, path)
			default:
				raw, err := os.ReadFile(target)
				require.NoError(t, err)
				require.Equal(t, untouched, string(raw))
			}
		})
	}
}

func TestResponsesCacheDiagnosticAdmissionConcurrency(t *testing.T) {
	const limit = 7
	c, key, path := responsesDiagnosticFixture(t, limit)
	o := NewResponsesCacheDiagnosticObserver(c, key)
	require.NotNil(t, o)
	body := []byte(`{"model":"gpt-5.6-sol","input":["a"]}`)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			transport := "http"
			if i%2 == 0 {
				transport = "ws"
			}
			d := o.Begin(body, responsesDiagnosticTestModel, transport)
			d.RecordAttempt(1, responsesDiagnosticTestModel)
			wg.Go(func() { d.Finish(1, nil, context.Canceled) })
			d.Finish(1, nil, context.Canceled)
		})
	}
	wg.Wait()
	events := responsesDiagnosticRecords(t, path)
	require.Len(t, events, limit)
	var sequences []int
	for _, e := range events {
		sequences = append(sequences, int(e.Get("sequence").Int()))
	}
	slices.Sort(sequences)
	require.Equal(t, []int{1, 2, 3, 4, 5, 6, 7}, sequences)
}

func TestResponsesCacheDiagnosticInputLimits(t *testing.T) {
	c, key, path := responsesDiagnosticFixture(t, 2)
	o := NewResponsesCacheDiagnosticObserver(c, key)
	body := []byte(`{"model":"gpt-5.6-sol","instructions":"` + strings.Repeat("x", responsesDiagnosticCanonicalBytes) +
		`","input":[` + strings.Repeat(`{"type":"private-unknown-type","role":"private-unknown-role"},`, userCacheDiagnosticMaxItems) + `0]}`)
	d := o.Begin(body, responsesDiagnosticTestModel, "http")
	require.NotNil(t, d)
	d.Finish(1, &OpenAIForwardResult{Model: responsesDiagnosticTestModel}, nil)
	e := responsesDiagnosticRecords(t, path)[0]
	require.Equal(t, "canonical_limit", e.Get("fields.instructions.state").Str)
	require.NotEmpty(t, e.Get("fields.instructions.hmac").Str)
	require.False(t, e.Get("fields.instructions.canonical_hmac").Exists())
	require.Len(t, e.Get("input.items").Array(), userCacheDiagnosticMaxItems)
	require.True(t, e.Get("input.truncated").Bool())
	require.Equal(t, "other", e.Get("input.items.0.type").Str)
	require.NotContains(t, e.Raw, "private-")

	body = []byte(`{"model":"gpt-5.6-sol","input":["small","` + strings.Repeat("x", responsesDiagnosticPrefixBytes) + `"]}`)
	d = o.Begin(body, responsesDiagnosticTestModel, "http")
	require.NotNil(t, d)
	d.Finish(1, &OpenAIForwardResult{Model: responsesDiagnosticTestModel}, nil)
	e = responsesDiagnosticRecords(t, path)[1]
	require.True(t, e.Get("input.truncated").Bool())
	require.NotEmpty(t, e.Get("input.items.0.canonical_hmac").Str)
	require.Equal(t, "limit", e.Get("input.items.1.state").Str)
	require.False(t, e.Get("input.items.1.hmac").Exists())
	require.EqualValues(t, responsesDiagnosticPrefixBytes+2, e.Get("input.items.1.bytes").Int())
}
