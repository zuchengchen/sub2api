package codexwire

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"sync"

	kflate "github.com/klauspost/compress/flate"
)

// pmdTail is the permessage-deflate trailing bytes stripped from each message.
var pmdTail = []byte{0x00, 0x00, 0xff, 0xff}

// pmdDefaultWindowSize is the LZ77 window for the default 15-bit negotiation.
const pmdDefaultWindowSize = 32 * 1024

// pmdCompressLevel is the klauspost level used for outbound messages.
// klauspost level ≥7 走真 deflate 窗口算法，才会对 preset dict 产生 backref；
// 低等级是 stateless 快路径，dict 不生效。
const pmdCompressLevel = 7

// pmdWriterPool reuses klauspost writers across messages and connections. A
// level-7 writer carries ~1MB of internal state; allocating one per message
// (the previous behavior) dominated per-message garbage, and keeping one per
// connection would pin that state on every idle socket. ResetDict fully
// reinitializes the writer, so pooled writers never leak state between
// messages or connections.
var pmdWriterPool = sync.Pool{
	New: func() any {
		w, err := kflate.NewWriter(io.Discard, pmdCompressLevel)
		if err != nil {
			panic(err) // level is a compile-time constant in range
		}
		return w
	},
}

// errPMDPoolType reports a pool entry of an unexpected type. The pools only
// ever hold values built by their own New funcs, so this is unreachable.
var errPMDPoolType = errors.New("codexwire: unexpected permessage-deflate pool entry type")

// pmdReaderPool reuses stdlib inflaters; Reset re-arms them with a new
// source and preset dictionary.
var pmdReaderPool = sync.Pool{
	New: func() any { return flate.NewReader(bytes.NewReader(nil)) },
}

// pmdWindow keeps the trailing window bytes of a message stream in a buffer
// whose capacity never exceeds the window size. Reslicing the tail of an
// appended buffer (the previous implementation) kept the whole backing array
// of the largest message alive for the connection's lifetime — a multi-MB
// Codex request pinned multi-MB of heap on every idle socket.
type pmdWindow struct {
	size int
	buf  []byte
}

func (w *pmdWindow) bytes() []byte { return w.buf }

func (w *pmdWindow) push(p []byte) {
	if w.size <= 0 || len(p) == 0 {
		return
	}
	if w.buf == nil {
		w.buf = make([]byte, 0, w.size)
	}
	if len(p) >= w.size {
		w.buf = append(w.buf[:0], p[len(p)-w.size:]...)
		return
	}
	if over := len(w.buf) + len(p) - w.size; over > 0 {
		n := copy(w.buf, w.buf[over:])
		w.buf = w.buf[:n]
	}
	w.buf = append(w.buf, p...)
}

func pmdWindowSizeForBits(windowBits int) int {
	if windowBits >= 8 && windowBits <= 15 {
		return 1 << windowBits
	}
	return pmdDefaultWindowSize
}

// pmdCompressor compresses outgoing messages. Context takeover keeps the last
// window bytes of plaintext as the preset dictionary for each message —
// matching zlib's persistent-stream semantics. klauspost/flate is used
// because its dictionary path emits real backreferences into the preset
// dictionary (Go's stdlib dict only engages once the window slides, ~64KB
// in). Every message is Z_SYNC_FLUSH terminated with the 00 00 ff ff tail
// stripped, matching the permessage-deflate wire format tungstenite emits.
type pmdCompressor struct {
	contextTakeover bool
	window          pmdWindow
}

// newPMDCompressor builds the outbound compressor. windowBits is the
// negotiated client_max_window_bits (0 = RFC 7692 default 15).
func newPMDCompressor(contextTakeover bool, windowBits int) *pmdCompressor {
	return &pmdCompressor{
		contextTakeover: contextTakeover,
		window:          pmdWindow{size: pmdWindowSizeForBits(windowBits)},
	}
}

// Compress returns the wire bytes for one message (RSV1 payload).
func (c *pmdCompressor) Compress(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, ok := pmdWriterPool.Get().(*kflate.Writer)
	if !ok {
		return nil, errPMDPoolType
	}
	defer func() {
		// Drop references to this message before pooling.
		w.ResetDict(io.Discard, nil)
		pmdWriterPool.Put(w)
	}()
	var dict []byte
	if c.contextTakeover {
		dict = c.window.bytes()
	}
	w.ResetDict(&buf, dict)
	if _, err := w.Write(payload); err != nil {
		return nil, err
	}
	// Z_SYNC_FLUSH emits a non-final stored block ending 00 00 ff ff, which
	// permessage-deflate requires senders to strip. No Close: Close would
	// append a final block the receiver must not see.
	if err := w.Flush(); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if bytes.HasSuffix(out, pmdTail) {
		out = out[:len(out)-len(pmdTail)]
	}
	if c.contextTakeover {
		c.window.push(payload)
	}
	return out, nil
}

// pmdDecompressor inflates incoming messages, retaining the prior window
// bytes of decompressed output as the dictionary when server context
// takeover is on.
type pmdDecompressor struct {
	contextTakeover bool
	window          pmdWindow
}

// newPMDDecompressor builds the inbound decompressor. windowBits is the
// negotiated server_max_window_bits (0 = default 15).
func newPMDDecompressor(contextTakeover bool, windowBits int) *pmdDecompressor {
	return &pmdDecompressor{
		contextTakeover: contextTakeover,
		window:          pmdWindow{size: pmdWindowSizeForBits(windowBits)},
	}
}

// pmdReadTail 追加在接收到的 PMD 载荷后，让 flate reader 读到合法块边界：
// 00 00 ff ff 补全发送方剥掉的同步块，01 00 00 ff ff 是一个 final 空块。
var pmdReadTail = []byte{0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff}

// Decompress inflates one wire message.
func (d *pmdDecompressor) Decompress(payload []byte) ([]byte, error) {
	src := make([]byte, 0, len(payload)+len(pmdReadTail))
	src = append(src, payload...)
	src = append(src, pmdReadTail...)
	r, ok := pmdReaderPool.Get().(io.ReadCloser)
	if !ok {
		return nil, errPMDPoolType
	}
	resetter, ok := r.(flate.Resetter)
	if !ok {
		return nil, errPMDPoolType
	}
	defer func() {
		_ = resetter.Reset(bytes.NewReader(nil), nil)
		pmdReaderPool.Put(r)
	}()
	var dict []byte
	if d.contextTakeover {
		dict = d.window.bytes()
	}
	if err := resetter.Reset(bytes.NewReader(src), dict); err != nil {
		return nil, err
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if d.contextTakeover {
		d.window.push(out)
	}
	return out, nil
}
