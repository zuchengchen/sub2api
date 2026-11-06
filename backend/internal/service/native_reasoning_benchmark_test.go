package service

// These measure the entire ForwardAsChatCompletions CPU path, including real
// production request construction, response conversion and client writes. The
// upstream and cache are bounded synthetic implementations; socket, Redis and
// authentication-middleware latency are deliberately outside this CPU fixture.
// The baseline overlay preserves the common fixture byte-for-byte and replaces
// only the candidate bootstrap block with an explicit feature-absent bootstrap.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

type nativeReplayBenchmarkStats struct{ PolicyReads, Gets, Hits, Puts, Entries, Errors int64 }
type nativeReplayBenchmarkProbe struct {
	requiresRuntime bool
	stats           func() nativeReplayBenchmarkStats
}

type nativeReplayBenchmarkUpstream struct {
	seed, continuation []byte
	seedPhase          bool
	inspect            bool
	calls              atomic.Int64
	mu                 sync.Mutex
	lastBody           []byte
}

func (u *nativeReplayBenchmarkUpstream) Do(req *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
	if req.Method != http.MethodPost || req.URL.Host != "chatgpt.com" || req.URL.Path != "/backend-api/codex/responses" || proxy != "" {
		return nil, errors.New("benchmark escaped its synthetic Responses upstream")
	}
	u.calls.Add(1)
	if u.inspect {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if req.Header.Get("Content-Encoding") == "zstd" {
			decoder, err := zstd.NewReader(nil)
			if err != nil {
				return nil, err
			}
			raw, err = decoder.DecodeAll(raw, nil)
			decoder.Close()
			if err != nil {
				return nil, err
			}
		}
		u.mu.Lock()
		u.lastBody = append(u.lastBody[:0], raw...)
		u.mu.Unlock()
	} else {
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
	}
	_ = req.Body.Close()
	wire := u.continuation
	if u.seedPhase {
		wire = u.seed
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(bytes.NewReader(wire)), Request: req}, nil
}
func (u *nativeReplayBenchmarkUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func nativeReplayBenchmarkJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
func nativeReplayBenchmarkResponse(label string) ([]byte, []json.RawMessage) {
	output := []json.RawMessage{
		nativeReplayBenchmarkJSON(map[string]any{"type": "reasoning", "id": "rs_bench_" + label, "encrypted_content": "opaque-bench-" + label, "summary": []any{}}),
		nativeReplayBenchmarkJSON(map[string]any{"type": "message", "id": "msg_bench_" + label, "role": "assistant", "phase": "commentary", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Synthetic " + label + " reply."}}}),
		nativeReplayBenchmarkJSON(map[string]any{"type": "function_call", "id": "fc_bench_" + label, "call_id": "call_bench_" + label, "name": "lookup_metric", "namespace": "functions", "arguments": `{"name":"cpu"}`, "status": "completed"}),
	}
	var wire bytes.Buffer
	emit := func(v any) {
		wire.WriteString("data: ")
		wire.Write(nativeReplayBenchmarkJSON(v))
		wire.WriteString("\n\n")
	}
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_bench_" + label, "model": "gpt-6-astra"}})
	for i, item := range output {
		emit(map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
		if i == 1 {
			emit(map[string]any{"type": "response.output_text.delta", "output_index": i, "content_index": 0, "delta": "Synthetic " + label + " reply."})
		}
		if i == 2 {
			emit(map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "delta": `{"name":"cpu"}`})
		}
		emit(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
	}
	emit(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_bench_" + label, "model": "gpt-6-astra", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 1081, "output_tokens": 17, "total_tokens": 1098, "input_tokens_details": map[string]any{"cached_tokens": 512}}}})
	return wire.Bytes(), output
}

type nativeReplayBenchmarkFixture struct {
	tb         testing.TB
	mode       string
	stream     bool
	svc        *OpenAIGatewayService
	account    *Account
	key        *APIKey
	engine     *gin.Engine
	upstream   *nativeReplayBenchmarkUpstream
	probe      *nativeReplayBenchmarkProbe
	body       []byte
	seedOutput []json.RawMessage
}

func newNativeReplayBenchmarkFixture(tb testing.TB, mode string, stream bool) *nativeReplayBenchmarkFixture {
	tb.Helper()
	gin.SetMode(gin.TestMode)
	tb.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_USER_ID", "")
	cfg := &config.Config{}
	yes, no := true, false
	cfg.Gateway.CodexFullSynthesis = &yes
	cfg.Gateway.CodexWire.MapOrderJitter = &no
	cfg.Gateway.CodexWire.Profile = "auto"
	cfg.JWT.Secret = "synthetic-native-replay-benchmark-secret"
	seed, output := nativeReplayBenchmarkResponse("seed")
	continuation, _ := nativeReplayBenchmarkResponse("continuation")
	upstream := &nativeReplayBenchmarkUpstream{seed: seed, continuation: continuation, seedPhase: true, inspect: true}
	account := &Account{ID: 27001, Name: "synthetic-native-replay-benchmark", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 2,
		Credentials: map[string]any{"access_token": "synthetic-benchmark-token", "chatgpt_account_id": "synthetic-benchmark-account"},
		Extra:       map[string]any{OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "recent_samples": []any{map[string]any{"verdict": "healthy", "at": "2020-01-01T00:00:00Z"}}}, "codex_fingerprint_mode": "full", "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111"}}
	svc := NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	svc.StopOpenAICodexTicketHarvester()
	svc.tiboRouteDisabled = true
	tb.Cleanup(svc.CloseOpenAIWSPool)
	f := &nativeReplayBenchmarkFixture{tb: tb, mode: mode, stream: stream, svc: svc, account: account, key: &APIKey{ID: 794, UserID: 428, User: &User{ID: 428}}, engine: gin.New(), upstream: upstream, seedOutput: output}
	f.probe = installNativeReplayBenchmarkRuntime(svc, cfg, mode)
	history := []json.RawMessage{
		nativeReplayBenchmarkJSON(map[string]any{"role": "system", "content": "Use the declared synthetic lookup tool and preserve its arguments."}),
		nativeReplayBenchmarkJSON(map[string]any{"role": "user", "content": strings.Repeat("Synthetic benchmark context with stable user content. ", 64)}),
	}
	seedBody := nativeReplayBenchmarkRequest(history, stream)
	client, err := f.forward(seedBody)
	if err != nil {
		tb.Fatal(err)
	}
	assistant, err := nativeReplayBenchmarkAssistant(client, stream)
	if err != nil {
		tb.Fatal(err)
	}
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_bench_seed" || assistant.ToolCalls[0].Function.Name != "lookup_metric" || assistant.ToolCalls[0].Function.Arguments != `{"name":"cpu"}` {
		tb.Fatal("benchmark seed did not execute the production Chat tool conversion")
	}
	history = append(history, nativeReplayBenchmarkJSON(assistant), json.RawMessage(`{"role":"tool","tool_call_id":"call_bench_seed","content":"{\"value\":42}"}`))
	f.body = nativeReplayBenchmarkRequest(history, stream)
	f.upstream.seedPhase = false
	if _, err := f.forward(f.body); err != nil {
		tb.Fatal(err)
	}
	if err := f.verify(); err != nil {
		tb.Fatal(err)
	}
	f.upstream.inspect = false
	return f
}
func nativeReplayBenchmarkRequest(messages []json.RawMessage, stream bool) []byte {
	return nativeReplayBenchmarkJSON(map[string]any{"model": "gpt-6-astra", "messages": messages, "stream": stream, "stream_options": map[string]any{"include_usage": true}, "reasoning_effort": "high", "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup_metric", "description": "Read a synthetic metric.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}}}}})
}
func (f *nativeReplayBenchmarkFixture) forward(body []byte) ([]byte, error) {
	recorder := httptest.NewRecorder()
	c := gin.CreateTestContextOnly(recorder, f.engine)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://benchmark.invalid/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.RemoteAddr = "127.0.0.1:41234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session-id", "synthetic-native-replay-benchmark-session")
	c.Request = req
	c.Set("api_key", f.key)
	result, err := f.svc.ForwardAsChatCompletions(req.Context(), c, f.account, body, "", "")
	if err != nil {
		return nil, err
	}
	if result == nil || result.Usage.InputTokens != 1081 || result.Usage.CacheReadInputTokens != 512 || result.Usage.OutputTokens != 17 || recorder.Code != 200 || recorder.Body.Len() == 0 {
		return nil, errors.New("benchmark failed to complete a real Chat forwarding operation")
	}
	return recorder.Body.Bytes(), nil
}
func nativeReplayBenchmarkAssistant(raw []byte, stream bool) (apicompat.ChatMessage, error) {
	if !stream {
		var response apicompat.ChatCompletionsResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			return apicompat.ChatMessage{}, err
		}
		if len(response.Choices) != 1 {
			return apicompat.ChatMessage{}, errors.New("benchmark seed has no unique Chat choice")
		}
		return response.Choices[0].Message, nil
	}
	message := apicompat.ChatMessage{Role: "assistant"}
	var content strings.Builder
	calls := map[int]apicompat.ChatToolCall{}
	finish, done := false, false
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 256<<10)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var chunk apicompat.ChatCompletionsChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return message, err
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				return message, errors.New("benchmark seed has multiple Chat choices")
			}
			if choice.FinishReason != nil {
				finish = true
			}
			if choice.Delta.Content != nil {
				content.WriteString(*choice.Delta.Content)
			}
			for _, call := range choice.Delta.ToolCalls {
				if call.Index == nil {
					return message, errors.New("benchmark seed tool lacks index")
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
	}
	if err := scanner.Err(); err != nil {
		return message, err
	}
	if !finish || !done {
		return message, errors.New("benchmark seed lacks complete Chat terminal")
	}
	if content.Len() > 0 {
		message.Content = nativeReplayBenchmarkJSON(content.String())
	}
	for index := 0; index < len(calls); index++ {
		call, ok := calls[index]
		if !ok {
			return message, errors.New("benchmark seed tool indexes are incomplete")
		}
		message.ToolCalls = append(message.ToolCalls, call)
	}
	return message, nil
}
func (f *nativeReplayBenchmarkFixture) verify() error {
	if f.key == nil || f.key.ID <= 0 || f.key.UserID <= 0 || f.key.User == nil || f.key.User.ID != f.key.UserID {
		return errors.New("benchmark lacks authenticated User/APIKey context")
	}
	stats := f.probe.stats()
	if stats.Errors != 0 {
		return fmt.Errorf("benchmark synthetic cache returned errors: %+v", stats)
	}
	if f.probe.requiresRuntime && stats.PolicyReads <= 0 {
		return errors.New("benchmark did not visit the runtime policy reader")
	}
	f.upstream.mu.Lock()
	request := append([]byte(nil), f.upstream.lastBody...)
	f.upstream.mu.Unlock()
	var body struct {
		Input []map[string]any `json:"input"`
	}
	if json.Unmarshal(request, &body) != nil || len(body.Input) == 0 {
		return errors.New("benchmark did not observe a real converted Responses request")
	}
	want := make([]map[string]any, len(f.seedOutput))
	for i, item := range f.seedOutput {
		if json.Unmarshal(item, &want[i]) != nil {
			return errors.New("invalid benchmark native output fixture")
		}
	}
	restored := false
	for i := range body.Input {
		if i+len(want) <= len(body.Input) && reflect.DeepEqual(body.Input[i:i+len(want)], want) {
			restored = true
		}
	}
	switch f.mode {
	case "off":
		if restored || stats.Gets != 0 || stats.Puts != 0 {
			return errors.New("disabled benchmark unexpectedly executed replay/cache work")
		}
	case "miss":
		if !f.probe.requiresRuntime || stats.Gets <= 0 || stats.Hits != 0 || stats.Puts < 2 || restored {
			return errors.New("enabled miss benchmark did not exercise a real cache miss and commit")
		}
	case "hit":
		if !f.probe.requiresRuntime || stats.Gets <= 0 || stats.Hits <= 0 || stats.Puts < 2 || !restored {
			return fmt.Errorf("enabled hit benchmark did not restore the complete native output through Chat: stats=%+v restored=%t", stats, restored)
		}
	default:
		return errors.New("unknown benchmark mode")
	}
	if stats.Entries > 2 {
		return errors.New("benchmark cache grew beyond the fixed two-turn fixture")
	}
	return nil
}
func runNativeReplayChatBenchmark(b *testing.B, mode string) {
	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "streamed"
		}
		b.Run(name, func(b *testing.B) {
			f := newNativeReplayBenchmarkFixture(b, mode, stream)
			before := f.probe.stats()
			calls := f.upstream.calls.Load()
			b.ReportAllocs()
			b.SetBytes(int64(len(f.body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := f.forward(f.body); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			after := f.probe.stats()
			actualCalls := f.upstream.calls.Load() - calls
			if actualCalls != int64(b.N) {
				b.Fatalf("expected %d full upstream forwarding calls, got %d", b.N, actualCalls)
			}
			if mode != "off" && after.Gets-before.Gets < int64(b.N) {
				b.Fatal("timed iterations bypassed the enabled cache lookup path")
			}
			if mode == "hit" && after.Hits-before.Hits < int64(b.N) {
				b.Fatal("timed hit iterations did not hit the native-state cache")
			}
			if mode == "miss" && after.Hits != before.Hits {
				b.Fatal("timed miss iterations unexpectedly hit the cache")
			}
			b.ReportMetric(float64(actualCalls)/float64(b.N), "upstream_calls/op")
			b.ReportMetric(float64(after.Gets-before.Gets)/float64(b.N), "replay_reads/op")
			b.ReportMetric(float64(after.Hits-before.Hits)/float64(b.N), "replay_hits/op")
			// Recheck real egress after measurement, so a setup-only cache hit cannot
			// masquerade as sustained restoration. This check is outside timing.
			f.upstream.inspect = true
			if _, err := f.forward(f.body); err != nil {
				b.Fatal(err)
			}
			if err := f.verify(); err != nil {
				b.Fatal(err)
			}
		})
	}
}
func BenchmarkNativeReasoningReplayChatOff(b *testing.B) { runNativeReplayChatBenchmark(b, "off") }
func TestNativeReasoningReplayBenchmarkFixtureExecution(t *testing.T) {
	for _, mode := range nativeReplayBenchmarkModes() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				f := newNativeReplayBenchmarkFixture(t, mode, stream)
				before := f.upstream.calls.Load()
				if _, err := f.forward(f.body); err != nil {
					t.Fatal(err)
				}
				if f.upstream.calls.Load() != before+1 {
					t.Fatal("fixture did not execute exactly one full forwarding call")
				}
				f.upstream.inspect = true
				if _, err := f.forward(f.body); err != nil {
					t.Fatal(err)
				}
				if err := f.verify(); err != nil {
					t.Fatal(err)
				}
				t.Logf("fixture executed mode=%s stream=%t runtime_required=%t stats=%+v full_forward_calls=%d", mode, stream, f.probe.requiresRuntime, f.probe.stats(), f.upstream.calls.Load())
			})
		}
	}
}

// BEGIN CANDIDATE NATIVE REPLAY BOOTSTRAP
// Only this block differs in the baseline overlay. The measured common request,
// response, config, transport and ForwardAsChatCompletions loop remain identical.
type nativeReplayBenchmarkPolicy struct {
	mode  string
	reads atomic.Int64
}

func (p *nativeReplayBenchmarkPolicy) NativeReasoningReplayPolicy(context.Context) (NativeReasoningReplayRuntimeConfig, error) {
	p.reads.Add(1)
	return NativeReasoningReplayRuntimeConfig{Mode: p.mode, UserIDs: []int64{428}}, nil
}

type nativeReplayBenchmarkStore struct {
	mu     sync.Mutex
	miss   bool
	values map[string]NativeReasoningReplayValue
	stats  nativeReplayBenchmarkStats
}

func (s *nativeReplayBenchmarkStore) GetNativeReasoningReplay(ctx context.Context, owner string, keys []string, maxBytes int) (map[string]NativeReasoningReplayValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Gets++
	if err := ctx.Err(); err != nil {
		s.stats.Errors++
		return nil, err
	}
	result := map[string]NativeReasoningReplayValue{}
	if s.miss {
		return result, nil
	}
	for _, key := range keys {
		if value, ok := s.values[owner+":"+key]; ok && len(value.Ciphertext) <= maxBytes {
			result[key] = value
			s.stats.Hits++
		}
	}
	return result, nil
}
func (s *nativeReplayBenchmarkStore) PutNativeReasoningReplay(ctx context.Context, value NativeReasoningReplayWrite, _ NativeReasoningReplayLimits) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Puts++
	if err := ctx.Err(); err != nil {
		s.stats.Errors++
		return err
	}
	key := value.Owner + ":" + value.Key
	if old, ok := s.values[key]; ok {
		if old.Digest != value.Digest {
			s.stats.Errors++
			return ErrNativeReasoningReplayAmbiguous
		}
		return nil
	}
	if len(s.values) >= 2 {
		s.stats.Errors++
		return ErrNativeReasoningReplayCapacity
	}
	s.values[key] = value.NativeReasoningReplayValue
	return nil
}
func installNativeReplayBenchmarkRuntime(svc *OpenAIGatewayService, cfg *config.Config, mode string) *nativeReplayBenchmarkProbe {
	policyMode := NativeReasoningReplayModeReplay
	if mode == "off" {
		policyMode = NativeReasoningReplayModeOff
	}
	policy := &nativeReplayBenchmarkPolicy{mode: policyMode}
	store := &nativeReplayBenchmarkStore{miss: mode == "miss", values: map[string]NativeReasoningReplayValue{}}
	svc.SetNativeReasoningReplayRuntime(policy, NewNativeReasoningReplay(store, newNativeReasoningReplayCipher(cfg), DefaultNativeReasoningReplayLimits()))
	return &nativeReplayBenchmarkProbe{requiresRuntime: true, stats: func() nativeReplayBenchmarkStats {
		store.mu.Lock()
		defer store.mu.Unlock()
		result := store.stats
		result.PolicyReads = policy.reads.Load()
		result.Entries = int64(len(store.values))
		return result
	}}
}
func nativeReplayBenchmarkModes() []string { return []string{"off", "miss", "hit"} }
func BenchmarkNativeReasoningReplayChatEnabledMiss(b *testing.B) {
	runNativeReplayChatBenchmark(b, "miss")
}
func BenchmarkNativeReasoningReplayChatEnabledHit(b *testing.B) {
	runNativeReplayChatBenchmark(b, "hit")
}
func TestNativeReasoningReplayBenchmarkRejectsMissingRuntimeEvidence(t *testing.T) {
	f := newNativeReplayBenchmarkFixture(t, "off", false)
	f.probe.stats = func() nativeReplayBenchmarkStats { return nativeReplayBenchmarkStats{} }
	if f.verify() == nil {
		t.Fatal("zero runtime activity was accepted as an off-path measurement")
	}
}

// END CANDIDATE NATIVE REPLAY BOOTSTRAP
