package codexwire

import (
	"bytes"
	"fmt"

	"golang.org/x/net/http2/hpack"
)

// h2-crate (hpack/src/encoder.rs + table.rs) faithful HPACK encoder.
//
// Official Codex HTTP requests are written by hyper + h2, whose encoder:
//   - emits a fully-indexed field when (name,value) matches the static or
//     dynamic table;
//   - emits literal-without-indexing (never stored) for headers in
//     skip_value_index: authorization, cookie, content-length, etag, age,
//     if-modified-since, if-none-match, location, set-cookie;
//   - emits literal-without-indexing when (len(name)+len(value)+32)*4 exceeds
//     max_table_size*3 (entry would be >75% of the table);
//   - otherwise emits literal-with-incremental-indexing and inserts the field
//     into the dynamic table;
//   - pseudo headers are never inserted into the dynamic table;
//   - encodes every string Huffman-coded when the Huffman length fits the
//     7-bit one-byte prefix (huff_len <= 126), raw otherwise
//     (hpack/src/huffman/mod.rs encode_str).

// rfc7541StaticTable is the RFC 7541 Appendix A static table (index 1-based).
var rfc7541StaticTable = [][2]string{
	{":authority", ""},
	{":method", "GET"},
	{":method", "POST"},
	{":path", "/"},
	{":path", "/index.html"},
	{":scheme", "http"},
	{":scheme", "https"},
	{":status", "200"},
	{":status", "204"},
	{":status", "206"},
	{":status", "304"},
	{":status", "400"},
	{":status", "404"},
	{":status", "500"},
	{"accept-charset", ""},
	{"accept-encoding", "gzip, deflate"},
	{"accept-language", ""},
	{"accept-ranges", ""},
	{"accept", ""},
	{"access-control-allow-origin", ""},
	{"age", ""},
	{"allow", ""},
	{"authorization", ""},
	{"cache-control", ""},
	{"content-disposition", ""},
	{"content-encoding", ""},
	{"content-language", ""},
	{"content-length", ""},
	{"content-location", ""},
	{"content-range", ""},
	{"content-type", ""},
	{"cookie", ""},
	{"date", ""},
	{"etag", ""},
	{"expect", ""},
	{"expires", ""},
	{"from", ""},
	{"host", ""},
	{"if-match", ""},
	{"if-modified-since", ""},
	{"if-none-match", ""},
	{"if-range", ""},
	{"if-unmodified-since", ""},
	{"last-modified", ""},
	{"link", ""},
	{"location", ""},
	{"max-forwards", ""},
	{"proxy-authenticate", ""},
	{"proxy-authorization", ""},
	{"range", ""},
	{"referer", ""},
	{"refresh", ""},
	{"retry-after", ""},
	{"server", ""},
	{"set-cookie", ""},
	{"strict-transport-security", ""},
	{"transfer-encoding", ""},
	{"user-agent", ""},
	{"vary", ""},
	{"via", ""},
	{"www-authenticate", ""},
}

// hpackSkipValueIndex mirrors h2's table::Index::skip_value_index — headers whose
// values are never indexed into the dynamic table.
var hpackSkipValueIndex = map[string]bool{
	"authorization":     true,
	"cookie":            true,
	"content-length":    true,
	"etag":              true,
	"age":               true,
	"if-modified-since": true,
	"if-none-match":     true,
	"location":          true,
	"set-cookie":        true,
}

const hpackEntryOverhead = 32

// hpackEncoder keeps the dynamic table state so per-connection header blocks
// track insertions and evictions like the h2 encoder does.
type hpackEncoder struct {
	dyn     []hpackDynEntry // newest first
	dynSize int
	maxSize int // dynamic table capacity advertised to us (default 4096)
}

type hpackDynEntry struct {
	name, value string
}

func newHpackEncoder() *hpackEncoder {
	return &hpackEncoder{maxSize: 4096}
}

// findFull returns the absolute index of an exact (name,value) match, checking
// the static table then the dynamic table (newest-first like RFC indexing).
func (e *hpackEncoder) findFull(name, value string) int {
	for i, ent := range rfc7541StaticTable {
		if ent[0] == name && ent[1] == value {
			return i + 1
		}
	}
	for i, ent := range e.dyn {
		if ent.name == name && ent.value == value {
			return len(rfc7541StaticTable) + i + 1
		}
	}
	return 0
}

// findName returns the absolute index of a name-only match.
func (e *hpackEncoder) findName(name string) int {
	for i, ent := range rfc7541StaticTable {
		if ent[0] == name {
			return i + 1
		}
	}
	for i, ent := range e.dyn {
		if ent.name == name {
			return len(rfc7541StaticTable) + i + 1
		}
	}
	return 0
}

func (e *hpackEncoder) insert(name, value string) {
	sz := len(name) + len(value) + hpackEntryOverhead
	if sz > e.maxSize {
		e.dyn = nil
		e.dynSize = 0
		return
	}
	for e.dynSize+sz > e.maxSize && len(e.dyn) > 0 {
		last := e.dyn[len(e.dyn)-1]
		e.dynSize -= len(last.name) + len(last.value) + hpackEntryOverhead
		e.dyn = e.dyn[:len(e.dyn)-1]
	}
	e.dyn = append([]hpackDynEntry{{name, value}}, e.dyn...)
	e.dynSize += sz
}

func appendHpackInt(dst []byte, prefixBits uint8, prefixFlag byte, val uint64) []byte {
	max := uint64(1<<prefixBits) - 1
	if val < max {
		return append(dst, prefixFlag|byte(val))
	}
	dst = append(dst, prefixFlag|byte(max))
	val -= max
	for val >= 128 {
		dst = append(dst, byte(val%128+128))
		val /= 128
	}
	return append(dst, byte(val))
}

// appendHpackString encodes a string like h2's huffman encode_str: Huffman-code
// whenever the encoded length fits in a single 7-bit-prefixed byte (<=126),
// otherwise fall back to the raw octets with a full integer length prefix.
func appendHpackString(dst []byte, s string) []byte {
	huffLen := hpack.HuffmanEncodeLength(s)
	if huffLen <= 126 {
		dst = append(dst, 0x80|byte(huffLen))
		return hpack.AppendHuffmanString(dst, s)
	}
	dst = appendHpackInt(dst, 7, 0x00, uint64(len(s)))
	return append(dst, s...)
}

// encodeField appends one header field using h2's index decision.
func (e *hpackEncoder) encodeField(dst []byte, name, value string, pseudo bool) []byte {
	if idx := e.findFull(name, value); idx != 0 {
		return appendHpackInt(dst, 7, 0x80, uint64(idx))
	}
	skip := pseudo || hpackSkipValueIndex[name]
	if !skip && (len(name)+len(value)+hpackEntryOverhead)*4 <= e.maxSize*3 {
		// literal with incremental indexing
		dst = e.encodeLiteral(dst, 0x40, 6, name, value)
		e.insert(name, value)
		return dst
	}
	// literal without indexing
	return e.encodeLiteral(dst, 0x00, 4, name, value)
}

func (e *hpackEncoder) encodeLiteral(dst []byte, flag byte, prefix uint8, name, value string) []byte {
	if idx := e.findName(name); idx != 0 {
		dst = appendHpackInt(dst, prefix, flag, uint64(idx))
	} else {
		dst = append(dst, flag)
		dst = appendHpackString(dst, name)
	}
	return appendHpackString(dst, value)
}

// EncodeBlock builds a complete header block: pseudo headers first in h2 order
// (:method :scheme :authority :path), then the ordered application fields.
func (e *hpackEncoder) EncodeBlock(method, scheme, authority, path string, fields []HeaderKV) ([]byte, error) {
	dst := make([]byte, 0, 256)
	dst = e.encodeField(dst, ":method", method, true)
	dst = e.encodeField(dst, ":scheme", scheme, true)
	dst = e.encodeField(dst, ":authority", authority, true)
	dst = e.encodeField(dst, ":path", path, true)
	for _, kv := range fields {
		name := kv.Name
		// HPACK requires lowercase; codex template already emits lowercase.
		dst = e.encodeField(dst, name, kv.Value, false)
	}
	return dst, nil
}

var errHpackDecode = fmt.Errorf("hpack decode failed")

// decodeBlock decodes a response header block into http.Header-style pairs.
func decodeHpackBlock(dec *hpackDecoderState, block []byte) ([]HeaderKV, error) {
	var out []HeaderKV
	dec.decoder.SetEmitFunc(func(f hpack.HeaderField) {
		out = append(out, HeaderKV{Name: f.Name, Value: f.Value})
	})
	if _, err := dec.decoder.Write(block); err != nil {
		return nil, fmt.Errorf("%w: %v", errHpackDecode, err)
	}
	if err := dec.decoder.Close(); err != nil {
		return nil, fmt.Errorf("%w: %v", errHpackDecode, err)
	}
	return out, nil
}

// hpackDecoderState wraps hpack.Decoder per connection.
type hpackDecoderState struct {
	decoder *hpack.Decoder
}

func newHpackDecoder() *hpackDecoderState {
	return &hpackDecoderState{decoder: hpack.NewDecoder(4096, nil)}
}

var _ = bytes.MinRead // keep bytes import if needed elsewhere
