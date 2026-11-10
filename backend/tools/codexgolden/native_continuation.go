//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"reflect"
	"sort"
	"strings"
)

// Fixture response evidence is separate from client egress semantics. Events
// are retained only after an actual complete socket write and are bound to the
// exact request/connection. A scripted ID alone is never proof of a response.
type nativeResponseEvidence struct {
	fixtureComplete           bool
	RequestObservedAtUnixNano int64             `json:"request_observed_at_unix_nano"`
	RequestSeq                int               `json:"request_seq"`
	Conn                      int               `json:"conn"`
	Status                    int               `json:"status"`
	Events                    []json.RawMessage `json:"events"`
	Complete                  bool              `json:"complete"`
	WriteError                bool              `json:"write_error"`
}

func writeFixtureBytes(c net.Conn, raw []byte) error {
	n, err := c.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	return err
}
func (f *fakeUpstream) nativeEvidence(seq, conn, status int, events []json.RawMessage, complete bool, err error) {
	evidence := &nativeResponseEvidence{RequestSeq: seq, Conn: conn, Status: status, Events: events, Complete: complete, WriteError: err != nil, fixtureComplete: complete}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.records {
		if f.records[i].Seq == seq {
			evidence.RequestObservedAtUnixNano = f.records[i].requestObservedAtUnixNano
			if h2 := f.records[i].H2; h2 != nil {
				evidence.Complete = false
				if delivered, known := f.h2NativeWrites[nativeH2Key{conn, h2.StreamID}]; known {
					evidence.Complete = complete && delivered && err == nil
					evidence.WriteError = evidence.WriteError || !delivered
				}
			}
			f.records[i].NativeResponse = evidence
			return
		}
	}
	f.captureErrors = append(f.captureErrors, fmt.Errorf("native response evidence has no request"))
}

type nativeH2Key struct {
	conn   int
	stream uint32
}

func (f *fakeUpstream) confirmNativeH2(conn int, stream uint32, delivered bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.h2NativeWrites == nil {
		f.h2NativeWrites = map[nativeH2Key]bool{}
	}
	f.h2NativeWrites[nativeH2Key{conn, stream}] = delivered
	for i := range f.records {
		row := &f.records[i]
		if row.Conn == conn && row.H2 != nil && row.H2.StreamID == stream && row.NativeResponse != nil {
			e := *row.NativeResponse
			e.Complete = e.fixtureComplete && delivered && !e.WriteError
			e.WriteError = e.WriteError || !delivered
			row.NativeResponse = &e
		}
	}
}

type nativeContinuationDifference struct{ message string }

func (e *nativeContinuationDifference) Error() string { return e.message }
func nativeDifference(format string, args ...any) error {
	return &nativeContinuationDifference{message: fmt.Sprintf(format, args...)}
}
func nativeContinuationExit(rows []record) int {
	err := validateNativeContinuation(rows)
	if err == nil {
		return 0
	}
	var difference *nativeContinuationDifference
	if errors.As(err, &difference) {
		return 1
	}
	return 2
}

type nativeToolOrigin struct {
	state nativeLifecycleState
	kind  string
	index int
}

type nativeLifecycleState struct {
	id               string
	conn, requestSeq int
	input            []any
	output           []any
	parameters       map[string]any
	compaction       bool
	turnID           string
	observedAt       int64
}

func nativeJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := nativeJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("trailing native JSON")
	}
	return value, nil
}
func nativeJSONValue(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, fmt.Errorf("native JSON nesting")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		out := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("native JSON key")
			}
			if _, ok := out[name]; ok {
				return nil, fmt.Errorf("duplicate native JSON field")
			}
			value, err := nativeJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			out[name] = value
		}
		last, err := d.Token()
		if err != nil || last != json.Delim('}') {
			return nil, fmt.Errorf("native JSON object end")
		}
		return out, nil
	case '[':
		out := []any{}
		for d.More() {
			value, err := nativeJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		last, err := d.Token()
		if err != nil || last != json.Delim(']') {
			return nil, fmt.Errorf("native JSON array end")
		}
		return out, nil
	}
	return nil, fmt.Errorf("native JSON delimiter")
}
func nativeParameters(body map[string]any) map[string]any {
	out := map[string]any{}
	// Fixed e54a5713 core/client.rs:429-499. Metadata/stream_options are not
	// semantic equality fields; no user/tool value is normalized here.
	for _, key := range []string{"model", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "store", "stream", "include", "service_tier", "prompt_cache_key", "prompt_cache_options", "text"} {
		if value, ok := body[key]; ok {
			out[key] = value
		}
	}
	return out
}

// The fixed source stamps durable history with the producing TurnContext.sub_id
// (session/mod.rs:3769-3780,3817-3819). That same id appears in these request
// carriers (turn_context.rs:1118-1142, responses_metadata.rs:348-405). Do not bind
// replayed output to a later request's new turn, or merely accept any UUID.
func nativeRequestTurn(row record, body map[string]any) (string, error) {
	turn := ""
	bound := false
	clientTurn, canonicalPrewarm, headerPrewarm := false, false, true
	prewarmInput, inputOK := body["input"].([]any)
	prewarm := row.Kind == "ws_message" && body["generate"] == false && inputOK && len(prewarmInput) == 0
	if _, exists := body["previous_response_id"]; exists {
		prewarm = false
	}
	bind := func(value any) error {
		id, ok := value.(string)
		if !ok || (id == "" && !prewarm) {
			return nativeDifference("invalid request turn identity")
		}
		if bound && turn != id {
			return nativeDifference("request turn metadata carriers disagree")
		}
		turn, bound = id, true
		return nil
	}
	embedded := func(value any, isCanonical bool) error {
		raw, ok := value.(string)
		if !ok {
			return nativeDifference("turn metadata must be encoded JSON")
		}
		parsed, err := nativeJSON([]byte(raw))
		if err != nil {
			return err
		}
		fields, ok := parsed.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid turn metadata envelope")
		}
		isPrewarm := fields["request_kind"] == "prewarm" && fields["turn_id"] == ""
		if isCanonical {
			canonicalPrewarm = isPrewarm
		} else {
			headerPrewarm = isPrewarm
		}
		return bind(fields["turn_id"])
	}
	if value, exists := body["client_metadata"]; exists {
		fields, ok := value.(map[string]any)
		if !ok {
			return "", nativeDifference("invalid client metadata")
		}
		if value, exists := fields["turn_id"]; exists {
			clientTurn = true
			if err := bind(value); err != nil {
				return "", err
			}
		}
		if value, exists := fields["x-codex-turn-metadata"]; exists {
			if err := embedded(value, true); err != nil {
				return "", err
			}
		}
	}
	foundHeader := false
	for _, header := range row.Headers {
		if strings.EqualFold(header.Name, "x-codex-turn-metadata") {
			if foundHeader {
				return "", fmt.Errorf("duplicate turn metadata header")
			}
			foundHeader = true
			if err := embedded(header.Value, false); err != nil {
				return "", err
			}
		}
	}
	if !bound {
		return "", nativeDifference("request has no turn identity binding")
	}
	if turn == "" {
		// Startup prewarm precedes any inference turn. Its empty identity is
		// permitted only with the fixed empty warmup request and a complete,
		// empty fixture response; no sampled output or tool item is exempt.
		if !prewarm || !clientTurn || !canonicalPrewarm || !headerPrewarm {
			return "", nativeDifference("unproven empty prewarm turn identity")
		}
		state, err := nativeEvidenceState(row)
		if err != nil {
			return "", err
		}
		if state.id == "" || len(state.output) != 0 || len(row.NativeResponse.Events) != 2 {
			return "", fmt.Errorf("prewarm response contains output or extra events")
		}
	}
	return turn, nil
}

// This is the narrow typed/history projection proven by bfdbf01. It changes no
// captured evidence and removes no field. Reasoning's missing Option content is
// emitted as null (models.rs:1117,1790-1796); other missing/null fields are never
// equated. History adds only the producing turn id, preserving all other values
// and existing non-empty ids (models.rs:1054-1063). Provider outputs do not gain
// create_time, unlike newly authored tool results (models.rs:1498-1527).
func nativeHistoryOutput(output []any, turn string) ([]any, error) {
	projected := make([]any, len(output))
	for i, raw := range output {
		source, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid native history item")
		}
		item := make(map[string]any, len(source)+2)
		for key, value := range source {
			item[key] = value
		}
		if item["type"] == "reasoning" {
			if _, exists := item["content"]; !exists {
				item["content"] = nil
			}
		}
		switch item["type"] {
		case "reasoning", "message", "function_call", "custom_tool_call":
			if turn != "" {
				metadata := map[string]any{}
				if value, exists := source["internal_chat_message_metadata_passthrough"]; exists && value != nil {
					original, ok := value.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("invalid source item metadata")
					}
					for key, value := range original {
						metadata[key] = value
					}
				}
				prior, exists := metadata["turn_id"]
				if exists && prior != nil {
					if _, ok := prior.(string); !ok {
						return nil, fmt.Errorf("invalid source item turn identity")
					}
				}
				if !exists || prior == nil || prior == "" {
					metadata["turn_id"] = turn
				}
				item["internal_chat_message_metadata_passthrough"] = metadata
			}
		}
		// Remote v2 compaction takes the replacement-history path, not the
		// ordinary durable-item stamping path: compact_remote_v2.rs:506,
		// compact_remote_v2_installation.rs:235-258, session/mod.rs:4701-4733.
		// Its original metadata (including absence/empty values) stays exact.
		projected[i] = item
	}
	return projected, nil
}

func nativeToolResultMetadata(item map[string]any, origin nativeLifecycleState, row record) error {
	value, exists := item["internal_chat_message_metadata_passthrough"]
	if origin.turnID == "" {
		if exists {
			return nativeDifference("tool metadata lacks a producing turn binding")
		}
		return nil
	}
	metadata, ok := value.(map[string]any)
	if !exists || !ok || len(metadata) != 2 || metadata["turn_id"] != origin.turnID {
		return nativeDifference("tool result metadata differs from the producing turn")
	}
	created, ok := metadata["create_time"].(json.Number)
	if !ok {
		return nativeDifference("tool result create_time must be a number")
	}
	seconds, err := created.Float64()
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return nativeDifference("invalid tool result creation time")
	}
	if row.NativeResponse == nil || origin.observedAt <= 0 || row.NativeResponse.RequestObservedAtUnixNano <= 0 {
		return fmt.Errorf("missing actual request time evidence for tool result")
	}
	if row.NativeResponse.RequestObservedAtUnixNano < origin.observedAt {
		return fmt.Errorf("request time evidence moves backwards")
	}
	// Both observations and the CLI use the same host clock. One millisecond
	// allows floating-point Unix-second precision; the original number is kept
	// and later occurrences must remain exactly equal, including metadata fields.
	lower := float64(origin.observedAt)/1e9 - 0.001
	upper := float64(row.NativeResponse.RequestObservedAtUnixNano)/1e9 + 0.001
	if seconds < lower || seconds > upper {
		return nativeDifference("tool result creation time is outside the observed call interval")
	}
	return nil
}

func nativeEvidenceState(row record) (nativeLifecycleState, error) {
	var result nativeLifecycleState
	evidence := row.NativeResponse
	if evidence == nil || evidence.RequestSeq != row.Seq || evidence.Conn != row.Conn || evidence.WriteError || !evidence.Complete {
		return result, fmt.Errorf("request %d has missing/incomplete response write evidence", row.Seq)
	}
	if evidence.Status == 401 && row.Kind == "http_request" && len(evidence.Events) == 0 {
		return result, nil
	}
	if evidence.Status != 200 || len(evidence.Events) < 2 {
		return result, fmt.Errorf("request %d lacks native terminal evidence", row.Seq)
	}
	created := ""
	done := []any{}
	added := map[int]string{}
	for index, raw := range evidence.Events {
		value, err := nativeJSON(raw)
		if err != nil {
			return result, err
		}
		event, ok := value.(map[string]any)
		if !ok {
			return result, fmt.Errorf("invalid native event")
		}
		switch event["type"] {
		case "response.created":
			response, ok := event["response"].(map[string]any)
			if !ok {
				return result, fmt.Errorf("missing created response")
			}
			id, _ := response["id"].(string)
			if id == "" || (created != "" && id != created) {
				return result, fmt.Errorf("created response identity mismatch")
			}
			created = id
		case "response.output_item.added", "response.output_item.done":
			number, ok := event["output_index"].(json.Number)
			if !ok {
				return result, fmt.Errorf("missing native item index")
			}
			n, err := number.Int64()
			if err != nil || n < 0 || n > 128 {
				return result, fmt.Errorf("bad native item index")
			}
			item, ok := event["item"].(map[string]any)
			if !ok {
				return result, fmt.Errorf("invalid native item")
			}
			id, _ := item["id"].(string)
			if id == "" {
				return result, fmt.Errorf("missing native item id")
			}
			if old, exists := added[int(n)]; exists && old != id {
				return result, fmt.Errorf("conflicting native item identity")
			}
			if event["type"] == "response.output_item.added" {
				if _, exists := added[int(n)]; exists {
					return result, fmt.Errorf("duplicate native added item")
				}
				added[int(n)] = id
			} else {
				// bfdbf01 client.rs:2716-2728 appends each done event in
				// arrival order. A map must not conceal duplicates or reorder it.
				if int(n) != len(done) || added[int(n)] != id {
					return result, fmt.Errorf("duplicate/out-of-order or unannounced native done item")
				}
				done = append(done, item)
			}
		case "response.completed":
			if index != len(evidence.Events)-1 || created == "" {
				return result, fmt.Errorf("terminal is not the last complete fixture event")
			}
			response, ok := event["response"].(map[string]any)
			if !ok || response["id"] != created || response["status"] != "completed" {
				return result, fmt.Errorf("terminal response identity/status mismatch")
			}
			output, ok := response["output"].([]any)
			if !ok || len(output) != len(done) || len(added) != len(done) {
				return result, fmt.Errorf("incomplete native output array")
			}
			for i, item := range output {
				if !reflect.DeepEqual(item, done[i]) {
					return result, fmt.Errorf("native terminal lost/changed an output item")
				}
			}
			result.id, result.conn, result.requestSeq, result.output = created, row.Conn, row.Seq, output
		case "response.failed", "response.incomplete", "error":
			return result, fmt.Errorf("failed native response cannot establish continuation")
		}
	}
	if result.id == "" {
		return result, fmt.Errorf("missing native completed event")
	}
	for _, item := range result.output {
		value, ok := item.(map[string]any)
		if !ok {
			return result, fmt.Errorf("native output is not an object")
		}
		if value["type"] == "compaction" {
			opaque, ok := value["encrypted_content"].(string)
			if !ok || opaque == "" {
				return result, fmt.Errorf("fixture compaction omitted opaque checkpoint")
			}
			result.compaction = true
		}
	}
	return result, nil
}
func nativeContainsBlock(input, block []any) bool { return nativeBlockIndex(input, block) >= 0 }
func nativeBlockIndex(input, block []any) int {
	if len(block) == 0 {
		return -1
	}
	matches, position := 0, -1
	for i := range input {
		if i+len(block) <= len(input) && reflect.DeepEqual(input[i:i+len(block)], block) {
			matches++
			position = i
		}
	}
	if matches != 1 {
		return -1
	}
	return position
}

// Fixed bfdbf01 client.rs:1627-1690 and2300-2308 permits suffix input only
// through the latest completed response on the same live WS session. The full
// conceptual input is prior request + all complete native output + suffix.
// HTTP and compaction/reset inputs still have to contain the original items.
func validateNativeContinuation(recs []record) error {
	rows := append([]record(nil), recs...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	latest := map[int]nativeLifecycleState{}
	responses := map[string]nativeLifecycleState{}
	tools := map[string]nativeToolOrigin{}
	nativeItems := map[string]nativeLifecycleState{}
	toolResults := map[string]any{}
	// These six fixtures have one ordered history. Compaction alone advances
	// its window; a reset to full input must preserve every completed native
	// block in that window, including responses with no tool call.
	var window []nativeLifecycleState
	observed := 0
	for _, row := range rows {
		if row.Kind != "ws_message" && (row.Kind != "http_request" || row.Method != "POST" || !strings.HasSuffix(row.Target, "/codex/responses")) {
			continue
		}
		observed++
		value, err := nativeJSON(row.BodyJSON)
		if err != nil {
			return err
		}
		body, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("request body is not an object")
		}
		input, ok := body["input"].([]any)
		if !ok {
			return fmt.Errorf("request lacks native input")
		}
		previous, _ := body["previous_response_id"].(string)
		if value, exists := body["previous_response_id"]; exists {
			if _, ok := value.(string); !ok {
				return nativeDifference("invalid previous-response identity type")
			}
			if row.Kind != "ws_message" {
				return nativeDifference("HTTP request cannot use previous-response continuation")
			}
		}
		turnID, err := nativeRequestTurn(row, body)
		if err != nil {
			return err
		}
		parameters := nativeParameters(body)
		effective := append([]any(nil), input...)
		if previous != "" {
			prior, known := latest[row.Conn]
			if row.Kind != "ws_message" || !known || prior.id != previous || prior.compaction || !reflect.DeepEqual(parameters, prior.parameters) {
				return nativeDifference("request %d has invalid previous-response continuation", row.Seq)
			}
			effective = append(append(append([]any(nil), prior.input...), prior.output...), input...)
		}
		lastPosition := -1
		for _, expected := range window {
			position := nativeBlockIndex(effective, expected.output)
			if position < 0 || position <= lastPosition {
				return nativeDifference("continuation lost/reordered native state in the current window")
			}
			lastPosition = position + len(expected.output) - 1
		}
		var checkpoint *nativeLifecycleState
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid native input item")
			}
			if item["type"] != "compaction" {
				continue
			}
			id, _ := item["id"].(string)
			origin, known := nativeItems[id]
			if !known || !origin.compaction || previous != "" || !nativeContainsBlock(input, origin.output) {
				return nativeDifference("compaction continuation lacks complete checkpoint")
			}
			if checkpoint != nil && checkpoint.id != origin.id {
				return nativeDifference("mixed compaction windows")
			}
			copy := origin
			checkpoint = &copy
		}
		for inputIndex, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid native input item")
			}
			kind, _ := item["type"].(string)
			if kind == "function_call_output" || kind == "custom_tool_call_output" {
				call, _ := item["call_id"].(string)
				toolOrigin, found := tools[call]
				origin := toolOrigin.state
				if !found {
					return nativeDifference("tool result has no complete originating native response")
				}
				if err := nativeToolResultMetadata(item, origin, row); err != nil {
					return err
				}
				if priorResult, exists := toolResults[call]; exists && !reflect.DeepEqual(priorResult, item) {
					return nativeDifference("previous tool result or its metadata changed")
				}
				toolResults[call] = item
				if kind != toolOrigin.kind+"_output" {
					return nativeDifference("tool result kind differs from its call")
				}
				resultIndex := len(effective) - len(input) + inputIndex
				count := 0
				for _, candidate := range effective {
					if value, ok := candidate.(map[string]any); ok && (value["type"] == "function_call_output" || value["type"] == "custom_tool_call_output") && value["call_id"] == call {
						count++
					}
				}
				if count != 1 {
					return nativeDifference("duplicate tool result")
				}
				// bfdbf01 compact_remote_v2.rs:532-580 retains selected tool
				// results but replaces pre-compaction reasoning with the opaque
				// checkpoint. Bind retained results to that completed checkpoint.
				covered := false
				if checkpoint != nil && origin.requestSeq < checkpoint.requestSeq {
					callBlock, resultPosition := nativeBlockIndex(checkpoint.input, origin.output), nativeBlockIndex(checkpoint.input, []any{item})
					covered = callBlock >= 0 && resultPosition >= callBlock+len(origin.output)
				}
				if !covered {
					if previous != "" {
						if latest[row.Conn].id != origin.id {
							return nativeDifference("tool result does not match the referenced response")
						}
					}
					blockIndex := nativeBlockIndex(effective, origin.output)
					if blockIndex < 0 || resultIndex < blockIndex+len(origin.output) {
						return nativeDifference("full/reset or suffix continuation lost or reordered complete native output")
					}
				}
			}
			if kind == "compaction" {
				id, _ := item["id"].(string)
				origin, known := nativeItems[id]
				if !known || !origin.compaction || previous != "" || !nativeContainsBlock(input, origin.output) {
					return nativeDifference("compaction continuation lacks a complete full-context item")
				}
			}
		}
		state, err := nativeEvidenceState(row)
		if err != nil {
			return err
		}
		if state.id == "" {
			continue
		}
		if _, exists := responses[state.id]; exists {
			return fmt.Errorf("duplicate native response identity")
		}
		state.input, state.parameters, state.turnID = effective, parameters, turnID
		state.observedAt = row.NativeResponse.RequestObservedAtUnixNano
		state.output, err = nativeHistoryOutput(state.output, turnID)
		if err != nil {
			return err
		}
		responses[state.id] = state
		latest[row.Conn] = state
		if state.compaction {
			window = nil
		}
		if len(state.output) > 0 {
			window = append(window, state)
		}
		for outputIndex, raw := range state.output {
			item := raw.(map[string]any)
			id, _ := item["id"].(string)
			if id == "" {
				return fmt.Errorf("missing output identity")
			}
			if _, exists := nativeItems[id]; exists {
				return fmt.Errorf("duplicate native output identity")
			}
			nativeItems[id] = state
			if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
				call, _ := item["call_id"].(string)
				if call == "" {
					return fmt.Errorf("missing native call id")
				}
				if _, exists := tools[call]; exists {
					return fmt.Errorf("duplicate native call id")
				}
				tools[call] = nativeToolOrigin{state: state, kind: item["type"].(string), index: outputIndex}
				opaque := false
				for _, out := range state.output {
					part := out.(map[string]any)
					if part["type"] == "reasoning" && part["encrypted_content"] == "golden-reasoning-"+state.id && part["id"] == "rs_golden_"+state.id {
						opaque = true
					}
				}
				if !opaque {
					return fmt.Errorf("fixture tool response omitted native reasoning state")
				}
			}
		}
	}
	if observed == 0 {
		return fmt.Errorf("native continuation validation observed zero requests")
	}
	return nil
}
