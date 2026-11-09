package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type gpt61PublicationObservedStore struct {
	NativeReasoningReplayStore
	seen context.Context
}

func (s *gpt61PublicationObservedStore) PutNativeReasoningReplay(ctx context.Context, v NativeReasoningReplayWrite, limits NativeReasoningReplayLimits) error {
	s.seen = ctx
	return s.NativeReasoningReplayStore.PutNativeReasoningReplay(ctx, v, limits)
}

func TestGPT61SolChatCachePublicationCompletionWriteBoundary(t *testing.T) {
	for _, stringWrite := range []bool{false, true} {
		name := "write"
		if stringWrite {
			name = "write_string"
		}
		t.Run(name, func(t *testing.T) {
			for _, fault := range []string{"complete", "forward_canceled_before_done", "client_canceled_before_done", "missing_done", "short_write", "write_error", "incomplete_native", "forward_error_after_handoff", "principal_changed_after_handoff", "invalid_client_after_handoff", "budget_exhausted", "commit_timeout", "non_target", "observe", "buffered", "non_200"} {
				t.Run(fault, func(t *testing.T) {
					producer, _, completion := publicationFixture(t)
					producer.request.Scope.Model = "gpt-6.1-sol"
					producer.request.Parameters = bytes.ReplaceAll(producer.request.Parameters, []byte("gpt-5.5"), []byte("gpt-6.1-sol"))
					completion.Response = bytes.ReplaceAll(completion.Response, []byte("gpt-5.5"), []byte("gpt-6.1-sol"))
					producer.mode = NativeReasoningReplayModeReplay
					if fault == "non_target" {
						producer.request.Scope.Model = "gpt-5.5"
					}
					if fault == "observe" {
						producer.mode = NativeReasoningReplayModeObserve
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					clientCtx, cancelClient := context.WithCancel(context.Background())
					defer cancelClient()
					_, admitted := producer.preparePublication(ctx)
					require.True(t, admitted)
					lease := producer.publication
					producer.sourceInput, producer.toolReverse = completion.SourceInput, completion.ToolNameReverse
					producer.principal = nativeReasoningTokenPrincipal("synthetic-token")
					wire := append(append([]byte("data: "), nativeCaptureJSON(map[string]any{"type": "response.completed", "response": completion.Response})...), '\n', '\n')
					if fault == "incomplete_native" {
						wire = []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_synthetic\"}}\n\n")
					}
					native := NewNativeReasoningResponseCapture(io.NopCloser(bytes.NewReader(wire)), producer.runtime.limits)
					_, err := io.Copy(io.Discard, native)
					require.NoError(t, err)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(clientCtx)
					writer := &nativeCaptureFaultWriter{ResponseWriter: c.Writer}
					delivery := NewNativeReasoningChatCapture(writer, fault != "buffered", producer.runtime.limits)
					c.Writer = delivery
					response := &http.Response{StatusCode: 200, Request: httptest.NewRequest(http.MethodPost, "/responses", nil)}
					response.Request.Header.Set("Authorization", "Bearer synthetic-token")
					delivery.onCompleteWrite = func() { producer.handoffCompletedStream(ctx, c, response, native) }
					var delta map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(completion.DeliveredAssistant, &delta))
					var calls []map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(delta["tool_calls"], &calls))
					for i := range calls {
						calls[i]["index"] = nativeCaptureJSON(i)
					}
					delta["tool_calls"] = nativeCaptureJSON(calls)
					chunk := append(append([]byte("data: "), nativeCaptureChatChunk(delta, "tool_calls")...), '\n', '\n')
					if fault == "buffered" {
						chunk = nativeCaptureJSON(map[string]any{"id": "chatcmpl_synthetic", "object": "chat.completion", "model": producer.request.Scope.Model, "choices": []any{map[string]any{"index": 0, "message": completion.DeliveredAssistant, "finish_reason": "tool_calls"}}})
					}
					write := func(p []byte) (int, error) {
						if stringWrite {
							return delivery.WriteString(string(p))
						}
						return delivery.Write(p)
					}
					if fault == "non_200" {
						delivery.WriteHeader(500)
					}
					_, err = write(chunk)
					require.NoError(t, err)
					require.Nil(t, producer.publicationContext, "a finish chunk alone must not detach cancellation")
					if fault == "forward_canceled_before_done" {
						cancel()
					}
					if fault == "client_canceled_before_done" {
						cancelClient()
					}
					if fault == "short_write" {
						writer.short = true
					}
					if fault == "write_error" {
						writer.writeError = io.ErrClosedPipe
					}
					if fault != "missing_done" && fault != "buffered" {
						_, err = write([]byte("data: [DONE]\n\n"))
						if fault == "write_error" {
							require.ErrorIs(t, err, io.ErrClosedPipe)
						} else {
							require.NoError(t, err)
						}
					}
					armed := producer.publicationContext
					expectArm := fault == "complete" || fault == "forward_error_after_handoff" || fault == "principal_changed_after_handoff" || fault == "invalid_client_after_handoff" || fault == "budget_exhausted" || fault == "commit_timeout"
					if expectArm {
						require.NotNil(t, armed)
						deadline, ok := armed.Deadline()
						require.True(t, ok)
						require.LessOrEqual(t, time.Until(deadline), producer.runtime.limits.IOTimeout)
						producer.handoffCompletedStream(ctx, c, response, native)
						require.Same(t, armed, producer.publicationContext, "the completion hook cannot restart the publication budget")
					} else {
						require.Nil(t, armed)
					}
					store := producer.runtime.engine.store.(*nativeReplayTestStore)
					require.Empty(t, store.values, "a completed write may reserve publication but must not commit")
					observed := &gpt61PublicationObservedStore{NativeReasoningReplayStore: store}
					producer.runtime.engine.store = observed
					var forwardErr error
					switch fault {
					case "forward_error_after_handoff":
						forwardErr = errors.New("synthetic forwarding failure")
					case "principal_changed_after_handoff":
						response.Request.Header.Set("Authorization", "Bearer another-synthetic-principal")
					case "invalid_client_after_handoff":
						_, _ = write([]byte("data: {\"error\":{}}\n\n"))
					case "budget_exhausted":
						<-armed.Done()
					case "commit_timeout":
						store.block = true
					}
					cancel()
					cancelClient()
					producer.finish(ctx, c, response, native, delivery, forwardErr)
					if fault == "complete" {
						require.Len(t, store.values, 1)
						require.NotNil(t, observed.seen)
						originalDeadline, originalOK := armed.Deadline()
						storeDeadline, storeOK := observed.seen.Deadline()
						require.True(t, originalOK && storeOK)
						require.Equal(t, originalDeadline, storeDeadline, "Commit's derived store context must retain the original deadline")
					} else {
						require.Empty(t, store.values)
					}
					require.Nil(t, producer.publicationContext)
					require.Nil(t, producer.publicationCancel)
					require.Nil(t, producer.publication)
					select {
					case <-lease.entry.done:
					default:
						t.Fatal("completion path leaked its publication lease")
					}
					if armed != nil {
						require.Error(t, armed.Err(), "finish must cancel the reserved context on success or failure")
					}
				})
			}
		})
	}
}
