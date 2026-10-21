package codexwire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed is returned on operations against a closed connection.
var ErrClosed = errors.New("codexwire: connection closed")

// ErrClosedByPeer reports a clean close frame received from the server.
type ErrClosedByPeer struct {
	Code   uint16
	Reason string
}

func (e *ErrClosedByPeer) Error() string {
	return fmt.Sprintf("codexwire: peer closed with code %d: %s", e.Code, e.Reason)
}

// WSConn is a byte-exact client WebSocket connection: raw frames over an
// established stream (TLS or plain), with permessage-deflate support.
// A single goroutine should own reads; writes are mutex-guarded.
type WSConn struct {
	conn     net.Conn
	br       *bufio.Reader
	writeMu  sync.Mutex
	compress *pmdCompressor
	inflate  *pmdDecompressor
	closed   atomic.Bool

	upstreamPings atomic.Int64

	// readStop/writeStop disarm the ctx watcher per direction
	// (context.AfterFunc stop funcs).
	readStop  atomic.Pointer[func() bool]
	writeStop atomic.Pointer[func() bool]

	// readLimit caps a single assembled message.
	readLimit int64
}

// NewWSConn wraps an established stream after a successful 101 upgrade.
// decompTakeover/compTakeover mirror the negotiated context-takeover flags.
func NewWSConn(conn net.Conn, br *bufio.Reader, negotiated *UpgradeResult) *WSConn {
	if br == nil {
		br = bufio.NewReader(conn)
	}
	c := &WSConn{conn: conn, br: br, readLimit: 16 << 20}
	if negotiated != nil && negotiated.PerMessageDeflate {
		c.compress = newPMDCompressor(!negotiated.ClientNoContextTakeover, negotiated.ClientMaxWindowBits)
		c.inflate = newPMDDecompressor(!negotiated.ServerNoContextTakeover, negotiated.ServerMaxWindowBits)
	}
	return c
}

// UpstreamPingCount reports how many ping frames the peer sent.
func (c *WSConn) UpstreamPingCount() int64 { return c.upstreamPings.Load() }

// SetReadLimit bounds one assembled message.
func (c *WSConn) SetReadLimit(n int64) {
	if n > 0 {
		c.readLimit = n
	}
}

// WriteJSON marshals value unless it is already raw bytes
// (json.RawMessage/[]byte/string), which are written verbatim so ordered
// serializers keep their exact key order.
func (c *WSConn) WriteJSON(ctx context.Context, value any) error {
	var payload []byte
	switch v := value.(type) {
	case nil:
		payload = []byte("null")
	case json.RawMessage:
		payload = v
	case []byte:
		payload = v
	case string:
		payload = []byte(v)
	default:
		b, err := json.Marshal(value)
		if err != nil {
			return err
		}
		payload = b
	}
	return c.WriteMessage(ctx, payload)
}

// WriteMessage sends payload as one text message (compressed when negotiated).
func (c *WSConn) WriteMessage(ctx context.Context, payload []byte) error {
	return c.write(ctx, opcodeText, payload)
}

// WriteMessageBinary sends payload as one binary message.
func (c *WSConn) WriteMessageBinary(ctx context.Context, payload []byte) error {
	return c.write(ctx, opcodeBinary, payload)
}

func (c *WSConn) write(ctx context.Context, opcode byte, payload []byte) error {
	if c.closed.Load() {
		return ErrClosed
	}
	rsv1 := false
	if c.compress != nil && (opcode == opcodeText || opcode == opcodeBinary) {
		compressed, err := c.compress.Compress(payload)
		if err != nil {
			return err
		}
		payload = compressed
		rsv1 = true
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.withDeadline(ctx, true); err != nil {
		return err
	}
	defer c.settleDeadline(true)
	return writeFrame(c.conn, true, rsv1, opcode, payload)
}

// Ping sends a ping frame; the read loop answers the pong automatically.
func (c *WSConn) Ping(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.withDeadline(ctx, true); err != nil {
		return err
	}
	defer c.settleDeadline(true)
	return writeFrame(c.conn, true, false, opcodePing, nil)
}

// ReadMessage assembles and returns the next data message (text or binary),
// transparently handling control frames and decompression.
func (c *WSConn) ReadMessage(ctx context.Context) ([]byte, error) {
	opcode, payload, err := c.readMessage(ctx)
	if err != nil {
		return nil, err
	}
	_ = opcode
	return payload, nil
}

// ReadFrame returns the next data message with its opcode mapped to
// 0=binary/1=text for callers that need the type.
func (c *WSConn) ReadFrame(ctx context.Context) (byte, []byte, error) {
	return c.readMessage(ctx)
}

func (c *WSConn) readMessage(ctx context.Context) (byte, []byte, error) {
	if err := c.withDeadline(ctx, false); err != nil {
		return 0, nil, err
	}
	defer c.settleDeadline(false)

	var message []byte
	var messageOpcode byte
	var compressed bool
	for {
		h, err := readFrameHeader(c.br)
		if err != nil {
			return 0, nil, err
		}
		switch h.opcode {
		case opcodePing:
			c.upstreamPings.Add(1)
			payload, err := c.readPayload(h, maxControlPayload)
			if err != nil {
				return 0, nil, err
			}
			if err := c.writeControl(opcodePong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case opcodePong:
			if _, err := c.readPayload(h, maxControlPayload); err != nil {
				return 0, nil, err
			}
			continue
		case opcodeClose:
			payload, err := c.readPayload(h, maxControlPayload)
			if err != nil {
				return 0, nil, err
			}
			code := uint16(1005)
			reason := ""
			if len(payload) >= 2 {
				code = uint16(payload[0])<<8 | uint16(payload[1])
				reason = string(payload[2:])
			}
			// Echo close then mark closed.
			_ = c.writeControl(opcodeClose, payload)
			c.closed.Store(true)
			return 0, nil, &ErrClosedByPeer{Code: code, Reason: reason}
		case opcodeText, opcodeBinary, opcodeContinuation:
		default:
			return 0, nil, fmt.Errorf("codexwire: unknown opcode %d", h.opcode)
		}
		if h.opcode != opcodeContinuation {
			if len(message) != 0 {
				return 0, nil, errors.New("codexwire: data frame inside unfinished message")
			}
			messageOpcode = h.opcode
			compressed = h.rsv1
		}
		payload, err := c.readPayload(h, c.readLimit-int64(len(message)))
		if err != nil {
			return 0, nil, err
		}
		message = append(message, payload...)
		if int64(len(message)) > c.readLimit {
			return 0, nil, errors.New("codexwire: message too large")
		}
		if !h.fin {
			continue
		}
		if compressed {
			if c.inflate == nil {
				return 0, nil, errors.New("codexwire: compressed frame without negotiated extension")
			}
			message, err = c.inflate.Decompress(message)
			if err != nil {
				return 0, nil, fmt.Errorf("codexwire: inflate: %w", err)
			}
		}
		return messageOpcode, message, nil
	}
}

// readPayload reads h.length bytes applying the mask when present.
func (c *WSConn) readPayload(h frameHeader, limit int64) ([]byte, error) {
	if limit >= 0 && int64(h.length) > limit {
		return nil, errors.New("codexwire: frame exceeds read limit")
	}
	payload := make([]byte, h.length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return nil, err
	}
	if h.masked {
		for i := range payload {
			payload[i] ^= h.maskKey[i%4]
		}
	}
	return payload, nil
}

func (c *WSConn) writeControl(opcode byte, payload []byte) error {
	if len(payload) > maxControlPayload {
		payload = payload[:maxControlPayload]
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	return writeFrame(c.conn, true, false, opcode, payload)
}

// withDeadline applies ctx cancellation via the underlying deadline; contexts
// without a deadline get a watcher that forces the deadline on cancel.
func (c *WSConn) withDeadline(ctx context.Context, write bool) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if write {
			return c.conn.SetWriteDeadline(deadline)
		}
		return c.conn.SetReadDeadline(deadline)
	}
	done := ctx.Done()
	if done == nil {
		return nil
	}
	stop := context.AfterFunc(ctx, func() {
		if write {
			_ = c.conn.SetWriteDeadline(time.Now())
		} else {
			_ = c.conn.SetReadDeadline(time.Now())
		}
	})
	if write {
		c.writeStop.Store(&stop)
	} else {
		c.readStop.Store(&stop)
	}
	return nil
}

// settleDeadline clears any forced deadline and disarms the ctx watcher.
func (c *WSConn) settleDeadline(write bool) {
	if write {
		if stop := c.writeStop.Swap(nil); stop != nil {
			(*stop)()
		}
		_ = c.conn.SetWriteDeadline(time.Time{})
		return
	}
	if stop := c.readStop.Swap(nil); stop != nil {
		(*stop)()
	}
	_ = c.conn.SetReadDeadline(time.Time{})
}

// Close sends a close frame and closes the transport.
func (c *WSConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.writeMu.Lock()
	_ = writeFrame(c.conn, true, false, opcodeClose, nil)
	c.writeMu.Unlock()
	return c.conn.Close()
}

// CloseNow drops the transport without a close handshake.
func (c *WSConn) CloseNow() error {
	c.closed.Store(true)
	return c.conn.Close()
}
