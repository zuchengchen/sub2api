package service_test

// Real local HTTP ingress and HTTP/SSE egress exercise the production bridge.
// Only the selected upstream transport and authenticated test identity are
// supplied by this fixture; no external origin can be dialled.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type gpt61ScopeHTTPPolicy struct {
	config service.NativeReasoningReplayRuntimeConfig
	err    error
	reads  atomic.Int64
}

func (p *gpt61ScopeHTTPPolicy) NativeReasoningReplayPolicy(context.Context) (service.NativeReasoningReplayRuntimeConfig, error) {
	p.reads.Add(1)
	return p.config, p.err
}

type gpt61ScopeHTTPHarness struct {
	server           *httptest.Server
	store            *nativeTrafficStore
	policy           *gpt61ScopeHTTPPolicy
	mu               sync.Mutex
	rows             []nativeTrafficRow
	forwardErrors    []error
	firstPairBarrier chan struct{}
}

func newGPT61ScopeHTTPHarness(t *testing.T, p *gpt61ScopeHTTPPolicy, mapping map[string]any, defaultModel string) *gpt61ScopeHTTPHarness {
	t.Helper()
	h := &gpt61ScopeHTTPHarness{policy: p, store: &nativeTrafficStore{values: map[string]service.NativeReasoningReplayValue{}, poison: map[string]bool{}}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err == nil && r.Header.Get("Content-Encoding") == "zstd" {
			d, e := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(8<<20))
			if e != nil {
				err = e
			} else {
				body, err = d.DecodeAll(body, nil)
				d.Close()
			}
		}
		if err != nil {
			http.Error(w, "read fixture request", 400)
			return
		}
		h.mu.Lock()
		round := len(h.rows) + 1
		output := nativeTrafficOutput(round)
		h.rows = append(h.rows, nativeTrafficRow{Body: body, Output: output, Status: 200, Terminal: true, Remote: r.RemoteAddr, AuthorizationHash: fmt.Sprintf("%x", sha256.Sum256([]byte(r.Header.Get("Authorization"))))})
		barrier := h.firstPairBarrier
		if barrier != nil && round == 2 {
			close(barrier)
		}
		h.mu.Unlock()
		if barrier != nil && round <= 2 {
			select {
			case <-barrier:
			case <-r.Context().Done():
				return
			}
		}
		model := gjson.GetBytes(body, "model").String()
		id := fmt.Sprintf("resp_fixture_%d", round)
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(v any) {
			wire := append(append([]byte("data: "), nativeTrafficJSON(v)...), '\n', '\n')
			for len(wire) > 0 {
				n := min(23, len(wire))
				if _, err := w.Write(wire[:n]); err != nil {
					return
				}
				wire = wire[n:]
				w.(http.Flusher).Flush()
			}
		}
		emit(map[string]any{"type": "response.created", "response": map[string]any{"id": id, "model": model, "status": "in_progress"}})
		for i, item := range output {
			emit(map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
			if i == 1 {
				emit(map[string]any{"type": "response.output_text.delta", "output_index": i, "content_index": 0, "delta": fmt.Sprintf("Inspecting synthetic step %d.", round)})
			}
			if i >= 2 {
				emit(map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "delta": `{"location":"synthetic"}`})
			}
			emit(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
		emit(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "model": model, "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 101, "output_tokens": 17, "total_tokens": 118, "input_tokens_details": map[string]any{"cached_tokens": 31}}}})
	}))
	t.Cleanup(upstream.Close)
	origin, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != origin.Host {
			return nil, errors.New("external fixture connection forbidden")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	sender := &nativeTrafficHTTP{origin: origin, client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("fixture redirect forbidden") }}}
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("17", 32)
	cfg.JWT.Secret = "synthetic-gpt61-scope-fixture"
	yes, no := true, false
	cfg.Gateway.CodexFullSynthesis = &yes
	cfg.Gateway.CodexWire.MapOrderJitter = &no
	cfg.Gateway.CodexWire.Profile = "auto"
	cipher, err := repository.NewAESEncryptor(cfg)
	require.NoError(t, err)
	svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, sender, nil, nil, nil, nil, nil, nil, nil, nil)
	svc.StopOpenAICodexTicketHarvester()
	t.Cleanup(svc.CloseOpenAIWSPool)
	svc.SetNativeReasoningReplayRuntime(p, service.NewNativeReasoningReplay(h.store, cipher, service.DefaultNativeReasoningReplayLimits()))
	account := &service.Account{ID: 7001, Name: "synthetic-gpt61-scope", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 8,
		Credentials: map[string]any{"access_token": "synthetic-local-token", "chatgpt_account_id": "synthetic-upstream-owner", "model_mapping": mapping},
		Extra:       map[string]any{"codex_fingerprint_mode": "full", "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111"}}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong fixture route", 404)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "read fixture ingress", 400)
			return
		}
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		user, _ := strconv.ParseInt(r.Header.Get("X-Fixture-User"), 10, 64)
		key, _ := strconv.ParseInt(r.Header.Get("X-Fixture-Key"), 10, 64)
		if user > 0 && key > 0 {
			c.Set("api_key", &service.APIKey{ID: key, UserID: user, User: &service.User{ID: user}})
		}
		selected := *account
		if r.Header.Get("X-Fixture-Account") == "other" {
			selected.ID++
			selected.Credentials = map[string]any{"access_token": "other-synthetic-local-token", "chatgpt_account_id": "other-synthetic-upstream-owner", "model_mapping": mapping}
		}
		if r.Header.Get("X-Fixture-Account") == "rotated_credentials" {
			// Keep the site account ID fixed while changing its upstream identity.
			selected.Credentials = map[string]any{"access_token": "rotated-synthetic-local-token", "chatgpt_account_id": "rotated-synthetic-upstream-owner", "model_mapping": mapping}
		}
		result, err := svc.ForwardAsChatCompletions(r.Context(), c, &selected, body, "", defaultModel)
		if err == nil && (result == nil || result.Usage.InputTokens != 101 || result.Usage.CacheReadInputTokens != 31 || result.Usage.OutputTokens != 17) {
			err = errors.New("fixture raw usage changed")
		}
		if err != nil {
			h.mu.Lock()
			h.forwardErrors = append(h.forwardErrors, err)
			h.mu.Unlock()
			if !c.Writer.Written() {
				c.JSON(502, gin.H{"error": "fixture forwarding failed"})
			}
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

type gpt61ScopeHTTPClient struct {
	user, key                   int64
	session, userAgent, account string
}

func (h *gpt61ScopeHTTPHarness) call(history []json.RawMessage, model string, stream bool, who gpt61ScopeHTTPClient) (apicompat.ChatMessage, nativeTrafficRow, error) {
	return h.callWithRequest(history, model, stream, who, nil)
}

// The decorator changes only explicitly supplied client metadata and headers in
// the existing real-HTTP fixture; default callers retain identical requests.
func (h *gpt61ScopeHTTPHarness) callWithRequest(history []json.RawMessage, model string, stream bool, who gpt61ScopeHTTPClient, decorate func(map[string]any, http.Header)) (apicompat.ChatMessage, nativeTrafficRow, error) {
	tools := []any{}
	for _, name := range []string{"lookup_temperature", "lookup_humidity"} {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": name, "description": "Read synthetic local fixture data.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"location": map[string]any{"type": "string"}}, "required": []string{"location"}}}})
	}
	payload := map[string]any{"model": model, "messages": history, "stream": stream, "stream_options": map[string]any{"include_usage": true}, "tools": tools, "reasoning_effort": "high", "parallel_tool_calls": true}
	body := nativeTrafficJSON(payload)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return apicompat.ChatMessage{}, nativeTrafficRow{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fixture-User", strconv.FormatInt(who.user, 10))
	req.Header.Set("X-Fixture-Key", strconv.FormatInt(who.key, 10))
	req.Header.Set("User-Agent", who.userAgent)
	if who.session != "" {
		req.Header.Set("session-id", who.session)
	}
	if who.account != "" {
		req.Header.Set("X-Fixture-Account", who.account)
	}
	if decorate != nil {
		decorate(payload, req.Header)
		body = nativeTrafficJSON(payload)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	response, err := h.server.Client().Do(req)
	if err != nil {
		return apicompat.ChatMessage{}, nativeTrafficRow{}, err
	}
	raw, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		return apicompat.ChatMessage{}, nativeTrafficRow{}, readErr
	}
	if response.StatusCode != 200 {
		return apicompat.ChatMessage{}, nativeTrafficRow{}, fmt.Errorf("fixture HTTP status %d", response.StatusCode)
	}
	var assistant apicompat.ChatMessage
	if stream {
		assistant, err = nativeTrafficDecodeChatSSE(raw)
	} else {
		var decoded apicompat.ChatCompletionsResponse
		err = json.Unmarshal(raw, &decoded)
		if err == nil && len(decoded.Choices) == 1 {
			assistant = decoded.Choices[0].Message
		} else if err == nil {
			err = errors.New("missing unique Chat completion")
		}
	}
	if err != nil {
		return assistant, nativeTrafficRow{}, err
	}
	if len(assistant.ToolCalls) != 2 {
		return assistant, nativeTrafficRow{}, errors.New("tool relationship lost")
	}
	rowID, err := strconv.Atoi(strings.TrimPrefix(assistant.ToolCalls[0].ID, "call_a_fixture_"))
	if err != nil {
		return assistant, nativeTrafficRow{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if rowID < 1 || rowID > len(h.rows) {
		return assistant, nativeTrafficRow{}, errors.New("missing upstream evidence")
	}
	return assistant, h.rows[rowID-1], nil
}
func (h *gpt61ScopeHTTPHarness) assertNoForwardErrors(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Empty(t, h.forwardErrors)
}
func gpt61ScopeAllUsersPolicy() *gpt61ScopeHTTPPolicy {
	return &gpt61ScopeHTTPPolicy{config: service.NativeReasoningReplayRuntimeConfig{Mode: "replay", UserIDs: []int64{428}, AllUsersModels: []string{"gpt-6.1-sol"}}}
}

func TestGPT61SolChatCacheModelScopeHTTPOrdinaryUsers(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%v", stream), func(t *testing.T) {
			h := newGPT61ScopeHTTPHarness(t, gpt61ScopeAllUsersPolicy(), nil, "")
			for _, who := range []gpt61ScopeHTTPClient{{101, 501, "shared-session", "WorkBuddy/1.0", ""}, {102, 502, "shared-session", "", ""}} {
				history := nativeTrafficInitialHistory()
				var previous []nativeTrafficRow
				for round := 0; round < 3; round++ {
					assistant, row, err := h.call(history, "gpt-6.1-sol", stream, who)
					require.NoError(t, err)
					require.Equal(t, round, strings.Count(string(row.Body), `"type":"reasoning"`), "user %d round %d", who.user, round)
					for _, old := range previous {
						require.Zero(t, nativeTrafficComparisonExit(row.Body, old.Output), "complete native output and tool relations must survive")
					}
					previous = append(previous, row)
					history = nativeTrafficContinue(history, assistant)
				}
			}
			h.assertNoForwardErrors(t)
			require.EqualValues(t, 1, h.policy.reads.Load())
		})
	}
}

func TestGPT61SolChatCacheModelScopeHTTPPolicyAndMapping(t *testing.T) {
	type scenario struct {
		name, requested, actual string
		mapping                 map[string]any
		defaultModel            string
		policy                  service.NativeReasoningReplayRuntimeConfig
		policyErr               error
		who                     gpt61ScopeHTTPClient
		replay, save            bool
	}
	ordinary := gpt61ScopeHTTPClient{101, 501, "synthetic-session", "WorkBuddy/1.0", ""}
	target := service.NativeReasoningReplayRuntimeConfig{Mode: "replay", UserIDs: []int64{428}, AllUsersModels: []string{"gpt-6.1-sol"}}
	legacy := service.NativeReasoningReplayRuntimeConfig{Mode: "replay", UserIDs: []int64{428}}
	expired := target
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	off := target
	off.Mode = "off"
	observe := target
	observe.Mode = "observe"
	canary := target
	canary.AllUsersModels = nil
	canary.ModelUserIDs = map[string][]int64{"gpt-6.1-sol": {101}}
	promoted := target
	promoted.ModelUserIDs = map[string][]int64{"gpt-6.1-sol": {101}}
	invalidCanary := canary
	invalidCanary.ModelUserIDs = map[string][]int64{"gpt-6.1-sol": {0}}
	invalid := target
	invalid.AllUsersModels = []string{"*"}
	cases := []scenario{
		{name: "canary_target", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: canary, who: ordinary, replay: true, save: true},
		{name: "canary_other_model_off", requested: "gpt-5.5", actual: "gpt-5.5", policy: canary, who: ordinary},
		{name: "ordinary_target_before_promotion_off", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: canary, who: gpt61ScopeHTTPClient{102, 502, "synthetic-session", "", ""}},
		{name: "ordinary_target_after_promotion", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: promoted, who: gpt61ScopeHTTPClient{102, 502, "synthetic-session", "", ""}, replay: true, save: true},
		{name: "invalid_canary_closes_legacy", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: invalidCanary, who: gpt61ScopeHTTPClient{428, 794, "legacy-session", "", ""}},
		{name: "non_target_ordinary_off", requested: "gpt-5.5", actual: "gpt-5.5", policy: target, who: ordinary},
		{name: "legacy_428_other_model", requested: "gpt-5.5", actual: "gpt-5.5", policy: legacy, who: gpt61ScopeHTTPClient{428, 794, "legacy-session", "", ""}, replay: true, save: true},
		{name: "legacy_ordinary_target_off", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: legacy, who: ordinary},
		{name: "legacy_empty_list_off", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: service.NativeReasoningReplayRuntimeConfig{Mode: "replay"}, who: ordinary},
		{name: "global_off", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: off, who: ordinary},
		{name: "observe_does_not_replay", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: observe, who: ordinary, save: true},
		{name: "expired", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: expired, who: ordinary},
		{name: "invalid_scope", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: invalid, who: ordinary},
		{name: "read_failure", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: target, policyErr: errors.New("synthetic policy read failure"), who: ordinary},
		{name: "oauth_stored_mapping_into_target_ignored", requested: "customer-model", actual: "customer-model", mapping: map[string]any{"customer-model": "gpt-6.1-sol"}, policy: target, who: ordinary},
		{name: "oauth_stored_mapping_out_of_target_ignored", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", mapping: map[string]any{"gpt-6.1-sol": "gpt-5.5"}, policy: target, who: ordinary, replay: true, save: true},
		{name: "normalized_dispatch_mapping_into_target", requested: "customer-model", actual: "gpt-6.1-sol", defaultModel: "GPT-6.1-SOL", policy: target, who: ordinary, replay: true, save: true},
		{name: "dispatch_mapping_into_target", requested: "customer-model", actual: "gpt-6.1-sol", defaultModel: "gpt-6.1-sol", policy: target, who: ordinary, replay: true, save: true},
		{name: "dispatch_mapping_out_of_target", requested: "gpt-6.1-sol", actual: "gpt-5.5", defaultModel: "gpt-5.5", policy: target, who: ordinary},
		{name: "missing_auth", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: target, who: gpt61ScopeHTTPClient{0, 0, "synthetic-session", "", ""}},
		{name: "missing_session", requested: "gpt-6.1-sol", actual: "gpt-6.1-sol", policy: target, who: gpt61ScopeHTTPClient{101, 501, "", "", ""}},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%v", tc.name, stream), func(t *testing.T) {
				h := newGPT61ScopeHTTPHarness(t, &gpt61ScopeHTTPPolicy{config: tc.policy, err: tc.policyErr}, tc.mapping, tc.defaultModel)
				history := nativeTrafficInitialHistory()
				first, firstRow, err := h.call(history, tc.requested, stream, tc.who)
				require.NoError(t, err)
				_, next, err := h.call(nativeTrafficContinue(history, first), tc.requested, stream, tc.who)
				require.NoError(t, err)
				require.Equal(t, tc.actual, gjson.GetBytes(next.Body, "model").String())
				if tc.replay {
					require.Zero(t, nativeTrafficComparisonExit(next.Body, firstRow.Output))
				} else {
					require.NotContains(t, string(next.Body), "opaque_reasoning_")
				}
				h.store.mu.Lock()
				puts := h.store.puts
				h.store.mu.Unlock()
				if tc.save {
					require.Greater(t, puts, 0)
				} else {
					require.Zero(t, puts)
				}
				h.assertNoForwardErrors(t)
			})
		}
	}
}

func TestGPT61SolChatCacheModelScopeHTTPIsolationAndParallel(t *testing.T) {
	t.Run("identities", func(t *testing.T) {
		h := newGPT61ScopeHTTPHarness(t, gpt61ScopeAllUsersPolicy(), nil, "")
		original := gpt61ScopeHTTPClient{101, 501, "shared-session", "", ""}
		history := nativeTrafficInitialHistory()
		first, _, err := h.call(history, "gpt-6.1-sol", true, original)
		require.NoError(t, err)
		continued := nativeTrafficContinue(history, first)
		for _, other := range []gpt61ScopeHTTPClient{{102, 501, "shared-session", "", ""}, {101, 502, "shared-session", "", ""}, {101, 501, "other-session", "", ""}, {101, 501, "shared-session", "", "other"}} {
			_, row, err := h.call(continued, "gpt-6.1-sol", true, other)
			require.NoError(t, err)
			require.NotContains(t, string(row.Body), "opaque_reasoning_", "different user/Key/session/credential cannot inject original state")
		}
		h.assertNoForwardErrors(t)
	})
	t.Run("parallel_users", func(t *testing.T) {
		h := newGPT61ScopeHTTPHarness(t, gpt61ScopeAllUsersPolicy(), nil, "")
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, user := range []int64{101, 102} {
			wg.Add(1)
			go func(user int64) {
				defer wg.Done()
				history := nativeTrafficInitialHistory()
				var previous []nativeTrafficRow
				for round := 0; round < 3; round++ {
					assistant, row, err := h.call(history, "gpt-6.1-sol", true, gpt61ScopeHTTPClient{user, 500 + user, "shared-session", "", ""})
					if err != nil {
						errs <- err
						return
					}
					if strings.Count(string(row.Body), `"type":"reasoning"`) != round {
						errs <- fmt.Errorf("user %d round %d incorrect restored count", user, round)
						return
					}
					for _, old := range previous {
						if nativeTrafficComparisonExit(row.Body, old.Output) != 0 {
							errs <- fmt.Errorf("user %d round %d native output mismatch", user, round)
							return
						}
					}
					previous = append(previous, row)
					history = nativeTrafficContinue(history, assistant)
				}
			}(user)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		h.assertNoForwardErrors(t)
		require.EqualValues(t, 1, h.policy.reads.Load())
	})
	t.Run("parallel_branches", func(t *testing.T) {
		h := newGPT61ScopeHTTPHarness(t, gpt61ScopeAllUsersPolicy(), nil, "")
		// The first upstream response cannot finish until both identical input
		// branches have reached the real upstream HTTP socket.
		h.firstPairBarrier = make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for branch := 0; branch < 2; branch++ {
			wg.Add(1)
			go func(branch int) {
				defer wg.Done()
				history := nativeTrafficInitialHistory()
				var previous []nativeTrafficRow
				for round := 0; round < 3; round++ {
					assistant, row, err := h.call(history, "gpt-6.1-sol", true, gpt61ScopeHTTPClient{101, 501, "shared-session", "", ""})
					if err != nil {
						errs <- err
						return
					}
					if strings.Count(string(row.Body), `"type":"reasoning"`) != round {
						errs <- fmt.Errorf("branch %d round %d incorrect restored count", branch, round)
						return
					}
					for _, old := range previous {
						if nativeTrafficComparisonExit(row.Body, old.Output) != 0 {
							errs <- fmt.Errorf("branch %d round %d inherited another native branch", branch, round)
							return
						}
					}
					previous = append(previous, row)
					history = nativeTrafficContinue(history, assistant)
				}
			}(branch)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		h.assertNoForwardErrors(t)
		require.EqualValues(t, 1, h.policy.reads.Load())
	})

}
