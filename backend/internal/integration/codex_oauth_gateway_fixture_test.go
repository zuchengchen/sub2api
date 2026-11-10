//go:build integration && embed

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type codexIntegrationRequest struct {
	accountID int64
	header    http.Header
	body      []byte
	marker    string
	websocket bool
}

type codexIntegrationUpstream struct {
	server, proxy  *httptest.Server
	mu             sync.Mutex
	requests       []codexIntegrationRequest
	accounts       map[string]int64
	errors         []string
	connections    map[net.Conn]struct{}
	failoverFirst  int64
	soft429Seen    bool
	cancelDone     chan struct{}
	cancelOnce     sync.Once
	connects       int
	isolationReady map[string]chan struct{}
	isolationCount map[string]int
}

func syntheticCodexIntegrationJWT(label string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"exp": 4102444800, "sub": "owner-" + label, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "workspace-" + label, "chatgpt_user_id": "owner-" + label, "chatgpt_plan_type": "plus"}})
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic-signature"
}

func newCodexIntegrationUpstream(t *testing.T) *codexIntegrationUpstream {
	t.Helper()
	u := &codexIntegrationUpstream{accounts: make(map[string]int64), connections: make(map[net.Conn]struct{}), cancelDone: make(chan struct{}), isolationReady: make(map[string]chan struct{}), isolationCount: make(map[string]int)}
	cert, ca := codexIntegrationCertificate(t)
	caPath := filepath.Join(t.TempDir(), "synthetic-ca.pem")
	require.NoError(t, os.WriteFile(caPath, ca, 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", caPath)
	u.server = httptest.NewUnstartedServer(http.HandlerFunc(u.serve))
	u.server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	u.server.EnableHTTP2 = true
	u.server.StartTLS()
	t.Cleanup(u.server.Close)
	u.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || (r.Host != "chatgpt.com:443" && r.Host != "auth.openai.com:443") {
			u.recordError("denied CONNECT: " + r.Method + " " + r.Host)
			http.Error(w, "local fixture target denied", http.StatusForbidden)
			return
		}
		// The CONNECT host is never resolved or dialled: every permitted target
		// goes to the same numeric loopback address, with normal TLS validation.
		remote, err := net.DialTimeout("tcp", u.server.Listener.Addr().String(), 5*time.Second)
		if err != nil {
			u.recordError("local CONNECT dial failed")
			http.Error(w, "local upstream unavailable", http.StatusBadGateway)
			return
		}
		client, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = remote.Close()
			u.recordError("CONNECT hijack failed")
			return
		}
		u.mu.Lock()
		u.connects++
		u.connections[client] = struct{}{}
		u.connections[remote] = struct{}{}
		u.mu.Unlock()
		defer func() {
			_ = client.Close()
			_ = remote.Close()
			u.mu.Lock()
			delete(u.connections, client)
			delete(u.connections, remote)
			u.mu.Unlock()
		}()
		if _, err = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(remote, buf); _ = remote.Close(); close(done) }()
		_, _ = io.Copy(client, remote)
		_ = client.Close()
		<-done
	}))
	t.Cleanup(func() {
		u.finishCancel()
		u.mu.Lock()
		for c := range u.connections {
			_ = c.Close()
		}
		u.mu.Unlock()
		u.proxy.Close()
	})
	return u
}

func codexIntegrationCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sub2api synthetic integration CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "chatgpt.com"}, DNSNames: []string{"chatgpt.com", "auth.openai.com", "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	require.NoError(t, err)
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
}

func (u *codexIntegrationUpstream) registerAccount(a *service.Account) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.accounts[a.GetOpenAIAccessToken()] = a.ID
}
func (u *codexIntegrationUpstream) recordError(message string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.errors = append(u.errors, message)
}
func (u *codexIntegrationUpstream) errorsSnapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.errors...)
}
func (u *codexIntegrationUpstream) requestCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}
func (u *codexIntegrationUpstream) connectCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.connects
}

// Both requests must have reached the same upstream before either completes.
// This makes isolation cover simultaneous branches instead of a serial replay.
func (u *codexIntegrationUpstream) waitIsolation(ctx context.Context, marker string) bool {
	if !strings.HasSuffix(marker, "-isolation-a") && !strings.HasSuffix(marker, "-isolation-b") {
		return true
	}
	pair := marker[:len(marker)-1]
	u.mu.Lock()
	ready := u.isolationReady[pair]
	if ready == nil {
		ready = make(chan struct{})
		u.isolationReady[pair] = ready
	}
	u.isolationCount[pair]++
	if u.isolationCount[pair] == 2 {
		close(ready)
	}
	u.mu.Unlock()
	select {
	case <-ready:
		return true
	case <-ctx.Done():
		u.recordError("concurrent isolation barrier cancelled")
		return false
	case <-time.After(10 * time.Second):
		u.recordError("concurrent isolation barrier timed out")
		return false
	}
}
func (u *codexIntegrationUpstream) finishCancel() { u.cancelOnce.Do(func() { close(u.cancelDone) }) }
func (u *codexIntegrationUpstream) requestsFor(marker string) []codexIntegrationRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []codexIntegrationRequest
	for _, r := range u.requests {
		if r.marker == marker {
			out = append(out, r)
		}
	}
	return out
}
func (u *codexIntegrationUpstream) requestsWithPrefix(prefix string) []codexIntegrationRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []codexIntegrationRequest
	for _, r := range u.requests {
		if strings.HasPrefix(r.marker, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (u *codexIntegrationUpstream) capture(r *http.Request, body []byte, ws bool) (codexIntegrationRequest, bool) {
	marker := gjson.GetBytes(body, "input.0.content.0.text").String()
	u.mu.Lock()
	defer u.mu.Unlock()
	id, ok := u.accounts[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok || marker == "" {
		u.errors = append(u.errors, "upstream request missing synthetic account or input marker")
		return codexIntegrationRequest{}, false
	}
	record := codexIntegrationRequest{accountID: id, header: r.Header.Clone(), body: append([]byte(nil), body...), marker: marker, websocket: ws}
	u.requests = append(u.requests, record)
	return record, true
}

func (u *codexIntegrationUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/backend-api/codex/responses" {
		u.recordError("unexpected upstream path: " + r.URL.Path)
		http.Error(w, "unexpected synthetic path", http.StatusNotFound)
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		u.serveWS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		u.recordError("unexpected upstream method: " + r.Method)
		http.Error(w, "expected POST", http.StatusMethodNotAllowed)
		return
	}
	var reader io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "zstd" {
		decoder, err := zstd.NewReader(r.Body)
		if err != nil {
			u.recordError("invalid synthetic zstd")
			http.Error(w, "zstd", 400)
			return
		}
		defer decoder.Close()
		reader = decoder
	}
	body, err := io.ReadAll(io.LimitReader(reader, 2<<20))
	if err != nil {
		u.recordError("reading synthetic upstream body")
		http.Error(w, "body", 400)
		return
	}
	record, ok := u.capture(r, body, false)
	if !ok {
		http.Error(w, "unknown synthetic account/body", 400)
		return
	}
	if !u.waitIsolation(r.Context(), record.marker) {
		http.Error(w, "isolation barrier", 500)
		return
	}
	if record.marker == "http-soft-429" {
		u.mu.Lock()
		fail := !u.soft429Seen
		u.soft429Seen = true
		u.mu.Unlock()
		if fail {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"synthetic transient limit"}}`)
			return
		}
	}
	if record.marker == "http-failover" {
		u.mu.Lock()
		if u.failoverFirst == 0 {
			u.failoverFirst = record.accountID
		}
		fail := u.failoverFirst == record.accountID
		u.mu.Unlock()
		if fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"account_deactivated","message":"synthetic account unavailable"}}`)
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("x-request-id", "request_"+record.marker)
	w.WriteHeader(http.StatusOK)
	flush := w.(http.Flusher)
	write := func(event map[string]any) {
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		flush.Flush()
	}
	write(codexIntegrationCreated(record.marker))
	if record.marker == "http-cancel" {
		write(map[string]any{"type": "response.output_text.delta", "response_id": "resp_" + record.marker, "item_id": "msg_" + record.marker, "output_index": 0, "content_index": 0, "delta": "partial", "usage": codexIntegrationUsageJSON()})
		// No terminal event is emitted. The caller cancels only after reading
		// this delta, then releases this barrier so the upstream also ends.
		select {
		case <-u.cancelDone:
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
			u.recordError("cancel fixture timed out")
		}
		return
	}
	write(codexIntegrationCompleted(record.marker))
	if record.marker == "http-duplicate-terminal" {
		write(codexIntegrationCompleted(record.marker))
	}
}

func (u *codexIntegrationUpstream) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		u.recordError("upstream WS accept failed")
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(2 << 20)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	for {
		_, body, err := conn.Read(ctx)
		if err != nil {
			return
		}
		record, ok := u.capture(r, body, true)
		if !ok {
			_ = conn.Close(websocket.StatusPolicyViolation, "unknown synthetic account/body")
			return
		}
		if !u.waitIsolation(ctx, record.marker) {
			return
		}
		if gjson.GetBytes(body, "type").String() != "response.create" {
			u.recordError("unexpected WS message type")
			_ = conn.Close(websocket.StatusPolicyViolation, "expected response.create")
			return
		}
		for _, event := range []map[string]any{codexIntegrationCreated(record.marker), codexIntegrationCompleted(record.marker)} {
			data, _ := json.Marshal(event)
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
	}
}

func codexIntegrationUsageJSON() map[string]any {
	return map[string]any{"input_tokens": 100, "output_tokens": 20, "total_tokens": 120, "input_tokens_details": map[string]any{"cached_tokens": 0}}
}
func codexIntegrationCreated(marker string) map[string]any {
	return map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_" + marker, "model": "gpt-5.2", "status": "in_progress", "output": []any{}}}
}
func codexIntegrationCompleted(marker string) map[string]any {
	return map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_" + marker, "model": "gpt-5.2", "status": "completed", "output": []any{map[string]any{"id": "msg_" + marker, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK"}}}}, "usage": codexIntegrationUsageJSON()}}
}
