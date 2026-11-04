//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const h2FrameTraceVersion = 1
const maxH2FrameTraceFrames = 1024

var errH2FrameTraceOverflow = errors.New("HTTP/2 frame trace limit exceeded")

// This records only physical frame metadata in the client -> fixture direction.
// DATA/HEADERS/CONTINUATION bytes, GOAWAY debug data and PING values are omitted.
type h2FrameSummary struct {
	Index             int     `json:"index"`
	Type              uint8   `json:"type"`
	TypeName          string  `json:"type_name"`
	Flags             uint8   `json:"flags"`
	StreamID          uint32  `json:"stream_id"`
	ReservedStreamBit bool    `json:"reserved_stream_bit,omitempty"`
	Length            uint32  `json:"length"`
	ErrorCode         *uint32 `json:"error_code,omitempty"`
	LastStreamID      *uint32 `json:"last_stream_id,omitempty"`
	WindowIncrement   *uint32 `json:"window_increment,omitempty"`
}

func (f h2FrameSummary) clone() h2FrameSummary {
	if f.ErrorCode != nil {
		value := *f.ErrorCode
		f.ErrorCode = &value
	}
	if f.LastStreamID != nil {
		value := *f.LastStreamID
		f.LastStreamID = &value
	}
	if f.WindowIncrement != nil {
		value := *f.WindowIncrement
		f.WindowIncrement = &value
	}
	return f
}

type h2FrameTrace struct {
	limit    int
	overflow bool
	frames   []h2FrameSummary
}

func (t *h2FrameTrace) snapshot() []h2FrameSummary {
	result := make([]h2FrameSummary, len(t.frames))
	for i, frame := range t.frames {
		result[i] = frame.clone()
	}
	return result
}
func h2FrameTypeName(kind uint8) string {
	names := [...]string{"DATA", "HEADERS", "PRIORITY", "RST_STREAM", "SETTINGS", "PUSH_PROMISE", "PING", "GOAWAY", "WINDOW_UPDATE", "CONTINUATION"}
	if int(kind) < len(names) {
		return names[kind]
	}
	return "UNKNOWN"
}

// observeRead sees the bytes consumed by one Framer.ReadFrame call. A decoded
// MetaHeadersFrame can consume several physical HEADERS/CONTINUATION frames.
// Incomplete final bytes remain the read-end evidence; no partial summary is
// invented. Overflow is fatal, not a silently truncated successful recording.
func (t *h2FrameTrace) observeRead(raw []byte) (int, error) {
	completed := 0
	for len(raw) >= 9 {
		length := uint32(raw[0])<<16 | uint32(raw[1])<<8 | uint32(raw[2])
		if uint64(len(raw)) < uint64(length)+9 {
			break
		}
		completed++
		if t.limit <= 0 || len(t.frames) >= t.limit {
			t.overflow = true
			return completed, errH2FrameTraceOverflow
		}
		stream := binary.BigEndian.Uint32(raw[5:9])
		frame := h2FrameSummary{Index: len(t.frames) + 1, Type: raw[3], TypeName: h2FrameTypeName(raw[3]), Flags: raw[4], StreamID: stream & 0x7fffffff, ReservedStreamBit: stream&0x80000000 != 0, Length: length}
		payload := raw[9 : 9+int(length)]
		switch frame.Type {
		case 3:
			if length != 4 {
				return completed, fmt.Errorf("HTTP/2 RST_STREAM summary requires 4 payload bytes")
			}
			code := binary.BigEndian.Uint32(payload)
			frame.ErrorCode = &code
		case 7:
			if length < 8 {
				return completed, fmt.Errorf("HTTP/2 GOAWAY summary requires at least 8 payload bytes")
			}
			last := binary.BigEndian.Uint32(payload[:4]) & 0x7fffffff
			code := binary.BigEndian.Uint32(payload[4:8])
			frame.LastStreamID = &last
			frame.ErrorCode = &code
		case 8:
			if length != 4 {
				return completed, fmt.Errorf("HTTP/2 WINDOW_UPDATE summary requires 4 payload bytes")
			}
			increment := binary.BigEndian.Uint32(payload) & 0x7fffffff
			frame.WindowIncrement = &increment
		}
		t.frames = append(t.frames, frame)
		raw = raw[9+int(length):]
	}
	return completed, nil
}
