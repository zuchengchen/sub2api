//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Drive the actual HTTP upgrade recorder, including its real socket response.
// An accepted connection is allowed to close without sending any data frame.
func wsHandshakeFixture(t *testing.T, sc scenario) (*fakeUpstream, []record, int) {
	t.Helper()
	server, client := net.Pipe()
	require.NoError(t, client.SetDeadline(time.Now().Add(3*time.Second)))
	defer client.Close()
	f := &fakeUpstream{scenario: sc, connHops: map[int]string{1: "gateway_upstream"}, connHellos: map[int]*clientHello{}}
	done := make(chan struct{})
	go func() { defer close(done); defer server.Close(); f.handleConn(server, 1, false) }()
	request := "GET /backend-api/codex/responses HTTP/1.1\r\nHost: fixture\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	_, err := client.Write([]byte(request))
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: "GET"})
	require.NoError(t, err)
	if response.StatusCode != 101 {
		_ = response.Body.Close()
	}
	require.NoError(t, client.Close())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("actual upgrade recorder did not terminate")
	}
	f.mu.Lock()
	rows := append([]record(nil), f.records...)
	f.mu.Unlock()
	require.Len(t, rows, 1)
	require.Equal(t, "ws_handshake", rows[0].Kind)
	return f, rows, response.StatusCode
}

func TestWSHandshakeIdleMissingEvidenceRejected(t *testing.T) {
	f, rows, status := wsHandshakeFixture(t, scenario{})
	require.Equal(t, 101, status)
	require.Len(t, f.wsConnections(), 1)
	_, err := validateWSLifecycle(f.wsConnections(), fingerprintTestRecordMap(rows))
	require.NoError(t, err)
	_, err = validateWSLifecycle(map[int]*wsConnectionEvidence{}, fingerprintTestRecordMap(rows))
	require.Error(t, err, "a positively observed successful idle upgrade cannot lose its lifecycle sidecar")
}

func TestWSHandshakeActualOutcomesWithOptionalIdleLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sc     scenario
		status int
	}{{"accepted idle", scenario{}, 101}, {"unauthorized", scenario{UnauthorizedUntilRefresh: true}, 401}, {"fallback", scenario{RejectWebSocket: true}, 426}} {
		t.Run(tc.name, func(t *testing.T) {
			f, rows, status := wsHandshakeFixture(t, tc.sc)
			require.Equal(t, tc.status, status)
			evidence := rows[0].NativeResponse
			require.NotNil(t, evidence)
			require.Equal(t, status, evidence.Status)
			require.True(t, evidence.Complete)
			require.False(t, evidence.WriteError)
			require.Equal(t, rows[0].Seq, evidence.RequestSeq)
			require.Equal(t, rows[0].Conn, evidence.Conn)
			require.Positive(t, evidence.RequestObservedAtUnixNano)
			require.Empty(t, evidence.Events)
			wantCaptures := 0
			if status == 101 {
				wantCaptures = 1
			}
			require.Len(t, f.wsConnections(), wantCaptures)
			require.NoError(t, f.validateModelsBarrier())
			_, err := validateWSLifecycle(f.wsConnections(), fingerprintTestRecordMap(rows))
			require.NoError(t, err)
		})
	}
}

type wsHandshakeShortWriteConn struct {
	nativeShortWriteConn
	input *bytes.Reader
}

func (c wsHandshakeShortWriteConn) Read(p []byte) (int, error) { return c.input.Read(p) }

func TestWSHandshakeShortResponseWritesCannotEstablishOutcome(t *testing.T) {
	for _, sc := range []scenario{{}, {RejectWebSocket: true}, {UnauthorizedUntilRefresh: true}} {
		f := &fakeUpstream{scenario: sc, connHops: map[int]string{1: "gateway_upstream"}, connHellos: map[int]*clientHello{}}
		conn := wsHandshakeShortWriteConn{input: bytes.NewReader([]byte("GET /backend-api/codex/responses HTTP/1.1\r\nHost: fixture\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))}
		f.handleConn(conn, 1, false)
		require.Len(t, f.records, 1)
		require.NotNil(t, f.records[0].NativeResponse)
		require.False(t, f.records[0].NativeResponse.Complete)
		require.True(t, f.records[0].NativeResponse.WriteError)
		require.Empty(t, f.wsConnections())
		require.ErrorContains(t, f.validateModelsBarrier(), io.ErrShortWrite.Error())
		_, err := validateWSLifecycle(f.wsConnections(), fingerprintTestRecordMap(f.records))
		require.Error(t, err)
	}
}

func TestWSHandshakeMissingForgedAndUnboundOutcomesFail(t *testing.T) {
	for _, fault := range []string{"missing", "unbound request", "unbound connection", "unknown status", "no timestamp", "incomplete", "write error", "response events", "missing idle capture", "capture after rejection", "message before upgrade", "second upgrade"} {
		t.Run(fault, func(t *testing.T) {
			f, rows, _ := wsHandshakeFixture(t, scenario{})
			captures := f.wsConnections()
			e := rows[0].NativeResponse
			switch fault {
			case "missing":
				rows[0].NativeResponse = nil
			case "unbound request":
				e.RequestSeq++
			case "unbound connection":
				e.Conn++
			case "unknown status":
				e.Status = 503
			case "no timestamp":
				e.RequestObservedAtUnixNano = 0
			case "incomplete":
				e.Complete = false
			case "write error":
				e.WriteError = true
			case "response events":
				e.Events = []json.RawMessage{json.RawMessage(`{"type":"response.completed"}`)}
			case "missing idle capture":
				captures = map[int]*wsConnectionEvidence{}
			case "capture after rejection":
				e.Status = 401
			case "message before upgrade":
				rows = append(rows, record{Seq: rows[0].Seq - 1, Conn: 1, Kind: "ws_message", BodyJSON: json.RawMessage(`{"type":"response.create"}`), Frame: &frameInfo{Opcode: 1, Masked: true, Fragments: 1, WireLen: 1}})
			case "second upgrade":
				copy := rows[0]
				proof := *copy.NativeResponse
				copy.Seq++
				copy.ConnReq++
				proof.RequestSeq = copy.Seq
				copy.NativeResponse = &proof
				rows = append(rows, copy)
			}
			_, err := validateWSLifecycle(captures, fingerprintTestRecordMap(rows))
			require.Error(t, err)
		})
	}
}

func addIdleWSHandshakeFixture(t *testing.T, dir string) int {
	t.Helper()
	path := filepath.Join(dir, "ws_initial.jsonl")
	mapped, err := readRecordMap(path)
	require.NoError(t, err)
	var rows []record
	var idle record
	maxConn, maxSeq := 0, 0
	for _, row := range mapped {
		rows = append(rows, row)
		if row.Kind == "ws_handshake" {
			idle = row
		}
		if row.Conn > maxConn {
			maxConn = row.Conn
		}
		if row.Seq > maxSeq {
			maxSeq = row.Seq
		}
	}
	require.Equal(t, "ws_handshake", idle.Kind)
	idle.Conn = maxConn + 1
	idle.Seq = maxSeq + 1
	idle.ConnReq = 1
	idle.NativeResponse = &nativeResponseEvidence{RequestObservedAtUnixNano: 1, RequestSeq: idle.Seq, Conn: idle.Conn, Status: 101, Complete: true}
	rows = append(rows, idle)
	writeJSONL(path, rows)
	sidecar := filepath.Join(dir, "raw", "ws_initial-ws-connections.json")
	raw, err := os.ReadFile(sidecar)
	require.NoError(t, err)
	var captures map[int]*wsConnectionEvidence
	require.NoError(t, json.Unmarshal(raw, &captures))
	captures[idle.Conn] = &wsConnectionEvidence{Contract: "ws_frames_v1", Frames: []wsFrameEvidence{}, End: "eof", Error: "EOF"}
	writeJSON(sidecar, captures)
	require.NoError(t, finalizeCaptureTraces(dir))
	return idle.Conn
}

func TestWSHandshakeFullCompareExitCalibration(t *testing.T) {
	for _, fault := range []string{"idle accepted", "idle missing", "missing outcome", "rejection status differs"} {
		t.Run(fault, func(t *testing.T) {
			base, candidate := t.TempDir(), t.TempDir()
			comparatorFixture(t, base)
			comparatorFixture(t, candidate)
			want := 2
			switch fault {
			case "idle accepted", "idle missing":
				addIdleWSHandshakeFixture(t, base)
				id := addIdleWSHandshakeFixture(t, candidate)
				if fault == "idle accepted" {
					want = 0
				} else {
					path := filepath.Join(candidate, "raw", "ws_initial-ws-connections.json")
					raw, err := os.ReadFile(path)
					require.NoError(t, err)
					var captures map[int]*wsConnectionEvidence
					require.NoError(t, json.Unmarshal(raw, &captures))
					delete(captures, id)
					writeJSON(path, captures)
					require.NoError(t, finalizeCaptureTraces(candidate))
				}
			case "missing outcome", "rejection status differs":
				path := filepath.Join(candidate, "http_fallback_tool_loop.jsonl")
				mapped, err := readRecordMap(path)
				require.NoError(t, err)
				var rows []record
				changed := false
				for _, row := range mapped {
					if row.Kind == "ws_handshake" && !changed {
						if fault == "missing outcome" {
							row.NativeResponse = nil
						} else {
							row.NativeResponse.Status = 401
							want = 1
						}
						changed = true
					}
					rows = append(rows, row)
				}
				require.True(t, changed)
				writeJSONL(path, rows)
			}
			require.Equal(t, want, compareMain([]string{"-golden", base, "-candidate", candidate}))
		})
	}
}
