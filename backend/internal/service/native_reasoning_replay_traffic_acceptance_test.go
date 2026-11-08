package service_test

// This suite enters through an actual local /v1/chat/completions HTTP socket,
// calls the production Chat bridge and its configured replay engine, and sends
// the resulting native request through another actual HTTP/SSE socket. Only the
// remote origin is replaced, after checking its exact production identity. TLS,
// deployed authentication/scheduling and real Redis have separate acceptance.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

type nativeTrafficPolicy struct{ mode string }

func (p *nativeTrafficPolicy) NativeReasoningReplayPolicy(context.Context) (service.NativeReasoningReplayRuntimeConfig, error) {
	return service.NativeReasoningReplayRuntimeConfig{Mode: p.mode, UserIDs: []int64{428, 429}}, nil
}

type nativeTrafficStore struct {
	failInvalidation bool
	mu               sync.Mutex
	values           map[string]service.NativeReasoningReplayValue
	poison           map[string]bool
	fail             bool
	gets, puts       int
	invalidations    int
}

func (s *nativeTrafficStore) GetNativeReasoningReplay(ctx context.Context, owner string, keys []string, maxBytes int) (map[string]service.NativeReasoningReplayValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.fail {
		return nil, service.ErrNativeReasoningReplayUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := map[string]service.NativeReasoningReplayValue{}
	for _, key := range keys {
		id := owner + ":" + key
		if s.poison[id] {
			continue
		}
		if v, ok := s.values[id]; ok && len(v.Ciphertext) <= maxBytes {
			result[key] = v
		}
	}
	return result, nil
}
func (s *nativeTrafficStore) PutNativeReasoningReplay(ctx context.Context, value service.NativeReasoningReplayWrite, _ service.NativeReasoningReplayLimits) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return service.ErrNativeReasoningReplayUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id := value.Owner + ":" + value.Key
	if s.poison[id] {
		return service.ErrNativeReasoningReplayAmbiguous
	}
	if old, ok := s.values[id]; ok {
		if old.Digest != value.Digest {
			s.poison[id] = true
			return service.ErrNativeReasoningReplayAmbiguous
		}
		return nil
	}
	s.values[id] = value.NativeReasoningReplayValue
	s.puts++
	return nil
}

func (s *nativeTrafficStore) InvalidateNativeReasoningReplay(ctx context.Context, owner string, refs []service.NativeReasoningReplayReference) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.fail {
		return service.ErrNativeReasoningReplayUnavailable
	}
	s.invalidations++
	if s.failInvalidation {
		return service.ErrNativeReasoningReplayUnavailable
	}
	for _, ref := range refs {
		id := owner + ":" + ref.Key
		if current, ok := s.values[id]; ok && current.Digest == ref.Digest {
			s.poison[id] = true
		}
	}
	return nil
}

// It cannot dial the original URL or any other host. No real token, proxy,
// refresh flow or TLS trust configuration is involved in this adapter.
type nativeTrafficHTTP struct {
	origin *url.URL
	client *http.Client
}

func (u *nativeTrafficHTTP) Do(req *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "chatgpt.com" || req.URL.Path != "/backend-api/codex/responses" || req.Method != http.MethodPost || proxy != "" {
		return nil, errors.New("native traffic fixture rejected unexpected upstream route")
	}
	copy := req.Clone(req.Context())
	target := *req.URL
	target.Scheme, target.Host = u.origin.Scheme, u.origin.Host
	copy.URL = &target
	copy.Host = u.origin.Host
	return u.client.Do(copy)
}
func (u *nativeTrafficHTTP) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

type nativeTrafficRow struct {
	Status             int               `json:"status"`
	ErrorBody          json.RawMessage   `json:"scripted_error,omitempty"`
	PartialBeforeError bool              `json:"partial_before_error,omitempty"`
	AuthorizationHash  string            `json:"authorization_sha256"`
	WireSHA256         string            `json:"wire_sha256"`
	BodyEncoding       string            `json:"body_encoding"`
	Body               json.RawMessage   `json:"body"`
	Output             []json.RawMessage `json:"scripted_output"`
	Remote             string            `json:"remote_address"`
	Terminal           bool              `json:"native_terminal_sent"`
}
type nativeTrafficClientRow struct {
	UpstreamStart int             `json:"upstream_start"`
	UpstreamEnd   int             `json:"upstream_end"`
	Request       json.RawMessage `json:"request"`
	ResponseWire  string          `json:"response_wire"`
	Status        int             `json:"status"`
	ContentType   string          `json:"content_type"`
	Identity      string          `json:"synthetic_identity"`
}
type nativeTrafficHarness struct {
	rejectRemaining      int
	rejectBody           json.RawMessage
	sseRejectNext        bool
	sseRejectAfterOutput bool
	clients              []nativeTrafficClientRow
	t                    *testing.T
	server, upstream     *httptest.Server
	store                *nativeTrafficStore
	policy               *nativeTrafficPolicy
	mu                   sync.Mutex
	rows                 []nativeTrafficRow
	results              []*service.OpenAIForwardResult
	routes               int
	errors               []string
	truncateNext         bool
	sparseTerminalOutput string                    // Test-only fixture: absent or empty terminal, done items unchanged.
	metadataEvents       bool                      // Test-only numbered non-output notifications interleaved with output.
	pureToolSecondRound  bool                      // Test-only fixture: no new opaque item in the middle tool turn.
	outputOverrides      map[int][]json.RawMessage // Test-only complete assistant output fixtures.
}

func nativeTrafficJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
func nativeTrafficOutput(round int) []json.RawMessage {
	prefix := fmt.Sprintf("fixture_%d", round)
	return []json.RawMessage{
		nativeTrafficJSON(map[string]any{"type": "reasoning", "id": "rs_" + prefix, "summary": []any{}, "encrypted_content": "opaque_reasoning_" + prefix, "status": "completed"}),
		nativeTrafficJSON(map[string]any{"type": "message", "id": "msg_" + prefix, "role": "assistant", "status": "completed", "phase": "commentary", "encrypted_content": "opaque_message_" + prefix, "content": []any{map[string]any{"type": "output_text", "text": fmt.Sprintf("Inspecting synthetic step %d.", round), "annotations": []any{}}}}),
		nativeTrafficJSON(map[string]any{"type": "function_call", "id": "fc_a_" + prefix, "call_id": "call_a_" + prefix, "name": "lookup_temperature", "namespace": "functions", "arguments": `{"location":"synthetic"}`, "encrypted_function_args": "opaque_arguments_a_" + prefix, "status": "completed"}),
		nativeTrafficJSON(map[string]any{"type": "function_call", "id": "fc_b_" + prefix, "call_id": "call_b_" + prefix, "name": "lookup_humidity", "namespace": "functions", "arguments": `{"location":"synthetic"}`, "status": "completed"}),
	}
}

func nativeTrafficPureToolOutput(round int) []json.RawMessage {
	var output []json.RawMessage
	for _, original := range nativeTrafficOutput(round)[2:] {
		var item map[string]json.RawMessage
		_ = json.Unmarshal(original, &item)
		delete(item, "encrypted_content")
		delete(item, "encrypted_function_args")
		item["namespace"] = json.RawMessage(`"functions"`)
		item["future_tool_state"] = json.RawMessage(`{"sequence":9007199254740993,"retained":null}`)
		output = append(output, nativeTrafficJSON(item))
	}
	return output
}

func newNativeTrafficHarness(t *testing.T, mode string, decorators ...func(service.NativeReasoningReplayStore) service.NativeReasoningReplayStore) *nativeTrafficHarness {
	t.Helper()
	h := &nativeTrafficHarness{t: t, store: &nativeTrafficStore{values: map[string]service.NativeReasoningReplayValue{}, poison: map[string]bool{}}, policy: &nativeTrafficPolicy{mode: mode}}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		wireHash := sha256.Sum256(body)
		if err == nil && r.Header.Get("Content-Encoding") == "zstd" {
			decoder, openErr := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(8<<20))
			if openErr != nil {
				err = openErr
			} else {
				body, err = decoder.DecodeAll(body, nil)
				decoder.Close()
			}
		}
		if err != nil {
			http.Error(w, "read failed", 400)
			return
		}
		h.mu.Lock()
		round := len(h.rows) + 1
		output := nativeTrafficOutput(round)
		if h.pureToolSecondRound && round == 2 {
			output = nativeTrafficPureToolOutput(round)
		}
		if override, exists := h.outputOverrides[round]; exists {
			output = override
		}
		truncated := h.truncateNext
		sparseShape := h.sparseTerminalOutput
		metadataEvents := h.metadataEvents
		h.truncateNext = false
		status := http.StatusOK
		var rejected json.RawMessage
		if h.rejectRemaining > 0 {
			status = http.StatusBadRequest
			rejected = append(json.RawMessage(nil), h.rejectBody...)
			h.rejectRemaining--
			output = nil
		}
		sseRejected, partial := h.sseRejectNext, h.sseRejectAfterOutput
		h.sseRejectNext = false
		if sseRejected {
			output = nil
			rejected = json.RawMessage(`{"error":{"code":"invalid_encrypted_content","message":"synthetic SSE failure"}}`)
		}
		authHash := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		h.rows = append(h.rows, nativeTrafficRow{Body: body, Output: output, Remote: r.RemoteAddr, Terminal: !truncated && status == 200 && !sseRejected, WireSHA256: hex.EncodeToString(wireHash[:]), BodyEncoding: r.Header.Get("Content-Encoding"), Status: status, ErrorBody: rejected, PartialBeforeError: partial && sseRejected, AuthorizationHash: hex.EncodeToString(authHash[:])})
		h.mu.Unlock()
		if status == http.StatusBadRequest {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(rejected)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		sequence := 0
		emit := func(v any) {
			if metadataEvents {
				v.(map[string]any)["sequence_number"] = sequence
				sequence++
			}
			data := append([]byte("data: "), nativeTrafficJSON(v)...)
			data = append(data, '\n', '\n')
			// Deliberately fragment writes across JSON strings and SSE framing.
			for len(data) > 0 {
				n := min(23, len(data))
				if _, err := w.Write(data[:n]); err != nil {
					return
				}
				data = data[n:]
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
		id := fmt.Sprintf("resp_fixture_%d", round)
		emitMetadata := func(kind string) {
			if metadataEvents {
				emit(map[string]any{"type": kind, "response_id": id, "metadata": map[string]any{"synthetic": true}})
			}
		}
		// response.created already produces an assistant-role Chat chunk and
		// commits HTTP 200. A genuine pre-output failure must omit it entirely.
		if !sseRejected || partial {
			emit(map[string]any{"type": "response.created", "response": map[string]any{"id": id, "model": "gpt-6-astra", "status": "in_progress"}})
		}
		if sseRejected {
			if partial {
				emit(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "partial synthetic answer"})
			}
			emit(map[string]any{"type": "response.failed", "response": map[string]any{"id": id, "status": "failed", "error": map[string]any{"code": "invalid_encrypted_content", "message": "synthetic SSE failure"}}})
			return
		}
		for index, item := range output {
			emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
			var decoded map[string]any
			_ = json.Unmarshal(item, &decoded)
			if decoded["type"] == "message" {
				for contentIndex, raw := range decoded["content"].([]any) {
					part := raw.(map[string]any)
					emit(map[string]any{"type": "response.output_text.delta", "output_index": index, "content_index": contentIndex, "delta": part["text"]})
				}
			}
			if decoded["type"] == "reasoning" {
				for summaryIndex, raw := range decoded["summary"].([]any) {
					part := raw.(map[string]any)
					emit(map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": index, "summary_index": summaryIndex, "delta": part["text"]})
				}
			}
			if decoded["type"] == "function_call" {
				emit(map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "delta": decoded["arguments"]})
				emitMetadata("response.metadata")
			}
			emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
			if index == 0 {
				emitMetadata("codex.response.metadata")
			}
		}
		if truncated {
			return
		}
		completed := map[string]any{"id": id, "model": "gpt-6-astra", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 101, "output_tokens": 17, "total_tokens": 118, "input_tokens_details": map[string]any{"cached_tokens": 31}, "output_tokens_details": map[string]any{"reasoning_tokens": 7}}}
		if sparseShape == "empty" {
			completed["output"] = []any{}
		} else if sparseShape == "absent" {
			delete(completed, "output")
		}
		emitMetadata("responsesapi.websocket_timing")
		emit(map[string]any{"type": "response.completed", "response": completed})
	}))
	t.Cleanup(h.upstream.Close)
	origin, err := url.Parse(h.upstream.URL)
	require.NoError(t, err)
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != origin.Host {
			return nil, errors.New("non-loopback socket attempt rejected")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	upstream := &nativeTrafficHTTP{origin: origin, client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect forbidden") }}}
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("17", 32)
	cfg.JWT.Secret = "synthetic-native-traffic-fixture-secret"
	yes, no := true, false
	cfg.Gateway.CodexFullSynthesis = &yes
	cfg.Gateway.CodexWire.MapOrderJitter = &no
	cfg.Gateway.CodexWire.Profile = "auto"
	cipher, err := repository.NewAESEncryptor(cfg)
	require.NoError(t, err)
	var store service.NativeReasoningReplayStore = h.store
	for _, decorate := range decorators {
		store = decorate(store)
	}
	engine := service.NewNativeReasoningReplay(store, cipher, service.DefaultNativeReasoningReplayLimits())
	svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	svc.StopOpenAICodexTicketHarvester()
	t.Cleanup(svc.CloseOpenAIWSPool)
	svc.SetNativeReasoningReplayRuntime(h.policy, engine)
	account := &service.Account{ID: 7001, Name: "synthetic-native-replay", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 2,
		Credentials: map[string]any{"access_token": "synthetic-local-only-token", "chatgpt_account_id": "synthetic-upstream-identity"},
		Extra:       map[string]any{service.OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "recent_samples": []any{map[string]any{"verdict": "healthy", "at": "2020-01-01T00:00:00Z"}}}, "codex_fingerprint_mode": "full", "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111"}}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "Chat ingress required", 404)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "read failed", 400)
			return
		}
		// Synthetic authenticated identities are chosen by this fixture only.
		// The production auth middleware and scheduler are outside this suite.
		userID, keyID := int64(428), int64(794)
		if r.Header.Get("X-Fixture-Identity") == "other-user" {
			userID, keyID = 429, 795
		}
		if r.Header.Get("X-Fixture-Identity") == "other-key" {
			keyID = 796
		}
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		c.Set("api_key", &service.APIKey{ID: keyID, UserID: userID, User: &service.User{ID: userID}})
		h.mu.Lock()
		h.routes++
		h.mu.Unlock()
		selected := *account
		if r.Header.Get("X-Fixture-Identity") == "other-account" {
			selected.ID++
			selected.Credentials = map[string]any{"access_token": "other-synthetic-local-token", "chatgpt_account_id": "other-synthetic-upstream-identity"}
		}
		if r.Header.Get("X-Fixture-Identity") == "queue-ping" {
			// Match the queue heartbeat's committed writer state before Forward.
			c.Header("Content-Type", "text/event-stream")
			if _, err := c.Writer.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			c.Writer.Flush()
		}
		result, err := svc.ForwardAsChatCompletions(r.Context(), c, &selected, body, "", "")
		if err != nil && !c.Writer.Written() {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "message": "synthetic forwarding error"}})
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.results = append(h.results, result)
		if err != nil {
			h.errors = append(h.errors, err.Error())
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

func nativeTrafficInitialHistory() []json.RawMessage {
	return []json.RawMessage{
		nativeTrafficJSON(map[string]any{"role": "system", "content": "Use the two declared synthetic inspection tools."}),
		nativeTrafficJSON(map[string]any{"role": "user", "content": "Inspect synthetic input. Preserve escaped text: \"x\"."}),
	}
}
func nativeTrafficContinue(history []json.RawMessage, assistant apicompat.ChatMessage) []json.RawMessage {
	out := append(append([]json.RawMessage{}, history...), nativeTrafficJSON(assistant))
	for _, call := range assistant.ToolCalls {
		out = append(out, nativeTrafficJSON(map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "synthetic-result"}))
	}
	return out
}
func (h *nativeTrafficHarness) call(history []json.RawMessage, stream bool, identity string) (apicompat.ChatMessage, error) {
	return h.callStatus(history, stream, identity, http.StatusOK, true)
}
func (h *nativeTrafficHarness) callStatus(history []json.RawMessage, stream bool, identity string, expectedStatus int, parseMessage bool) (apicompat.ChatMessage, error) {
	t := h.t
	t.Helper()
	tools := []any{}
	for _, name := range []string{"lookup_temperature", "lookup_humidity"} {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": name, "description": "Read synthetic local fixture data.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"location": map[string]any{"type": "string"}}, "required": []string{"location"}}}})
	}
	effort := "high"
	if identity == "changed-parameters" {
		effort = "medium"
	}
	body := nativeTrafficJSON(map[string]any{"model": "gpt-6-astra", "messages": history, "stream": stream, "stream_options": map[string]any{"include_usage": true}, "tools": tools, "reasoning_effort": effort, "parallel_tool_calls": true})
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session-id", "synthetic-replay-session")
	if identity == "other-session" {
		req.Header.Set("session-id", "other-synthetic-session")
	}
	req.Header.Set("X-Fixture-Identity", identity)
	h.mu.Lock()
	upstreamStart := len(h.rows)
	h.mu.Unlock()
	response, err := h.server.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	h.mu.Lock()
	h.clients = append(h.clients, nativeTrafficClientRow{Request: body, ResponseWire: string(raw), Status: response.StatusCode, ContentType: response.Header.Get("Content-Type"), Identity: identity, UpstreamStart: upstreamStart, UpstreamEnd: len(h.rows)})
	h.mu.Unlock()
	require.Equal(t, expectedStatus, response.StatusCode, "%s", raw)
	if !parseMessage {
		return apicompat.ChatMessage{}, nil
	}
	if !stream {
		var result apicompat.ChatCompletionsResponse
		if err := json.Unmarshal(raw, &result); err != nil {
			return apicompat.ChatMessage{}, err
		}
		if len(result.Choices) != 1 {
			return apicompat.ChatMessage{}, errors.New("missing Chat choice")
		}
		h.checkAssistant(result.Choices[0].Message)
		return result.Choices[0].Message, nil
	}
	message, err := nativeTrafficDecodeChatSSE(raw)
	if err == nil {
		h.checkAssistant(message)
	}
	return message, err
}

func (h *nativeTrafficHarness) checkAssistant(message apicompat.ChatMessage) {
	t := h.t
	t.Helper()
	h.mu.Lock()
	round := len(h.rows)
	pureTool := h.pureToolSecondRound && round == 2
	override, overridden := h.outputOverrides[round]
	h.mu.Unlock()
	require.Equal(t, "assistant", message.Role)
	if overridden {
		var text, reasoning string
		for _, raw := range override {
			var item struct {
				Type    string `json:"type"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
			}
			require.NoError(t, json.Unmarshal(raw, &item))
			require.Contains(t, []string{"message", "reasoning"}, item.Type)
			for _, part := range item.Content {
				text += part.Text
			}
			for _, part := range item.Summary {
				reasoning += part.Text
			}
		}
		require.JSONEq(t, string(nativeTrafficJSON(text)), string(message.Content))
		require.Equal(t, reasoning, message.ReasoningContent)
		require.Empty(t, message.ToolCalls)
		return
	}
	if pureTool {
		require.True(t, len(message.Content) == 0 || string(message.Content) == "null", "pure tool output has no invented assistant text")
	} else {
		require.JSONEq(t, string(nativeTrafficJSON(fmt.Sprintf("Inspecting synthetic step %d.", round))), string(message.Content))
	}
	require.Len(t, message.ToolCalls, 2)
	for i, name := range []string{"lookup_temperature", "lookup_humidity"} {
		call := message.ToolCalls[i]
		require.Equal(t, name, call.Function.Name, "the declared client tool capability must survive conversion")
		require.Equal(t, "function", call.Type)
		require.Equal(t, fmt.Sprintf("call_%c_fixture_%d", 'a'+i, round), call.ID)
		require.JSONEq(t, `{"location":"synthetic"}`, call.Function.Arguments)
	}
}

// Independent downstream decoder: only observed deltas build the next request.
// A missing terminal or malformed/truncated event is invalid evidence (exit 2).
func nativeTrafficDecodeChatSSE(raw []byte) (apicompat.ChatMessage, error) {
	message := apicompat.ChatMessage{Role: "assistant"}
	var content, reasoning string
	calls := map[int]apicompat.ChatToolCall{}
	done, finish := false, false
	var data []string
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 4<<20)
	consume := func() error {
		if len(data) == 0 {
			return nil
		}
		joined := strings.Join(data, "\n")
		data = nil
		if joined == "[DONE]" {
			done = true
			return nil
		}
		if done {
			return errors.New("event after Chat DONE")
		}
		var chunk apicompat.ChatCompletionsChunk
		if err := json.Unmarshal([]byte(joined), &chunk); err != nil {
			return err
		}
		if chunk.Object != "chat.completion.chunk" {
			return errors.New("unexpected Chat event")
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				return errors.New("unexpected Chat choice index")
			}
			if choice.FinishReason != nil {
				finish = true
			}
			d := choice.Delta
			if d.Content != nil {
				content += *d.Content
			}
			if d.ReasoningContent != nil {
				reasoning += *d.ReasoningContent
			}
			for _, call := range d.ToolCalls {
				if call.Index == nil || *call.Index < 0 {
					return errors.New("missing tool delta index")
				}
				old := calls[*call.Index]
				if call.ID != "" {
					old.ID = call.ID
				}
				if call.Type != "" {
					old.Type = call.Type
				}
				old.Function.Name += call.Function.Name
				old.Function.Arguments += call.Function.Arguments
				calls[*call.Index] = old
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := consume(); err != nil {
				return message, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return message, err
	}
	if content != "" {
		message.Content = nativeTrafficJSON(content)
	}
	message.ReasoningContent = reasoning
	for i := 0; i < len(calls); i++ {
		call, ok := calls[i]
		if !ok || call.ID == "" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
			return message, errors.New("incomplete tool delta evidence")
		}
		message.ToolCalls = append(message.ToolCalls, call)
	}
	if len(data) > 0 || !done || !finish {
		return message, errors.New("incomplete Chat SSE evidence")
	}
	return message, nil
}

// 0 means the complete original native block is present exactly once, in order.
// 1 is an actual product loss/mismatch; 2 is invalid or missing test evidence.
func nativeTrafficComparisonExit(request json.RawMessage, expected []json.RawMessage) int {
	return nativeTrafficCompareBlock(request, expected, true)
}

// Existing acceptance keeps its opaque-bearing fixture requirement. The pure
// tool regression separately proves the earlier opaque block and uses the same
// full structural/unique/ordered comparison for the later tool-only block.
func nativeTrafficCompareBlock(request json.RawMessage, expected []json.RawMessage, requireEncrypted bool) int {
	return nativeTrafficCompareOutputBlock(request, expected, requireEncrypted, true)
}

func nativeTrafficCompareOutputBlock(request json.RawMessage, expected []json.RawMessage, requireEncrypted, requireTool bool) int {
	decode := func(raw []byte, target any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(target); err != nil {
			return err
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			return errors.New("trailing JSON")
		}
		return nil
	}
	var body map[string]json.RawMessage
	if decode(request, &body) != nil || len(expected) == 0 {
		return 2
	}
	var input []map[string]any
	if decode(body["input"], &input) != nil || input == nil {
		return 2
	}
	want := make([]map[string]any, len(expected))
	seenIDs := map[string]bool{}
	hasEncrypted, hasTool := false, false
	for i, item := range expected {
		if decode(item, &want[i]) != nil {
			return 2
		}
		id, ok := want[i]["id"].(string)
		if !ok || id == "" || seenIDs[id] {
			return 2
		}
		seenIDs[id] = true
		typ, ok := want[i]["type"].(string)
		if !ok || typ == "" {
			return 2
		}
		if encrypted, _ := want[i]["encrypted_content"].(string); encrypted != "" {
			hasEncrypted = true
		}
		if typ == "function_call" || typ == "custom_tool_call" {
			hasTool = true
		}
	}
	if requireEncrypted && !hasEncrypted || requireTool && !hasTool {
		return 2
	}
	matches := 0
	for i := range input {
		if i+len(want) <= len(input) && reflect.DeepEqual(input[i:i+len(want)], want) {
			matches++
		}
	}
	if matches != 1 {
		return 1
	}
	for _, item := range want {
		count := 0
		for _, got := range input {
			if got["id"] == item["id"] {
				count++
			}
		}
		if count != 1 {
			return 1
		}
	}
	return 0
}
func (h *nativeTrafficHarness) snapshot() ([]nativeTrafficRow, int, int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return append([]nativeTrafficRow(nil), h.rows...), h.routes, h.store.gets, h.store.puts
}

func TestNativeReasoningReplayTrafficAcceptance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := os.Getenv("NATIVE_REASONING_REPLAY_TRAFFIC_CANDIDATE_DIR")
	if dir == "" {
		dir = filepath.Join(t.TempDir(), "capture")
	} else {
		var err error
		dir, err = filepath.Abs(dir)
		require.NoError(t, err)
	}
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "native traffic evidence must use a new directory")
	require.NoError(t, os.MkdirAll(dir, 0700))
	started := time.Now().UTC()
	var scenarios []map[string]any
	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "buffered_multiturn", true: "streamed_multiturn"}[stream]
		ok := t.Run(name, func(t *testing.T) {
			h := newNativeTrafficHarness(t, "replay")
			t.Cleanup(func() {
				if t.Failed() {
					rows, _, _, _ := h.snapshot()
					recordNativeTrafficScenario(t, dir, name, rows, h.clientEvidence())
				}
			})
			history := nativeTrafficInitialHistory()
			for turn := 0; turn < 3; turn++ {
				assistant, err := h.call(history, stream, "")
				require.NoError(t, err)
				require.Len(t, assistant.ToolCalls, 2)
				rows, _, _, _ := h.snapshot()
				for old := 0; old < turn; old++ {
					require.Equal(t, 0, nativeTrafficComparisonExit(rows[turn].Body, rows[old].Output), "complete native round %d was not restored in round %d", old, turn)
				}
				history = nativeTrafficContinue(history, assistant)
			}
			// A Chat client compacts its history. The old native context must not
			// cross that boundary; a new complete tool turn can start a new chain.
			history = []json.RawMessage{json.RawMessage(`{"role":"system","content":"Use the two declared synthetic inspection tools."}`), json.RawMessage(`{"role":"user","content":"Synthetic compacted summary; start the next inspection."}`)}
			assistant, err := h.call(history, stream, "")
			require.NoError(t, err)
			rows, _, _, _ := h.snapshot()
			for _, old := range rows[:3] {
				require.Equal(t, 1, nativeTrafficComparisonExit(rows[3].Body, old.Output))
			}
			history = nativeTrafficContinue(history, assistant)
			_, err = h.call(history, stream, "")
			require.NoError(t, err)
			rows, routes, gets, puts := h.snapshot()
			require.Equal(t, 0, nativeTrafficComparisonExit(rows[4].Body, rows[3].Output))
			require.Len(t, rows, 5)
			require.Equal(t, 5, routes)
			require.Positive(t, gets)
			require.Equal(t, 5, puts)
			h.mu.Lock()
			errorsSeen := append([]string(nil), h.errors...)
			results := append([]*service.OpenAIForwardResult(nil), h.results...)
			h.mu.Unlock()
			require.Empty(t, errorsSeen)
			require.Len(t, results, 5)
			for _, result := range results {
				require.NotNil(t, result)
				require.Equal(t, 101, result.Usage.InputTokens)
				require.Equal(t, 31, result.Usage.CacheReadInputTokens)
				require.Equal(t, 17, result.Usage.OutputTokens)
			}
			h.store.mu.Lock()
			ciphertexts := make([]string, 0, len(h.store.values))
			for _, v := range h.store.values {
				ciphertexts = append(ciphertexts, v.Ciphertext)
			}
			h.store.mu.Unlock()
			for _, ciphertext := range ciphertexts {
				require.NotContains(t, ciphertext, "opaque_")
				require.NotContains(t, ciphertext, "synthetic input")
			}
			recordNativeTrafficScenario(t, dir, name, rows, h.clientEvidence())
			scenarios = append(scenarios, map[string]any{"name": name, "requests": routes, "store_reads": gets, "store_writes": puts, "status": "pass"})
		})
		require.True(t, ok)
	}
	for _, fault := range []string{"off_baseline", "other-user", "other-key", "other-session", "other-account", "changed-parameters", "cache_unavailable", "truncated_terminal"} {
		ok := t.Run(fault, func(t *testing.T) {
			mode := "replay"
			if fault == "off_baseline" {
				mode = "off"
			}
			h := newNativeTrafficHarness(t, mode)
			t.Cleanup(func() {
				if t.Failed() {
					rows, _, _, _ := h.snapshot()
					recordNativeTrafficScenario(t, dir, fault, rows, h.clientEvidence())
				}
			})
			history := nativeTrafficInitialHistory()
			if fault == "truncated_terminal" {
				h.mu.Lock()
				h.truncateNext = true
				h.mu.Unlock()
			}
			assistant, err := h.call(history, true, "")
			if fault == "truncated_terminal" {
				require.Error(t, err, "a truncated native response must not become complete Chat evidence")
				h.checkAssistant(assistant)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, assistant.ToolCalls, 2)
			_, _, _, puts := h.snapshot()
			if fault == "off_baseline" || fault == "truncated_terminal" {
				require.Zero(t, puts)
			} else {
				require.Equal(t, 1, puts)
			}
			history = nativeTrafficContinue(history, assistant)
			if fault == "cache_unavailable" {
				h.store.mu.Lock()
				h.store.fail = true
				h.store.mu.Unlock()
			}
			_, err = h.call(history, true, fault)
			require.NoError(t, err)
			rows, routes, gets, puts := h.snapshot()
			require.Len(t, rows, 2)
			require.Equal(t, 2, routes)
			require.Equal(t, 1, nativeTrafficComparisonExit(rows[1].Body, rows[0].Output), "unsafe replay across %s", fault)
			if fault == "off_baseline" {
				require.Zero(t, gets)
				require.Zero(t, puts)
			}
			recordNativeTrafficScenario(t, dir, fault, rows, h.clientEvidence())
			scenarios = append(scenarios, map[string]any{"name": fault, "requests": routes, "store_reads": gets, "store_writes": puts, "status": "pass"})
		})
		require.True(t, ok)
	}
	for _, twice := range []bool{false, true} {
		name := "encrypted_rejection_retry_once"
		if twice {
			name = "encrypted_rejection_second_failure"
		}
		ok := t.Run(name, func(t *testing.T) {
			h := newNativeTrafficHarness(t, "replay")
			t.Cleanup(func() {
				if t.Failed() {
					rows, _, _, _ := h.snapshot()
					recordNativeTrafficScenario(t, dir, name, rows, h.clientEvidence())
				}
			})
			history := nativeTrafficInitialHistory()
			assistant, err := h.call(history, true, "")
			require.NoError(t, err)
			history = nativeTrafficContinue(history, assistant)
			h.mu.Lock()
			h.rejectBody = json.RawMessage(`{"error":{"code":"invalid_encrypted_content","message":"synthetic rejection"}}`)
			h.rejectRemaining = 1
			if twice {
				h.rejectRemaining = 2
			}
			h.mu.Unlock()
			if twice {
				_, err = h.callStatus(history, true, "", 400, false)
			} else {
				_, err = h.call(history, true, "")
			}
			require.NoError(t, err)
			rows, routes, _, _ := h.snapshot()
			require.Equal(t, 2, routes)
			require.Len(t, rows, 3, "exactly one additional attempt is allowed")
			require.Equal(t, 400, rows[1].Status)
			if twice {
				require.Equal(t, 400, rows[2].Status)
			} else {
				require.Equal(t, 200, rows[2].Status)
			}
			require.Equal(t, 0, nativeTrafficComparisonExit(rows[1].Body, rows[0].Output), "the rejection must target an actually restored group")
			require.Equal(t, 1, nativeTrafficComparisonExit(rows[2].Body, rows[0].Output), "retry must use the original compatibility body")
			require.Equal(t, rows[1].AuthorizationHash, rows[2].AuthorizationHash, "the fallback must use the same synthetic upstream credential")
			h.store.mu.Lock()
			invalidations := h.store.invalidations
			h.store.mu.Unlock()
			require.Equal(t, 1, invalidations)
			_, err = h.call(history, true, "")
			require.NoError(t, err)
			rows, routes, gets, puts := h.snapshot()
			require.Equal(t, 3, routes)
			require.Len(t, rows, 4)
			require.Equal(t, 1, nativeTrafficComparisonExit(rows[3].Body, rows[0].Output), "poisoned state must miss on a later request")
			h.mu.Lock()
			results := append([]*service.OpenAIForwardResult(nil), h.results...)
			h.mu.Unlock()
			for _, result := range results {
				if result != nil {
					require.Equal(t, 101, result.Usage.InputTokens)
					require.Equal(t, 31, result.Usage.CacheReadInputTokens)
					require.Equal(t, 17, result.Usage.OutputTokens)
				}
			}
			recordNativeTrafficScenario(t, dir, name, rows, h.clientEvidence())
			scenarios = append(scenarios, map[string]any{"name": name, "requests": routes, "upstream_requests": len(rows), "store_reads": gets, "store_writes": puts, "invalidations": invalidations, "status": "pass"})
		})
		require.True(t, ok)
	}
	okBarrier := t.Run("poison_write_failure_local_barrier", func(t *testing.T) {
		name := "poison_write_failure_local_barrier"
		h := newNativeTrafficHarness(t, "replay")
		t.Cleanup(func() {
			if t.Failed() {
				rows, _, _, _ := h.snapshot()
				recordNativeTrafficScenario(t, dir, name, rows, h.clientEvidence())
			}
		})
		history := nativeTrafficInitialHistory()
		assistant, err := h.call(history, true, "")
		require.NoError(t, err)
		history = nativeTrafficContinue(history, assistant)
		h.store.mu.Lock()
		originals := map[string]service.NativeReasoningReplayValue{}
		for key, value := range h.store.values {
			originals[key] = value
		}
		h.store.failInvalidation = true
		h.store.mu.Unlock()
		require.Len(t, originals, 1, "a complete native seed must exist before the simulated poison failure")
		h.mu.Lock()
		h.rejectBody = json.RawMessage(`{"error":{"code":"invalid_encrypted_content","message":"synthetic rejection with failed poison write"}}`)
		h.rejectRemaining = 1
		h.mu.Unlock()
		_, err = h.call(history, true, "")
		require.NoError(t, err)
		rows, routes, _, _ := h.snapshot()
		require.Len(t, rows, 3)
		require.Equal(t, 2, routes)
		require.Equal(t, 0, nativeTrafficComparisonExit(rows[1].Body, rows[0].Output))
		require.Equal(t, 400, rows[1].Status)
		require.Equal(t, 1, nativeTrafficComparisonExit(rows[2].Body, rows[0].Output))
		require.Equal(t, 200, rows[2].Status)
		h.store.mu.Lock()
		invalidations := h.store.invalidations
		poisonCount := len(h.store.poison)
		h.store.mu.Unlock()
		require.Equal(t, 1, invalidations)
		require.Zero(t, poisonCount, "the injected storage failure must not secretly poison the fixture store")
		for combined, want := range originals {
			owner, key, found := strings.Cut(combined, ":")
			require.True(t, found)
			values, err := h.store.GetNativeReasoningReplay(context.Background(), owner, []string{key}, service.DefaultNativeReasoningReplayLimits().MaxEntryBytes)
			require.NoError(t, err)
			require.Equal(t, want, values[key], "stale ciphertext must remain readable; the local rejection barrier provides protection")
		}
		_, err = h.call(history, true, "")
		require.NoError(t, err)
		rows, routes, gets, puts := h.snapshot()
		require.Equal(t, 3, routes)
		require.Len(t, rows, 4)
		require.Equal(t, 1, nativeTrafficComparisonExit(rows[3].Body, rows[0].Output), "a failed poison write must not allow rejected state to be injected again")
		recordNativeTrafficScenario(t, dir, name, rows, h.clientEvidence())
		scenarios = append(scenarios, map[string]any{"name": name, "requests": routes, "upstream_requests": len(rows), "store_reads": gets, "store_writes": puts, "invalidations": invalidations, "store_invalidation_failed": true, "stale_record_readable": true, "local_rejection_barrier": true, "status": "pass"})
	})
	require.True(t, okBarrier)
	for _, tc := range []struct {
		name, body            string
		invalidate, committed bool
	}{
		{"quoted_code", `{"error":{"code":"other","message":"invalid_encrypted_content"}}`, false, false},
		{"echoed_code", `{"error":{"code":"other"},"echo":{"error":{"code":"invalid_encrypted_content"}}}`, false, false},
		{"duplicate_code", `{"error":{"code":"other","code":"invalid_encrypted_content"}}`, false, false},
		{"rejection_with_usage", `{"error":{"code":"invalid_encrypted_content"},"usage":{"input_tokens":1}}`, true, false},
		{"rejection_with_partial_output", `{"error":{"code":"invalid_encrypted_content"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial synthetic"}]}]}`, true, false},
		{"rejection_after_queue_ping", `{"error":{"code":"invalid_encrypted_content"}}`, true, true},
	} {
		ok := t.Run(tc.name, func(t *testing.T) {
			h := newNativeTrafficHarness(t, "replay")
			t.Cleanup(func() {
				if t.Failed() {
					rows, _, _, _ := h.snapshot()
					recordNativeTrafficScenario(t, dir, tc.name, rows, h.clientEvidence())
				}
			})
			history := nativeTrafficInitialHistory()
			assistant, err := h.call(history, true, "")
			require.NoError(t, err)
			history = nativeTrafficContinue(history, assistant)
			h.mu.Lock()
			h.rejectBody = json.RawMessage(tc.body)
			h.rejectRemaining = 1
			h.mu.Unlock()
			status, identity := http.StatusBadRequest, ""
			if tc.committed {
				status, identity = http.StatusOK, "queue-ping"
			}
			_, err = h.callStatus(history, true, identity, status, false)
			require.NoError(t, err)
			rows, routes, _, _ := h.snapshot()
			require.Equal(t, 2, routes)
			require.Len(t, rows, 2, "a rejection unsafe to retry must not cause an extra upstream request")
			require.Equal(t, 400, rows[1].Status)
			require.Equal(t, 0, nativeTrafficComparisonExit(rows[1].Body, rows[0].Output))
			h.store.mu.Lock()
			invalidations := h.store.invalidations
			h.store.mu.Unlock()
			wantInvalidations, wantRestoreComparison := 0, 0
			if tc.invalidate {
				wantInvalidations, wantRestoreComparison = 1, 1
			}
			require.Equal(t, wantInvalidations, invalidations, "invalidation depends on a verified rejection, not on permission to retry")
			if tc.committed {
				client := h.clientEvidence()
				require.True(t, strings.HasPrefix(client[1].ResponseWire, ": ping\n\n"), "the heartbeat must actually reach the client before the rejected attempt")
			}
			_, err = h.call(history, true, "")
			require.NoError(t, err)
			rows, routes, gets, puts := h.snapshot()
			require.Equal(t, 3, routes)
			require.Len(t, rows, 3)
			require.Equal(t, wantRestoreComparison, nativeTrafficComparisonExit(rows[2].Body, rows[0].Output), "later requests must miss rejected state and retain state after unverified errors")
			recordNativeTrafficScenario(t, dir, tc.name, rows, h.clientEvidence())
			scenarios = append(scenarios, map[string]any{"name": tc.name, "requests": routes, "upstream_requests": len(rows), "store_reads": gets, "store_writes": puts, "invalidations": invalidations, "client_committed": tc.committed, "status": "pass"})
		})
		require.True(t, ok)
	}
	for _, partial := range []bool{false, true} {
		name := "sse_rejection_no_retry"
		if partial {
			name = "sse_partial_rejection_no_retry"
		}
		ok := t.Run(name, func(t *testing.T) {
			h := newNativeTrafficHarness(t, "replay")
			t.Cleanup(func() {
				if t.Failed() {
					rows, _, _, _ := h.snapshot()
					recordNativeTrafficScenario(t, dir, name, rows, h.clientEvidence())
				}
			})
			history := nativeTrafficInitialHistory()
			assistant, err := h.call(history, true, "")
			require.NoError(t, err)
			history = nativeTrafficContinue(history, assistant)
			h.mu.Lock()
			h.sseRejectNext = true
			h.sseRejectAfterOutput = partial
			h.mu.Unlock()
			status := http.StatusBadGateway
			if partial {
				status = http.StatusOK
			}
			_, err = h.callStatus(history, true, "", status, false)
			require.NoError(t, err)
			rows, routes, _, _ := h.snapshot()
			require.Equal(t, 2, routes)
			require.Len(t, rows, 2)
			require.Equal(t, 0, nativeTrafficComparisonExit(rows[1].Body, rows[0].Output))
			h.store.mu.Lock()
			invalidations := h.store.invalidations
			h.store.mu.Unlock()
			require.Equal(t, 1, invalidations)
			_, err = h.call(history, true, "")
			require.NoError(t, err)
			rows, routes, gets, puts := h.snapshot()
			require.Len(t, rows, 3)
			require.Equal(t, 1, nativeTrafficComparisonExit(rows[2].Body, rows[0].Output))
			require.Equal(t, 3, routes)
			recordNativeTrafficScenario(t, dir, name, rows, h.clientEvidence())
			scenarios = append(scenarios, map[string]any{"name": name, "requests": routes, "upstream_requests": len(rows), "store_reads": gets, "store_writes": puts, "invalidations": invalidations, "status": "pass"})
		})
		require.True(t, ok)
	}
	require.Len(t, scenarios, 21)
	artifactHashes := map[string]string{}
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, paths, 21)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		sum := sha256.Sum256(raw)
		artifactHashes[filepath.Base(path)] = hex.EncodeToString(sum[:])
	}
	hashFile := func(path string) string {
		t.Helper()
		file, err := os.Open(path)
		require.NoError(t, err)
		defer file.Close()
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		require.NoError(t, err)
		return hex.EncodeToString(hash.Sum(nil))
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	receipt := map[string]any{"contract": 1, "test_binary_sha256": hashFile(executable), "go_mod_sha256": hashFile("../../go.mod"), "go_sum_sha256": hashFile("../../go.sum"), "go_version": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "started_at": started.Format(time.RFC3339Nano), "completed_at": time.Now().UTC().Format(time.RFC3339Nano), "entrypoint": "POST /v1/chat/completions -> ForwardAsChatCompletions", "upstream": "loopback HTTP/SSE; production target checked before origin-only adapter", "reference_kind": "independently scripted complete native output and actual downstream Chat deltas", "scenarios": scenarios, "artifact_sha256": artifactHashes, "comparison_exit_codes": map[string]int{"match": 0, "product_difference": 1, "invalid_evidence": 2}, "status": "pass"}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "acceptance.json"), nativeTrafficJSON(receipt), 0600))
	t.Logf("native reasoning replay traffic acceptance: %s", dir)
}
func (h *nativeTrafficHarness) clientEvidence() []nativeTrafficClientRow {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]nativeTrafficClientRow(nil), h.clients...)
}
func recordNativeTrafficScenario(t *testing.T, dir, name string, rows []nativeTrafficRow, clients []nativeTrafficClientRow) {
	t.Helper()
	var data bytes.Buffer
	for _, row := range rows {
		host, _, err := net.SplitHostPort(row.Remote)
		require.NoError(t, err)
		require.True(t, net.ParseIP(host).IsLoopback())
		require.True(t, json.Valid(row.Body))
		data.Write(nativeTrafficJSON(map[string]any{"kind": "upstream", "capture": row}))
		data.WriteByte('\n')
	}
	for _, row := range clients {
		require.True(t, json.Valid(row.Request))
		require.NotEmpty(t, row.ResponseWire)
		data.Write(nativeTrafficJSON(map[string]any{"kind": "chat_client", "capture": row}))
		data.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".jsonl"), data.Bytes(), 0600))
	previous := 0
	for _, client := range clients {
		require.Equal(t, previous, client.UpstreamStart)
		require.Greater(t, client.UpstreamEnd, client.UpstreamStart)
		require.LessOrEqual(t, client.UpstreamEnd, len(rows))
		previous = client.UpstreamEnd
	}
	require.Equal(t, len(rows), previous, "every observed upstream socket belongs to a real synthetic client call")
}

func TestNativeReasoningReplayTrafficVerifierCalibration(t *testing.T) {
	// Calibrate the fixture's HTTP boundary against the actual production
	// converter: a role-only chunk is already output, even without text.
	state := apicompat.NewResponsesEventToChatState()
	chunks := apicompat.ResponsesEventToChatChunks(&apicompat.ResponsesStreamEvent{Type: "response.created", Response: &apicompat.ResponsesResponse{ID: "resp_role_only", Model: "gpt-6-astra"}}, state)
	require.Len(t, chunks, 1)
	require.Equal(t, "assistant", chunks[0].Choices[0].Delta.Role)
	require.Nil(t, chunks[0].Choices[0].Delta.Content)
	roleWire, roleErr := apicompat.ChatChunkToSSE(chunks[0])
	require.NoError(t, roleErr)
	roleRecorder := httptest.NewRecorder()
	roleContext, _ := gin.CreateTestContext(roleRecorder)
	n, roleErr := roleContext.Writer.WriteString(roleWire)
	require.NoError(t, roleErr)
	require.Equal(t, len(roleWire), n)
	require.True(t, roleContext.Writer.Written())
	roleContext.Writer.WriteHeader(http.StatusBadGateway)
	require.Equal(t, http.StatusOK, roleContext.Writer.Status(), "role-only delivery has already committed the client status")
	require.Equal(t, http.StatusOK, roleRecorder.Code)
	output := nativeTrafficOutput(1)
	request := nativeTrafficJSON(map[string]any{"input": output})
	require.Equal(t, 0, nativeTrafficComparisonExit(request, output))
	require.Equal(t, 1, nativeTrafficComparisonExit(nativeTrafficJSON(map[string]any{"input": output[1:]}), output))
	require.Equal(t, 1, nativeTrafficComparisonExit(bytes.ReplaceAll(request, []byte("opaque_reasoning_fixture_1"), []byte("opaque_wrong")), output))
	require.Equal(t, 1, nativeTrafficComparisonExit(nativeTrafficJSON(map[string]any{"input": append(append([]json.RawMessage{}, output...), output...)}), output))
	require.Equal(t, 2, nativeTrafficComparisonExit(request[:len(request)-1], output))
	require.Equal(t, 2, nativeTrafficComparisonExit(request, nil))
	require.Equal(t, 2, nativeTrafficComparisonExit(request, output[:1]), "partial expected native lifecycle is invalid evidence")
	require.Equal(t, 2, nativeTrafficComparisonExit(request, append(append([]json.RawMessage{}, output...), output[0])), "duplicate expected identity is invalid evidence")
	var mapBody map[string]any
	require.NoError(t, json.Unmarshal(request, &mapBody))
	require.Equal(t, 0, nativeTrafficComparisonExit(nativeTrafficJSON(mapBody), output), "ordinary map ordering is not a difference")
	chunk := map[string]any{"object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "escaped \"text\"\n"}, "finish_reason": "stop"}}}
	raw := string(nativeTrafficJSON(chunk))
	var pretty bytes.Buffer
	require.NoError(t, json.Indent(&pretty, []byte(raw), "", "  "))
	valid := []byte("data: " + strings.ReplaceAll(pretty.String(), "\n", "\ndata: ") + "\n\ndata: [DONE]\n\n")
	message, err := nativeTrafficDecodeChatSSE(valid)
	require.NoError(t, err)
	require.JSONEq(t, `"escaped \"text\"\n"`, string(message.Content))
	_, err = nativeTrafficDecodeChatSSE(bytes.ReplaceAll(valid, []byte("data: [DONE]\n\n"), nil))
	require.Error(t, err)
	_, err = nativeTrafficDecodeChatSSE(append([]byte("data: {broken}\n\n"), valid...))
	require.Error(t, err)
}
