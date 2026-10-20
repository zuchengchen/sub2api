package codexwire

import (
	"bytes"
	"compress/flate"
	"io"

	kflate "github.com/klauspost/compress/flate"
)

// pmdTail is the permessage-deflate trailing bytes stripped from each message.
var pmdTail = []byte{0x00, 0x00, 0xff, 0xff}

const pmdWindowSize = 32 * 1024

// pmdCompressor compresses outgoing messages. Context takeover keeps the last
// 32 KiB of plaintext as the preset dictionary for each message — matching
// zlib's persistent-stream semantics. klauspost/flate is used because its
// NewWriterDict emits real backreferences into the preset dictionary (Go's
// stdlib dict only engages once the window slides, ~64KB in).
// Every message is Z_SYNC_FLUSH terminated with the 00 00 ff ff tail stripped,
// matching the permessage-deflate wire format tungstenite emits.
type pmdCompressor struct {
	contextTakeover bool
	level           int
	dict            []byte
}

func newPMDCompressor(contextTakeover bool) *pmdCompressor {
	// klauspost level ≥7 走真 deflate 窗口算法，才会对 preset dict 产生 backref；
	// 低等级是 stateless 快路径，dict 不生效。
	return &pmdCompressor{contextTakeover: contextTakeover, level: 7}
}

// Compress returns the wire bytes for one message (RSV1 payload).
func (c *pmdCompressor) Compress(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	var w *kflate.Writer
	var err error
	if c.contextTakeover && len(c.dict) > 0 {
		w, err = kflate.NewWriterDict(&buf, c.level, c.dict)
	} else {
		w, err = kflate.NewWriter(&buf, c.level)
	}
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(payload); err != nil {
		_ = w.Close()
		return nil, err
	}
	// Z_SYNC_FLUSH emits a non-final stored block ending 00 00 ff ff, which
	// permessage-deflate requires senders to strip. Close 会补一个 final 块进
	// buf，必须先快照再 Close。
	if err := w.Flush(); err != nil {
		_ = w.Close()
		return nil, err
	}
	out := append([]byte(nil), buf.Bytes()...)
	_ = w.Close()
	if len(out) >= len(pmdTail) && bytes.Equal(out[len(out)-len(pmdTail):], pmdTail) {
		out = out[:len(out)-len(pmdTail)]
	}
	if c.contextTakeover {
		c.dict = append(c.dict, payload...)
		if len(c.dict) > pmdWindowSize {
			c.dict = c.dict[len(c.dict)-pmdWindowSize:]
		}
	}
	return out, nil
}

// pmdDecompressor inflates incoming messages, retaining the prior 32 KiB of
// decompressed output as the dictionary when server context takeover is on.
type pmdDecompressor struct {
	contextTakeover bool
	dict            []byte
}

func newPMDDecompressor(contextTakeover bool) *pmdDecompressor {
	return &pmdDecompressor{contextTakeover: contextTakeover}
}

// pmdReadTail 追加在接收到的 PMD 载荷后，让 flate reader 读到合法块边界：
// 00 00 ff ff 补全发送方剥掉的同步块，01 00 00 ff ff 是一个 final 空块。
var pmdReadTail = []byte{0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff}

// Decompress inflates one wire message.
func (d *pmdDecompressor) Decompress(payload []byte) ([]byte, error) {
	src := make([]byte, 0, len(payload)+len(pmdReadTail))
	src = append(src, payload...)
	src = append(src, pmdReadTail...)
	var r io.ReadCloser
	if d.contextTakeover && len(d.dict) > 0 {
		r = flate.NewReaderDict(bytes.NewReader(src), d.dict)
	} else {
		r = flate.NewReader(bytes.NewReader(src))
	}
	out, err := io.ReadAll(r)
	closeErr := r.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if d.contextTakeover {
		d.dict = append(d.dict, out...)
		if len(d.dict) > pmdWindowSize {
			d.dict = d.dict[len(d.dict)-pmdWindowSize:]
		}
	}
	return out, nil
}
