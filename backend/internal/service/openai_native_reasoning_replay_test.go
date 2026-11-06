package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

type nativeReplayTestStore struct {
	mu     sync.Mutex
	values map[string]NativeReasoningReplayValue
	poison map[string]bool
	block  bool
	err    error
	gets   int
}

func newNativeReplayTestStore() *nativeReplayTestStore {
	return &nativeReplayTestStore{values: map[string]NativeReasoningReplayValue{}, poison: map[string]bool{}}
}
func (s *nativeReplayTestStore) GetNativeReasoningReplay(ctx context.Context, owner string, keys []string, maxBytes int) (map[string]NativeReasoningReplayValue, error) {
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]NativeReasoningReplayValue{}
	for _, key := range keys {
		if v, ok := s.values[owner+key]; ok && !s.poison[owner+key] {
			out[key] = v
		}
	}
	return out, nil
}
func (s *nativeReplayTestStore) PutNativeReasoningReplay(ctx context.Context, value NativeReasoningReplayWrite, limits NativeReasoningReplayLimits) error {
	if s.block {
		<-ctx.Done()
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	key := value.Owner + value.Key
	if s.poison[key] {
		return ErrNativeReasoningReplayAmbiguous
	}
	if old, ok := s.values[key]; ok {
		if old.Digest != value.Digest {
			s.poison[key] = true
			return ErrNativeReasoningReplayAmbiguous
		}
		return nil
	}
	s.values[key] = value.NativeReasoningReplayValue
	return nil
}

func (s *nativeReplayTestStore) InvalidateNativeReasoningReplay(ctx context.Context, owner string, refs []NativeReasoningReplayReference) error {
	if s.block {
		<-ctx.Done()
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	for _, ref := range refs {
		key := owner + ref.Key
		if value, ok := s.values[key]; ok && value.Digest == ref.Digest {
			s.poison[key] = true
		}
	}
	return nil
}

func nativeReplayFixture(t testing.TB) (*NativeReasoningReplay, *nativeReplayTestStore, NativeReasoningReplayRequest, NativeReasoningReplayCompletion) {
	t.Helper()
	store := newNativeReplayTestStore()
	cipher := &liveAttestationAES{key: sha256.Sum256([]byte("synthetic-native-replay-key"))}
	r := NewNativeReasoningReplay(store, cipher, NativeReasoningReplayLimits{})
	req := NativeReasoningReplayRequest{
		Scope:      NativeReasoningReplayScope{UserID: 101, APIKeyID: 202, SessionID: "synthetic-session", Model: "gpt-5.5", CredentialIdentity: "synthetic-owner/revision-1"},
		Messages:   []json.RawMessage{json.RawMessage(`{"role":"system","content":"Use the declared tool."}`), json.RawMessage(`{"role":"user","content":"Look up synthetic weather."}`)},
		Parameters: json.RawMessage(`{"model":"gpt-5.5","instructions":"synthetic instructions","tools":[{"type":"function","name":"python__custom","parameters":{"type":"object"}}],"reasoning":{"effort":"high"}}`),
	}
	response := json.RawMessage(`{"id":"resp_synthetic","status":"completed","model":"gpt-5.5","output":[{"type":"reasoning","id":"rs_synthetic","summary":[{"type":"summary_text","text":"I will inspect the fixture."}],"encrypted_content":"opaque-synthetic-reasoning","future":{"sequence":9007199254740993}},{"type":"message","id":"msg_synthetic","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"Checking."}],"encrypted_content":"opaque-synthetic-message"},{"type":"function_call","id":"fc_synthetic","status":"completed","call_id":"call_synthetic","name":"python__custom","namespace":"functions","arguments":"{\"city\":\"fixture\"}","encrypted_function_args":["opaque-synthetic-args"]}]}`)
	var native apicompat.ResponsesResponse
	require.NoError(t, json.Unmarshal(response, &native))
	native.Output[2].Name = "python"
	chat := apicompat.ResponsesToChatCompletions(&native, req.Scope.Model)
	assistant, err := json.Marshal(chat.Choices[0].Message)
	require.NoError(t, err)
	completion := NativeReasoningReplayCompletion{Response: response, SourceInput: nativeReplayConvertedInput(t, req), DeliveredAssistant: assistant, DeliveredComplete: true, ToolNameReverse: map[string]string{"python__custom": "python"}}
	return r, store, req, completion
}

func nativeReplayContinuation(req NativeReasoningReplayRequest, c NativeReasoningReplayCompletion) NativeReasoningReplayRequest {
	out := req
	out.Messages = append(append([]json.RawMessage(nil), req.Messages...), c.DeliveredAssistant, json.RawMessage(`{"role":"tool","tool_call_id":"call_synthetic","content":"synthetic clear"}`))
	return out
}

func nativeReplayConvertedInput(t testing.TB, req NativeReasoningReplayRequest) json.RawMessage {
	t.Helper()
	var messages []apicompat.ChatMessage
	for _, raw := range req.Messages {
		var m apicompat.ChatMessage
		require.NoError(t, json.Unmarshal(raw, &m))
		messages = append(messages, m)
	}
	converted, err := apicompat.ChatCompletionsToResponses(&apicompat.ChatCompletionsRequest{Model: req.Scope.Model, Messages: messages})
	require.NoError(t, err)
	return converted.Input
}

func TestNativeReasoningReplayRoundTripProductionConversion(t *testing.T) {
	r, store, req, completion := nativeReplayFixture(t)
	require.NoError(t, r.Commit(context.Background(), req, completion))
	for _, value := range store.values {
		require.NotContains(t, value.Ciphertext, "opaque-synthetic")
		require.NotContains(t, value.Ciphertext, "Checking")
		require.NotContains(t, value.Ciphertext, "python")
	}
	next := nativeReplayContinuation(req, completion)
	compat := nativeReplayConvertedInput(t, next)
	// Reproduce the old production conversion's loss, not a fake projection.
	require.Contains(t, string(compat), `\u003cthinking\u003e`)
	require.NotContains(t, string(compat), "encrypted_content")
	require.NotContains(t, string(compat), "commentary")
	replacements, err := r.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Len(t, replacements, 1)
	require.Equal(t, 2, replacements[0].MessageIndex)
	require.Len(t, replacements[0].Output, 3)
	replayed, err := ApplyNativeReasoningReplayInput(compat, replacements)
	require.NoError(t, err)
	var items []json.RawMessage
	require.NoError(t, json.Unmarshal(replayed, &items))
	var original struct {
		Output []json.RawMessage `json:"output"`
	}
	require.NoError(t, json.Unmarshal(completion.Response, &original))
	for i, item := range original.Output {
		require.JSONEq(t, string(item), string(items[i+2]))
	}
	require.NotContains(t, string(replayed), `\u003cthinking\u003e`)
	require.Contains(t, string(replayed), "9007199254740993")
	require.Contains(t, string(replayed), "encrypted_function_args")
	require.Contains(t, string(replayed), "commentary")
	require.Equal(t, "python", replacements[0].ToolNameReverse["python__custom"])
	// Input slices and messages remain owned by the caller.
	require.Contains(t, string(compat), `\u003cthinking\u003e`)
}

func TestNativeReasoningReplayMultipleToolsAndConsecutiveRounds(t *testing.T) {
	r, _, req, c := nativeReplayFixture(t)
	var response map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(c.Response, &response))
	var output []json.RawMessage
	require.NoError(t, json.Unmarshal(response["output"], &output))
	output = append(output, json.RawMessage(`{"type":"function_call","id":"fc_second_tool","call_id":"call_second_tool","name":"lookup","arguments":"{\"id\":2}"}`))
	response["output"], _ = json.Marshal(output)
	c.Response, _ = json.Marshal(response)
	var typed apicompat.ResponsesResponse
	require.NoError(t, json.Unmarshal(c.Response, &typed))
	typed.Output[2].Name = "python"
	chat := apicompat.ResponsesToChatCompletions(&typed, req.Scope.Model)
	c.DeliveredAssistant, _ = json.Marshal(chat.Choices[0].Message)
	require.NoError(t, r.Commit(context.Background(), req, c))
	next := nativeReplayContinuation(req, c)
	got, err := r.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Empty(t, got, "a partial set of tool results cannot replay the assistant group")
	next.Messages = append(next.Messages, json.RawMessage(`{"role":"tool","tool_call_id":"call_second_tool","content":"second result"}`))
	got, err = r.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Output, 4)
	// The next native tool round is bound to the entire previous round including
	// both results. It must not overwrite that round's entry or output ordering.
	_, _, _, second := nativeReplayFixture(t)
	second.Response = json.RawMessage(strings.ReplaceAll(string(second.Response), "synthetic", "round_two"))
	second.DeliveredAssistant = json.RawMessage(strings.ReplaceAll(string(second.DeliveredAssistant), "synthetic", "round_two"))
	second.SourceInput, err = ApplyNativeReasoningReplayInput(nativeReplayConvertedInput(t, next), got)
	require.NoError(t, err)
	require.NoError(t, r.Commit(context.Background(), next, second))
	third := nativeReplayContinuation(next, second)
	third.Messages[len(third.Messages)-1] = json.RawMessage(`{"role":"tool","tool_call_id":"call_round_two","content":"third result"}`)
	replacements, err := r.Restore(context.Background(), third)
	require.NoError(t, err)
	require.Len(t, replacements, 2)
	require.Equal(t, []int{2, 5}, []int{replacements[0].MessageIndex, replacements[1].MessageIndex})
	replayed, err := ApplyNativeReasoningReplayInput(nativeReplayConvertedInput(t, third), replacements)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(replayed), `"type":"reasoning"`))
	require.Equal(t, 3, strings.Count(string(replayed), `"type":"function_call_output"`))
	third.Messages[3] = json.RawMessage(`{"role":"tool","tool_call_id":"call_synthetic","content":"edited first result"}`)
	replacements, err = r.Restore(context.Background(), third)
	require.NoError(t, err)
	require.Len(t, replacements, 1, "history edits invalidate only dependent native output")
	require.Equal(t, 2, replacements[0].MessageIndex)
}

func TestNativeReasoningReplayIsolationAndHistoryBoundaries(t *testing.T) {
	cases := map[string]func(*NativeReasoningReplayRequest){
		"user":                func(r *NativeReasoningReplayRequest) { r.Scope.UserID++ },
		"key":                 func(r *NativeReasoningReplayRequest) { r.Scope.APIKeyID++ },
		"session":             func(r *NativeReasoningReplayRequest) { r.Scope.SessionID += "-other" },
		"model":               func(r *NativeReasoningReplayRequest) { r.Scope.Model = "gpt-6.1-sol" },
		"credential_owner":    func(r *NativeReasoningReplayRequest) { r.Scope.CredentialIdentity = "other-owner" },
		"credential_revision": func(r *NativeReasoningReplayRequest) { r.Scope.CredentialIdentity += "-rotated" },
		"parameters": func(r *NativeReasoningReplayRequest) {
			r.Parameters = json.RawMessage(strings.ReplaceAll(string(r.Parameters), "high", "low"))
		},
		"tools": func(r *NativeReasoningReplayRequest) {
			r.Parameters = json.RawMessage(strings.ReplaceAll(string(r.Parameters), "python__custom", "shell"))
		},
		"history_edit": func(r *NativeReasoningReplayRequest) {
			r.Messages[0] = json.RawMessage(`{"role":"system","content":"Different instruction"}`)
		},
		"compaction": func(r *NativeReasoningReplayRequest) {
			r.Messages = append([]json.RawMessage{json.RawMessage(`{"role":"user","content":"Compacted fixture history"}`)}, r.Messages[2:]...)
		},
		"shortened": func(r *NativeReasoningReplayRequest) { r.Messages = r.Messages[1:] },
		"reasoning_changed": func(r *NativeReasoningReplayRequest) {
			r.Messages[2] = json.RawMessage(strings.ReplaceAll(string(r.Messages[2]), "I will inspect", "I might inspect"))
		},
		"assistant_changed": func(r *NativeReasoningReplayRequest) {
			r.Messages[2] = json.RawMessage(strings.ReplaceAll(string(r.Messages[2]), "Checking.", "Checked."))
		},
		"tool_name_changed": func(r *NativeReasoningReplayRequest) {
			r.Messages[2] = json.RawMessage(strings.ReplaceAll(string(r.Messages[2]), "python", "shell"))
		},
		"call_id_changed": func(r *NativeReasoningReplayRequest) {
			r.Messages[2] = json.RawMessage(strings.ReplaceAll(string(r.Messages[2]), "call_synthetic", "call_other"))
			r.Messages[3] = json.RawMessage(strings.ReplaceAll(string(r.Messages[3]), "call_synthetic", "call_other"))
		},
		"missing_tool_result": func(r *NativeReasoningReplayRequest) { r.Messages = r.Messages[:3] },
		"foreign_tool_result": func(r *NativeReasoningReplayRequest) {
			r.Messages[3] = json.RawMessage(strings.ReplaceAll(string(r.Messages[3]), "call_synthetic", "call_other"))
		},
		"duplicate_tool_result": func(r *NativeReasoningReplayRequest) { r.Messages = append(r.Messages, r.Messages[3]) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			r, _, req, c := nativeReplayFixture(t)
			require.NoError(t, r.Commit(context.Background(), req, c))
			next := nativeReplayContinuation(req, c)
			edit(&next)
			got, err := r.Restore(context.Background(), next)
			require.NoError(t, err)
			require.Empty(t, got)
		})
	}
}

func TestNativeReasoningReplayConcurrentForksAndAmbiguity(t *testing.T) {
	r, _, req, c := nativeReplayFixture(t)
	other := c
	other.Response = json.RawMessage(strings.ReplaceAll(string(c.Response), "synthetic", "branch"))
	other.DeliveredAssistant = json.RawMessage(strings.ReplaceAll(string(c.DeliveredAssistant), "synthetic", "branch"))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, completion := range []NativeReasoningReplayCompletion{c, other} {
		wg.Add(1)
		go func(c NativeReasoningReplayCompletion) {
			defer wg.Done()
			errs <- r.Commit(context.Background(), req, c)
		}(completion)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	for _, completion := range []NativeReasoningReplayCompletion{c, other} {
		next := nativeReplayContinuation(req, completion)
		if string(completion.Response) == string(other.Response) {
			next.Messages[3] = json.RawMessage(strings.ReplaceAll(string(next.Messages[3]), "call_synthetic", "call_branch"))
		}
		got, err := r.Restore(context.Background(), next)
		require.NoError(t, err)
		require.Len(t, got, 1)
	}
	// Random application encryption nonces and changing envelope timestamps do
	// not poison a genuinely identical native result.
	require.NoError(t, r.Commit(context.Background(), req, c))
	conflict := c
	conflict.Response = json.RawMessage(strings.ReplaceAll(string(c.Response), "opaque-synthetic-reasoning", "opaque-different-reasoning"))
	require.ErrorIs(t, r.Commit(context.Background(), req, conflict), ErrNativeReasoningReplayAmbiguous)
	require.ErrorIs(t, r.Commit(context.Background(), req, c), ErrNativeReasoningReplayAmbiguous)
	got, err := r.Restore(context.Background(), nativeReplayContinuation(req, c))
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestNativeReasoningReplayCausalPrefixAndOutOfOrderCommits(t *testing.T) {
	for _, scenario := range []string{"missing_ancestor", "out_of_order_commits", "different_native_generation_prefix"} {
		t.Run(scenario, func(t *testing.T) {
			r, store, req, first := nativeReplayFixture(t)
			secondRequest := nativeReplayContinuation(req, first)
			_, _, _, second := nativeReplayFixture(t)
			second.Response = json.RawMessage(strings.ReplaceAll(string(second.Response), "synthetic", "second"))
			second.DeliveredAssistant = json.RawMessage(strings.ReplaceAll(string(second.DeliveredAssistant), "synthetic", "second"))
			// Produce the real native prefix for round B using the same exact
			// splice as production, then delay or lose round A's cache commit.
			require.NoError(t, r.Commit(context.Background(), req, first))
			firstReplacements, err := r.Restore(context.Background(), secondRequest)
			require.NoError(t, err)
			require.Len(t, firstReplacements, 1)
			second.SourceInput, err = ApplyNativeReasoningReplayInput(nativeReplayConvertedInput(t, secondRequest), firstReplacements)
			require.NoError(t, err)
			for key := range store.values {
				delete(store.values, key)
			}
			if scenario == "different_native_generation_prefix" {
				second.SourceInput = nativeReplayConvertedInput(t, secondRequest)
			}
			require.NoError(t, r.Commit(context.Background(), secondRequest, second))
			thirdRequest := nativeReplayContinuation(secondRequest, second)
			thirdRequest.Messages[len(thirdRequest.Messages)-1] = json.RawMessage(`{"role":"tool","tool_call_id":"call_second","content":"second result"}`)
			if scenario != "missing_ancestor" {
				require.NoError(t, r.Commit(context.Background(), req, first))
			}
			matches, err := r.Restore(context.Background(), thirdRequest)
			require.NoError(t, err)
			replayed, err := ApplyNativeReasoningReplayInput(nativeReplayConvertedInput(t, thirdRequest), matches)
			require.NoError(t, err)
			switch scenario {
			case "missing_ancestor":
				require.Len(t, matches, 1, "visible history alone still finds B")
				require.NotContains(t, string(replayed), "encrypted_content", "causal prefix check must reject B without A")
			case "out_of_order_commits":
				require.Len(t, matches, 2)
				require.Contains(t, string(replayed), "opaque-synthetic-reasoning")
				require.Contains(t, string(replayed), "opaque-second-reasoning")
			case "different_native_generation_prefix":
				require.Len(t, matches, 2)
				require.Contains(t, string(replayed), "opaque-synthetic-reasoning")
				require.NotContains(t, string(replayed), "opaque-second-reasoning", "B generated on compatibility history cannot attach to a different native prefix")
			}
		})
	}
}

func TestNativeReasoningReplayInvalidatesOnlyRejectedScopeAndDigest(t *testing.T) {
	r, _, req, c := nativeReplayFixture(t)
	require.NoError(t, r.Commit(context.Background(), req, c))
	next := nativeReplayContinuation(req, c)
	matches, err := r.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	ref := matches[0].Reference
	other := req.Scope
	other.APIKeyID++
	require.ErrorIs(t, r.Invalidate(context.Background(), other, []NativeReasoningReplayReference{ref}), ErrNativeReasoningReplayInvalid)
	stale := ref
	stale.Digest = nativeReplayDigest([]byte("stale synthetic record"))
	require.NoError(t, r.Invalidate(context.Background(), req.Scope, []NativeReasoningReplayReference{stale}))
	matches, err = r.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.NoError(t, r.Invalidate(context.Background(), req.Scope, []NativeReasoningReplayReference{ref}))
	matches, err = r.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Empty(t, matches)
	require.ErrorIs(t, r.Commit(context.Background(), req, c), ErrNativeReasoningReplayAmbiguous)
}

func TestNativeReasoningReplayRejectsIncompleteAndMismatchedDelivery(t *testing.T) {
	cases := map[string]func(*NativeReasoningReplayCompletion){
		"missing_source_input": func(c *NativeReasoningReplayCompletion) { c.SourceInput = nil },
		"undelivered":          func(c *NativeReasoningReplayCompletion) { c.DeliveredComplete = false },
		"incomplete": func(c *NativeReasoningReplayCompletion) {
			c.Response = json.RawMessage(strings.Replace(string(c.Response), `"status":"completed"`, `"status":"incomplete"`, 1))
		},
		"failed": func(c *NativeReasoningReplayCompletion) {
			c.Response = json.RawMessage(strings.Replace(string(c.Response), `"status":"completed"`, `"status":"failed"`, 1))
		},
		"missing_output": func(c *NativeReasoningReplayCompletion) {
			c.Response = json.RawMessage(`{"id":"resp_missing","status":"completed"}`)
		},
		"model_mismatch": func(c *NativeReasoningReplayCompletion) {
			c.Response = json.RawMessage(strings.ReplaceAll(string(c.Response), "gpt-5.5", "gpt-6.1-sol"))
		},
		"duplicate_item_id": func(c *NativeReasoningReplayCompletion) {
			c.Response = json.RawMessage(strings.ReplaceAll(string(c.Response), "fc_synthetic", "rs_synthetic"))
		},
		"unknown_visible_type": func(c *NativeReasoningReplayCompletion) {
			c.Response = json.RawMessage(strings.ReplaceAll(string(c.Response), `"type":"message"`, `"type":"future_output"`))
		},
		"partial_item": func(c *NativeReasoningReplayCompletion) {
			c.Response = json.RawMessage(strings.ReplaceAll(string(c.Response), `"id":"msg_synthetic","role":"assistant","status":"completed"`, `"id":"msg_synthetic","role":"assistant","status":"in_progress"`))
		},
		"partial_client_text": func(c *NativeReasoningReplayCompletion) {
			c.DeliveredAssistant = json.RawMessage(strings.ReplaceAll(string(c.DeliveredAssistant), "Checking.", "Check"))
		},
		"duplicate_client_delta": func(c *NativeReasoningReplayCompletion) {
			c.DeliveredAssistant = json.RawMessage(strings.ReplaceAll(string(c.DeliveredAssistant), "Checking.", "Checking.Checking."))
		},
		"wrong_tool_projection": func(c *NativeReasoningReplayCompletion) { c.ToolNameReverse = nil },
		"duplicate_json_member": func(c *NativeReasoningReplayCompletion) {
			c.Response = json.RawMessage(strings.Replace(string(c.Response), `"status":"completed"`, `"status":"failed","status":"completed"`, 1))
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			r, store, req, c := nativeReplayFixture(t)
			edit(&c)
			require.ErrorIs(t, r.Commit(context.Background(), req, c), ErrNativeReasoningReplayInvalid)
			require.Empty(t, store.values)
		})
	}
	r, store, req, c := nativeReplayFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, r.Commit(ctx, req, c))
	require.Empty(t, store.values)
}

func TestNativeReasoningReplayFailureExpiryAndCapacity(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		r, store, req, c := nativeReplayFixture(t)
		r.limits.IOTimeout = 15 * time.Millisecond
		store.block = true
		start := time.Now()
		_, err := r.Restore(context.Background(), nativeReplayContinuation(req, c))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, time.Since(start), 200*time.Millisecond)
		start = time.Now()
		require.ErrorIs(t, r.Commit(context.Background(), req, c), context.DeadlineExceeded)
		require.Less(t, time.Since(start), 200*time.Millisecond)
	})
	for _, scenario := range []string{"unavailable", "corrupt", "expired", "scope_substitution", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			r, store, req, c := nativeReplayFixture(t)
			require.NoError(t, r.Commit(context.Background(), req, c))
			switch scenario {
			case "unavailable":
				store.err = errors.New("synthetic offline")
			case "corrupt":
				for key, value := range store.values {
					value.Ciphertext = "corrupted"
					store.values[key] = value
				}
			case "expired":
				r.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
			case "scope_substitution":
				for key, value := range store.values {
					plain, err := r.cipher.Decrypt(value.Ciphertext)
					require.NoError(t, err)
					var env nativeReasoningReplayEnvelope
					require.NoError(t, json.Unmarshal([]byte(plain), &env))
					env.Record.ScopeDigest = "another-scope"
					raw, _ := json.Marshal(env)
					value.Ciphertext, err = r.cipher.Encrypt(string(raw))
					require.NoError(t, err)
					store.values[key] = value
				}
			case "oversize":
				r.limits.MaxEntryBytes = 32
			}
			got, err := r.Restore(context.Background(), nativeReplayContinuation(req, c))
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
	t.Run("write_limits", func(t *testing.T) {
		r, store, req, c := nativeReplayFixture(t)
		r.limits.MaxEntryBytes = 128
		require.ErrorIs(t, r.Commit(context.Background(), req, c), ErrNativeReasoningReplayCapacity)
		require.Empty(t, store.values)
	})
	t.Run("history_limits", func(t *testing.T) {
		r, _, req, c := nativeReplayFixture(t)
		r.limits.MaxHistoryBytes = 64
		_, err := r.Restore(context.Background(), nativeReplayContinuation(req, c))
		require.ErrorIs(t, err, ErrNativeReasoningReplayCapacity)
	})
	t.Run("missing_cipher", func(t *testing.T) {
		r, _, req, c := nativeReplayFixture(t)
		r.cipher = nil
		require.ErrorIs(t, r.Commit(context.Background(), req, c), ErrNativeReasoningReplayUnavailable)
	})
}

func TestNativeReasoningReplayCanonicalizationAndAnchors(t *testing.T) {
	r, _, req, c := nativeReplayFixture(t)
	require.NoError(t, r.Commit(context.Background(), req, c))
	next := nativeReplayContinuation(req, c)
	next.Messages[0] = json.RawMessage(`{ "content": "Use the declared tool.", "role": "system" }`)
	replacements, err := r.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Len(t, replacements, 1)
	input := nativeReplayConvertedInput(t, next)
	tampered := json.RawMessage(strings.ReplaceAll(string(input), "synthetic clear", "different result"))
	_, err = ApplyNativeReasoningReplayInput(tampered, replacements)
	require.ErrorIs(t, err, ErrNativeReasoningReplayInvalid)
	_, err = ApplyNativeReasoningReplayInput(input, append(replacements, replacements...))
	require.ErrorIs(t, err, ErrNativeReasoningReplayInvalid)
	// Unknown semantic parameters and duplicate members cannot be ignored.
	next.Parameters = json.RawMessage(`{"reasoning":{"effort":"low","effort":"high"}}`)
	_, err = r.Restore(context.Background(), next)
	require.ErrorIs(t, err, ErrNativeReasoningReplayInvalid)
	limits := (NativeReasoningReplayLimits{TTL: 24 * time.Hour, MaxEntriesGlobal: 1000000}).Normalize()
	require.Equal(t, time.Hour, limits.TTL)
	require.Equal(t, 1024, limits.MaxEntriesGlobal)
}

func TestNativeReasoningReplayCollectorLifecycle(t *testing.T) {
	_, _, _, completion := nativeReplayFixture(t)
	var response struct {
		Output []json.RawMessage `json:"output"`
	}
	require.NoError(t, json.Unmarshal(completion.Response, &response))
	event := func(kind string, index int, item json.RawMessage) []byte {
		raw, _ := json.Marshal(map[string]any{"type": kind, "output_index": index, "item": item})
		return raw
	}
	terminal := append([]byte(`{"type":"response.completed","response":`), completion.Response...)
	terminal = append(terminal, '}')
	t.Run("out_of_order_done_and_duplicates", func(t *testing.T) {
		c := NewNativeReasoningReplayCollector(NativeReasoningReplayLimits{})
		for _, index := range []int{2, 0, 1, 2} {
			c.Observe(event("response.output_item.done", index, response.Output[index]), "")
		}
		_, ok := c.CompletedResponse()
		require.False(t, ok)
		c.Observe(terminal, "")
		c.Observe(terminal, "")
		got, ok := c.CompletedResponse()
		require.True(t, ok)
		require.JSONEq(t, string(completion.Response), string(got))
	})
	t.Run("event_line_type", func(t *testing.T) {
		c := NewNativeReasoningReplayCollector(NativeReasoningReplayLimits{})
		c.Observe([]byte(`{"response":`+string(completion.Response)+`}`), "response.completed")
		_, ok := c.CompletedResponse()
		require.True(t, ok)
	})
	for _, kind := range []string{"response.failed", "response.incomplete", "response.cancelled", "error"} {
		t.Run(kind, func(t *testing.T) {
			c := NewNativeReasoningReplayCollector(NativeReasoningReplayLimits{})
			c.Observe([]byte(fmt.Sprintf(`{"type":%q}`, kind)), "")
			c.Observe(terminal, "")
			_, ok := c.CompletedResponse()
			require.False(t, ok)
		})
	}
	t.Run("conflicting_duplicate", func(t *testing.T) {
		c := NewNativeReasoningReplayCollector(NativeReasoningReplayLimits{})
		c.Observe(event("response.output_item.done", 0, response.Output[0]), "")
		c.Observe(event("response.output_item.done", 0, json.RawMessage(strings.ReplaceAll(string(response.Output[0]), "opaque-synthetic-reasoning", "different"))), "")
		c.Observe(terminal, "")
		_, ok := c.CompletedResponse()
		require.False(t, ok)
	})
	t.Run("done_terminal_mismatch", func(t *testing.T) {
		c := NewNativeReasoningReplayCollector(NativeReasoningReplayLimits{})
		c.Observe(event("response.output_item.done", 0, response.Output[1]), "")
		c.Observe(terminal, "")
		_, ok := c.CompletedResponse()
		require.False(t, ok)
	})
	t.Run("added_missing_from_terminal", func(t *testing.T) {
		c := NewNativeReasoningReplayCollector(NativeReasoningReplayLimits{})
		c.Observe(event("response.output_item.added", 3, json.RawMessage(`{"id":"lost"}`)), "")
		c.Observe(terminal, "")
		_, ok := c.CompletedResponse()
		require.False(t, ok)
	})
	t.Run("disconnect_without_terminal", func(t *testing.T) {
		c := NewNativeReasoningReplayCollector(NativeReasoningReplayLimits{})
		for i, item := range response.Output {
			c.Observe(event("response.output_item.done", i, item), "")
		}
		_, ok := c.CompletedResponse()
		require.False(t, ok)
	})
	t.Run("terminal_without_output", func(t *testing.T) {
		c := NewNativeReasoningReplayCollector(NativeReasoningReplayLimits{})
		c.Observe([]byte(`{"type":"response.completed","response":{"id":"resp","status":"completed"}}`), "")
		_, ok := c.CompletedResponse()
		require.False(t, ok)
	})
	t.Run("after_terminal_delta", func(t *testing.T) {
		c := NewNativeReasoningReplayCollector(NativeReasoningReplayLimits{})
		c.Observe(terminal, "")
		c.Observe([]byte(`{"type":"response.output_text.delta","delta":"late"}`), "")
		_, ok := c.CompletedResponse()
		require.False(t, ok)
	})
	t.Run("oversize", func(t *testing.T) {
		c := NewNativeReasoningReplayCollector(NativeReasoningReplayLimits{MaxEntryBytes: 128})
		c.Observe(terminal, "")
		_, ok := c.CompletedResponse()
		require.False(t, ok)
	})
}

func BenchmarkNativeReasoningReplayCore(b *testing.B) {
	r, _, req, c := nativeReplayFixture(b)
	require.NoError(b, r.Commit(context.Background(), req, c))
	next := nativeReplayContinuation(req, c)
	b.Run("hit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := r.Restore(context.Background(), next)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	next.Scope.APIKeyID++
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := r.Restore(context.Background(), next)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
