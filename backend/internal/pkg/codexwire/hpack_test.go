package codexwire

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/http2/hpack"
)

// encodeBlock is the shared helper producing one header block.
func encodeBlock(t *testing.T, enc *hpackEncoder, method, path string, fields []HeaderKV) []byte {
	t.Helper()
	if enc == nil {
		enc = newHpackEncoder()
	}
	block, err := enc.EncodeBlock(method, "https", "chatgpt.com", path, fields)
	if err != nil {
		t.Fatalf("EncodeBlock: %v", err)
	}
	return block
}

// decodeAll decodes a header block with a fresh decoder (static table only for
// assertions that don't cross-check dynamic insertions).
func decodeAll(t *testing.T, block []byte) []hpack.HeaderField {
	t.Helper()
	var out []hpack.HeaderField
	dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) { out = append(out, f) })
	if _, err := dec.Write(block); err != nil {
		t.Fatalf("hpack decode: %v", err)
	}
	if err := dec.Close(); err != nil {
		t.Fatalf("hpack close: %v", err)
	}
	return out
}

func TestHpackPseudoHeaderIndexedAndOrdered(t *testing.T) {
	block := encodeBlock(t, nil, "POST", "/backend-api/codex/responses", nil)
	fields := decodeAll(t, block)
	if len(fields) != 4 {
		t.Fatalf("fields = %d, want 4 pseudo headers", len(fields))
	}
	want := [][2]string{
		{":method", "POST"}, {":scheme", "https"},
		{":authority", "chatgpt.com"}, {":path", "/backend-api/codex/responses"},
	}
	for i, w := range want {
		if fields[i].Name != w[0] || fields[i].Value != w[1] {
			t.Fatalf("field[%d] = %v, want %v", i, fields[i], w)
		}
	}
	// :method POST and :scheme https are static-table fully-indexed → 0x83, 0x87.
	if block[0] != 0x83 || block[1] != 0x87 {
		t.Fatalf("pseudo indexed bytes = %02x %02x, want 83 87", block[0], block[1])
	}
	// :path /path is literal-not-indexed with name index 4 → 0x04 prefix.
	if block[2]&0x80 != 0 {
		t.Fatalf(":authority should be literal, got indexed byte %02x", block[2])
	}
}

func TestHpackSkipValueIndexNotIndexed(t *testing.T) {
	enc := newHpackEncoder()
	block := encodeBlock(t, enc, "POST", "/r", []HeaderKV{
		{Name: "authorization", Value: "Bearer secret"},
		{Name: "content-length", Value: "42"},
		{Name: "cookie", Value: "a=b"},
	})
	// None of these may land in the dynamic table.
	for _, e := range enc.dyn {
		if e.name == "authorization" || e.name == "content-length" || e.name == "cookie" {
			t.Fatalf("sensitive header %q inserted into dynamic table", e.name)
		}
	}
	// Literal-without-indexing uses 0x00 (4-bit prefix) — verify by scanning
	// for the authorization field's leading byte after the pseudo block.
	fields := decodeAll(t, block)
	var seen bool
	for _, f := range fields {
		if f.Name == "authorization" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("authorization missing from block")
	}
	// Byte-level: find the literal-without-indexing for authorization —
	// name index 23 → 0x0f continuation byte (4-bit prefix: 15 then +8).
	// Just verify no indexed repr with dynamic index exists: full-match of
	// (authorization, Bearer secret) must not be present as 0x80|idx>61.
	if idx := enc.findFull("authorization", "Bearer secret"); idx != 0 {
		t.Fatalf("authorization unexpectedly indexed at %d", idx)
	}
}

func TestHpackIncrementalIndexingInserts(t *testing.T) {
	enc := newHpackEncoder()
	encodeBlock(t, enc, "POST", "/r", []HeaderKV{
		{Name: "x-client-request-id", Value: "uuid-1"},
	})
	if len(enc.dyn) == 0 || enc.dyn[0].name != "x-client-request-id" {
		t.Fatalf("expected dynamic insert, dyn=%v", enc.dyn)
	}
	// Second identical field on the same connection → fully indexed. A
	// decoder sharing the dynamic table (inserted by the first block) resolves
	// it back to the same name/value.
	block := encodeBlock(t, enc, "POST", "/r2", []HeaderKV{
		{Name: "x-client-request-id", Value: "uuid-1"},
	})
	var fields []hpack.HeaderField
	dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) { fields = append(fields, f) })
	if _, err := dec.Write(encodeBlock(t, newHpackEncoder(), "POST", "/r", []HeaderKV{
		{Name: "x-client-request-id", Value: "uuid-1"},
	})); err != nil {
		t.Fatalf("seed decode: %v", err)
	}
	fields = fields[:0]
	if _, err := dec.Write(block); err != nil {
		t.Fatalf("decode second block: %v", err)
	}
	var found bool
	for _, f := range fields {
		if f.Name == "x-client-request-id" && f.Value == "uuid-1" {
			found = true
		}
	}
	if !found {
		t.Fatal("dynamic-table hit not emitted")
	}
	// Byte-level: the block tail is a single indexed repr (0x80|index > 61)
	// for the dynamic entry.
	last := block[len(block)-1]
	if last&0x80 == 0 || int(last&0x7f) <= len(rfc7541StaticTable) {
		t.Fatalf("expected dynamic-table indexed field, tail byte=%02x", last)
	}
}

func TestHpackHuffmanAlwaysWhenFits(t *testing.T) {
	// "x-test" Huffman-encodes shorter; h2 always uses Huffman when it fits in
	// a single-byte prefix — even values where Huffman is longer than raw.
	// A 1-char string huffman-encodes to 1 byte (same length) — h2 still emits
	// the H bit. Check the flag bit rather than length.
	enc := newHpackEncoder()
	dst := enc.encodeLiteral(nil, 0x00, 4, "x-a", "b") // name literal + tiny value
	// After flag byte, name is written as huffman when it fits: high bit set.
	if dst[1]&0x80 == 0 {
		t.Fatalf("name not huffman-encoded: %02x", dst[1])
	}
}

func TestHpackRawFallbackForOversizedValue(t *testing.T) {
	enc := newHpackEncoder()
	long := strings.Repeat("x", 300) // huffman len > 126 → raw path
	dst := enc.encodeLiteral(nil, 0x00, 4, "x-big", long)
	// The value length prefix must not have the H bit set — raw path.
	// Locate value start: flag byte + name literal (len-prefixed, huffman).
	i := 1
	nameLen := int(dst[i] & 0x7f)
	i++
	i += nameLen
	if dst[i]&0x80 != 0 {
		t.Fatalf("oversized value huffman-encoded: byte=%02x", dst[i])
	}
}

func TestHpackLargeEntryNotIndexed(t *testing.T) {
	enc := newHpackEncoder()
	// (name+value+32)*4 > 4096*3 → 12288 → entry ≥ ~3048 bytes.
	big := strings.Repeat("y", 4000)
	encodeBlock(t, enc, "POST", "/r", []HeaderKV{
		{Name: "x-blob", Value: big},
	})
	for _, e := range enc.dyn {
		if e.name == "x-blob" {
			t.Fatal("oversized entry inserted into dynamic table")
		}
	}
}

func TestHpackRoundTripPreservesOrder(t *testing.T) {
	ordered := []HeaderKV{
		{Name: "chatgpt-account-id", Value: "acct-1"},
		{Name: "authorization", Value: "Bearer tok"},
		{Name: "user-agent", Value: "codex_cli_rs/0.111.0"},
		{Name: "originator", Value: "codex_cli_rs"},
		{Name: "accept", Value: "text/event-stream"},
	}
	block := encodeBlock(t, nil, "POST", "/p", ordered)
	fields := decodeAll(t, block)
	got := fields[4:]
	if len(got) != len(ordered) {
		t.Fatalf("decoded %d fields, want %d", len(got), len(ordered))
	}
	for i, kv := range ordered {
		if got[i].Name != kv.Name || got[i].Value != kv.Value {
			t.Fatalf("field[%d] = %v, want %v", i, got[i], kv)
		}
	}
	if !bytes.Contains(block, []byte{0x83, 0x87}) {
		t.Fatal("missing indexed pseudo headers")
	}
}
