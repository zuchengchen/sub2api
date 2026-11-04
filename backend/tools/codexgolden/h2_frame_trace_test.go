//go:build linux

package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func summaryTestFrame(kind, flags uint8, stream uint32, payload []byte) []byte {
	raw := []byte{byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload)), kind, flags}
	raw = binary.BigEndian.AppendUint32(raw, stream)
	return append(raw, payload...)
}
func TestH2FrameSummaryRecordsOnlyBoundedMetadata(t *testing.T) {
	trace := &h2FrameTrace{limit: maxH2FrameTraceFrames}
	var raw []byte
	raw = append(raw, summaryTestFrame(4, 1, 0, nil)...)
	raw = append(raw, summaryTestFrame(8, 0, 0, binary.BigEndian.AppendUint32(nil, 5177345))...)
	raw = append(raw, summaryTestFrame(8, 0, 3, binary.BigEndian.AppendUint32(nil, 4096))...)
	raw = append(raw, summaryTestFrame(3, 0, 3, binary.BigEndian.AppendUint32(nil, 8))...)
	goAway := binary.BigEndian.AppendUint32(nil, 3)
	goAway = binary.BigEndian.AppendUint32(goAway, 0)
	goAway = append(goAway, []byte("GOAWAY_PRIVATE_DEBUG")...)
	raw = append(raw, summaryTestFrame(7, 0, 0, goAway)...)
	raw = append(raw, summaryTestFrame(1, 4, 1, []byte("HEADERS_PRIVATE_CONTENT"))...)
	raw = append(raw, summaryTestFrame(0, 1, 1, []byte("DATA_PRIVATE_CONTENT"))...)
	raw = append(raw, summaryTestFrame(6, 1, 0, []byte("PINGDATA"))...)
	count, err := trace.observeRead(raw)
	if err != nil {
		t.Fatal(err)
	}
	if count != 8 || len(trace.frames) != 8 {
		t.Fatalf("count=%d records=%d", count, len(trace.frames))
	}
	if trace.frames[0].TypeName != "SETTINGS" || trace.frames[0].Flags != 1 {
		t.Fatal("SETTINGS ACK lost")
	}
	if trace.frames[1].WindowIncrement == nil || *trace.frames[1].WindowIncrement != 5177345 || trace.frames[2].StreamID != 3 {
		t.Fatal("window relation lost")
	}
	if trace.frames[3].ErrorCode == nil || *trace.frames[3].ErrorCode != 8 {
		t.Fatal("RST error code lost")
	}
	if trace.frames[4].ErrorCode == nil || *trace.frames[4].ErrorCode != 0 || trace.frames[4].LastStreamID == nil || *trace.frames[4].LastStreamID != 3 {
		t.Fatal("GOAWAY zero error/last stream lost")
	}
	encoded, err := json.Marshal(trace.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"GOAWAY_PRIVATE_DEBUG", "HEADERS_PRIVATE_CONTENT", "DATA_PRIVATE_CONTENT", "PINGDATA"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("payload content leaked into frame summary: %s", private)
		}
	}
	snapshot := trace.snapshot()
	*snapshot[3].ErrorCode = 99
	snapshot[0].Flags = 7
	if *trace.frames[3].ErrorCode != 8 || trace.frames[0].Flags != 1 {
		t.Fatal("snapshot aliases recorded control evidence")
	}
}
func TestH2FrameSummaryDoesNotInventTruncatedFrames(t *testing.T) {
	complete := summaryTestFrame(4, 0, 0, nil)
	for _, tail := range [][]byte{{0, 0, 4}, summaryTestFrame(8, 0, 0, []byte{1, 2, 3, 4})[:11]} {
		trace := &h2FrameTrace{limit: maxH2FrameTraceFrames}
		count, err := trace.observeRead(append(append([]byte(nil), complete...), tail...))
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 || len(trace.frames) != 1 {
			t.Fatal("partial header/payload became a complete frame")
		}
	}
}
func TestH2FrameSummaryOverflowCannotLookComplete(t *testing.T) {
	trace := &h2FrameTrace{limit: 2}
	raw := append(summaryTestFrame(4, 0, 0, nil), summaryTestFrame(4, 1, 0, nil)...)
	raw = append(raw, summaryTestFrame(4, 1, 0, nil)...)
	count, err := trace.observeRead(raw)
	if !errors.Is(err, errH2FrameTraceOverflow) || !trace.overflow || len(trace.frames) != 2 || count != 3 {
		t.Fatalf("overflow did not fail closed: count=%d frames=%d error=%v", count, len(trace.frames), err)
	}
}
func TestH2FrameSummaryRejectsMalformedControlLengths(t *testing.T) {
	for _, kind := range []uint8{3, 7, 8} {
		trace := &h2FrameTrace{limit: 8}
		_, err := trace.observeRead(summaryTestFrame(kind, 0, 1, []byte{0, 1, 2}))
		if err == nil {
			t.Fatalf("malformed control type %d passed", kind)
		}
	}
}
