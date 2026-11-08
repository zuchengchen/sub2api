package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func userCacheDiagnosticTestEnv(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_USER_ID", "428")
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_PATH", path)
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_UNTIL", time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339))
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "30")
	return path
}

func userCacheDiagnosticTestContext(userID int64) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, "REQUEST_SECRET"))
	c.Request.Header.Set("Session-Id", "SESSION_SECRET")
	c.Set("api_key", &APIKey{ID: 794, UserID: userID})
	return c
}

func userCacheDiagnosticReadEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var events []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var event map[string]any
		require.NoError(t, json.Unmarshal(line, &event))
		events = append(events, event)
	}
	return events
}

func TestUserCacheDiagnosticAdmissionBoundaries(t *testing.T) {
	userCacheDiagnosticTestEnv(t)
	now := time.Now()
	for _, tc := range []struct{ name, key, value string }{
		{"disabled", "SUB2_USER_CACHE_DIAGNOSTIC_USER_ID", ""},
		{"missing expiry", "SUB2_USER_CACHE_DIAGNOSTIC_UNTIL", ""},
		{"invalid expiry", "SUB2_USER_CACHE_DIAGNOSTIC_UNTIL", "forever"},
		{"expired", "SUB2_USER_CACHE_DIAGNOSTIC_UNTIL", now.Add(-time.Minute).Format(time.RFC3339)},
		{"expiry too distant", "SUB2_USER_CACHE_DIAGNOSTIC_UNTIL", now.Add(2 * time.Hour).Format(time.RFC3339)},
		{"relative path", "SUB2_USER_CACHE_DIAGNOSTIC_PATH", "trace.jsonl"},
		{"missing path", "SUB2_USER_CACHE_DIAGNOSTIC_PATH", ""},
		{"invalid count", "SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "many"},
		{"unbounded count", "SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "101"},
		{"zero count", "SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			var m userCacheDiagnosticManager
			require.Nil(t, m.admit(428, now))
		})
	}
	var manager userCacheDiagnosticManager
	require.Nil(t, manager.admit(429, now))
	require.Nil(t, manager.admit(0, now))
	require.NotNil(t, manager.admit(428, now))
	require.Nil(t, manager.admit(428, now.Add(11*time.Minute)))
}

func TestUserCacheDiagnosticAdmissionConcurrentLimit(t *testing.T) {
	userCacheDiagnosticTestEnv(t)
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "3")
	var manager userCacheDiagnosticManager
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if manager.admit(428, time.Now()) != nil {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 3, admitted.Load())
}

func TestUserCacheDiagnosticMemoizesAdmissionAndFiltersUsers(t *testing.T) {
	path := userCacheDiagnosticTestEnv(t)
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "1")
	require.Nil(t, beginUserCacheDiagnostic(userCacheDiagnosticTestContext(429), []byte(`{"input":"OTHER_SECRET"}`)))
	c := userCacheDiagnosticTestContext(428)
	d := beginUserCacheDiagnostic(c, []byte(`{"messages":[{"role":"user","content":"PROMPT_SECRET"}]}`))
	require.NotNil(t, d)
	require.Same(t, d, beginUserCacheDiagnostic(c, []byte(`{}`)))
	require.Nil(t, beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(`{}`)))
	events := userCacheDiagnosticReadEvents(t, path)
	require.Len(t, events, 1)
	require.Equal(t, diagnosticSHA256([]byte("client:REQUEST_SECRET")), events[0]["usage_request_id_sha256"])
	require.Equal(t, diagnosticSHA256([]byte("SESSION_SECRET")), events[0]["session_sha256"])
}

func TestUserCacheDiagnosticHTTPHookPreservesResponseAndRedacts(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(strconvBoolForUserCacheTest(streaming), func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			body := []byte(`{"model":"gpt-6-astra","messages":[{"role":"system","content":"SYSTEM_SECRET"},{"role":"user","content":"PROMPT_SECRET"}],"tools":[{"type":"function","function":{"name":"TOOL_SECRET","parameters":{"type":"object"}}}],"stream":` + strconvBoolForUserCacheTest(streaming) + `}`)
			terminal := `{"type":"response.completed","response":{"id":"RESPONSE_SECRET","model":"gpt-6-astra","status":"completed","output":[{"type":"reasoning","id":"REASON_ID_SECRET","summary":[{"type":"summary_text","text":"SUMMARY_SECRET"}],"encrypted_content":"ENCRYPTED_SECRET"},{"type":"message","content":[{"type":"output_text","text":"EXPECTED_ANSWER"}]}],"usage":{"input_tokens":10000,"input_tokens_details":{"cached_tokens":9500},"output_tokens":300,"output_tokens_details":{"reasoning_tokens":250},"total_tokens":10300,"unknown":"USAGE_SECRET"}}}`
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + terminal + "\n\ndata: [DONE]\n\n"))}}
			svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
			account := newCodexSynthTestAccount(23157, AccountTypeOAuth)
			c := userCacheDiagnosticTestContext(428)
			result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			require.NoError(t, err)
			require.Equal(t, 10000, result.Usage.InputTokens)
			require.Equal(t, 9500, result.Usage.CacheReadInputTokens)
			require.Equal(t, 300, result.Usage.OutputTokens)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			for _, secret := range []string{"SYSTEM_SECRET", "PROMPT_SECRET", "TOOL_SECRET", "RESPONSE_SECRET", "REASON_ID_SECRET", "SUMMARY_SECRET", "ENCRYPTED_SECRET", "USAGE_SECRET", "REQUEST_SECRET", "SESSION_SECRET", "oauth-token", "EXPECTED_ANSWER"} {
				require.NotContains(t, string(raw), secret)
			}
			events := userCacheDiagnosticReadEvents(t, path)
			require.Len(t, events, 3)
			require.Equal(t, "ingress", events[0]["stage"])
			require.Equal(t, "upstream_request", events[1]["stage"])
			require.Equal(t, "upstream_response", events[2]["stage"])
			require.Equal(t, "response.completed", events[2]["terminal"])
			usage := events[2]["usage"].(map[string]any)["response.usage"].(map[string]any)
			require.EqualValues(t, 10000, usage["input_tokens"])
			require.EqualValues(t, 9500, usage["input_tokens_details.cached_tokens"])
			require.EqualValues(t, 250, usage["output_tokens_details.reasoning_tokens"])
			item := events[2]["output_items"].(map[string]any)["0"].(map[string]any)
			require.Equal(t, "reasoning", item["type"])
			require.NotNil(t, item["encrypted_content"])
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		})
	}
}

func strconvBoolForUserCacheTest(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func TestUserCacheDiagnosticObserverFragmentationLimitsAndPassthrough(t *testing.T) {
	path := userCacheDiagnosticTestEnv(t)
	d := beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(`{}`))
	oversized := "data: {\"text\":\"" + strings.Repeat("x", userCacheDiagnosticMaxLine) + "\"}\n\n"
	terminal := `data: {"type":"response.completed","response":{"output":[],"usage":{"input_tokens":17,"input_tokens_details":{"cached_tokens":8}}}}` + "\n\n"
	wire := oversized + terminal
	o := &userCacheResponseObserver{ReadCloser: io.NopCloser(strings.NewReader(wire)), diagnostic: d, accountID: 1, status: 200, attempt: 1, streaming: true, items: map[string]userCacheItemSummary{}, itemRanks: map[string]int{}}
	var received bytes.Buffer
	buf := make([]byte, 37)
	_, err := io.CopyBuffer(&received, o, buf)
	require.NoError(t, err)
	require.NoError(t, o.Close())
	require.Equal(t, wire, received.String())
	events := userCacheDiagnosticReadEvents(t, path)
	require.Len(t, events, 2)
	require.EqualValues(t, 1, events[1]["truncated_events"])
	require.Equal(t, "response.completed", events[1]["terminal"])
}

func TestUserCacheDiagnosticRetryAndStorageBounds(t *testing.T) {
	path := userCacheDiagnosticTestEnv(t)
	d := beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(`{}`))
	a := &Account{ID: 1}
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(`{"input":"PROMPT_SECRET"}`)), nil
		}
		attempt := d.upstreamRequest(a, req)
		resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":5}}`))}
		attempt.upstreamResponse(resp, nil)
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, `{"usage":{"input_tokens":5}}`, string(raw))
		require.NoError(t, resp.Body.Close())
	}
	require.Len(t, userCacheDiagnosticReadEvents(t, path), 9)
	file, err := os.OpenFile(path, os.O_WRONLY, 0600)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(userCacheDiagnosticMaxFile))
	require.NoError(t, file.Close())
	(&userCacheDiagnostic{config: d.config}).write("must_not_append", 1, 1, "", nil)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.EqualValues(t, userCacheDiagnosticMaxFile, info.Size())
	d.config.until = time.Now().Add(-time.Second)
	d.write("expired", 1, 1, "", nil)
	info, err = os.Stat(path)
	require.NoError(t, err)
	require.EqualValues(t, userCacheDiagnosticMaxFile, info.Size())
}

func TestUserCacheDiagnosticUnsafeAndUnavailableDestination(t *testing.T) {
	for _, kind := range []string{"symlink", "public_file", "missing_parent"} {
		t.Run(kind, func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			switch kind {
			case "symlink":
				target := path + ".target"
				require.NoError(t, os.WriteFile(target, []byte("unchanged"), 0600))
				require.NoError(t, os.Symlink(target, path))
			case "public_file":
				require.NoError(t, os.WriteFile(path, []byte("unchanged"), 0644))
				require.NoError(t, os.Chmod(path, 0644))
			case "missing_parent":
				path = filepath.Join(path, "missing", "trace.jsonl")
				t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_PATH", path)
			}
			d := beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(`{"input":"SECRET"}`))
			require.NotNil(t, d)
			if kind != "missing_parent" {
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, "unchanged", string(raw))
			}
		})
	}
}

// This test calibrates the event parser with legitimate multiline and escaped
// content, and with a malformed event. A parser error must be visible evidence.
func TestUserCacheDiagnosticMultilineRawNumbersAndMalformedEvidence(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(strconvBoolForUserCacheTest(streaming), func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			d := beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(`{}`))
			req := httptest.NewRequest(http.MethodPost, "/responses", nil)
			a := d.upstreamRequest(&Account{ID: 2}, req)
			body := "{\n\"id\":\"private-response\",\"status\":\"completed\",\n\"output\":[{\"type\":\"reasoning\",\"id\":\"secret-id\",\"encrypted_content\":\"opaque\\nprivate\"}],\n\"usage\":{\"input_tokens\":9007199254740993,\"output_tokens\":1.2500,\"total_tokens\":1e4,\"unknown\":\"secret-usage\"}\n}"
			contentType := "application/json"
			wire := body
			if streaming {
				contentType = "text/event-stream; charset=utf-8"
				event := "{\"type\":\"response.completed\",\"response\":" + body + "}"
				wire = ": keepalive\r\nevent: response.completed\r\ndata: " + strings.ReplaceAll(event, "\n", "\r\ndata: ") + "\r\n\r\ndata: {broken}\r\n\r\n"
			}
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(wire))}
			a.upstreamResponse(response, nil)
			var received bytes.Buffer
			for {
				buf := make([]byte, 7)
				n, err := response.Body.Read(buf)
				received.Write(buf[:n])
				if err != nil {
					require.ErrorIs(t, err, io.EOF)
					break
				}
			}
			require.NoError(t, response.Body.Close())
			require.Equal(t, wire, received.String())
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			for _, value := range []string{`9007199254740993`, `1.2500`, `1e4`} {
				require.Contains(t, string(raw), value)
			}
			for _, secret := range []string{"private-response", "secret-id", "opaque", "private", "secret-usage", "unknown"} {
				require.NotContains(t, string(raw), secret)
			}
			events := userCacheDiagnosticReadEvents(t, path)
			require.Len(t, events, 3)
			require.Equal(t, "response.completed", events[2]["terminal"])
			require.Len(t, events[2]["output_items"], 1)
			// A framed terminal candidate freezes the observed prefix; malformed read-ahead
			// is counted separately instead of changing that prefix's validity.
			require.EqualValues(t, 0, events[2]["invalid_json_events"])
			if streaming {
				require.Positive(t, events[2]["post_terminal_bytes"])
			}
			require.Equal(t, false, events[2]["partial_event"])
		})
	}
}

func TestUserCacheDiagnosticLateRetryResponseKeepsIdentity(t *testing.T) {
	path := userCacheDiagnosticTestEnv(t)
	c := userCacheDiagnosticTestContext(428)
	c.Request = c.Request.WithContext(context.Background())
	d := beginUserCacheDiagnostic(c, []byte(`{}`))
	a := d.upstreamRequest(&Account{ID: 10}, httptest.NewRequest(http.MethodPost, "/responses", nil))
	b := d.upstreamRequest(&Account{ID: 20}, httptest.NewRequest(http.MethodPost, "/responses", nil))
	makeResponse := func(id string) *http.Response {
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": []string{id}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":1}}`))}
	}
	first, second := makeResponse("first-secret"), makeResponse("second-secret")
	a.upstreamResponse(first, nil)
	b.upstreamResponse(second, nil)
	_, err := io.ReadAll(second.Body)
	require.NoError(t, err)
	_, err = io.ReadAll(first.Body)
	require.NoError(t, err)
	require.NoError(t, first.Body.Close())
	require.NoError(t, second.Body.Close())
	events := userCacheDiagnosticReadEvents(t, path)
	require.Len(t, events, 5)
	require.EqualValues(t, 2, events[3]["attempt"])
	require.EqualValues(t, 20, events[3]["account_id"])
	require.Equal(t, diagnosticSHA256([]byte("second-secret")), events[3]["usage_request_id_sha256"])
	require.EqualValues(t, 1, events[4]["attempt"])
	require.EqualValues(t, 10, events[4]["account_id"])
	require.Equal(t, diagnosticSHA256([]byte("first-secret")), events[4]["usage_request_id_sha256"])
}

type userCacheDiagnosticBlockedBody struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *userCacheDiagnosticBlockedBody) Read(p []byte) (int, error) {
	close(b.started)
	<-b.closed
	return copy(p, []byte("data: SECRET_LATE_READ\n")), io.ErrClosedPipe
}
func (b *userCacheDiagnosticBlockedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}
func TestUserCacheDiagnosticConcurrentReadClose(t *testing.T) {
	path := userCacheDiagnosticTestEnv(t)
	d := beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(`{}`))
	a := d.upstreamRequest(&Account{ID: 1}, httptest.NewRequest(http.MethodPost, "/responses", nil))
	body := &userCacheDiagnosticBlockedBody{started: make(chan struct{}), closed: make(chan struct{})}
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}
	a.upstreamResponse(response, nil)
	o := response.Body.(*userCacheResponseObserver)
	done := make(chan error, 1)
	go func() {
		_, err := o.Read(make([]byte, 1024))
		done <- err
	}()
	<-body.started
	require.NoError(t, o.Close())
	select {
	case err := <-done:
		require.ErrorIs(t, err, io.ErrClosedPipe)
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock the normal response Read")
	}
	require.NoError(t, o.Close())
	o.mu.Lock()
	require.True(t, o.finished)
	require.Empty(t, o.line)
	require.Empty(t, o.data)
	o.mu.Unlock()
	events := userCacheDiagnosticReadEvents(t, path)
	require.Len(t, events, 3)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "SECRET_LATE_READ")
}

func TestUserCacheDiagnosticIncompleteAndEventLimitEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, wire       string
		limited, partial bool
	}{
		{"partial", `data: {"type":"response.completed","response":{"output":[]}}`, false, true},
		{"event limit", strings.Repeat("data: {}\n\n", userCacheDiagnosticMaxEvents+1), true, false},
		{"byte limit", strings.Repeat(":"+strings.Repeat("x", 1022)+"\n", userCacheDiagnosticMaxBytes/1024+1), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			d := beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(`{}`))
			a := d.upstreamRequest(&Account{ID: 1}, httptest.NewRequest(http.MethodPost, "/responses", nil))
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.wire))}
			a.upstreamResponse(response, nil)
			received, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, tc.wire, string(received))
			require.NoError(t, response.Body.Close())
			events := userCacheDiagnosticReadEvents(t, path)
			require.Len(t, events, 3)
			require.Equal(t, tc.limited, events[2]["observation_limited"])
			require.Equal(t, tc.partial, events[2]["partial_event"])
			require.Empty(t, events[2]["terminal"], "partial or unobserved terminal must not be fabricated")
		})
	}
}

func TestUserCacheDiagnosticDoneItemSurvivesLateAdded(t *testing.T) {
	path := userCacheDiagnosticTestEnv(t)
	d := beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(`{}`))
	a := d.upstreamRequest(&Account{ID: 1}, httptest.NewRequest(http.MethodPost, "/responses", nil))
	wire := "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"OPAQUE_SECRET\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\"}}\n\n" +
		"data: {\"type\":\"response.failed\"}\n\n"
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
	a.upstreamResponse(response, nil)
	_, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	events := userCacheDiagnosticReadEvents(t, path)
	require.Equal(t, "response.failed", events[2]["terminal"])
	item := events[2]["output_items"].(map[string]any)["0"].(map[string]any)
	require.Contains(t, item, "encrypted_content")
}
