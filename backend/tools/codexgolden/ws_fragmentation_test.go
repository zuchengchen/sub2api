//go:build linux

package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func wsFixtureFrame(opcode byte, fin, compressed bool, payload []byte) []byte {
	first := opcode
	if fin {
		first |= 0x80
	}
	if compressed {
		first |= 0x40
	}
	frame := []byte{first}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n <= 65535:
		frame = append(frame, 0x80|126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	mask := []byte{0x12, 0x34, 0x56, 0x78}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	return frame
}

func wsFixtureStart(t *testing.T, extension string) (*fakeUpstream, net.Conn, *bufio.Reader, <-chan struct{}) {
	t.Helper()
	server, client := net.Pipe()
	require.NoError(t, client.SetDeadline(time.Now().Add(3*time.Second)))
	f := &fakeUpstream{connHops: map[int]string{1: "gateway_upstream"}, connHellos: map[int]*clientHello{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		f.serveWebSocket(server, bufio.NewReader(server), 1, "dGhlIHNhbXBsZSBub25jZQ==", extension, 0)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("fixture did not terminate")
		}
	})
	reader := bufio.NewReader(client)
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if line == "\r\n" {
			break
		}
	}
	return f, client, reader, done
}

func wsFixtureReadServerFrame(t *testing.T, reader *bufio.Reader) (byte, []byte) {
	t.Helper()
	var hdr [2]byte
	_, err := io.ReadFull(reader, hdr[:])
	require.NoError(t, err)
	require.Zero(t, hdr[1]&0x80, "server frames must not be masked")
	require.NotZero(t, hdr[0]&0x80)
	n := int(hdr[1] & 0x7f)
	if n == 126 {
		var ext [2]byte
		_, err = io.ReadFull(reader, ext[:])
		require.NoError(t, err)
		n = int(binary.BigEndian.Uint16(ext[:]))
	}
	require.Less(t, n, 65536)
	payload := make([]byte, n)
	_, err = io.ReadFull(reader, payload)
	require.NoError(t, err)
	return hdr[0] & 15, payload
}

func wsFixtureFinish(t *testing.T, client net.Conn, reader *bufio.Reader, done <-chan struct{}) {
	t.Helper()
	_, err := client.Write(wsFixtureFrame(8, true, false, []byte{3, 232}))
	require.NoError(t, err)
	opcode, _ := wsFixtureReadServerFrame(t, reader)
	require.Equal(t, byte(8), opcode)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture did not terminate")
	}
}

// Regression through the real fixture loop: ping must be answered before the
// last data fragment exists, and must not destroy the pending first fragment.
func TestWSFragmentedPingRetainsWholeMessage(t *testing.T) {
	f, client, reader, done := wsFixtureStart(t, "")
	body := []byte(`{"type":"fixture.noop","value":"whole message"}`)
	split := 19
	first := wsFixtureFrame(1, false, false, body[:split])
	ping := wsFixtureFrame(9, true, false, []byte("ping-before-continuation"))
	_, err := client.Write(append(first, ping...))
	require.NoError(t, err)
	opcode, payload := wsFixtureReadServerFrame(t, reader)
	require.Equal(t, byte(10), opcode)
	require.Equal(t, []byte("ping-before-continuation"), payload)
	_, err = client.Write(wsFixtureFrame(0, true, false, body[split:]))
	require.NoError(t, err)
	wsFixtureFinish(t, client, reader, done)
	f.mu.Lock()
	rows := append([]record(nil), f.records...)
	f.mu.Unlock()
	require.Len(t, rows, 1)
	require.JSONEq(t, string(body), string(rows[0].BodyJSON))
	require.Equal(t, 1, rows[0].Frame.Opcode)
	require.Equal(t, 2, rows[0].Frame.Fragments)
	require.True(t, rows[0].Frame.Masked)
}

func TestWSFrameReaderControlOrderingAndCompression(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "deflate"}[compressed], func(t *testing.T) {
			body := []byte(`{"type":"fixture.noop","value":"compressed fragments survive"}`)
			wireBody := body
			if compressed {
				var encoded bytes.Buffer
				writer, err := flate.NewWriter(&encoded, flate.DefaultCompression)
				require.NoError(t, err)
				_, err = writer.Write(body)
				require.NoError(t, err)
				require.NoError(t, writer.Flush())
				wireBody = append([]byte(nil), encoded.Bytes()[:encoded.Len()-4]...)
				require.NoError(t, writer.Close())
			}
			split := len(wireBody) / 2
			wire := wsFixtureFrame(1, false, compressed, wireBody[:split])
			wire = append(wire, wsFixtureFrame(9, true, false, []byte("one"))...)
			wire = append(wire, wsFixtureFrame(10, true, false, []byte("unsolicited pong"))...)
			wire = append(wire, wsFixtureFrame(9, true, false, []byte("two"))...)
			final := wsFixtureFrame(0, true, false, wireBody[split:])
			wire = append(wire, final...)
			wire = append(wire, wsFixtureFrame(1, true, false, []byte(`{"type":"next"}`))...)
			var frames []wsFrameEvidence
			reader := &wsClientMessageReader{br: bufio.NewReader(bytes.NewReader(wire)), deflate: compressed, onFrame: func(frame wsFrameEvidence) error { frames = append(frames, frame); return nil }}
			for _, expected := range []struct {
				opcode  int
				payload string
			}{{9, "one"}, {10, "unsolicited pong"}, {9, "two"}} {
				payload, info, err := reader.Read()
				require.NoError(t, err)
				require.Equal(t, expected.opcode, info.Opcode)
				require.Equal(t, expected.payload, string(payload))
				require.Equal(t, 1, reader.info.Fragments)
			}
			payload, info, err := reader.Read()
			require.NoError(t, err)
			require.Equal(t, wireBody, payload)
			require.Equal(t, 1, info.Opcode)
			require.Equal(t, 2, info.Fragments)
			require.Equal(t, compressed, info.Compressed)
			require.Equal(t, len(wsFixtureFrame(1, false, compressed, wireBody[:split]))+len(final), info.WireLen, "control frame bytes must not inflate data message length")
			if compressed {
				inflater := &inflater{}
				actual, err := inflater.inflate(payload)
				require.NoError(t, err)
				require.Equal(t, body, actual)
			}
			payload, info, err = reader.Read()
			require.NoError(t, err)
			require.Equal(t, `{"type":"next"}`, string(payload))
			require.Equal(t, 1, info.Fragments)
			require.False(t, info.Compressed)
			_, _, err = reader.Read()
			require.ErrorIs(t, err, io.EOF)
			require.Len(t, frames, 6)
			require.Equal(t, []byte{1, 9, 10, 9, 0, 1}, []byte{frames[0].Opcode, frames[1].Opcode, frames[2].Opcode, frames[3].Opcode, frames[4].Opcode, frames[5].Opcode})
		})
	}
}

func TestWSCaptureControlPayloadAndCloseEvidence(t *testing.T) {
	f, client, reader, done := wsFixtureStart(t, "")
	body := []byte(`{"type":"fixture.noop","value":"preserve"}`)
	first := wsFixtureFrame(1, false, false, body[:15])
	ping := []byte{0, 0xff, 'p'}
	_, err := client.Write(append(first, wsFixtureFrame(9, true, false, ping)...))
	require.NoError(t, err)
	opcode, payload := wsFixtureReadServerFrame(t, reader)
	require.Equal(t, byte(10), opcode)
	require.Equal(t, ping, payload)
	remaining := append(wsFixtureFrame(10, true, false, []byte("unsolicited")), wsFixtureFrame(0, true, false, body[15:])...)
	_, err = client.Write(remaining)
	require.NoError(t, err)
	closeBody := append([]byte{3, 233}, []byte("fixture shutdown")...)
	_, err = client.Write(wsFixtureFrame(8, true, false, closeBody))
	require.NoError(t, err)
	opcode, payload = wsFixtureReadServerFrame(t, reader)
	require.Equal(t, byte(8), opcode)
	require.Equal(t, closeBody, payload)
	<-done
	proof := f.wsConnections()[1]
	require.Equal(t, "peer_close", proof.End)
	require.Empty(t, proof.Error)
	require.Zero(t, proof.PendingFragments)
	require.False(t, proof.Truncated)
	require.Len(t, proof.Frames, 7)
	require.Equal(t, []byte{1, 9, 10, 10, 0, 8, 8}, []byte{proof.Frames[0].Opcode, proof.Frames[1].Opcode, proof.Frames[2].Opcode, proof.Frames[3].Opcode, proof.Frames[4].Opcode, proof.Frames[5].Opcode, proof.Frames[6].Opcode})
	require.Equal(t, ping, proof.Frames[1].ControlPayload)
	require.Equal(t, ping, proof.Frames[2].ControlPayload)
	require.Equal(t, "server_to_client", proof.Frames[2].Direction)
	require.NotNil(t, proof.Frames[2].WriteComplete)
	require.True(t, *proof.Frames[2].WriteComplete)
	require.Equal(t, closeBody, proof.Frames[5].ControlPayload)
	require.Equal(t, closeBody, proof.Frames[6].ControlPayload)
	for i, frame := range proof.Frames {
		require.Equal(t, i+1, frame.Sequence)
		require.Len(t, frame.PayloadSHA256, 64)
	}
	require.NoError(t, f.validateModelsBarrier())
	raw, err := json.Marshal(proof)
	require.NoError(t, err)
	var restored wsConnectionEvidence
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.Equal(t, ping, restored.Frames[1].ControlPayload, "non-UTF8 controls must survive JSON without replacement")
	proof.Frames[1].ControlPayload[0] = 7
	require.Equal(t, byte(0), f.wsConnections()[1].Frames[1].ControlPayload[0], "evidence accessor must not alias mutable slices")
	*proof.Frames[2].WriteComplete = false
	require.True(t, *f.wsConnections()[1].Frames[2].WriteComplete, "snapshot must not alias internal write outcome")
}

func TestWSCaptureCloseAbortsPendingMessage(t *testing.T) {
	f, client, reader, done := wsFixtureStart(t, "")
	_, err := client.Write(wsFixtureFrame(1, false, false, []byte("unfinished")))
	require.NoError(t, err)
	_, err = client.Write(wsFixtureFrame(8, true, false, nil))
	require.NoError(t, err)
	opcode, payload := wsFixtureReadServerFrame(t, reader)
	require.Equal(t, byte(8), opcode)
	require.Empty(t, payload)
	<-done
	require.Empty(t, f.records, "closing a fragmented message must not invent a completed data request")
	proof := f.wsConnections()[1]
	require.Equal(t, "peer_close", proof.End)
	require.Equal(t, 1, proof.PendingFragments)
	require.Len(t, proof.Frames, 3)
	require.NoError(t, f.validateModelsBarrier())
}

func TestWSFrameReaderRejectsMalformedFrames(t *testing.T) {
	frame := func(opcode byte, fin, compressed bool, payload string) []byte {
		return wsFixtureFrame(opcode, fin, compressed, []byte(payload))
	}
	tests := []struct {
		name    string
		wire    []byte
		deflate bool
	}{
		{"orphan continuation", frame(0, true, false, "a"), false},
		{"restarted fragmented message", append(frame(1, false, false, "a"), frame(1, true, false, "b")...), false},
		{"fragmented ping", frame(9, false, false, "a"), false},
		{"oversized ping", wsFixtureFrame(9, true, false, bytes.Repeat([]byte("a"), 126)), false},
		{"compressed ping", frame(9, true, true, "a"), true},
		{"compressed continuation", append(frame(1, false, true, "a"), frame(0, true, true, "b")...), true},
		{"compression without negotiation", frame(1, true, true, "a"), false},
		{"reserved control", frame(11, true, false, "a"), false},
		{"reserved data", frame(3, true, false, "a"), false},
		{"unmasked client", []byte{0x81, 1, 'a'}, false},
		{"reserved bit", func() []byte { b := frame(1, true, false, "a"); b[0] |= 0x20; return b }(), false},
		{"one byte close", wsFixtureFrame(8, true, false, []byte{3}), false},
		{"invalid close code", wsFixtureFrame(8, true, false, []byte{3, 237}), false},
		{"invalid close reason", wsFixtureFrame(8, true, false, []byte{3, 232, 0xff}), false},
		{"invalid text UTF8", wsFixtureFrame(1, true, false, []byte{0xff}), false},
		{"nonminimal length", []byte{0x81, 0xfe, 0, 1}, false},
		{"oversized frame", []byte{0x81, 0xff, 0, 0, 0, 0, 4, 0, 0, 1}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &wsClientMessageReader{br: bufio.NewReader(bytes.NewReader(test.wire)), deflate: test.deflate}
			payload, info, err := reader.Read()
			var protocolErr *wsProtocolError
			require.ErrorAs(t, err, &protocolErr)
			require.Nil(t, payload)
			require.Nil(t, info)
		})
	}
}

func TestWSFrameReaderTruncationIsNotCleanEOF(t *testing.T) {
	cases := [][]byte{{0x81}, {0x81, 0x81}, {0x81, 0x81, 1, 2}, {0x81, 0x81, 1, 2, 3, 4}, wsFixtureFrame(1, false, false, []byte("unfinished"))}
	for i, wire := range cases {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			reader := &wsClientMessageReader{br: bufio.NewReader(bytes.NewReader(wire))}
			_, _, err := reader.Read()
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		})
	}
	reader := &wsClientMessageReader{br: bufio.NewReader(bytes.NewReader(nil))}
	_, _, err := reader.Read()
	require.ErrorIs(t, err, io.EOF)
}

func TestWSCaptureInvalidControlCannotLookSuccessful(t *testing.T) {
	f, client, reader, done := wsFixtureStart(t, "")
	_, err := client.Write(wsFixtureFrame(9, false, false, []byte("bad")))
	require.NoError(t, err)
	opcode, payload := wsFixtureReadServerFrame(t, reader)
	require.Equal(t, byte(8), opcode)
	require.Equal(t, []byte{3, 234}, payload)
	<-done
	proof := f.wsConnections()[1]
	require.Equal(t, "protocol_error", proof.End)
	require.Contains(t, proof.Error, "control frame")
	require.Len(t, proof.Frames, 2)
	require.False(t, proof.Frames[0].FIN)
	require.Error(t, f.validateModelsBarrier())
	require.Empty(t, f.records)
}

func TestWSCaptureIncompleteAndOverflowFail(t *testing.T) {
	f := &fakeUpstream{}
	f.startWSCapture(1)
	require.ErrorContains(t, f.validateModelsBarrier(), "incomplete close evidence")
	frame := wsFrameEvidence{Direction: "client_to_server", FIN: true, Opcode: 10, Masked: true}
	for i := 0; i < maxWSFrameEvidence; i++ {
		require.NoError(t, f.observeWSFrame(1, frame))
	}
	require.Error(t, f.observeWSFrame(1, frame))
	f.finishWSCapture(1, "peer_close", nil, 0)
	proof := f.wsConnections()[1]
	require.True(t, proof.Truncated)
	require.Len(t, proof.Frames, maxWSFrameEvidence)
	require.Error(t, f.validateModelsBarrier())
}

func TestWSCaptureControlWriteFailureIsRetained(t *testing.T) {
	f := &fakeUpstream{}
	f.startWSCapture(1)
	err := f.writeWSControl(nativeShortWriteConn{}, 1, 10, []byte("ping"))
	require.ErrorIs(t, err, io.ErrShortWrite)
	f.finishWSCapture(1, "write_error", err, 0)
	proof := f.wsConnections()[1]
	require.Len(t, proof.Frames, 1)
	require.NotNil(t, proof.Frames[0].WriteComplete)
	require.False(t, *proof.Frames[0].WriteComplete)
	require.Contains(t, proof.Frames[0].WriteError, io.ErrShortWrite.Error())
}

func TestWSFrameReaderCloseStopsBeforeTrailingData(t *testing.T) {
	wire := append(wsFixtureFrame(8, true, false, []byte{3, 232}), wsFixtureFrame(1, true, false, []byte(`{"ignored":true}`))...)
	var frames int
	reader := &wsClientMessageReader{br: bufio.NewReader(bytes.NewReader(wire)), onFrame: func(wsFrameEvidence) error { frames++; return nil }}
	_, info, err := reader.Read()
	require.NoError(t, err)
	require.Equal(t, 8, info.Opcode)
	_, _, err = reader.Read()
	require.True(t, errors.Is(err, io.EOF))
	require.Equal(t, 1, frames)
}

func TestWSCaptureCompressedFragmentsThroughServeLoop(t *testing.T) {
	f, client, reader, done := wsFixtureStart(t, "permessage-deflate")
	body := []byte(`{"type":"fixture.noop","value":"compressed live loop"}`)
	var encoded bytes.Buffer
	writer, err := flate.NewWriter(&encoded, flate.DefaultCompression)
	require.NoError(t, err)
	_, err = writer.Write(body)
	require.NoError(t, err)
	require.NoError(t, writer.Flush())
	compressed := append([]byte(nil), encoded.Bytes()[:encoded.Len()-4]...)
	require.NoError(t, writer.Close())
	split := len(compressed) / 2
	_, err = client.Write(append(wsFixtureFrame(1, false, true, compressed[:split]), wsFixtureFrame(9, true, false, []byte("compressed-ping"))...))
	require.NoError(t, err)
	opcode, pong := wsFixtureReadServerFrame(t, reader)
	require.Equal(t, byte(10), opcode)
	require.Equal(t, "compressed-ping", string(pong))
	_, err = client.Write(wsFixtureFrame(0, true, false, compressed[split:]))
	require.NoError(t, err)
	wsFixtureFinish(t, client, reader, done)
	require.Len(t, f.records, 1)
	require.JSONEq(t, string(body), string(f.records[0].BodyJSON))
	require.True(t, f.records[0].Frame.Compressed)
	require.Equal(t, 2, f.records[0].Frame.Fragments)
	proof := f.wsConnections()[1]
	require.Equal(t, byte(0x40), proof.Frames[0].RSV)
	require.Zero(t, proof.Frames[3].RSV)
	require.NoError(t, f.validateModelsBarrier())
}

func TestWSCaptureTruncatedDataEndsWithFailureEvidence(t *testing.T) {
	f, client, _, done := wsFixtureStart(t, "")
	_, err := client.Write(wsFixtureFrame(1, false, false, []byte("unfinished")))
	require.NoError(t, err)
	require.NoError(t, client.Close())
	<-done
	proof := f.wsConnections()[1]
	require.Equal(t, "read_error", proof.End)
	require.Equal(t, 1, proof.PendingFragments)
	require.Contains(t, proof.Error, io.ErrUnexpectedEOF.Error())
	require.Len(t, proof.Frames, 1)
	require.Empty(t, f.records)
	require.Error(t, f.validateModelsBarrier())
}
