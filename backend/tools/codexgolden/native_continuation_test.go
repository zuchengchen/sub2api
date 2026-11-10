//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func nativeTestEvidence(seq, conn int, id string, output []any) *nativeResponseEvidence {
	events := []json.RawMessage{mustJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})}
	for i, item := range output {
		events = append(events, mustJSON(map[string]any{"type": "response.output_item.added", "output_index": i, "item": item}), mustJSON(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item}))
	}
	events = append(events, mustJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": output}}))
	return &nativeResponseEvidence{RequestSeq: seq, Conn: conn, Status: 200, Events: events, Complete: true, RequestObservedAtUnixNano: int64(seq) * 10e9}
}
func nativeTestTurnBody(body map[string]any, turn string) json.RawMessage {
	kind := "turn"
	if turn == "" {
		kind = "prewarm"
	}
	body["client_metadata"] = map[string]any{"turn_id": turn, "x-codex-turn-metadata": string(mustJSON(map[string]any{"turn_id": turn, "request_kind": kind}))}
	return mustJSON(body)
}
func nativeTestLifecycle(full bool) []record {
	output := []any{map[string]any{"type": "reasoning", "id": "rs_golden_resp_golden_002", "summary": []any{}, "encrypted_content": "golden-reasoning-resp_golden_002"}, map[string]any{"type": "custom_tool_call", "id": "ct_golden_resp_golden_002", "call_id": "call_golden_code_mode_resp_golden_002", "name": "exec", "input": "text('synthetic');"}}
	prompt := map[string]any{"type": "message", "role": "user", "content": "synthetic prompt"}
	result := map[string]any{"type": "custom_tool_call_output", "call_id": "call_golden_code_mode_resp_golden_002", "output": syntheticGoldenCodeModeOutput("aabbcc", "0.1", "golden\n", 0), "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn-original", "create_time": 25.125}}
	body := func(input []any, previous string, prewarm bool) json.RawMessage {
		v := map[string]any{"model": "gpt-6-astra", "stream": true, "input": input}
		if previous != "" {
			v["previous_response_id"] = previous
		}
		if prewarm {
			v["generate"] = false
		}
		turn := "turn-original"
		if prewarm {
			turn = ""
		}
		return nativeTestTurnBody(v, turn)
	}
	rows := []record{{Seq: 1, Conn: 9, Kind: "ws_message", BodyJSON: body([]any{}, "", true), NativeResponse: nativeTestEvidence(1, 9, "resp_golden_001", []any{})}, {Seq: 2, Conn: 9, Kind: "ws_message", BodyJSON: body([]any{prompt}, "resp_golden_001", false), NativeResponse: nativeTestEvidence(2, 9, "resp_golden_002", output)}, {Seq: 3, Conn: 9, Kind: "ws_message", BodyJSON: body([]any{result}, "resp_golden_002", false), NativeResponse: nativeTestEvidence(3, 9, "resp_golden_003", []any{map[string]any{"type": "message", "id": "msg_final", "role": "assistant", "content": []any{}}})}}
	if full {
		output[0].(map[string]any)["content"] = nil
		for _, raw := range output {
			raw.(map[string]any)["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": "turn-original"}
		}
		rows[2].BodyJSON = body(append(append([]any{prompt}, output...), result), "", false)
	}
	return rows
}
func TestNativeContinuationAcceptsProvenSameConnectionSuffixAndFull(t *testing.T) {
	for _, full := range []bool{false, true} {
		require.NoError(t, validateNativeContinuation(nativeTestLifecycle(full)))
	}
	rows := nativeTestLifecycle(true)[1:]
	var first map[string]any
	require.NoError(t, json.Unmarshal(rows[0].BodyJSON, &first))
	delete(first, "previous_response_id")
	rows[0].BodyJSON = mustJSON(first)
	for i := range rows {
		rows[i].Kind = "http_request"
		rows[i].Method = "POST"
		rows[i].Target = "/backend-api/codex/responses"
		rows[i].Conn = 20 + i
		rows[i].NativeResponse.Conn = rows[i].Conn
	}
	require.NoError(t, validateNativeContinuation(rows), "HTTP must carry the full original block across connections")
}
func TestNativeContinuationRejectsUnprovenOrLostState(t *testing.T) {
	for _, fault := range []string{"wrong response id", "old response id", "other connection", "changed model", "missing response evidence", "missing terminal", "incomplete socket write", "terminal mismatch", "wrong tool id", "full lost opaque", "full duplicate block", "HTTP previous id", "source opaque missing"} {
		t.Run(fault, func(t *testing.T) {
			rows := nativeTestLifecycle(strings.HasPrefix(fault, "full "))
			var body map[string]any
			require.NoError(t, json.Unmarshal(rows[2].BodyJSON, &body))
			switch fault {
			case "wrong response id":
				body["previous_response_id"] = "resp_unknown"
			case "old response id":
				body["previous_response_id"] = "resp_golden_001"
			case "other connection":
				rows[2].Conn = 10
				rows[2].NativeResponse.Conn = 10
			case "changed model":
				body["model"] = "other-model"
			case "missing response evidence":
				rows[1].NativeResponse = nil
			case "missing terminal":
				rows[1].NativeResponse.Events = rows[1].NativeResponse.Events[:len(rows[1].NativeResponse.Events)-1]
			case "incomplete socket write":
				rows[1].NativeResponse.Complete = false
				rows[1].NativeResponse.WriteError = true
			case "terminal mismatch":
				last := len(rows[1].NativeResponse.Events) - 1
				rows[1].NativeResponse.Events[last] = json.RawMessage(strings.ReplaceAll(string(rows[1].NativeResponse.Events[last]), "resp_golden_002", "resp_other"))
			case "wrong tool id":
				body["input"].([]any)[0].(map[string]any)["call_id"] = "call_golden_foreign"
			case "full lost opaque":
				delete(body["input"].([]any)[1].(map[string]any), "encrypted_content")
			case "full duplicate block":
				items := body["input"].([]any)
				body["input"] = append(items, items[1:3]...)
			case "HTTP previous id":
				rows[2].Kind = "http_request"
				rows[2].Method = "POST"
				rows[2].Target = "/backend-api/codex/responses"
			case "source opaque missing":
				for i, raw := range rows[1].NativeResponse.Events {
					rows[1].NativeResponse.Events[i] = json.RawMessage(strings.ReplaceAll(string(raw), "golden-reasoning-resp_golden_002", "lost-opaque"))
				}
			}
			rows[2].BodyJSON = mustJSON(body)
			require.Error(t, validateNativeContinuation(rows))
		})
	}
}
func TestNativeContinuationCompactionRequiresFullNewWindow(t *testing.T) {
	compact := map[string]any{"type": "compaction", "id": "cmp_fixture", "encrypted_content": "opaque-compaction", "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn-original"}}
	first := record{Seq: 1, Conn: 1, Kind: "ws_message", BodyJSON: nativeTestTurnBody(map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"type": "compaction_trigger"}}}, "turn-original"), NativeResponse: nativeTestEvidence(1, 1, "resp_compact", []any{compact})}
	second := record{Seq: 2, Conn: 1, Kind: "ws_message", BodyJSON: nativeTestTurnBody(map[string]any{"model": "gpt-6-astra", "input": []any{compact, map[string]any{"type": "message", "role": "user", "content": "continue"}}}, "turn-original"), NativeResponse: nativeTestEvidence(2, 1, "resp_after", []any{map[string]any{"type": "message", "id": "msg_after", "role": "assistant"}})}
	require.NoError(t, validateNativeContinuation([]record{first, second}))
	var body map[string]any
	require.NoError(t, json.Unmarshal(second.BodyJSON, &body))
	body["previous_response_id"] = "resp_compact"
	second.BodyJSON = mustJSON(body)
	require.Error(t, validateNativeContinuation([]record{first, second}))
	delete(body, "previous_response_id")
	delete(body["input"].([]any)[0].(map[string]any), "encrypted_content")
	second.BodyJSON = mustJSON(body)
	require.Error(t, validateNativeContinuation([]record{first, second}))
}

type nativeShortWriteConn struct{}

func (nativeShortWriteConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (nativeShortWriteConn) Write(p []byte) (int, error)      { return len(p) - 1, nil }
func (nativeShortWriteConn) Close() error                     { return nil }
func (nativeShortWriteConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (nativeShortWriteConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (nativeShortWriteConn) SetDeadline(time.Time) error      { return nil }
func (nativeShortWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (nativeShortWriteConn) SetWriteDeadline(time.Time) error { return nil }
func TestNativeContinuationFixtureShortWritesCannotEstablishTerminal(t *testing.T) {
	require.ErrorIs(t, writeServerFrame(nativeShortWriteConn{}, 1, []byte(`{}`)), io.ErrShortWrite)
	f := &fakeUpstream{}
	seq := f.add(record{Conn: 1, Kind: "http_request"})
	f.writeSSE(nativeShortWriteConn{}, seq, 1, []map[string]any{{"type": "response.completed"}})
	require.NotNil(t, f.records[0].NativeResponse)
	require.False(t, f.records[0].NativeResponse.Complete)
	require.True(t, f.records[0].NativeResponse.WriteError)
}

func TestNativeContinuationCompactionCheckpointCoversRetainedToolResult(t *testing.T) {
	rows := nativeTestLifecycle(false)
	var trigger map[string]any
	require.NoError(t, json.Unmarshal(rows[2].BodyJSON, &trigger))
	result := trigger["input"].([]any)[0]
	trigger["input"] = append(trigger["input"].([]any), map[string]any{"type": "compaction_trigger"})
	rows[2].BodyJSON = mustJSON(trigger)
	compact := map[string]any{"type": "compaction", "id": "cmp_after_tool", "encrypted_content": "opaque-checkpoint", "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn-original"}}
	rows[2].NativeResponse = nativeTestEvidence(3, 9, "resp_compact", []any{compact})
	full := map[string]any{"model": "gpt-6-astra", "stream": true, "input": []any{result, compact, map[string]any{"type": "message", "role": "user", "content": "continue"}}}
	rows = append(rows, record{Seq: 4, Conn: 9, Kind: "ws_message", BodyJSON: nativeTestTurnBody(full, "turn-original"), NativeResponse: nativeTestEvidence(4, 9, "resp_after_compact", []any{map[string]any{"type": "message", "id": "msg_after_compact", "role": "assistant"}})})
	require.NoError(t, validateNativeContinuation(rows), "retained tool output need not reintroduce pre-compaction reasoning")
	var changed map[string]any
	require.NoError(t, json.Unmarshal(rows[3].BodyJSON, &changed))
	changed["input"].([]any)[0].(map[string]any)["output"] = "changed result"
	rows[3].BodyJSON = mustJSON(changed)
	require.Error(t, validateNativeContinuation(rows), "a checkpoint cannot authorize a mutated retained tool result")
}

func TestNativeContinuationExitCodesSeparateStateLossFromInvalidEvidence(t *testing.T) {
	require.Equal(t, 0, nativeContinuationExit(nativeTestLifecycle(false)))
	rows := nativeTestLifecycle(true)
	rows[2].BodyJSON = json.RawMessage(strings.ReplaceAll(string(rows[2].BodyJSON), "golden-reasoning-resp_golden_002", "lost-state"))
	require.Equal(t, 1, nativeContinuationExit(rows))
	rows = nativeTestLifecycle(false)
	rows[1].NativeResponse = nil
	require.Equal(t, 2, nativeContinuationExit(rows))
	rows = nativeTestLifecycle(false)
	rows[1].NativeResponse.Events = rows[1].NativeResponse.Events[:len(rows[1].NativeResponse.Events)-1]
	require.Equal(t, 2, nativeContinuationExit(rows))
}

func TestNativeContinuationRejectsToolPairingAndOrderFaults(t *testing.T) {
	for _, fault := range []string{"wrong kind", "result before call", "duplicate result"} {
		t.Run(fault, func(t *testing.T) {
			rows := nativeTestLifecycle(true)
			var body map[string]any
			require.NoError(t, json.Unmarshal(rows[2].BodyJSON, &body))
			input := body["input"].([]any)
			switch fault {
			case "wrong kind":
				input[3].(map[string]any)["type"] = "function_call_output"
			case "result before call":
				input = []any{input[0], input[3], input[1], input[2]}
			case "duplicate result":
				input = append(input, input[3])
			}
			body["input"] = input
			rows[2].BodyJSON = mustJSON(body)
			require.Equal(t, 1, nativeContinuationExit(rows))
		})
	}
}
func TestNativeContinuationRejectsDoneArrivalFaults(t *testing.T) {
	for _, fault := range []string{"duplicate done", "reordered done", "unfinished added"} {
		t.Run(fault, func(t *testing.T) {
			rows := nativeTestLifecycle(false)
			events := rows[1].NativeResponse.Events
			switch fault {
			case "duplicate done":
				events = append(events[:3], append([]json.RawMessage{events[2]}, events[3:]...)...)
			case "reordered done":
				events[2], events[4] = events[4], events[2]
			case "unfinished added":
				extra := mustJSON(map[string]any{"type": "response.output_item.added", "output_index": 2, "item": map[string]any{"id": "unfinished", "type": "message"}})
				events = append(events[:len(events)-1], extra, events[len(events)-1])
			}
			rows[1].NativeResponse.Events = events
			require.Equal(t, 2, nativeContinuationExit(rows))
		})
	}
}

type nativeH2TestWriter struct {
	header     http.Header
	accepted   int
	failAfter  int
	flushError error
	deadlines  int
}

func (w *nativeH2TestWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (*nativeH2TestWriter) WriteHeader(int) {}
func (w *nativeH2TestWriter) Write(p []byte) (int, error) {
	if w.failAfter >= 0 && w.accepted+len(p) > w.failAfter {
		n := max(0, w.failAfter-w.accepted)
		w.accepted += n
		return n, nil
	}
	w.accepted += len(p)
	return len(p), nil
}
func (w *nativeH2TestWriter) FlushError() error                { return w.flushError }
func (w *nativeH2TestWriter) SetWriteDeadline(time.Time) error { w.deadlines++; return nil }
func TestNativeContinuationActualH2DeliveryRequired(t *testing.T) {
	const wire = "data: created\n\ndata: completed\n\n"
	for _, fault := range []string{"success", "before body", "terminal short write", "flush error"} {
		t.Run(fault, func(t *testing.T) {
			writer := &nativeH2TestWriter{failAfter: -1}
			switch fault {
			case "before body":
				writer.failAfter = 0
			case "terminal short write":
				writer.failAfter = len(wire) - 2
			case "flush error":
				writer.flushError = errors.New("stream reset")
			}
			f := &fakeUpstream{}
			seq := f.add(record{Conn: 1, Kind: "http_request", H2: &h2Observation{StreamID: 3}})
			f.nativeEvidence(seq, 1, 200, []json.RawMessage{json.RawMessage(`{"type":"response.completed"}`)}, true, nil)
			before := f.records[0].NativeResponse
			require.False(t, before.Complete, "pipe completion alone cannot prove H2 delivery")
			err := copyNativeH2Response(writer, strings.NewReader(wire))
			f.confirmNativeH2(1, 3, err == nil)
			if fault == "success" {
				require.NoError(t, err)
				require.True(t, f.records[0].NativeResponse.Complete)
			} else {
				require.Error(t, err)
				require.False(t, f.records[0].NativeResponse.Complete)
				require.True(t, f.records[0].NativeResponse.WriteError)
			}
			require.False(t, before.Complete, "a late confirmation must not mutate a returned record snapshot")
			require.False(t, before.WriteError, "immutable pending snapshot")
			require.Equal(t, 2, writer.deadlines)
		})
	}
}

func TestNativeContinuationRejectsSourceCheckpointWithoutOpaque(t *testing.T) {
	for _, opaque := range []any{nil, "", false} {
		t.Run(fmt.Sprint(opaque), func(t *testing.T) {
			compact := map[string]any{"type": "compaction", "id": "cmp_fixture"}
			if opaque != nil {
				compact["encrypted_content"] = opaque
			}
			row := record{Seq: 1, Conn: 1, Kind: "ws_message", BodyJSON: nativeTestTurnBody(map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"type": "compaction_trigger"}}}, "turn-original"), NativeResponse: nativeTestEvidence(1, 1, "resp_compact", []any{compact})}
			require.Equal(t, 2, nativeContinuationExit([]record{row}))
		})
	}
}
func TestNativeContinuationNonToolFullResetRequiresCompleteWindow(t *testing.T) {
	prompt := map[string]any{"type": "message", "role": "user", "content": "hello"}
	reasoning := map[string]any{"type": "reasoning", "id": "rs_first", "encrypted_content": "opaque-reasoning", "summary": []any{}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn-original"}}
	message := map[string]any{"type": "message", "id": "msg_first", "role": "assistant", "encrypted_content": "opaque-message", "phase": "final_answer", "content": []any{}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn-original"}}
	next := map[string]any{"type": "message", "role": "user", "content": "continue"}
	for _, fault := range []string{"none", "lost reasoning", "lost all output", "message opaque missing", "reordered blocks"} {
		t.Run(fault, func(t *testing.T) {
			first := record{Seq: 1, Conn: 1, Kind: "http_request", Method: "POST", Target: "/backend-api/codex/responses", BodyJSON: nativeTestTurnBody(map[string]any{"model": "gpt-6-astra", "input": []any{prompt}}, "turn-original"), NativeResponse: nativeTestEvidence(1, 1, "resp_first", []any{reasoning, message})}
			historyReasoning := map[string]any{}
			for key, value := range reasoning {
				historyReasoning[key] = value
			}
			historyReasoning["content"] = nil
			input := []any{prompt, historyReasoning, message, next}
			switch fault {
			case "lost reasoning":
				input = []any{prompt, message, next}
			case "lost all output":
				input = []any{prompt, next}
			case "message opaque missing":
				altered := map[string]any{}
				for k, v := range message {
					altered[k] = v
				}
				delete(altered, "encrypted_content")
				input[2] = altered
			case "reordered blocks":
				input = []any{prompt, message, reasoning, next}
			}
			second := record{Seq: 2, Conn: 2, Kind: "http_request", Method: "POST", Target: "/backend-api/codex/responses", BodyJSON: nativeTestTurnBody(map[string]any{"model": "gpt-6-astra", "input": input}, "turn-original"), NativeResponse: nativeTestEvidence(2, 2, "resp_second", []any{})}
			want := 1
			if fault == "none" {
				want = 0
			}
			require.Equal(t, want, nativeContinuationExit([]record{first, second}))
		})
	}
}

func TestNativeContinuationRealH2ResponseProducesCompletionEvidence(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
	f := &fakeUpstream{connHops: map[int]string{}, connHellos: map[int]*clientHello{}}
	done := make(chan struct{})
	go func() { defer close(done); f.serveH2(server, 9, false) }()
	transport := &http2.Transport{}
	cc, err := transport.NewClientConn(client)
	require.NoError(t, err)
	req, err := http.NewRequest("POST", "https://localhost/backend-api/codex/responses", strings.NewReader(string(nativeTestTurnBody(map[string]any{"model": "gpt-6-astra", "stream": true, "input": []any{}}, "turn-original"))))
	require.NoError(t, err)
	resp, err := cc.RoundTrip(req)
	require.NoError(t, err)
	wire, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Contains(t, string(wire), "response.completed")
	require.NoError(t, cc.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("actual H2 fixture did not close")
	}
	f.mu.Lock()
	rows := append([]record(nil), f.records...)
	f.mu.Unlock()
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].NativeResponse)
	require.True(t, rows[0].NativeResponse.Complete)
	require.False(t, rows[0].NativeResponse.WriteError)
	require.NoError(t, validateNativeContinuation(rows))
}

// A new request turn must not restamp history produced by an older turn.
func nativeMetadataLifecycle(t *testing.T) []record {
	t.Helper()
	rows := nativeTestLifecycle(true)
	for i := range rows {
		var body map[string]any
		require.NoError(t, json.Unmarshal(rows[i].BodyJSON, &body))
		turn, kind := "turn-origin", "turn"
		if i == 0 {
			turn, kind = "", "prewarm"
		}
		if i == 2 {
			turn = "turn-consumer"
		}
		envelope := string(mustJSON(map[string]any{"turn_id": turn, "request_kind": kind}))
		body["client_metadata"] = map[string]any{"turn_id": turn, "x-codex-turn-metadata": envelope}
		rows[i].Headers = []headerKV{{Name: "x-codex-turn-metadata", Value: envelope}}
		rows[i].NativeResponse.RequestObservedAtUnixNano = int64(10+i*10) * 1e9
		if i == 2 {
			input := body["input"].([]any)
			for _, index := range []int{1, 2} {
				input[index].(map[string]any)["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": "turn-origin"}
			}
			input[3].(map[string]any)["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": "turn-origin", "create_time": 25.125}
		}
		rows[i].BodyJSON = mustJSON(body)
	}
	return rows
}
func TestNativeContinuationTypedHistoryProjectionPreservesSourceFields(t *testing.T) {
	rows := nativeMetadataLifecycle(t)
	before := string(rows[1].NativeResponse.Events[2])
	require.NoError(t, validateNativeContinuation(rows))
	require.Equal(t, before, string(rows[1].NativeResponse.Events[2]), "history projection must not rewrite response evidence")
	for _, fault := range []string{"missing null", "non-null content", "wrong origin turn", "metadata missing", "provider time added", "unknown metadata", "turn type", "missing original field", "opaque changed", "result new turn", "result time string", "result time before call", "result time after request", "result extra field", "carrier mismatch", "missing time evidence", "backward time evidence"} {
		t.Run(fault, func(t *testing.T) {
			rows := nativeMetadataLifecycle(t)
			var body map[string]any
			require.NoError(t, json.Unmarshal(rows[2].BodyJSON, &body))
			input := body["input"].([]any)
			reason := input[1].(map[string]any)
			meta := reason["internal_chat_message_metadata_passthrough"].(map[string]any)
			resultMeta := input[3].(map[string]any)["internal_chat_message_metadata_passthrough"].(map[string]any)
			want := 1
			switch fault {
			case "missing null":
				delete(reason, "content")
			case "non-null content":
				reason["content"] = []any{}
			case "wrong origin turn":
				meta["turn_id"] = "turn-consumer"
			case "metadata missing":
				delete(reason, "internal_chat_message_metadata_passthrough")
			case "provider time added":
				meta["create_time"] = 25.125
			case "unknown metadata":
				meta["unproved"] = true
			case "turn type":
				meta["turn_id"] = 25
			case "missing original field":
				delete(reason, "summary")
			case "opaque changed":
				reason["encrypted_content"] = "different"
			case "result new turn":
				resultMeta["turn_id"] = "turn-consumer"
			case "result time string":
				resultMeta["create_time"] = "25.125"
			case "result time before call":
				resultMeta["create_time"] = 19.0
			case "result time after request":
				resultMeta["create_time"] = 31.0
			case "result extra field":
				resultMeta["other"] = false
			case "carrier mismatch":
				body["client_metadata"].(map[string]any)["turn_id"] = "wrong"
			case "missing time evidence":
				rows[2].NativeResponse.RequestObservedAtUnixNano = 0
				want = 2
			case "backward time evidence":
				rows[2].NativeResponse.RequestObservedAtUnixNano = 19e9
				want = 2
			}
			rows[2].BodyJSON = mustJSON(body)
			require.Equal(t, want, nativeContinuationExit(rows))
		})
	}
}
func TestNativeHistoryProjectionOnlyChangesSourceProvenMissingFields(t *testing.T) {
	for _, oldTurn := range []any{nil, "", " ", "older-turn"} {
		t.Run(fmt.Sprint(oldTurn), func(t *testing.T) {
			original := map[string]any{"type": "reasoning", "id": "rs", "summary": []any{}, "encrypted_content": "opaque", "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": oldTurn, "create_time": json.Number("123.125"), "content_item_kinds": []any{"source"}}}
			before := mustJSON(original)
			projected, err := nativeHistoryOutput([]any{original}, "new-turn")
			require.NoError(t, err)
			require.Equal(t, before, mustJSON(original))
			expected := map[string]any{}
			for key, value := range original {
				expected[key] = value
			}
			expected["content"] = nil
			expectedTurn := oldTurn
			if oldTurn == nil || oldTurn == "" {
				expectedTurn = "new-turn"
			}
			expected["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": expectedTurn, "create_time": json.Number("123.125"), "content_item_kinds": []any{"source"}}
			require.Equal(t, expected, projected[0])
		})
	}
	for _, content := range []any{nil, []any{}, []any{map[string]any{"type": "reasoning_text", "text": "kept"}}} {
		source := map[string]any{"type": "reasoning", "content": content, "encrypted_content": "opaque"}
		output, err := nativeHistoryOutput([]any{source}, "")
		require.NoError(t, err)
		require.Equal(t, source, output[0])
	}
	output, err := nativeHistoryOutput([]any{map[string]any{"type": "message", "role": "assistant"}}, "")
	require.NoError(t, err)
	require.NotContains(t, output[0], "content", "missing/null rule is exclusive to Reasoning.content")
}
func TestNativeContinuationToolResultMetadataCannotChangeOnReplay(t *testing.T) {
	rows := nativeMetadataLifecycle(t)
	var previous map[string]any
	require.NoError(t, json.Unmarshal(rows[2].BodyJSON, &previous))
	input := previous["input"].([]any)
	final := map[string]any{"type": "message", "id": "msg_final", "role": "assistant", "content": []any{}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn-consumer"}}
	previous["input"] = append(input, final, map[string]any{"type": "message", "role": "user", "content": "continue"})
	fourth := record{Seq: 4, Conn: 10, Kind: "ws_message", Headers: rows[2].Headers, BodyJSON: mustJSON(previous), NativeResponse: nativeTestEvidence(4, 10, "resp_fourth", []any{})}
	fourth.NativeResponse.RequestObservedAtUnixNano = 40e9
	rows = append(rows, fourth)
	require.NoError(t, validateNativeContinuation(rows))
	input[3].(map[string]any)["internal_chat_message_metadata_passthrough"].(map[string]any)["create_time"] = 26.0
	rows[3].BodyJSON = mustJSON(previous)
	require.Equal(t, 1, nativeContinuationExit(rows), "a still-in-range number cannot replace the original creation time")
}

func TestNativeContinuationEmptyTurnRequiresProvenPurePrewarm(t *testing.T) {
	for _, fault := range []string{"none", "generating", "generate missing", "generate string", "input nonempty", "HTTP", "previous id", "direct null", "direct missing", "canonical missing", "canonical kind", "canonical mismatch", "header mismatch", "header kind", "response output", "response delta", "response incomplete"} {
		t.Run(fault, func(t *testing.T) {
			rows := nativeTestLifecycle(false)
			var body map[string]any
			require.NoError(t, json.Unmarshal(rows[0].BodyJSON, &body))
			metadata := body["client_metadata"].(map[string]any)
			switch fault {
			case "generating":
				body["generate"] = true
			case "generate missing":
				delete(body, "generate")
			case "generate string":
				body["generate"] = "false"
			case "input nonempty":
				body["input"] = []any{map[string]any{"type": "message", "role": "user", "content": "generate"}}
			case "HTTP":
				rows[0].Kind = "http_request"
				rows[0].Method = "POST"
				rows[0].Target = "/backend-api/codex/responses"
			case "previous id":
				body["previous_response_id"] = ""
			case "direct null":
				metadata["turn_id"] = nil
			case "direct missing":
				delete(metadata, "turn_id")
			case "canonical missing":
				delete(metadata, "x-codex-turn-metadata")
			case "canonical kind":
				metadata["x-codex-turn-metadata"] = `{"turn_id":"","request_kind":"turn"}`
			case "canonical mismatch":
				metadata["x-codex-turn-metadata"] = `{"turn_id":"nonempty","request_kind":"prewarm"}`
			case "header mismatch":
				rows[0].Headers = []headerKV{{Name: "x-codex-turn-metadata", Value: `{"turn_id":"other","request_kind":"prewarm"}`}}
			case "header kind":
				rows[0].Headers = []headerKV{{Name: "x-codex-turn-metadata", Value: `{"turn_id":"","request_kind":"turn"}`}}
			case "response output":
				rows[0].NativeResponse = nativeTestEvidence(1, 9, "resp_golden_001", []any{map[string]any{"type": "message", "id": "msg_illegal", "role": "assistant", "content": []any{}}})
			case "response delta":
				events := rows[0].NativeResponse.Events
				rows[0].NativeResponse.Events = []json.RawMessage{events[0], json.RawMessage(`{"type":"response.output_text.delta","output_index":0,"delta":"sampled"}`), events[1]}
			case "response incomplete":
				rows[0].NativeResponse.Complete = false
			}
			rows[0].BodyJSON = mustJSON(body)
			if fault == "none" {
				require.NoError(t, validateNativeContinuation(rows))
				return
			}
			want := 1
			if strings.HasPrefix(fault, "response ") {
				want = 2
			}
			require.Equal(t, want, nativeContinuationExit(rows))
		})
	}
}
func TestNativeContinuationGenerationCannotOmitTurnIdentity(t *testing.T) {
	for _, fault := range []string{"absent", "empty", "null"} {
		t.Run(fault, func(t *testing.T) {
			rows := nativeTestLifecycle(false)
			var body map[string]any
			require.NoError(t, json.Unmarshal(rows[1].BodyJSON, &body))
			switch fault {
			case "absent":
				delete(body, "client_metadata")
			case "empty":
				body["client_metadata"] = map[string]any{"turn_id": "", "x-codex-turn-metadata": `{"turn_id":"","request_kind":"turn"}`}
			case "null":
				body["client_metadata"] = map[string]any{"turn_id": nil}
			}
			rows[1].BodyJSON = mustJSON(body)
			require.Equal(t, 1, nativeContinuationExit(rows))
		})
	}
}

func TestNativeCompactionReplacementPreservesOnlyOriginalMetadata(t *testing.T) {
	for _, variant := range []string{"absent", "empty object", "empty turn", "provided"} {
		faults := []string{"none", "metadata added or changed", "ID changed", "cipher changed", "cipher missing", "wrong window", "duplicate checkpoint", "previous response"}
		if variant != "absent" {
			faults = append(faults, "metadata lost")
		}
		if variant == "provided" {
			faults = append(faults, "original field lost")
		}
		for _, fault := range faults {
			t.Run(variant+"/"+fault, func(t *testing.T) {
				first := map[string]any{"type": "compaction", "id": "cmp_window_one", "encrypted_content": "opaque-one"}
				second := map[string]any{"type": "compaction", "id": "cmp_window_two", "encrypted_content": "opaque-two"}
				switch variant {
				case "empty object":
					second["internal_chat_message_metadata_passthrough"] = map[string]any{}
				case "empty turn":
					second["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": ""}
				case "provided":
					second["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": "provider-original-turn", "create_time": json.Number("123.125")}
				}
				response := func(seq int, id string, input, output []any) record {
					return record{Seq: seq, Conn: 1, Kind: "ws_message", BodyJSON: nativeTestTurnBody(map[string]any{"model": "gpt-6-astra", "input": input}, "request-turn"), NativeResponse: nativeTestEvidence(seq, 1, id, output)}
				}
				rows := []record{response(1, "response_one", []any{map[string]any{"type": "compaction_trigger"}}, []any{first}), response(2, "response_two", []any{first, map[string]any{"type": "compaction_trigger"}}, []any{second}), response(3, "response_three", []any{second, map[string]any{"type": "message", "role": "user", "content": "continue"}}, []any{})}
				evidenceBefore := string(mustJSON(rows[1].NativeResponse.Events))
				var body map[string]any
				require.NoError(t, json.Unmarshal(rows[2].BodyJSON, &body))
				input := body["input"].([]any)
				checkpoint := input[0].(map[string]any)
				switch fault {
				case "metadata added or changed":
					checkpoint["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": "request-turn"}
				case "metadata lost":
					delete(checkpoint, "internal_chat_message_metadata_passthrough")
				case "original field lost":
					delete(checkpoint["internal_chat_message_metadata_passthrough"].(map[string]any), "create_time")
				case "ID changed":
					checkpoint["id"] = "cmp_foreign"
				case "cipher changed":
					checkpoint["encrypted_content"] = "other-cipher"
				case "cipher missing":
					delete(checkpoint, "encrypted_content")
				case "wrong window":
					input[0] = first
				case "duplicate checkpoint":
					body["input"] = append(input, checkpoint)
				case "previous response":
					body["previous_response_id"] = "response_two"
				}
				rows[2].BodyJSON = mustJSON(body)
				want := 1
				if fault == "none" {
					want = 0
				}
				require.Equal(t, want, nativeContinuationExit(rows))
				require.Equal(t, evidenceBefore, string(mustJSON(rows[1].NativeResponse.Events)))
			})
		}
	}
}

// The official e54a5713 client resets the incremental input when host-scoped
// prompt cache options change. A valid new full request is still allowed.
func TestNativeContinuationPromptCacheOptionsBoundary(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after string
		full          bool
		wantExit      int
	}{
		{name: "absent stays absent"},
		{name: "same 12h", before: "12h", after: "12h"},
		{name: "same 30m", before: "30m", after: "30m"},
		{name: "changed ttl cannot use suffix", before: "12h", after: "30m", wantExit: 1},
		{name: "reverse ttl cannot use suffix", before: "30m", after: "12h", wantExit: 1},
		{name: "added ttl cannot use suffix", after: "12h", wantExit: 1},
		{name: "removed ttl cannot use suffix", before: "12h", wantExit: 1},
		{name: "changed ttl with full replay", before: "12h", after: "30m", full: true},
		{name: "added ttl with full replay", after: "12h", full: true},
		{name: "removed ttl with full replay", before: "12h", full: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Begin at a real generation, rather than adding options to the
			// prewarm request (the official client leaves them absent there).
			rows := nativeTestLifecycle(tc.full)[1:]
			for i, ttl := range []string{tc.before, tc.after} {
				var body map[string]any
				require.NoError(t, json.Unmarshal(rows[i].BodyJSON, &body))
				if i == 0 {
					delete(body, "previous_response_id")
				}
				if ttl != "" {
					body["prompt_cache_options"] = map[string]any{"ttl": ttl}
				}
				rows[i].BodyJSON = mustJSON(body)
			}
			require.Equal(t, tc.wantExit, nativeContinuationExit(rows))
		})
	}
}
