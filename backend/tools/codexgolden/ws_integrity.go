//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
)

func expectedWSEvidencePaths() []string {
	paths := make([]string, 0, len(fullScenarioNames))
	for _, name := range fullScenarioNames {
		paths = append(paths, filepath.Join("raw", name+"-ws-connections.json"))
	}
	sort.Strings(paths)
	return paths
}

func retainReferenceWSEvidence(out, source, scenario string) error {
	target := filepath.Join(out, "raw", scenario+"-ws-connections.json")
	from := filepath.Join(source, scenario+"-ws-connections.json")
	fromAbs, err := filepath.Abs(from)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if fromAbs == targetAbs {
		return nil
	}
	raw, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	return os.WriteFile(target, raw, 0600)
}

func finalizeWSEvidenceHashes(dir string, meta map[string]any) error {
	hashes := map[string]string{}
	for _, path := range expectedWSEvidencePaths() {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			return fmt.Errorf("retain WS evidence: %w", err)
		}
		hashes[path] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	meta["ws_evidence_sha256"] = hashes
	return nil
}

func validateWSEvidenceHashes(dir string, hashes map[string]string) error {
	paths := expectedWSEvidencePaths()
	if len(hashes) != len(paths) {
		return fmt.Errorf("missing WS lifecycle evidence hashes")
	}
	for _, path := range paths {
		hash, ok := hashes[path]
		if !ok || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) {
			return fmt.Errorf("missing WS lifecycle hash: %s", path)
		}
		raw, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != hash {
			return fmt.Errorf("missing or changed WS lifecycle evidence: %s", path)
		}
	}
	return nil
}

func readWSLifecycle(dir, scenario string, rows map[recordKey]record) ([]any, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "raw", scenario+"-ws-connections.json"))
	if err != nil {
		return nil, err
	}
	var captures map[int]*wsConnectionEvidence
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&captures); err != nil || captures == nil {
		return nil, fmt.Errorf("invalid WS lifecycle JSON: %v", err)
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return nil, fmt.Errorf("trailing WS lifecycle JSON")
	}
	return validateWSLifecycle(captures, rows)
}

// A handshake status is evidence only after the actual fixture write completed.
// Reuse the response-write receipt used by native continuation, with no events.
func validateWSHandshakeOutcome(row record) error {
	e := row.NativeResponse
	if e == nil || e.RequestSeq != row.Seq || e.Conn != row.Conn || e.RequestObservedAtUnixNano <= 0 || !e.Complete || e.WriteError || len(e.Events) != 0 {
		return fmt.Errorf("missing, incomplete or unbound WS handshake outcome for request %d", row.Seq)
	}
	if e.Status != 101 && e.Status != 401 && e.Status != 426 {
		return fmt.Errorf("unexpected WS fixture handshake status %d", e.Status)
	}
	return nil
}

// Associate complete frame runs with actual request records before comparing.
// No controls are required: an otherwise complete connection may simply end at
// a frame boundary. All observed controls and their payloads stay observable.
func validateWSLifecycle(captures map[int]*wsConnectionEvidence, rows map[recordKey]record) ([]any, error) {
	handshakes := map[int]int{}
	accepted := map[int]int{}
	messages := map[int][]record{}
	var attempts []record
	for _, row := range rows {
		if row.Kind == "ws_handshake" && gatewayCoreTarget(row.Target) {
			if err := validateWSHandshakeOutcome(row); err != nil {
				return nil, err
			}
			attempts = append(attempts, row)
		}
		if row.Kind == "ws_message" {
			messages[row.Conn] = append(messages[row.Conn], row)
		}
	}
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].Seq < attempts[j].Seq })
	connections := map[int]int{}
	handshakeEvents := make([]any, 0, len(attempts))
	for _, row := range attempts {
		if accepted[row.Conn] != 0 {
			return nil, fmt.Errorf("HTTP handshake after successful WS upgrade on %d", row.Conn)
		}
		if connections[row.Conn] == 0 {
			connections[row.Conn] = len(connections) + 1
			handshakes[row.Conn] = row.Seq
		}
		if row.NativeResponse.Status == 101 {
			accepted[row.Conn] = row.Seq
			if captures[row.Conn] == nil {
				return nil, fmt.Errorf("successful idle/active WS connection %d lacks lifecycle capture", row.Conn)
			}
		}
		handshakeEvents = append(handshakeEvents, map[string]any{"connection": connections[row.Conn], "request_index": row.ConnReq, "status": row.NativeResponse.Status})
	}
	for conn, group := range messages {
		for _, row := range group {
			if accepted[conn] == 0 || row.Seq <= accepted[conn] {
				return nil, fmt.Errorf("WS message has no preceding successful upgrade on %d", conn)
			}
		}
		if captures[conn] == nil {
			return nil, fmt.Errorf("WS message connection %d lacks lifecycle capture", conn)
		}
		sort.Slice(group, func(i, j int) bool { return group[i].Seq < group[j].Seq })
	}
	ids := make([]int, 0, len(captures))
	for id := range captures {
		if accepted[id] == 0 {
			return nil, fmt.Errorf("WS lifecycle has no successful core handshake: %d", id)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return handshakes[ids[i]] < handshakes[ids[j]] })
	normalized := []any{map[string]any{"handshakes": handshakeEvents}}
	for _, id := range ids {
		capture := captures[id]
		if capture == nil || capture.Contract != "ws_frames_v1" || capture.Truncated || len(capture.Frames) > maxWSFrameEvidence {
			return nil, fmt.Errorf("invalid or truncated WS lifecycle for %d", id)
		}
		if capture.End != "peer_close" && capture.End != "eof" {
			return nil, fmt.Errorf("incomplete/failed WS lifecycle for %d: %s", id, capture.End)
		}
		if capture.End == "peer_close" && capture.Error != "" || capture.End == "eof" && capture.Error != "" && capture.Error != "EOF" {
			return nil, fmt.Errorf("unexpected WS lifecycle error for %d", id)
		}
		var pending frameInfo
		count := 0
		closed := false
		var replyOpcode byte
		var replyPayload []byte
		events := make([]any, 0, len(capture.Frames))
		for i, frame := range capture.Frames {
			invalid := func(reason string) ([]any, error) {
				return nil, fmt.Errorf("WS lifecycle %d frame %d: %s", id, i+1, reason)
			}
			if frame.RSV & ^byte(0x70) != 0 || frame.Sequence != i+1 || frame.PayloadLength < 0 || frame.PayloadLength > maxWSMessageBytes || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(frame.PayloadSHA256) {
				return invalid("invalid sequence, size or digest")
			}
			overhead := 2
			if frame.PayloadLength >= 126 {
				overhead += 2
			}
			if frame.PayloadLength > 65535 {
				overhead += 6
			}
			if frame.Masked {
				overhead += 4
			}
			if frame.WireLength != frame.PayloadLength+overhead {
				return invalid("wire length mismatch")
			}
			if replyOpcode != 0 && frame.Direction != "server_to_client" {
				return invalid("fixture control response missing")
			}
			control := frame.Opcode >= 8
			if control {
				if !frame.FIN || frame.RSV != 0 || frame.PayloadLength > 125 || len(frame.ControlPayload) != frame.PayloadLength || fmt.Sprintf("%x", sha256.Sum256(frame.ControlPayload)) != frame.PayloadSHA256 {
					return invalid("invalid control evidence")
				}
				if frame.Opcode != 8 && frame.Opcode != 9 && frame.Opcode != 10 {
					return invalid("reserved control opcode")
				}
				if frame.Opcode == 8 {
					if err := validateWSClosePayload(frame.ControlPayload); err != nil {
						return invalid(err.Error())
					}
				}
			} else if len(frame.ControlPayload) != 0 {
				return invalid("data frame carries control payload")
			}
			switch frame.Direction {
			case "server_to_client":
				if !control || frame.Masked || frame.WriteComplete == nil || !*frame.WriteComplete || frame.WriteError != "" || frame.Opcode != replyOpcode || !bytes.Equal(frame.ControlPayload, replyPayload) {
					return invalid("unconfirmed or incorrect control response")
				}
				replyOpcode = 0
				replyPayload = nil
			case "client_to_server":
				if closed || !frame.Masked || frame.WriteComplete != nil || frame.WriteError != "" || frame.RSV&0x30 != 0 {
					return invalid("invalid client frame flags/state")
				}
				if control {
					if frame.Opcode == 9 {
						replyOpcode = 10
						replyPayload = frame.ControlPayload
					}
					if frame.Opcode == 8 {
						closed = true
						replyOpcode = 8
						replyPayload = frame.ControlPayload
					}
				} else {
					switch frame.Opcode {
					case 0:
						if pending.Fragments == 0 || frame.RSV != 0 {
							return invalid("invalid continuation")
						}
					case 1, 2:
						if pending.Fragments != 0 {
							return invalid("new data before final continuation")
						}
						pending = frameInfo{Opcode: int(frame.Opcode), Masked: true, Compressed: frame.RSV&0x40 != 0}
					default:
						return invalid("reserved data opcode")
					}
					pending.Fragments++
					pending.WireLen += frame.WireLength
					if frame.FIN {
						if count >= len(messages[id]) || messages[id][count].Frame == nil || !reflect.DeepEqual(pending, *messages[id][count].Frame) {
							return invalid("completed frame run disagrees with request capture")
						}
						count++
						pending = frameInfo{}
					}
				}
			default:
				return invalid("unknown direction")
			}
			event := map[string]any{"direction": frame.Direction, "fin": frame.FIN, "rsv": frame.RSV, "opcode": frame.Opcode, "masked": frame.Masked}
			if control {
				event["payload"] = frame.ControlPayload
				event["wire_length"] = frame.WireLength
			}
			// Data sizes/digests vary with UUIDs, clocks and compression. Only
			// these variable values are omitted; every frame/order is retained.
			events = append(events, event)
		}
		if count != len(messages[id]) || pending.Fragments != capture.PendingFragments || replyOpcode != 0 || closed != (capture.End == "peer_close") || capture.End == "eof" && pending.Fragments != 0 {
			return nil, fmt.Errorf("WS lifecycle %d does not explain messages/termination", id)
		}
		normalized = append(normalized, map[string]any{"connection": connections[id], "events": events, "end": capture.End, "pending_fragments": capture.PendingFragments})
	}
	return normalized, nil
}
