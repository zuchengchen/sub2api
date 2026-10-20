package codexwire

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"

	"io"
)

// WebSocket opcodes.
const (
	opcodeContinuation byte = 0x0
	opcodeText         byte = 0x1
	opcodeBinary       byte = 0x2
	opcodeClose        byte = 0x8
	opcodePing         byte = 0x9
	opcodePong         byte = 0xA
)

const maxControlPayload = 125

// frameHeader holds a parsed frame header.
type frameHeader struct {
	fin     bool
	rsv1    bool
	opcode  byte
	masked  bool
	length  uint64
	maskKey [4]byte
}

// readFrameHeader parses one frame header from br.
func readFrameHeader(br *bufio.Reader) (frameHeader, error) {
	var h frameHeader
	b0, err := br.ReadByte()
	if err != nil {
		return h, err
	}
	b1, err := br.ReadByte()
	if err != nil {
		return h, err
	}
	h.fin = b0&0x80 != 0
	h.rsv1 = b0&0x40 != 0
	h.opcode = b0 & 0x0f
	h.masked = b1&0x80 != 0
	lengthCode := b1 & 0x7f
	switch lengthCode {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return h, err
		}
		h.length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return h, err
		}
		h.length = binary.BigEndian.Uint64(ext[:])
		if h.length>>63 != 0 {
			return h, errors.New("ws frame length exceeds int63")
		}
	default:
		h.length = uint64(lengthCode)
	}
	if h.masked {
		if _, err := io.ReadFull(br, h.maskKey[:]); err != nil {
			return h, err
		}
	}
	return h, nil
}

// writeFrame encodes one client frame: always masked (RFC 6455 client rule),
// single frame per call.
func writeFrame(w io.Writer, fin bool, rsv1 bool, opcode byte, payload []byte) error {
	var head [14]byte
	b0 := opcode & 0x0f
	if fin {
		b0 |= 0x80
	}
	if rsv1 {
		b0 |= 0x40
	}
	head[0] = b0
	n := 2
	maskBit := byte(0x80)
	switch l := uint64(len(payload)); {
	case l < 126:
		head[1] = maskBit | byte(l)
	case l <= 0xffff:
		head[1] = maskBit | 126
		binary.BigEndian.PutUint16(head[2:4], uint16(l))
		n = 4
	default:
		head[1] = maskBit | 127
		binary.BigEndian.PutUint64(head[2:10], l)
		n = 10
	}
	var maskKey [4]byte
	if _, err := rand.Read(maskKey[:]); err != nil {
		return err
	}
	copy(head[n:n+4], maskKey[:])
	n += 4
	if _, err := w.Write(head[:n]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	// Mask on the fly without mutating the caller's payload.
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}
	_, err := w.Write(masked)
	return err
}
