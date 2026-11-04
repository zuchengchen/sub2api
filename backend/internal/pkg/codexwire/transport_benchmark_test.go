package codexwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type benchmarkWireConn struct {
	reader io.Reader
	bytes  int64
}

func (c *benchmarkWireConn) Read(p []byte) (int, error) {
	if c.reader == nil {
		return 0, io.EOF
	}
	return c.reader.Read(p)
}
func (c *benchmarkWireConn) Write(p []byte) (int, error) {
	c.bytes += int64(len(p))
	return len(p), nil
}
func (*benchmarkWireConn) Close() error                     { return nil }
func (*benchmarkWireConn) LocalAddr() net.Addr              { return nil }
func (*benchmarkWireConn) RemoteAddr() net.Addr             { return nil }
func (*benchmarkWireConn) SetDeadline(time.Time) error      { return nil }
func (*benchmarkWireConn) SetReadDeadline(time.Time) error  { return nil }
func (*benchmarkWireConn) SetWriteDeadline(time.Time) error { return nil }

func BenchmarkCodexWireCompressedWrite(b *testing.B) {
	input := make([]map[string]any, 32)
	for i := range input {
		input[i] = map[string]any{"type": "message", "role": "user", "content": fmt.Sprintf("Explain implementation choice %d and keep tool call associations.", i)}
	}
	payload, err := json.Marshal(map[string]any{"type": "response.create", "model": "gpt-5.5", "input": input, "stream": true})
	if err != nil {
		b.Fatal(err)
	}
	wire := &benchmarkWireConn{}
	c := NewWSConn(wire, nil, &UpgradeResult{PerMessageDeflate: true})
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.WriteMessage(ctx, payload); err != nil {
			b.Fatal(err)
		}
	}
	if wire.bytes == 0 {
		b.Fatal("benchmark sent no bytes")
	}
}

func BenchmarkCodexWireStreamingRead(b *testing.B) {
	payload := []byte(`{"type":"response.output_text.delta","item_id":"msg_benchmark","output_index":0,"content_index":0,"delta":"a short streamed text delta"}`)
	for _, compressed := range []bool{false, true} {
		b.Run(fmt.Sprint(compressed), func(b *testing.B) {
			wirePayload := payload
			if compressed {
				var err error
				wirePayload, err = newPMDCompressor(false, 0).Compress(payload)
				if err != nil {
					b.Fatal(err)
				}
			}
			// Keep the fixture self-contained for the identical baseline overlay.
			// Frame construction is outside the measured read loop.
			first := byte(0x80) | opcodeText
			if compressed {
				first |= 0x40
			}
			frameBytes := []byte{first}
			if len(wirePayload) < 126 {
				frameBytes = append(frameBytes, byte(len(wirePayload)))
			} else if len(wirePayload) <= 0xffff {
				frameBytes = append(frameBytes, 126, byte(len(wirePayload)>>8), byte(len(wirePayload)))
			} else {
				b.Fatal("streaming benchmark fixture exceeds its 16-bit frame bound")
			}
			frameBytes = append(frameBytes, wirePayload...)
			var frame bytes.Buffer
			_, _ = frame.Write(frameBytes)
			reader := bytes.NewReader(frame.Bytes())
			conn := &benchmarkWireConn{reader: reader}
			br := bufio.NewReader(conn)
			c := NewWSConn(conn, br, &UpgradeResult{PerMessageDeflate: compressed, ServerNoContextTakeover: true})
			defer c.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
			defer cancel()
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				reader.Reset(frame.Bytes())
				br.Reset(conn)
				got, err := c.ReadMessage(ctx)
				if err != nil || !bytes.Equal(got, payload) {
					b.Fatalf("read actual frame: length=%d error=%v", len(got), err)
				}
			}
		})
	}
}

func BenchmarkCodexWireH2Headers(b *testing.B) {
	wire := &benchmarkWireConn{}
	c := newH2ConnForTest(wire)
	c.framer = http2.NewFramer(wire, nil)
	fields := []HeaderKV{{Name: "authorization", Value: "Bearer synthetic-benchmark"}, {Name: "content-type", Value: "application/json"}, {Name: "content-length", Value: "4096"}}
	st := &h2Stream{id: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.writeHeaders(st, "POST", "https", "example.invalid", "/responses", fields, false); err != nil {
			b.Fatal(err)
		}
	}
	if wire.bytes == 0 {
		b.Fatal("benchmark sent no headers")
	}
}
