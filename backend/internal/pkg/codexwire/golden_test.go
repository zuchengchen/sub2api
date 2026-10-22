package codexwire

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goldenDir = "testdata/golden/0.163.0"

type goldenRecord struct {
	Kind    string `json:"kind"`
	Method  string `json:"method"`
	Target  string `json:"target"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	BodyEncoding string          `json:"body_encoding"`
	BodyJSON     json.RawMessage `json:"body_json"`
}

func loadGolden(t *testing.T, name string) []goldenRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var out []goldenRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r goldenRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("decode golden line: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// goldenToHeader converts golden headers into a Go http.Header (canonical,
// unordered), dropping transport-owned names.
func goldenToHeader(rec goldenRecord, drop ...string) (http.Header, []string) {
	h := http.Header{}
	var names []string
outer:
	for _, kv := range rec.Headers {
		for _, d := range drop {
			if strings.EqualFold(kv.Name, d) {
				continue outer
			}
		}
		h.Add(kv.Name, kv.Value)
		names = append(names, kv.Name)
	}
	return h, names
}

// TestGolden_HTTPResponsesHeaderOrder replays every captured Codex HTTP POST
// /responses header set through the h1 writer and requires byte-identical
// name order and casing, including hyper's trailing host/content-length.
func TestGolden_HTTPResponsesHeaderOrder(t *testing.T) {
	var checked int
	for _, rec := range loadGolden(t, "http_fallback_tool_loop.jsonl") {
		if rec.Kind != "http_request" || !strings.HasSuffix(rec.Target, "/codex/responses") {
			continue
		}
		checked++
		h, _ := goldenToHeader(rec, "host", "content-length")
		var wantNames []string
		for _, kv := range rec.Headers {
			wantNames = append(wantNames, kv.Name)
		}

		client, server := net.Pipe()
		got := make(chan []string, 1)
		go func() {
			br := bufio.NewReader(server)
			req, err := http.ReadRequest(br)
			if err != nil {
				got <- nil
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\ncontent-length: 0\r\n\r\n")
			got <- nil
		}()
		// Capture the raw request bytes via a tee on the client side.
		var raw strings.Builder
		tee := &teeConn{Conn: client, w: &raw}
		ordered := OrderCodexHTTPHeaders(h)
		body := []byte(`{"model":"gpt-5.5"}`)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := writeHTTP1Request(ctx, tee, "POST", "/backend-api/codex/responses", "<UPSTREAM>", ordered, body, 0)
		cancel()
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = resp.Body.Close()
		<-got
		var names []string
		for _, line := range strings.Split(raw.String(), "\r\n")[1:] {
			if line == "" {
				break
			}
			name, _, _ := strings.Cut(line, ":")
			names = append(names, name)
		}
		if strings.Join(names, ",") != strings.Join(wantNames, ",") {
			t.Fatalf("header order mismatch:\n ours   %v\n golden %v", names, wantNames)
		}
	}
	if checked == 0 {
		t.Fatal("golden has no HTTP /responses requests")
	}
}

// TestGolden_WSHandshakeHeaderOrder replays the captured WS handshake headers
// through the upgrade builder.
func TestGolden_WSHandshakeHeaderOrder(t *testing.T) {
	var checked int
	for _, rec := range loadGolden(t, "ws_tool_loop.jsonl") {
		if rec.Kind != "ws_handshake" {
			continue
		}
		checked++
		h, _ := goldenToHeader(rec, "host", "connection", "upgrade", "sec-websocket-version", "sec-websocket-key", "sec-websocket-extensions")
		ordered, leftover := SplitCodexWSHeaders(h)
		if len(leftover) != 0 {
			t.Fatalf("golden handshake headers missing from template: %+v", leftover)
		}
		raw, _, err := BuildWSUpgradeRequest("wss://<UPSTREAM>/backend-api/codex/responses", ordered, "AAAAAAAAAAAAAAAAAAAAAA==")
		if err != nil {
			t.Fatal(err)
		}
		var names, wantNames []string
		for _, line := range strings.Split(string(raw), "\r\n")[1:] {
			if line == "" {
				break
			}
			name, _, _ := strings.Cut(line, ":")
			names = append(names, name)
		}
		for _, kv := range rec.Headers {
			wantNames = append(wantNames, kv.Name)
		}
		if strings.Join(names, ",") != strings.Join(wantNames, ",") {
			t.Fatalf("ws handshake order mismatch:\n ours   %v\n golden %v", names, wantNames)
		}
	}
	if checked == 0 {
		t.Fatal("golden has no ws handshake")
	}
}

type teeConn struct {
	net.Conn
	w io.Writer
}

func (c *teeConn) Write(p []byte) (int, error) {
	_, _ = c.w.Write(p)
	return c.Conn.Write(p)
}
