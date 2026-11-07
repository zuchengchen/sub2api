package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func nativeAssistantFixture(t *testing.T) (*NativeReasoningReplay, *nativeReplayTestStore, NativeReasoningReplayRequest, NativeReasoningReplayCompletion) {
	t.Helper()
	engine, store, request, completion := nativeReplayFixture(t)
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(completion.Response, &envelope))
	var output []json.RawMessage
	require.NoError(t, json.Unmarshal(envelope["output"], &output))
	output = append(output[:2], json.RawMessage(`{"type":"message","id":"msg_final","status":"completed","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":" Final answer."}],"future":{"integer":9007199254740993,"empty":null}}`))
	envelope["output"], _ = json.Marshal(output)
	envelope["usage"] = json.RawMessage(`{"input_tokens":17,"output_tokens":5}`)
	completion.Response, _ = json.Marshal(envelope)
	var typed apicompat.ResponsesResponse
	require.NoError(t, json.Unmarshal(completion.Response, &typed))
	completion.DeliveredAssistant, _ = json.Marshal(apicompat.ResponsesToChatCompletions(&typed, request.Scope.Model).Choices[0].Message)
	return engine, store, request, completion
}

func nativeAssistantNext(request NativeReasoningReplayRequest, completion NativeReasoningReplayCompletion) NativeReasoningReplayRequest {
	next := request
	next.Messages = append(append([]json.RawMessage(nil), request.Messages...), completion.DeliveredAssistant, json.RawMessage(`{"role":"user","content":"Explain the final answer."}`))
	return next
}

func TestNativeReasoningReplayAssistantMessageCompletionAndHistory(t *testing.T) {
	for _, shape := range []string{"full", "empty", "absent"} {
		t.Run(shape, func(t *testing.T) {
			engine, _, request, completion := nativeAssistantFixture(t)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(completion.Response, &fields))
			var output []json.RawMessage
			require.NoError(t, json.Unmarshal(fields["output"], &output))
			frames := [][]byte{[]byte(`{"type":"response.created","response":{"id":"resp_synthetic","status":"in_progress"}}`)}
			for index, item := range output {
				frames = append(frames, sparseReplayFrame("response.output_item.added", index, item), sparseReplayFrame("response.output_item.done", index, item))
			}
			if shape == "empty" {
				fields["output"] = json.RawMessage(`[]`)
			}
			if shape == "absent" {
				delete(fields, "output")
			}
			terminal, _ := json.Marshal(fields)
			frames = append(frames, nativeCaptureJSON(map[string]any{"type": "response.completed", "response": json.RawMessage(terminal)}))
			var ok bool
			completion.Response, ok = sparseReplayCollect(frames, engine.limits)
			require.True(t, ok)
			require.NoError(t, engine.Commit(context.Background(), request, completion))
			next := nativeAssistantNext(request, completion)
			replacements, err := engine.Restore(context.Background(), next)
			require.NoError(t, err)
			require.Len(t, replacements, 1)
			updated, err := ApplyNativeReasoningReplayInput(nativeReplayConvertedInput(t, next), replacements)
			require.NoError(t, err)
			var input []json.RawMessage
			require.NoError(t, json.Unmarshal(updated, &input))
			for i, item := range output {
				require.JSONEq(t, string(item), string(input[len(request.Messages)+i]))
			}
			require.NotContains(t, string(updated), `\u003cthinking\u003e`)
		})
	}
}

func TestNativeReasoningReplayAssistantMessageIsolationAndInvalidDelivery(t *testing.T) {
	for _, fault := range []string{"user", "key", "session", "model", "credential", "parameters", "history", "assistant", "unpaired_tool"} {
		t.Run(fault, func(t *testing.T) {
			engine, _, request, completion := nativeAssistantFixture(t)
			require.NoError(t, engine.Commit(context.Background(), request, completion))
			next := nativeAssistantNext(request, completion)
			switch fault {
			case "user":
				next.Scope.UserID++
			case "key":
				next.Scope.APIKeyID++
			case "session":
				next.Scope.SessionID += "changed"
			case "model":
				next.Scope.Model += "changed"
			case "credential":
				next.Scope.CredentialIdentity += "changed"
			case "parameters":
				next.Parameters = json.RawMessage(`{"changed":true}`)
			case "history":
				next.Messages[0] = json.RawMessage(`{"role":"system","content":"Edited context"}`)
			case "assistant":
				next.Messages[2] = json.RawMessage(strings.ReplaceAll(string(next.Messages[2]), "Checking.", "Edited."))
			case "unpaired_tool":
				next.Messages[3] = json.RawMessage(`{"role":"tool","tool_call_id":"foreign","content":"result"}`)
			}
			got, err := engine.Restore(context.Background(), next)
			require.NoError(t, err)
			require.Empty(t, got)
		})
	}
	for _, fault := range []string{"canceled", "partial", "mismatch", "empty_visible", "reasoning_only", "unknown_output", "legacy_function"} {
		t.Run(fault, func(t *testing.T) {
			engine, store, request, completion := nativeAssistantFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch fault {
			case "canceled":
				cancel()
			case "partial":
				completion.DeliveredComplete = false
			case "mismatch":
				completion.DeliveredAssistant = json.RawMessage(`{"role":"assistant","content":"not actually delivered"}`)
			case "empty_visible":
				completion.Response = json.RawMessage(`{"id":"resp_empty","status":"completed","output":[{"type":"message","id":"msg_empty","role":"assistant","status":"completed","content":[]}]}`)
				completion.DeliveredAssistant = json.RawMessage(`{"role":"assistant"}`)
			case "reasoning_only":
				completion.Response = json.RawMessage(`{"id":"resp_reasoning","status":"completed","output":[{"type":"reasoning","id":"rs_empty","encrypted_content":"opaque","summary":[]}]}`)
				completion.DeliveredAssistant = json.RawMessage(`{"role":"assistant"}`)
			case "unknown_output":
				completion.Response = json.RawMessage(strings.Replace(string(completion.Response), `"type":"reasoning"`, `"type":"unknown_future"`, 1))
			case "legacy_function":
				completion.DeliveredAssistant = json.RawMessage(`{"role":"assistant","content":"Final","function_call":{"name":"legacy","arguments":"{}"}}`)
			}
			require.Error(t, engine.Commit(ctx, request, completion))
			require.Empty(t, store.values)
		})
	}
}

func nativeAssistantProjectionFixture(t *testing.T) ([]byte, []NativeReasoningReplayReplacement, map[string]string) {
	t.Helper()
	engine, _, request, completion := nativeAssistantFixture(t)
	chat := nativeProjectionChat(t, request)
	initial, reverse := nativeProjectionNormalized(t, chat, true)
	var err error
	request.Parameters, err = nativeReasoningProjectionParameters(chat, initial, reverse)
	require.NoError(t, err)
	completion.SourceInput = json.RawMessage(gjson.GetBytes(initial, "input").Raw)
	require.NoError(t, engine.Commit(context.Background(), request, completion))
	next := nativeAssistantNext(request, completion)
	chat = nativeProjectionChat(t, next)
	body, reverse := nativeProjectionNormalized(t, chat, true)
	next.Parameters, err = nativeReasoningProjectionParameters(chat, body, reverse)
	require.NoError(t, err)
	require.JSONEq(t, string(request.Parameters), string(next.Parameters))
	groups, err := engine.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	return body, groups, reverse
}

func TestNativeReasoningReplayAssistantMessageProjectionUniqueAnchors(t *testing.T) {
	for _, fault := range []string{"none", "same_text_different_anchors", "ambiguous_repeated_group", "changed_fragment", "changed_anchor", "changed_prefix", "native_tool_hidden", "unknown_native"} {
		t.Run(fault, func(t *testing.T) {
			body, groups, reverse := nativeAssistantProjectionFixture(t)
			var input []json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(body, "input").Raw), &input))
			switch fault {
			case "same_text_different_anchors":
				input = append(input, input[2])
			case "ambiguous_repeated_group":
				input = append(input, input[1], input[2], input[3])
			case "changed_fragment":
				input[2] = json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Different answer"}]}`)
			case "changed_anchor":
				input[3] = json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"Different user"}]}`)
			case "changed_prefix":
				input[0] = json.RawMessage(`{"type":"message","role":"developer","content":[{"type":"input_text","text":"Different synthesized prefix"}]}`)
			case "native_tool_hidden":
				groups[0].Output = append(groups[0].Output, json.RawMessage(`{"type":"function_call","id":"fc_hidden","call_id":"call_hidden","name":"hidden","arguments":"{}"}`))
			case "unknown_native":
				groups[0].Output = append(groups[0].Output, json.RawMessage(`{"type":"unknown_future","id":"future_item"}`))
			}
			var err error
			body, err = sjson.SetRawBytes(body, "input", nativeReasoningProjectionArray(input))
			require.NoError(t, err)
			original := append([]byte(nil), body...)
			updated, refs, err := applyNativeReasoningProjection(body, groups, reverse, NativeReasoningReplayLimits{})
			require.NoError(t, err)
			require.Equal(t, original, body)
			if fault == "none" || fault == "same_text_different_anchors" {
				require.Len(t, refs, 1)
				out := gjson.GetBytes(updated, "input").Array()
				for i, raw := range groups[0].Output {
					require.JSONEq(t, string(raw), out[i+2].Raw)
				}
				require.Contains(t, string(updated), "9007199254740993")
			} else {
				require.Empty(t, refs)
				require.Equal(t, body, updated)
			}
		})
	}
}

func TestNativeReasoningReplayAssistantMessagePublicationUsesSameCandidate(t *testing.T) {
	engine, store, request, completion := nativeAssistantFixture(t)
	runtime := &nativeReasoningRuntime{engine: engine, limits: engine.limits}
	producer := &nativeReasoningChat{runtime: runtime, request: request, eligible: true}
	_, admitted := producer.preparePublication(context.Background())
	require.True(t, admitted)
	defer producer.releasePublication()
	consumer := &nativeReasoningChat{runtime: runtime, request: nativeAssistantNext(request, completion)}
	prepared, err := engine.prepare(consumer.request)
	require.NoError(t, err)
	require.Equal(t, []int{len(request.Messages)}, nativeReplayRestoreCandidates(prepared.messages, 16))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err = consumer.restoreAfterPublication(ctx, prepared)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, store.gets)
	require.NoError(t, engine.Commit(context.Background(), request, completion))
	producer.releasePublication()
	got, err := consumer.restoreAfterPublication(context.Background(), prepared)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestNativeReasoningReplayAssistantMessageForksRemainAmbiguous(t *testing.T) {
	engine, _, request, completion := nativeAssistantFixture(t)
	require.NoError(t, engine.Commit(context.Background(), request, completion))
	conflict := completion
	conflict.Response = json.RawMessage(strings.ReplaceAll(string(completion.Response), "opaque-synthetic-reasoning", "different-native-state"))
	require.Equal(t, completion.DeliveredAssistant, conflict.DeliveredAssistant)
	require.ErrorIs(t, engine.Commit(context.Background(), request, conflict), ErrNativeReasoningReplayAmbiguous)
	got, err := engine.Restore(context.Background(), nativeAssistantNext(request, completion))
	require.NoError(t, err)
	require.Empty(t, got, "identical visible text cannot select between different native completions")
}

func TestNativeReasoningReplayAssistantMessageRequiresToolAncestor(t *testing.T) {
	engine, request, _, body, first, reverse := nativeProjectionFixture(t)
	prior, refs, err := applyNativeReasoningProjection(body, first, reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	_, _, _, completion := nativeAssistantFixture(t)
	completion.Response = json.RawMessage(strings.ReplaceAll(string(completion.Response), "synthetic", "answer_second"))
	completion.DeliveredAssistant = json.RawMessage(strings.ReplaceAll(string(completion.DeliveredAssistant), "synthetic", "answer_second"))
	completion.SourceInput = json.RawMessage(gjson.GetBytes(prior, "input").Raw)
	require.NoError(t, engine.Commit(context.Background(), request, completion))
	next := nativeAssistantNext(request, completion)
	chat := nativeProjectionChat(t, next)
	body, reverse = nativeProjectionNormalized(t, chat, true)
	next.Parameters, err = nativeReasoningProjectionParameters(chat, body, reverse)
	require.NoError(t, err)
	groups, err := engine.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	updated, refs, err := applyNativeReasoningProjection(body, groups, reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Len(t, refs, 2)
	require.Contains(t, string(updated), "opaque-synthetic-reasoning")
	require.Contains(t, string(updated), "opaque-answer_second-reasoning")
	require.NotContains(t, string(updated), `"call_id":"fc_synthetic"`, "tool result repair must precede the assistant causal check")
	missing, refs, err := applyNativeReasoningProjection(body, groups[1:], reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Empty(t, refs)
	require.Equal(t, body, missing)
}

func TestNativeReasoningReplayAssistantMessageFinishRequiresActualDelivery(t *testing.T) {
	for _, fault := range []string{"complete", "forward_canceled", "client_canceled", "partial_client", "short_write", "incomplete_native", "forward_error"} {
		t.Run(fault, func(t *testing.T) {
			engine, store, request, completion := nativeAssistantFixture(t)
			runtime := &nativeReasoningRuntime{engine: engine, limits: engine.limits}
			producer := &nativeReasoningChat{runtime: runtime, request: request, eligible: true}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clientCtx, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			_, admitted := producer.preparePublication(ctx)
			require.True(t, admitted)
			lease := producer.publication
			producer.sourceInput, producer.toolReverse = completion.SourceInput, completion.ToolNameReverse
			producer.principal = nativeReasoningTokenPrincipal("synthetic-token")
			wire := append([]byte("data: "), nativeCaptureJSON(map[string]any{"type": "response.completed", "response": completion.Response})...)
			wire = append(wire, '\n', '\n')
			if fault == "incomplete_native" {
				wire = []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_synthetic\"}}\n\n")
			}
			native := NewNativeReasoningResponseCapture(io.NopCloser(bytes.NewReader(wire)), engine.limits)
			_, err := io.Copy(io.Discard, native)
			require.NoError(t, err)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(clientCtx)
			writer := c.Writer
			if fault == "short_write" {
				writer = &nativeCaptureFaultWriter{ResponseWriter: writer, short: true}
			}
			delivery := NewNativeReasoningChatCapture(writer, false, engine.limits)
			c.Writer = delivery
			chat := nativeCaptureJSON(map[string]any{"id": "chatcmpl_assistant", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": completion.DeliveredAssistant, "finish_reason": "stop"}}})
			if fault == "partial_client" {
				chat = chat[:len(chat)-1]
			}
			_, err = delivery.Write(chat)
			require.NoError(t, err)
			response := &http.Response{StatusCode: http.StatusOK, Request: httptest.NewRequest(http.MethodPost, "/responses", nil)}
			response.Request.Header.Set("Authorization", "Bearer synthetic-token")
			var forwardErr error
			switch fault {
			case "forward_canceled":
				cancel()
			case "client_canceled":
				cancelClient()
			case "forward_error":
				forwardErr = errors.New("synthetic failure")
			}
			producer.finish(ctx, c, response, native, delivery, forwardErr)
			if fault == "complete" {
				require.Len(t, store.values, 1)
			} else {
				require.Empty(t, store.values)
			}
			select {
			case <-lease.entry.done:
			default:
				t.Fatal("assistant publication remained registered after finish")
			}
		})
	}
}
