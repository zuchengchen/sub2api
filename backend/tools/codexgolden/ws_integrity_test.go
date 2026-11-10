//go:build linux

package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These frames are new, explicit synthetic verifier inputs constructed from
// historical request-only fixtures. They are never official capture evidence.
func writeSyntheticWSLifecycleFixture(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "raw"), 0700))
	for _, name := range fullScenarioNames {
		records, err := readRecordMap(filepath.Join(dir, name+".jsonl"))
		require.NoError(t, err)
		rows := make([]record, 0, len(records))
		for _, row := range records {
			rows = append(rows, row)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
		f := &fakeUpstream{}
		lastHandshake := map[int]int{}
		for _, row := range rows {
			if row.Kind == "ws_handshake" {
				lastHandshake[row.Conn] = row.Seq
			}
		}
		for i := range rows {
			row := &rows[i]
			if row.Kind != "ws_handshake" {
				continue
			}
			// Explicit synthetic outcomes, never inferred for real captures.
			status := 101
			if strings.HasPrefix(name, "http_") {
				hasMessage := false
				for _, message := range rows {
					if message.Kind == "ws_message" && message.Conn == row.Conn {
						hasMessage = true
					}
				}
				if !hasMessage {
					status = 426
				}
			} else if row.Seq != lastHandshake[row.Conn] {
				status = 401
			}
			if name == "ws_unauthorized_refresh" {
				hasMessage := false
				for _, message := range rows {
					if message.Kind == "ws_message" && message.Conn == row.Conn {
						hasMessage = true
					}
				}
				if !hasMessage {
					status = 401
				}
			}
			row.NativeResponse = &nativeResponseEvidence{RequestObservedAtUnixNano: 1, RequestSeq: row.Seq, Conn: row.Conn, Status: status, Complete: true}
			if status == 101 {
				f.startWSCapture(row.Conn)
			}
		}
		for i := range rows {
			row := &rows[i]
			if row.Kind != "ws_message" {
				continue
			}
			if f.wsCaptures[row.Conn] == nil {
				f.startWSCapture(row.Conn)
			}
			body := []byte(row.BodyJSON)
			if row.Frame.Compressed {
				var compressed bytes.Buffer
				writer, err := flate.NewWriter(&compressed, flate.DefaultCompression)
				require.NoError(t, err)
				_, err = writer.Write(body)
				require.NoError(t, err)
				require.NoError(t, writer.Flush())
				body = append([]byte(nil), compressed.Bytes()[:compressed.Len()-4]...)
				require.NoError(t, writer.Close())
			}
			row.Frame.WireLen = 0
			for fragment := 0; fragment < row.Frame.Fragments; fragment++ {
				opcode := byte(0)
				if fragment == 0 {
					opcode = byte(row.Frame.Opcode)
				}
				wire := wsFixtureFrame(opcode, fragment == row.Frame.Fragments-1, fragment == 0 && row.Frame.Compressed, body[fragment*len(body)/row.Frame.Fragments:(fragment+1)*len(body)/row.Frame.Fragments])
				_, frame, err := readWSClientFrame(bufio.NewReader(bytes.NewReader(wire)))
				require.NoError(t, err)
				row.Frame.WireLen += frame.WireLength
				require.NoError(t, f.observeWSFrame(row.Conn, frame))
			}
		}
		for conn := range f.wsCaptures {
			f.finishWSCapture(conn, "eof", io.EOF, 0)
		}
		writeJSONL(filepath.Join(dir, name+".jsonl"), rows)
		writeJSON(filepath.Join(dir, "raw", name+"-ws-connections.json"), f.wsConnections())
	}
}

func syntheticWSControl(opcode byte, payload []byte, server bool) wsFrameEvidence {
	_, frame, err := readWSClientFrame(bufio.NewReader(bytes.NewReader(wsFixtureFrame(opcode, true, false, payload))))
	if err != nil {
		panic(err)
	}
	if server {
		complete := true
		frame.Direction = "server_to_client"
		frame.Masked = false
		frame.WireLength -= 4
		frame.WriteComplete = &complete
	}
	return frame
}

func mutateWSLifecycleFixture(t *testing.T, dir string, mutate func(*wsConnectionEvidence)) {
	t.Helper()
	path := filepath.Join(dir, "raw", "ws_initial-ws-connections.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var captures map[int]*wsConnectionEvidence
	require.NoError(t, json.Unmarshal(raw, &captures))
	ids := make([]int, 0, len(captures))
	for id := range captures {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	require.NotEmpty(t, ids)
	capture := captures[ids[0]]
	mutate(capture)
	for i := range capture.Frames {
		capture.Frames[i].Sequence = i + 1
	}
	writeJSON(path, captures)
	require.NoError(t, finalizeCaptureTraces(dir))
}

func TestWSLifecycleComparatorInputAndDifferenceCalibration(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"none", 0}, {"missing sidecar", 2}, {"changed sidecar hash", 2}, {"missing hash", 2}, {"malformed rebound JSON", 2},
		{"missing connection", 2}, {"dropped data frame", 2}, {"wrong wire length", 2}, {"truncated evidence", 2}, {"unknown end", 2},
		{"added ping", 1}, {"added unsolicited pong", 1}, {"close instead of eof", 1}, {"wrong pong echo", 2}, {"unknown reserved bit", 2},
		{"dynamic data digest", 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			base, candidate := t.TempDir(), t.TempDir()
			comparatorFixture(t, base)
			comparatorFixture(t, candidate)
			path := filepath.Join(candidate, "raw", "ws_initial-ws-connections.json")
			switch test.name {
			case "missing sidecar":
				require.NoError(t, os.Remove(path))
			case "changed sidecar hash":
				require.NoError(t, os.WriteFile(path, []byte("{}"), 0600))
			case "missing hash":
				meta, err := readCaptureMetadata(candidate)
				require.NoError(t, err)
				delete(meta.WSEvidenceHashes, filepath.Join("raw", "ws_initial-ws-connections.json"))
				writeJSON(filepath.Join(candidate, "meta.json"), meta)
			case "malformed rebound JSON":
				require.NoError(t, os.WriteFile(path, []byte("{broken"), 0600))
				require.NoError(t, finalizeCaptureTraces(candidate))
			case "missing connection":
				writeJSON(path, map[int]*wsConnectionEvidence{})
				require.NoError(t, finalizeCaptureTraces(candidate))
			case "dropped data frame":
				mutateWSLifecycleFixture(t, candidate, func(c *wsConnectionEvidence) { c.Frames = c.Frames[1:] })
			case "wrong wire length":
				mutateWSLifecycleFixture(t, candidate, func(c *wsConnectionEvidence) { c.Frames[0].WireLength++ })
			case "truncated evidence":
				mutateWSLifecycleFixture(t, candidate, func(c *wsConnectionEvidence) { c.Truncated = true })
			case "unknown end":
				mutateWSLifecycleFixture(t, candidate, func(c *wsConnectionEvidence) { c.End = "not observed" })
			case "added ping", "wrong pong echo":
				mutateWSLifecycleFixture(t, candidate, func(c *wsConnectionEvidence) {
					ping := syntheticWSControl(9, []byte("actual ping"), false)
					pong := syntheticWSControl(10, []byte("actual ping"), true)
					if test.name == "wrong pong echo" {
						pong = syntheticWSControl(10, []byte("wrong"), true)
					}
					c.Frames = append([]wsFrameEvidence{ping, pong}, c.Frames...)
				})
			case "added unsolicited pong":
				mutateWSLifecycleFixture(t, candidate, func(c *wsConnectionEvidence) {
					c.Frames = append([]wsFrameEvidence{syntheticWSControl(10, []byte("unsolicited"), false)}, c.Frames...)
				})
			case "close instead of eof":
				mutateWSLifecycleFixture(t, candidate, func(c *wsConnectionEvidence) {
					c.Frames = append(c.Frames, syntheticWSControl(8, []byte{3, 232}, false), syntheticWSControl(8, []byte{3, 232}, true))
					c.End = "peer_close"
					c.Error = ""
				})
			case "unknown reserved bit":
				mutateWSLifecycleFixture(t, candidate, func(c *wsConnectionEvidence) { c.Frames[0].RSV |= 0x80 })
			case "dynamic data digest":
				mutateWSLifecycleFixture(t, candidate, func(c *wsConnectionEvidence) {
					for i := range c.Frames {
						c.Frames[i].PayloadSHA256 = strings.Repeat("f", 64)
					}
				})
			}
			require.Equal(t, test.want, compareMain([]string{"-golden", base, "-candidate", candidate}))
		})
	}
}

func TestWSLifecyclePreservesCloseCodePayloadAndControlOrder(t *testing.T) {
	for _, change := range []string{"same binary controls", "different ping payload", "different close code", "different position"} {
		t.Run(change, func(t *testing.T) {
			base, candidate := t.TempDir(), t.TempDir()
			comparatorFixture(t, base)
			comparatorFixture(t, candidate)
			for _, dir := range []string{base, candidate} {
				mutateWSLifecycleFixture(t, dir, func(c *wsConnectionEvidence) {
					ping := []byte{0, 255, 1}
					code := []byte{3, 232}
					if dir == candidate && change == "different ping payload" {
						ping = []byte{0, 254, 1}
					}
					if dir == candidate && change == "different close code" {
						code = []byte{3, 233}
					}
					controls := []wsFrameEvidence{syntheticWSControl(9, ping, false), syntheticWSControl(10, ping, true)}
					if dir == candidate && change == "different position" {
						c.Frames = append(c.Frames, controls...)
					} else {
						c.Frames = append(controls, c.Frames...)
					}
					c.Frames = append(c.Frames, syntheticWSControl(8, code, false), syntheticWSControl(8, code, true))
					c.End = "peer_close"
					c.Error = ""
				})
			}
			want := 1
			if change == "same binary controls" {
				want = 0
			}
			require.Equal(t, want, compareMain([]string{"-golden", base, "-candidate", candidate}))
		})
	}
}

func TestWSLifecycleConnectionIDsAreLocalButMessageBindingIsRequired(t *testing.T) {
	dir := t.TempDir()
	comparatorFixture(t, dir)
	rows, err := readRecordMap(filepath.Join(dir, "ws_initial.jsonl"))
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, "raw", "ws_initial-ws-connections.json"))
	require.NoError(t, err)
	var captures map[int]*wsConnectionEvidence
	require.NoError(t, json.Unmarshal(raw, &captures))
	expected, err := validateWSLifecycle(captures, rows)
	require.NoError(t, err)
	shiftedRows := map[recordKey]record{}
	for _, row := range rows {
		row.Conn += 100
		if row.NativeResponse != nil {
			copy := *row.NativeResponse
			copy.Conn = row.Conn
			row.NativeResponse = &copy
		}
		shiftedRows[recordKey{Conn: row.Conn, Seq: row.Seq}] = row
	}
	shiftedCaptures := map[int]*wsConnectionEvidence{}
	for id, capture := range captures {
		shiftedCaptures[id+100] = capture
	}
	actual, err := validateWSLifecycle(shiftedCaptures, shiftedRows)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(expected, actual))
	_, err = validateWSLifecycle(captures, shiftedRows)
	require.Error(t, err)
}

func TestWSLifecycleCustomKeepPathRetainedAndHashesRequired(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	for _, name := range fullScenarioNames {
		data := []byte(`{}`)
		require.NoError(t, os.WriteFile(filepath.Join(source, name+"-ws-connections.json"), data, 0600))
		require.NoError(t, retainReferenceWSEvidence(root, source, name))
	}
	meta := map[string]any{}
	require.NoError(t, finalizeWSEvidenceHashes(root, meta))
	hashes := meta["ws_evidence_sha256"].(map[string]string)
	require.NoError(t, validateWSEvidenceHashes(root, hashes))
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(`{}`))), hashes[filepath.Join("raw", "ws_initial-ws-connections.json")])
	hashes["unexpected"] = strings.Repeat("f", 64)
	require.Error(t, validateWSEvidenceHashes(root, hashes))
}
