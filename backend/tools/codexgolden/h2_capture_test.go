//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func h2FixtureRequests(t *testing.T, collision bool) []byte {
	t.Helper()
	var wire, block bytes.Buffer
	wire.WriteString(http2.ClientPreface)
	fw := http2.NewFramer(&wire, nil)
	require.NoError(t, fw.WriteSettings(http2.Setting{ID: 2, Val: 0}, http2.Setting{ID: 4, Val: 2097152}))
	require.NoError(t, fw.WriteWindowUpdate(0, 5177345))
	enc := hpack.NewEncoder(&block)
	for _, id := range []uint32{1, 3} {
		block.Reset()
		for _, h := range []hpack.HeaderField{{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "https"}, {Name: ":authority", Value: "localhost"}, {Name: ":path", Value: "/collision"}, {Name: "user-agent", Value: "same-client"}, {Name: "content-type", Value: "application/json"}, {Name: "x-text", Value: "literal \"stream 3\"; same-client /collision"}} {
			require.NoError(t, enc.WriteField(h))
		}
		if collision {
			require.NoError(t, enc.WriteField(hpack.HeaderField{Name: h2ObservationHeader, Value: "1"}))
		}
		b := block.Bytes()
		require.NoError(t, fw.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: b[:len(b)/2]}))
		require.NoError(t, fw.WriteContinuation(id, true, b[len(b)/2:]))
	}
	require.NoError(t, fw.WriteData(3, true, []byte(`{"stream":3}`)))
	require.NoError(t, fw.WriteData(1, true, []byte(`{"stream":1}`)))
	return wire.Bytes()
}

func TestH2ObservationPreservesConcurrentIdenticalHeaders(t *testing.T) {
	observations := &h2Observations{pending: map[uint32]*h2Observation{}}
	var parsed bytes.Buffer
	err := relayObservedH2(bytes.NewReader(h2FixtureRequests(t, false)), &parsed, observations)
	require.ErrorIs(t, err, io.EOF)
	fr := http2.NewFramer(io.Discard, bytes.NewReader(parsed.Bytes()[len(http2.ClientPreface):]))
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	markers := map[uint32]string{}
	for {
		frame, err := fr.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if h, ok := frame.(*http2.MetaHeadersFrame); ok {
			for _, field := range h.Fields {
				if field.Name == h2ObservationHeader {
					markers[h.StreamID] = field.Value
				}
			}
		}
	}
	require.Equal(t, map[uint32]string{1: "1", 3: "3"}, markers)
	for _, id := range []uint32{3, 1} {
		r := httptest.NewRequest("POST", "https://localhost/collision", nil)
		r.Header.Set("User-Agent", "same-client")
		r.Header.Set(h2ObservationHeader, markers[id])
		observation, err := observations.take(r)
		require.NoError(t, err)
		require.Equal(t, id, observation.StreamID, "reverse handler scheduling must not consume a method/path/UA FIFO")
		require.Equal(t, int((id+1)/2), observation.RequestIndex)
		require.EqualValues(t, 5177345, observation.ConnectionWindowIncrement)
		require.Equal(t, []h2Setting{{2, 0}, {4, 2097152}}, observation.Settings)
		require.Empty(t, r.Header.Get(h2ObservationHeader))
		for _, field := range observation.Headers {
			require.NotEqual(t, h2ObservationHeader, field.Name, "internal marker is never evidence")
		}
		_, err = observations.take(r)
		require.Error(t, err, "observation cannot be reused")
	}
}

func TestH2ObservationRejectsMarkerCollisionAndTruncatedFrames(t *testing.T) {
	wire := h2FixtureRequests(t, false)
	for _, tc := range []struct {
		name string
		wire []byte
	}{
		{"reserved marker", h2FixtureRequests(t, true)},
		{"truncated", wire[:len(wire)-1]},
		{"invalid preface", []byte(strings.Repeat("x", len(http2.ClientPreface)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := relayObservedH2(bytes.NewReader(tc.wire), io.Discard, &h2Observations{pending: map[uint32]*h2Observation{}})
			require.Error(t, err)
			require.NotErrorIs(t, err, io.EOF)
		})
	}
}

func TestH2CaptureAssociatesBodiesWithRealStreams(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
	f := &fakeUpstream{connHops: map[int]string{}, connHellos: map[int]*clientHello{}}
	done := make(chan struct{})
	go func() { defer close(done); f.serveH2(server, 9, false) }()
	written := make(chan error, 1)
	wire := h2FixtureRequests(t, false)
	go func() { _, err := client.Write(wire); written <- err }()
	fr := http2.NewFramer(io.Discard, client)
	completed := map[uint32]bool{}
	for len(completed) < 2 {
		frame, err := fr.ReadFrame()
		require.NoError(t, err)
		switch v := frame.(type) {
		case *http2.HeadersFrame:
			if v.StreamEnded() {
				completed[v.StreamID] = true
			}
		case *http2.DataFrame:
			if v.StreamEnded() {
				completed[v.StreamID] = true
			}
		}
	}
	require.NoError(t, <-written)
	require.NoError(t, client.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP/2 recorder did not close")
	}
	require.NoError(t, f.validateModelsBarrier())
	require.Len(t, f.records, 2)
	for _, row := range f.records {
		require.NoError(t, validateRecord(row))
		require.Equal(t, "HTTP/2.0", row.Proto)
		require.Equal(t, 9, row.Conn)
		if row.H2.StreamID == 1 {
			require.JSONEq(t, `{"stream":1}`, string(row.BodyJSON))
		} else {
			require.EqualValues(t, 3, row.H2.StreamID)
			require.JSONEq(t, `{"stream":3}`, string(row.BodyJSON))
		}
	}
}

func TestValidateRecordRequiresActualH2Evidence(t *testing.T) {
	base := record{Seq: 1, Conn: 1, ConnReq: 1, Kind: "http_request", Method: "GET", Target: "/backend-api/codex/models", Proto: "HTTP/2.0", Headers: []headerKV{{Name: ":method", Value: "GET"}}, H2: &h2Observation{StreamID: 1, Settings: []h2Setting{{ID: 2, Value: 0}}}}
	require.NoError(t, validateRecord(base))
	for _, tc := range []struct {
		name   string
		mutate func(*record)
	}{
		{"missing tap", func(r *record) { r.H2 = nil }}, {"missing stream", func(r *record) { r.H2.StreamID = 0 }}, {"server stream", func(r *record) { r.H2.StreamID = 2 }}, {"missing settings", func(r *record) { r.H2.Settings = nil }}, {"wrong protocol", func(r *record) { r.Proto = "HTTP/1.1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := base
			observation := *base.H2
			rec.H2 = &observation
			tc.mutate(&rec)
			require.Error(t, validateRecord(rec))
		})
	}
}

type h2ResetReader struct{}

func (h2ResetReader) Read([]byte) (int, error) { return 0, syscall.ECONNRESET }

func TestH2ReadCloseBoundaryDistinguishesEmptyCompleteAndTruncated(t *testing.T) {
	complete := h2FixtureRequests(t, false)
	var pending bytes.Buffer
	pending.WriteString(http2.ClientPreface)
	fw := http2.NewFramer(&pending, nil)
	require.NoError(t, fw.WriteSettings(http2.Setting{ID: 2, Val: 0}))
	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, h := range []hpack.HeaderField{{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "https"}, {Name: ":authority", Value: "localhost"}, {Name: ":path", Value: "/pending"}} {
		require.NoError(t, enc.WriteField(h))
	}
	require.NoError(t, fw.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndHeaders: true}))
	for _, tc := range []struct {
		name   string
		wire   []byte
		clean  bool
		frames int
		open   int
	}{
		{"zero preface bytes", nil, true, 0, 0},
		{"partial preface", []byte(http2.ClientPreface[:5]), false, 0, 0},
		{"complete preface idle", []byte(http2.ClientPreface), true, 0, 0},
		{"partial frame header", complete[:len(http2.ClientPreface)+3], false, 0, 0},
		{"truncated DATA payload", complete[:len(complete)-1], false, 7, 1},
		{"complete frames pending stream", pending.Bytes(), false, 2, 1},
		{"complete streams", complete, true, 8, 0},
	} {
		for _, reset := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reset=%v", tc.name, reset), func(t *testing.T) {
				var source io.Reader = bytes.NewReader(tc.wire)
				if reset {
					source = io.MultiReader(source, h2ResetReader{})
				}
				err := relayObservedH2(source, io.Discard, &h2Observations{pending: map[uint32]*h2Observation{}})
				var end *h2ReadEnd
				require.ErrorAs(t, err, &end)
				require.Equal(t, tc.clean, end.CleanPeerClose)
				require.Equal(t, tc.frames, end.CompletedFrames)
				require.Equal(t, h2FrameTraceVersion, end.FrameTraceVersion)
				require.Equal(t, "client_to_fixture", end.FrameDirection)
				require.Equal(t, maxH2FrameTraceFrames, end.FrameTraceLimit)
				require.False(t, end.FrameTraceOverflow)
				require.Len(t, end.Frames, tc.frames)
				require.Len(t, end.PendingStreams, tc.open)
				if strings.Contains(tc.name, "partial") || strings.Contains(tc.name, "truncated") {
					require.Positive(t, end.PartialBytes)
				}
			})
		}
	}
}

func TestH2EmptyTCPResetRetainsCloseEvidence(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	f := &fakeUpstream{connHops: map[int]string{}, connHellos: map[int]*clientHello{}}
	done := make(chan struct{})
	accepted := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		close(accepted)
		f.serveH2(conn, 3, false)
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	<-accepted
	require.NoError(t, client.(*net.TCPConn).SetLinger(0))
	require.NoError(t, client.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("empty reset did not close recorder")
	}
	require.NoError(t, f.validateModelsBarrier())
	ends := f.h2ConnectionEnds()
	require.Contains(t, ends, 3)
	require.Zero(t, ends[3].PartialBytes)
	require.Zero(t, ends[3].CompletedFrames)
	require.Equal(t, h2FrameTraceVersion, ends[3].FrameTraceVersion)
	require.Empty(t, ends[3].Frames)
	require.False(t, ends[3].FrameTraceOverflow)
	require.Empty(t, ends[3].PendingStreams)
	require.Empty(t, f.records)
}

func TestH2RelayFrameTraceRetainsControlSequenceAfterCompletedStreams(t *testing.T) {
	wire := h2FixtureRequests(t, false)
	var controls bytes.Buffer
	fw := http2.NewFramer(&controls, nil)
	require.NoError(t, fw.WriteSettingsAck())
	require.NoError(t, fw.WriteWindowUpdate(0, 4096))
	require.NoError(t, fw.WriteWindowUpdate(3, 2048))
	require.NoError(t, fw.WriteRSTStream(3, http2.ErrCodeCancel))
	require.NoError(t, fw.WriteGoAway(3, http2.ErrCodeNo, []byte("private-debug-not-retained")))
	require.NoError(t, fw.WritePing(true, [8]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	err := relayObservedH2(bytes.NewReader(append(wire, controls.Bytes()...)), io.Discard, &h2Observations{pending: map[uint32]*h2Observation{}})
	var end *h2ReadEnd
	require.ErrorAs(t, err, &end)
	require.True(t, end.CleanPeerClose)
	require.Empty(t, end.PendingStreams)
	require.Equal(t, 14, end.CompletedFrames)
	require.Len(t, end.Frames, 14)
	want := []uint8{4, 8, 1, 9, 1, 9, 0, 0, 4, 8, 8, 3, 7, 6}
	for i, kind := range want {
		require.Equal(t, kind, end.Frames[i].Type)
		require.Equal(t, i+1, end.Frames[i].Index)
	}
	require.EqualValues(t, 1, end.Frames[8].Flags)
	require.EqualValues(t, 4096, *end.Frames[9].WindowIncrement)
	require.EqualValues(t, 3, end.Frames[10].StreamID)
	require.EqualValues(t, 8, *end.Frames[11].ErrorCode)
	require.EqualValues(t, 0, *end.Frames[12].ErrorCode)
	require.EqualValues(t, 3, *end.Frames[12].LastStreamID)
	require.EqualValues(t, 1, end.Frames[13].Flags)
}
func TestH2RelayFrameTraceOverflowFailsInsteadOfTruncating(t *testing.T) {
	var wire bytes.Buffer
	wire.WriteString(http2.ClientPreface)
	fw := http2.NewFramer(&wire, nil)
	require.NoError(t, fw.WriteSettings())
	for i := 0; i < maxH2FrameTraceFrames; i++ {
		require.NoError(t, fw.WritePing(false, [8]byte{}))
	}
	err := relayObservedH2(bytes.NewReader(wire.Bytes()), io.Discard, &h2Observations{pending: map[uint32]*h2Observation{}})
	var end *h2ReadEnd
	require.ErrorAs(t, err, &end)
	require.ErrorIs(t, err, errH2FrameTraceOverflow)
	require.True(t, end.FrameTraceOverflow)
	require.False(t, end.CleanPeerClose)
	require.Len(t, end.Frames, maxH2FrameTraceFrames)
	require.Equal(t, maxH2FrameTraceFrames+1, end.CompletedFrames)
}
func TestH2RelayIncompleteContinuationKeepsPhysicalHeaderEvidence(t *testing.T) {
	var wire, block bytes.Buffer
	wire.WriteString(http2.ClientPreface)
	fw := http2.NewFramer(&wire, nil)
	require.NoError(t, fw.WriteSettings())
	encoder := hpack.NewEncoder(&block)
	require.NoError(t, encoder.WriteField(hpack.HeaderField{Name: ":method", Value: "GET"}))
	require.NoError(t, fw.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndStream: true, EndHeaders: false}))
	err := relayObservedH2(bytes.NewReader(wire.Bytes()), io.Discard, &h2Observations{pending: map[uint32]*h2Observation{}})
	var end *h2ReadEnd
	require.ErrorAs(t, err, &end)
	require.False(t, end.CleanPeerClose)
	require.Positive(t, end.PartialBytes)
	require.Equal(t, 2, end.CompletedFrames)
	require.Len(t, end.Frames, 2)
	require.EqualValues(t, 1, end.Frames[1].Type)
	require.EqualValues(t, 1, end.Frames[1].Flags)
}
