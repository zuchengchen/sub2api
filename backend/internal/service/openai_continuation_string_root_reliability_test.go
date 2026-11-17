package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityContinuationStringRootCanonicalHistory(t *testing.T) {
	for _, text := range []string{"", "plain", "line one\n\"quoted\" 中文 \\ literal"} {
		body, err := json.Marshal(map[string]any{"input": text})
		require.NoError(t, err)
		original := append([]byte(nil), body...)
		items, exists, err := openAIWSExtractNormalizedInputSequence(body)
		require.NoError(t, err)
		require.True(t, exists)
		require.Len(t, items, 1)
		require.Equal(t, "message", gjson.GetBytes(items[0], "type").String())
		require.Equal(t, "user", gjson.GetBytes(items[0], "role").String())
		require.Equal(t, text, gjson.GetBytes(items[0], "content").String())
		require.Equal(t, original, body)
		require.True(t, openAIContinuationReplayInputProof(body, items, true, true))
	}
	// Canonicalization applies only to the documented top-level shorthand.
	body := []byte(`{"input":["not a typed Responses item"]}`)
	items, exists, err := openAIWSExtractNormalizedInputSequence(body)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, `"not a typed Responses item"`, string(items[0]))
	require.False(t, openAIContinuationReplayInputProof(body, items, true, true))
}

func TestOAuthReliabilityContinuationStringRootBridgeReplay(t *testing.T) {
	for _, mode := range []string{"complete", "complete_full_implicit", "complete_full_typed_block", "complete_repeated_message", "unanswered_call", "untyped_array_root"} {
		t.Run(mode, func(t *testing.T) {
			cfg := diagnosticContinuationConfig()
			output := `[{"type":"function_call","id":"fc_string_root","call_id":"call_string_root","name":"inspect","arguments":"{}"}]`
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				diagnosticContinuationSSE("resp_string_root", output), diagnosticContinuationSSE("resp_string_next", `[]`),
			}}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), tiboRouteDisabled: true}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server, done := startPassthroughLifecycleServer(t, ctx, svc, diagnosticContinuationAccount())
			defer server.Close()
			rootText := "root text\nwith a \"quote\" and 中文"
			var rootInput any = rootText
			if mode == "untyped_array_root" {
				rootInput = []string{rootText}
			}
			body, err := json.Marshal(map[string]any{"type": "response.create", "model": "gpt-6-astra", "input": rootInput})
			require.NoError(t, err)
			client := dialPassthroughLifecycleClientWithPayload(t, server, string(body))
			defer client.CloseNow()
			_, event, err := client.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "resp_string_root", gjson.GetBytes(event, "response.id").String())
			nextInput := `[{"type":"function_call_output","call_id":"call_string_root","output":"preserve result"}]`
			var expectedFullRoot []byte
			if mode == "complete_full_implicit" || mode == "complete_full_typed_block" || mode == "complete_repeated_message" {
				rootItem := map[string]any{"role": "user", "content": rootText}
				if mode == "complete_full_typed_block" {
					rootItem["type"] = "message"
					rootItem["content"] = []any{map[string]any{"type": "input_text", "text": rootText}}
				}
				expectedFullRoot, err = json.Marshal(rootItem)
				require.NoError(t, err)
				items := []any{rootItem, json.RawMessage(gjson.Parse(output).Array()[0].Raw), map[string]any{"type": "function_call_output", "call_id": "call_string_root", "output": "preserve result"}}
				if mode == "complete_repeated_message" {
					items = append(items, rootItem)
				}
				encoded, encodeErr := json.Marshal(items)
				require.NoError(t, encodeErr)
				nextInput = string(encoded)
			}
			if mode == "unanswered_call" {
				nextInput = `[{"role":"user","content":"continue without answering the tool"}]`
			}
			next := []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_string_root","input":` + nextInput + `}`)
			require.NoError(t, client.Write(ctx, coderws.MessageText, next))
			_, event, err = client.Read(ctx)
			if strings.HasPrefix(mode, "complete") {
				require.NoError(t, err)
				require.Equal(t, "resp_string_next", gjson.GetBytes(event, "response.id").String())
			} else {
				require.Error(t, err)
			}
			_ = client.CloseNow()
			select {
			case proxyErr := <-done:
				if !strings.HasPrefix(mode, "complete") {
					var contextErr *OpenAIContinuationContextError
					require.True(t, errors.As(proxyErr, &contextErr))
				}
			case <-ctx.Done():
				t.Fatal("bridge did not terminate")
			}
			if !strings.HasPrefix(mode, "complete") {
				require.Len(t, upstream.bodies, 1, "incomplete history must not be rewritten or sent again")
				return
			}
			require.Len(t, upstream.bodies, 2)
			replayed := gjson.ParseBytes(upstream.bodies[1])
			require.False(t, replayed.Get("previous_response_id").Exists())
			wantItems := 3
			if mode == "complete_repeated_message" {
				wantItems = 4
				require.JSONEq(t, string(expectedFullRoot), replayed.Get("input.3").Raw, "a later repeated message is genuine input, not a duplicate prefix")
			}
			require.Len(t, replayed.Get("input").Array(), wantItems)
			require.Equal(t, "user", replayed.Get("input.0.role").String())
			if len(expectedFullRoot) > 0 {
				require.JSONEq(t, string(expectedFullRoot), replayed.Get("input.0").Raw, "comparison must retain the client's original item representation")
			} else {
				require.Equal(t, rootText, replayed.Get("input.0.content").String())
			}
			require.Equal(t, "function_call", replayed.Get("input.1.type").String())
			require.Equal(t, "call_string_root", replayed.Get("input.1.call_id").String())
			require.Equal(t, "call_string_root", replayed.Get("input.2.call_id").String())
			require.Equal(t, "preserve result", replayed.Get("input.2.output").String())
		})
	}
}

func TestOAuthReliabilityContinuationUserMessagePrefixEquivalence(t *testing.T) {
	canonical := json.RawMessage(`{"type":"message","role":"user","content":"same"}`)
	for _, tc := range []struct {
		name string
		item string
		want bool
	}{
		{"implicit_type", `{"role":"user","content":"same"}`, true},
		{"single_text", `{"type":"message","role":"user","content":[{"type":"input_text","text":"same"}]}`, true},
		{"different_role", `{"role":"assistant","content":"same"}`, false},
		{"different_text", `{"role":"user","content":"changed"}`, false},
		{"extra_item_field", `{"role":"user","content":"same","future_semantics":true}`, false},
		{"extra_part_field", `{"role":"user","content":[{"type":"input_text","text":"same","future_semantics":true}]}`, false},
		{"two_parts", `{"role":"user","content":[{"type":"input_text","text":"sa"},{"type":"input_text","text":"me"}]}`, false},
		{"output_text", `{"role":"user","content":[{"type":"output_text","text":"same"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := json.RawMessage(tc.item)
			items := []json.RawMessage{item}
			require.Equal(t, tc.want, openAIWSRawItemsHasPrefix(items, []json.RawMessage{canonical}))
			require.Equal(t, tc.item, string(items[0]), "comparison never changes original content")
		})
	}
}

func TestOAuthReliabilityContinuationStringRootRepeatedDeltaIsPreserved(t *testing.T) {
	for _, mode := range []string{"single_repeated", "two_incremental", "client_supplied_output_id"} {
		t.Run(mode, func(t *testing.T) {
			cfg := diagnosticContinuationConfig()
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				diagnosticContinuationSSE("resp_repeated_root", `[]`), diagnosticContinuationSSE("resp_repeated_next", `[]`),
			}}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), tiboRouteDisabled: true}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server, done := startPassthroughLifecycleServer(t, ctx, svc, diagnosticContinuationAccount())
			defer server.Close()
			root := `"same"`
			delta := `[{"role":"user","content":"same"}]`
			want := 2
			if mode == "two_incremental" {
				delta = `[{"role":"user","content":"same"},{"role":"user","content":"new"}]`
				want = 3
			}
			if mode == "client_supplied_output_id" {
				root = `[{"role":"user","content":"same"},{"type":"message","id":"msg_client_only","role":"assistant","content":[]}]`
				delta = `[{"role":"user","content":"same"},{"type":"message","id":"msg_client_only","role":"assistant","content":[]},{"role":"user","content":"new"}]`
			}
			client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-6-astra","input":`+root+`}`)
			defer client.CloseNow()
			_, _, err := client.Read(ctx)
			require.NoError(t, err)
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_repeated_root","input":`+delta+`}`)))
			_, event, err := client.Read(ctx)
			if mode == "client_supplied_output_id" {
				require.Error(t, err, "client input IDs cannot authorize replacing observed history")
			} else {
				require.NoError(t, err)
				require.Equal(t, "resp_repeated_next", gjson.GetBytes(event, "response.id").String())
			}
			_ = client.CloseNow()
			select {
			case proxyErr := <-done:
				if mode == "client_supplied_output_id" {
					var contextErr *OpenAIContinuationContextError
					require.True(t, errors.As(proxyErr, &contextErr))
				}
			case <-ctx.Done():
				t.Fatal("bridge did not terminate")
			}
			if mode == "client_supplied_output_id" {
				require.Len(t, upstream.bodies, 1)
				return
			}
			require.Len(t, upstream.bodies, 2)
			items := gjson.GetBytes(upstream.bodies[1], "input").Array()
			require.Len(t, items, want, "matching user text does not prove full history")
			require.Equal(t, "same", items[0].Get("content").String())
			require.Equal(t, "same", items[1].Get("content").String())
			if want == 3 {
				require.Equal(t, "new", items[2].Get("content").String())
			}
		})
	}
}

func TestOAuthReliabilityContinuationObservedOutputProvenance(t *testing.T) {
	output := json.RawMessage(`{"type":"message","id":"msg_observed","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)
	var firstSession, secondSession openAIWSObservedOutputProvenance
	firstSession.observeCompleteOutput([]json.RawMessage{output})
	require.True(t, firstSession.provesPrefix([]json.RawMessage{output}))
	require.False(t, secondSession.provesPrefix([]json.RawMessage{output}), "an identical client-supplied ID in another session is not observed output")
	changed := json.RawMessage(`{"type":"message","id":"msg_observed","role":"assistant","content":[{"type":"output_text","text":"answer"}],"future_semantics":true}`)
	require.False(t, firstSession.provesPrefix([]json.RawMessage{changed}), "unknown fields remain part of output identity")
	require.False(t, firstSession.provesPrefix([]json.RawMessage{json.RawMessage(`{"type":"item_reference","id":"msg_observed"}`)}))
	firstSession.observeCompleteOutput(nil)
	require.True(t, firstSession.provesPrefix([]json.RawMessage{output}), "a complete empty output retains prior chain evidence")
	firstSession.reset()
	require.False(t, firstSession.provesPrefix([]json.RawMessage{output}), "a new root or invalidated chain discards evidence")
	for i := 0; i <= openAIContinuationProofMaxItems; i++ {
		firstSession.observeCompleteOutput([]json.RawMessage{json.RawMessage(fmt.Sprintf(`{"type":"message","id":"msg_%d","role":"assistant","content":[]}`, i))})
	}
	require.True(t, firstSession.exhausted)
	require.Empty(t, firstSession.digests)
	firstSession.observeCompleteOutput(nil)
	firstSession.observeCompleteOutput([]json.RawMessage{output})
	require.False(t, firstSession.provesPrefix([]json.RawMessage{output}), "capacity exhaustion stays disabled on the same chain")
	firstSession.reset()
	firstSession.observeCompleteOutput([]json.RawMessage{output})
	require.True(t, firstSession.provesPrefix([]json.RawMessage{output}))
	legacy := openAIWSObservedOutputProvenance{legacyPrefix: true}
	require.True(t, legacy.provesPrefix(nil), "other platforms retain their existing prefix policy")
}
