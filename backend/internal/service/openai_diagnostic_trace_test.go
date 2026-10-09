package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestDiagnosticTraceDisabledByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv(diagnosticTraceEnabledEnv, "")
	t.Setenv(diagnosticTracePathEnv, path)
	TraceOpenAIRequestIngress(nil, []byte(`{"input":"PROMPT_SECRET"}`))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled trace created a file: %v", err)
	}
	t.Setenv(diagnosticTraceEnabledEnv, "true")
	t.Setenv(diagnosticTracePathEnv, "")
	if _, enabled := diagnosticTraceEnabled(); enabled {
		t.Fatal("trace enabled without a configured path")
	}
}

func TestDiagnosticTraceJSONLRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv(diagnosticTraceEnabledEnv, "true")
	t.Setenv(diagnosticTracePathEnv, path)
	t.Setenv(diagnosticTraceRunEnv, "synthetic-run")
	body := []byte(`{"model":"gpt-6-astra","stream":false,"store":true,"reasoning":{"effort":"xhigh"},"input":[{"role":"user","content":"PROMPT_SECRET"}],"instructions":"INSTRUCTIONS_SECRET","tools":[{"type":"function","name":"TOOL_SECRET"}],"previous_response_id":"PREVIOUS_SECRET","prompt_cache_key":"CACHE_SECRET","metadata":{"secret":"METADATA_SECRET"}}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("X-Request-ID", "REQUEST_ID_SECRET")
	c.Request.Header.Set("X-Client-Request-ID", "CLIENT_REQUEST_ID_SECRET")
	account := &Account{ID: 42, Type: AccountTypeOAuth, Platform: PlatformOpenAI}
	request, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses?secret=QUERY_SECRET", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer TOKEN_SECRET")
	request.Header.Set("session_id", "SESSION_SECRET")
	request.Header.Set("conversation_id", "CONVERSATION_SECRET")
	request.Header.Set("chatgpt-account-id", "ACCOUNT_SECRET")
	request.Header.Set("x-codex-turn-state", "TURN_SECRET")
	request.Header.Set("X-Unlisted-Secret", "UNLISTED_SECRET")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "Codex CLI/1.2.3")
	request.Header.Set("originator", "codex_cli_rs")
	request.Header.Set("version", "1.2.3")
	TraceOpenAIRequestIngress(c, body)
	traceOpenAIInbound(context.Background(), c, account, body)
	traceOpenAIUpstream(context.Background(), c, account, 1, request)
	traceOpenAIUpstreamResult(c, account, 1, &http.Response{StatusCode: http.StatusOK}, nil)
	traceOpenAIUpstreamResult(c, account, 2, nil, errors.New("TRANSPORT_SECRET"))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PROMPT_SECRET", "INSTRUCTIONS_SECRET", "TOOL_SECRET", "PREVIOUS_SECRET", "CACHE_SECRET", "METADATA_SECRET", "REQUEST_ID_SECRET", "CLIENT_REQUEST_ID_SECRET", "TOKEN_SECRET", "SESSION_SECRET", "CONVERSATION_SECRET", "ACCOUNT_SECRET", "TURN_SECRET", "QUERY_SECRET", "UNLISTED_SECRET", "TRANSPORT_SECRET"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("raw secret leaked: %s", secret)
		}
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	if len(lines) != 5 {
		t.Fatalf("expected 5 JSONL events, got %d", len(lines))
	}
	stages := []string{"http_ingress", "forward_entry", "upstream", "upstream_result", "upstream_result"}
	for i, line := range lines {
		var event diagnosticTraceEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("event %d is invalid JSON: %v", i, err)
		}
		if event.Stage != stages[i] || event.Time == "" || event.RunID != "synthetic-run" {
			t.Fatalf("unexpected event %d: %+v", i, event)
		}
		if event.RequestID != diagnosticSHA256([]byte("REQUEST_ID_SECRET")) || event.ClientRequestID != diagnosticSHA256([]byte("CLIENT_REQUEST_ID_SECRET")) {
			t.Fatalf("request correlation hashes missing for event %d", i)
		}
		if i < 3 && (event.BodySHA256 != diagnosticSHA256(body) || event.BodyBytes != len(body)) {
			t.Fatalf("body fingerprint changed for event %d", i)
		}
		if i == 2 {
			if event.PublicHeaders["user-agent"] != "Codex CLI/1.2.3" || event.PublicHeaders["originator"] != "codex_cli_rs" || event.PublicHeaders["version"] != "1.2.3" {
				t.Fatalf("public client identity missing: %+v", event.PublicHeaders)
			}
			if event.URL == nil || event.URL.Host != "chatgpt.com" || event.URL.Path != "/backend-api/codex/responses" {
				t.Fatalf("unexpected URL summary: %+v", event.URL)
			}
			for _, key := range []string{"authorization", "session_id", "conversation_id", "chatgpt-account-id", "x-codex-turn-state"} {
				digest, ok := event.Headers[key]
				if !ok || !digest.Present || digest.Length == 0 || len(digest.Digest) != 64 {
					t.Fatalf("missing digest for %s", key)
				}
			}
			if _, ok := event.Headers["x-unlisted-secret"]; ok {
				t.Fatal("unlisted header was captured")
			}
		}
	}
	remaining, err := io.ReadAll(request.Body)
	if err != nil || !bytes.Equal(remaining, body) {
		t.Fatalf("trace consumed or changed the request body: %v", err)
	}
	if diagnosticHeaderValueDigest("SESSION_SECRET") != diagnosticHeaderValueDigest("SESSION_SECRET") || diagnosticHeaderValueDigest("SESSION_SECRET") == diagnosticHeaderValueDigest("OTHER_SESSION") {
		t.Fatal("header digest is not stable and value-specific in this process")
	}
}

func TestDiagnosticTraceSemanticHashesAndAllowlist(t *testing.T) {
	first := diagnosticSummarizeBody([]byte(`{"input":{"a":1,"b":2},"instructions":"secret","include":["reasoning.encrypted_content","CUSTOM_SECRET"],"store":false,"stream":true}`))
	second := diagnosticSummarizeBody([]byte(` { "stream":true, "store":false, "include": ["reasoning.encrypted_content", "CUSTOM_SECRET"], "instructions":"secret", "input":{"b":2,"a":1} } `))
	if first.SemanticSHA256 == "" || first.SemanticSHA256 != second.SemanticSHA256 {
		t.Fatalf("whole body semantic hashes differ: %q %q", first.SemanticSHA256, second.SemanticSHA256)
	}
	if first.Canonicalization != "sorted-key-compact-json" {
		t.Fatalf("canonicalization label missing: %q", first.Canonicalization)
	}
	for _, key := range []string{"input", "instructions", "include"} {
		if first.FieldSHA256[key] == "" || first.FieldSHA256[key] != second.FieldSHA256[key] {
			t.Fatalf("field %s semantic hashes differ", key)
		}
	}
	if len(first.IncludeValues) != 1 || first.IncludeValues[0] != "reasoning.encrypted_content" {
		t.Fatalf("include allowlist failed: %+v", first.IncludeValues)
	}
	encoded, err := json.Marshal(first)
	if err != nil || bytes.Contains(encoded, []byte("CUSTOM_SECRET")) || bytes.Contains(encoded, []byte("secret")) {
		t.Fatalf("raw field content leaked in summary: %s (%v)", encoded, err)
	}
	if diagnosticFieldSHA256(map[string]json.RawMessage{"input": json.RawMessage(`{"a":1,"b":2}`)})["input"] != diagnosticFieldSHA256(map[string]json.RawMessage{"input": json.RawMessage(`{"b":2,"a":1}`)})["input"] {
		t.Fatal("nested object order changes field semantic hash")
	}
	unicodeFirst := diagnosticCanonicalSHA256([]byte(`{"input":"中文 <tag> & value","n":1.2300}`))
	unicodeSecond := diagnosticCanonicalSHA256([]byte(`{"n":1.2300,"input":"中文 <tag> & value"}`))
	if unicodeFirst == "" || unicodeFirst != unicodeSecond {
		t.Fatalf("canonical hash is not stable for UTF-8/HTML-sensitive content: %q %q", unicodeFirst, unicodeSecond)
	}
	if unicodeFirst != diagnosticSHA256([]byte(`{"input":"中文 <tag> & value","n":1.2300}`)) {
		t.Fatal("canonical hash does not match compact ensure_ascii=false JSON bytes")
	}
}

func TestDiagnosticPublicHeadersRejectOpaqueValues(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", strings.Repeat("x", 129))
	headers.Set("originator", "bearer TOKEN_SECRET")
	headers.Set("version", "1.2.3\nTOKEN_SECRET")
	if got := diagnosticPublicHeaders(headers); len(got) != 0 {
		t.Fatalf("opaque/overlong values logged as public: %+v", got)
	}
}

func TestDiagnosticTracePassthroughHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passthrough.jsonl")
	t.Setenv(diagnosticTraceEnabledEnv, "true")
	t.Setenv(diagnosticTracePathEnv, path)
	body := []byte(`{"model":"gpt-5.4","stream":false,"input":"PROMPT_SECRET"}`)
	c, recorder := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_trace","status":"completed","output":[],"usage":{}}`)),
	}}
	svc := openAIClientToolsTestService(upstream)
	account := &Account{ID: 6241, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "TOKEN_SECRET"}}
	_, err := svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "gpt-5.4", false, nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = recorder
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("TOKEN_SECRET")) || bytes.Contains(raw, []byte("PROMPT_SECRET")) {
		t.Fatal("passthrough trace leaked raw content")
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("expected upstream and result events, got %d", len(lines))
	}
	var sent, result diagnosticTraceEvent
	if err := json.Unmarshal(lines[0], &sent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lines[1], &result); err != nil {
		t.Fatal(err)
	}
	if sent.Stage != "upstream" || sent.Attempt != 1 || sent.Body.FieldSHA256["input"] == "" || result.Stage != "upstream_result" || result.Attempt != 1 || result.Status != http.StatusOK {
		t.Fatalf("passthrough hooks did not record both boundaries: %+v %+v", sent, result)
	}
}

func TestDiagnosticTraceStreamStoreIndependent(t *testing.T) {
	for _, tc := range []struct {
		body          string
		stream, store bool
	}{
		{`{"stream":false,"store":true}`, false, true},
		{`{"stream":true,"store":false}`, true, false},
	} {
		summary := diagnosticSummarizeBody([]byte(tc.body))
		if summary.Stream == nil || summary.Store == nil || *summary.Stream != tc.stream || *summary.Store != tc.store || summary.Stream == summary.Store {
			t.Fatalf("stream/store lost independent values for %s", tc.body)
		}
	}
	absent := diagnosticSummarizeBody([]byte(`{"input":"hello"}`))
	if absent.Stream != nil || absent.Store != nil {
		t.Fatal("absent fields acquired values")
	}
	invalid := diagnosticSummarizeBody([]byte(`{"stream":"true","store":0}`))
	if invalid.Stream != nil || invalid.Store != nil {
		t.Fatal("invalid field types acquired boolean values")
	}
	nullValues := diagnosticSummarizeBody([]byte(`{"stream":null,"store":null}`))
	if nullValues.Stream != nil || nullValues.Store != nil {
		t.Fatal("null fields acquired boolean values")
	}
	encoded, err := json.Marshal(diagnosticSummarizeBody([]byte(`{"stream":false,"store":true}`)))
	if err != nil || !strings.Contains(string(encoded), `"stream":false`) || !strings.Contains(string(encoded), `"store":true`) {
		t.Fatalf("false/true values were omitted from JSON: %s (%v)", encoded, err)
	}
}
