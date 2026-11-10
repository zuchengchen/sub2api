//go:build linux

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"unicode/utf8"
)

const maxWSMessageBytes = 64 << 20
const maxWSFrameEvidence = 8192

// Raw frame observations are supplemental transport evidence, not Responses
// messages. Keep exact control payload bytes; data payloads need only a digest
// because the assembled request is already retained in the raw JSONL.
type wsFrameEvidence struct {
	Sequence       int    `json:"sequence"`
	Direction      string `json:"direction"`
	FIN            bool   `json:"fin"`
	RSV            byte   `json:"rsv"`
	Opcode         byte   `json:"opcode"`
	Masked         bool   `json:"masked"`
	PayloadLength  int    `json:"payload_length"`
	WireLength     int    `json:"wire_length"`
	PayloadSHA256  string `json:"payload_sha256"`
	ControlPayload []byte `json:"control_payload_base64,omitempty"`
	WriteComplete  *bool  `json:"write_complete,omitempty"`
	WriteError     string `json:"write_error,omitempty"`
}

type wsConnectionEvidence struct {
	Contract         string            `json:"contract"`
	Frames           []wsFrameEvidence `json:"frames"`
	End              string            `json:"end"`
	Error            string            `json:"error,omitempty"`
	PendingFragments int               `json:"pending_fragments"`
	Truncated        bool              `json:"truncated"`
}

func (f *fakeUpstream) startWSCapture(connID int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wsCaptures == nil {
		f.wsCaptures = map[int]*wsConnectionEvidence{}
	}
	f.wsCaptures[connID] = &wsConnectionEvidence{Contract: "ws_frames_v1", Frames: []wsFrameEvidence{}}
}

func (f *fakeUpstream) observeWSFrame(connID int, frame wsFrameEvidence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	capture := f.wsCaptures[connID]
	if capture == nil {
		return fmt.Errorf("WebSocket frame has no connection capture")
	}
	if len(capture.Frames) >= maxWSFrameEvidence {
		err := fmt.Errorf("WebSocket frame evidence limit exceeded on connection %d", connID)
		if !capture.Truncated {
			f.captureErrors = append(f.captureErrors, err)
			capture.Truncated = true
		}
		return err
	}
	frame.Sequence = len(capture.Frames) + 1
	frame.ControlPayload = append([]byte(nil), frame.ControlPayload...)
	capture.Frames = append(capture.Frames, frame)
	return nil
}

func (f *fakeUpstream) finishWSCapture(connID int, end string, err error, pending int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	capture := f.wsCaptures[connID]
	if capture == nil {
		return
	}
	capture.End = end
	capture.PendingFragments = pending
	if err != nil {
		capture.Error = err.Error()
	}
}

func (f *fakeUpstream) wsConnections() map[int]*wsConnectionEvidence {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]*wsConnectionEvidence{}
	for id, capture := range f.wsCaptures {
		copy := *capture
		copy.Frames = append([]wsFrameEvidence(nil), capture.Frames...)
		for i := range copy.Frames {
			copy.Frames[i].ControlPayload = append([]byte(nil), copy.Frames[i].ControlPayload...)
			if complete := copy.Frames[i].WriteComplete; complete != nil {
				value := *complete
				copy.Frames[i].WriteComplete = &value
			}
		}
		out[id] = &copy
	}
	return out
}

func (f *fakeUpstream) writeWSControl(c net.Conn, connID int, opcode byte, payload []byte) error {
	err := writeServerFrame(c, opcode, payload)
	complete := err == nil
	frame := wsFrameEvidence{Direction: "server_to_client", FIN: true, Opcode: opcode, PayloadLength: len(payload), WireLength: 2 + len(payload), PayloadSHA256: fmt.Sprintf("%x", sha256.Sum256(payload)), ControlPayload: payload, WriteComplete: &complete}
	if err != nil {
		frame.WriteError = err.Error()
	}
	evidenceErr := f.observeWSFrame(connID, frame)
	if err != nil {
		return err
	}
	return evidenceErr
}

type wsProtocolError struct{ reason string }

func (e *wsProtocolError) Error() string { return "invalid WebSocket frame: " + e.reason }

// A reader belongs to one connection. RFC 6455 section 5.4 permits controls
// between data fragments; returning a control must preserve the pending data.
// RFC 7692 section 6.1 allows RSV1 only on the first compressed data fragment.
type wsClientMessageReader struct {
	br      *bufio.Reader
	deflate bool
	onFrame func(wsFrameEvidence) error
	payload []byte
	info    frameInfo
	closed  bool
}

func (r *wsClientMessageReader) Read() ([]byte, *frameInfo, error) {
	if r.closed {
		return nil, nil, io.EOF
	}
	for {
		payload, frame, err := readWSClientFrame(r.br)
		if err != nil {
			if err == io.EOF && r.info.Fragments != 0 {
				err = io.ErrUnexpectedEOF
			}
			return nil, nil, err
		}
		if r.onFrame != nil {
			if err := r.onFrame(frame); err != nil {
				return nil, nil, err
			}
		}
		invalid := func(reason string) ([]byte, *frameInfo, error) { return nil, nil, &wsProtocolError{reason} }
		if !frame.Masked {
			return invalid("client frame is not masked")
		}
		if frame.RSV&0x30 != 0 {
			return invalid("unsupported RSV2/RSV3 bits")
		}
		if frame.Opcode >= 8 {
			if !frame.FIN || frame.PayloadLength > 125 || frame.RSV != 0 {
				return invalid("fragmented, oversized or compressed control frame")
			}
			switch frame.Opcode {
			case 8:
				if err := validateWSClosePayload(payload); err != nil {
					return invalid(err.Error())
				}
				r.closed = true
			case 9, 10:
			default:
				return invalid("reserved control opcode")
			}
			return payload, &frameInfo{Opcode: int(frame.Opcode), Fragments: 1, Masked: true, WireLen: frame.WireLength}, nil
		}
		switch frame.Opcode {
		case 0:
			if r.info.Fragments == 0 {
				return invalid("continuation without a pending data message")
			}
			if frame.RSV != 0 {
				return invalid("continuation sets RSV1")
			}
		case 1, 2:
			if r.info.Fragments != 0 {
				return invalid("new data message before final continuation")
			}
			if frame.RSV&0x40 != 0 && !r.deflate {
				return invalid("compression was not negotiated")
			}
			r.info = frameInfo{Opcode: int(frame.Opcode), Masked: true, Compressed: frame.RSV&0x40 != 0}
		default:
			return invalid("reserved data opcode")
		}
		if len(r.payload)+len(payload) > maxWSMessageBytes {
			return invalid("assembled message exceeds size limit")
		}
		r.info.Fragments++
		r.info.WireLen += frame.WireLength
		r.payload = append(r.payload, payload...)
		if frame.FIN {
			if r.info.Opcode == 1 && !r.info.Compressed && !utf8.Valid(r.payload) {
				return invalid("text message is not UTF-8")
			}
			message, info := r.payload, r.info
			r.payload = nil
			r.info = frameInfo{}
			return message, &info, nil
		}
	}
}

func readWSClientFrame(br *bufio.Reader) ([]byte, wsFrameEvidence, error) {
	frame := wsFrameEvidence{Direction: "client_to_server"}
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, frame, err
	}
	frame.FIN = hdr[0]&0x80 != 0
	frame.RSV = hdr[0] & 0x70
	frame.Opcode = hdr[0] & 0x0f
	frame.Masked = hdr[1]&0x80 != 0
	truncated := func(err error) error {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	n := uint64(hdr[1] & 0x7f)
	wire := 2
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, frame, truncated(err)
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
		wire += 2
		if n < 126 {
			return nil, frame, &wsProtocolError{"nonminimal 16-bit length"}
		}
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, frame, truncated(err)
		}
		n = binary.BigEndian.Uint64(ext[:])
		wire += 8
		if n <= 65535 || n>>63 != 0 {
			return nil, frame, &wsProtocolError{"invalid 64-bit length"}
		}
	}
	if n > maxWSMessageBytes {
		return nil, frame, &wsProtocolError{"frame exceeds size limit"}
	}
	var mask [4]byte
	if frame.Masked {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return nil, frame, truncated(err)
		}
		wire += 4
	}
	payload := make([]byte, int(n))
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, frame, truncated(err)
	}
	if frame.Masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	frame.PayloadLength = len(payload)
	frame.WireLength = wire + len(payload)
	frame.PayloadSHA256 = fmt.Sprintf("%x", sha256.Sum256(payload))
	if frame.Opcode >= 8 {
		frame.ControlPayload = payload
	}
	return payload, frame, nil
}

func validateWSClosePayload(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) == 1 {
		return fmt.Errorf("one-byte close payload")
	}
	code := binary.BigEndian.Uint16(payload[:2])
	valid := code >= 3000 && code <= 4999
	switch code {
	case 1000, 1001, 1002, 1003, 1007, 1008, 1009, 1010, 1011, 1012, 1013, 1014:
		valid = true
	}
	if !valid {
		return fmt.Errorf("invalid close code %d", code)
	}
	if !utf8.Valid(payload[2:]) {
		return fmt.Errorf("close reason is not UTF-8")
	}
	return nil
}
