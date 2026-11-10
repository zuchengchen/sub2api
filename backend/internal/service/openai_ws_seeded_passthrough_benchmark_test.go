package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// This benchmark must compile on the pre-fix baseline. It reaches the existing
// production ingress, first-frame transform, and subsequent-frame relay filter.
// Only the upstream network peer is replaced. Timed work starts before the real
// downstream WS write and ends at the actual upstream WriteFrame boundary.
// Handshake setup, response acknowledgements, validation and teardown are outside
// the timer; no sleeps or synthetic latency hide transform cost.
func BenchmarkCodexOAuthPassthroughSeededIdentity(b *testing.B) {
	previousLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(previousLogOutput)
	for _, size := range []int{1024, 256 * 1024} {
		for _, phase := range []string{"first", "subsequent"} {
			b.Run(fmt.Sprintf("%s/bytes=%d", phase, size), func(b *testing.B) {
				b.StopTimer()
				input, headers := seededPassthroughBenchmarkPayload(b, size)
				b.SetBytes(int64(len(input)))
				b.ReportAllocs()
				var session *seededPassthroughBenchmarkSession
				if phase == "subsequent" {
					session = newSeededPassthroughBenchmarkSession(b, headers)
					session.write(b, input)
					session.checkAndComplete(b, session.receive(b))
				}
				var measuredWrites int
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if phase == "first" {
						session = newSeededPassthroughBenchmarkSession(b, headers)
					}
					b.StartTimer()
					session.write(b, input)
					actual := session.receive(b)
					b.StopTimer()
					measuredWrites++
					session.checkAndComplete(b, actual)
					if phase == "first" {
						session.close(b)
						session = nil
					}
				}
				if session != nil {
					session.close(b)
				}
				if measuredWrites != b.N {
					b.Fatalf("actual upstream writes: got %d want %d", measuredWrites, b.N)
				}
				b.ReportMetric(float64(measuredWrites), "measured_writes")
			})
		}
	}
}

const seededPassthroughBenchmarkSessionID = "01a1c2c0-1012-7141-88a7-2b5cbe3269ef"
const seededPassthroughBenchmarkInstallation = "a1e48840-2a1a-44ad-8890-bfa930340c78"

func seededPassthroughBenchmarkPayload(b *testing.B, size int) ([]byte, http.Header) {
	b.Helper()
	embedded := map[string]any{"installation_id": seededPassthroughBenchmarkInstallation, "session_id": seededPassthroughBenchmarkSessionID, "thread_id": seededPassthroughBenchmarkSessionID, "turn_id": "01a1c2c0-11b5-7d2c-8665-4c940b5e694c", "root_turn_id": "01a1c2c0-11b5-7d2c-8665-4c940b5e694c", "window_id": seededPassthroughBenchmarkSessionID + ":0", "window_number": 0, "request_kind": "turn", "turn_started_at_unix_ms": int64(1794268729796), "sandbox": "seccomp"}
	encoded, err := json.Marshal(embedded)
	if err != nil {
		b.Fatal(err)
	}
	metadata := map[string]any{"x-codex-installation-id": seededPassthroughBenchmarkInstallation, "session_id": seededPassthroughBenchmarkSessionID, "thread_id": seededPassthroughBenchmarkSessionID, "turn_id": embedded["turn_id"], "root_turn_id": embedded["root_turn_id"], "x-codex-window-id": seededPassthroughBenchmarkSessionID + ":0", "x-codex-turn-metadata": string(encoded)}
	raw, err := json.Marshal(map[string]any{"type": "response.create", "model": "gpt-5.5", "instructions": "synthetic benchmark", "input": []any{map[string]any{"role": "user", "content": strings.Repeat("x", size)}}, "store": false, "prompt_cache_key": seededPassthroughBenchmarkSessionID, "client_metadata": metadata})
	if err != nil {
		b.Fatal(err)
	}
	return raw, http.Header{"User-Agent": {"codex_cli_rs/0.0.0"}, "Session-Id": {seededPassthroughBenchmarkSessionID}, "Thread-Id": {seededPassthroughBenchmarkSessionID}, "X-Client-Request-Id": {seededPassthroughBenchmarkSessionID}, "X-Codex-Turn-Metadata": {string(encoded)}}
}

type seededPassthroughBenchmarkConn struct {
	writes     chan []byte
	events     chan []byte
	closed     chan struct{}
	closeOnce  sync.Once
	writeCount atomic.Int64
}

func (c *seededPassthroughBenchmarkConn) WriteFrame(ctx context.Context, _ coderws.MessageType, payload []byte) error {
	c.writeCount.Add(1)
	select {
	case c.writes <- append([]byte(nil), payload...):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return io.EOF
	}
}
func (c *seededPassthroughBenchmarkConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	select {
	case raw := <-c.events:
		return coderws.MessageText, raw, nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-c.closed:
		return 0, nil, io.EOF
	}
}
func (c *seededPassthroughBenchmarkConn) WriteJSON(ctx context.Context, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, raw)
}
func (c *seededPassthroughBenchmarkConn) ReadMessage(ctx context.Context) ([]byte, error) {
	_, raw, err := c.ReadFrame(ctx)
	return raw, err
}
func (c *seededPassthroughBenchmarkConn) Ping(context.Context) error { return nil }
func (c *seededPassthroughBenchmarkConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

type seededPassthroughBenchmarkDialer struct {
	conn  *seededPassthroughBenchmarkConn
	calls atomic.Int64
}

func (d *seededPassthroughBenchmarkDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	d.calls.Add(1)
	return d.conn, 101, http.Header{}, nil
}

type seededPassthroughBenchmarkSession struct {
	ctx          context.Context
	cancel       context.CancelFunc
	client       *coderws.Conn
	server       *httptest.Server
	done         chan error
	upstream     *seededPassthroughBenchmarkConn
	dialer       *seededPassthroughBenchmarkDialer
	acknowledged int64
}

func newSeededPassthroughBenchmarkSession(b *testing.B, headers http.Header) *seededPassthroughBenchmarkSession {
	b.Helper()
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 10
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 10
	account := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"access_token": "synthetic-benchmark-only", "chatgpt_account_id": "acct-golden", "chatgpt_user_id": "user-golden"}, Extra: map[string]any{"codex_fingerprint_mode": "full", "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111", "codex_experimental_fingerprint_convergence": true, "openai_oauth_responses_websockets_v2_enabled": true, "openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough}}
	ctx, cancel := context.WithCancel(context.Background())
	upstream := &seededPassthroughBenchmarkConn{writes: make(chan []byte, 1), events: make(chan []byte, 2), closed: make(chan struct{})}
	dialer := &seededPassthroughBenchmarkDialer{conn: upstream}
	svc := &OpenAIGatewayService{cfg: cfg, tiboRouteDisabled: true, openaiWSPassthroughDialer: dialer, toolCorrector: NewCodexToolCorrector()}
	session := &seededPassthroughBenchmarkSession{ctx: ctx, cancel: cancel, upstream: upstream, dialer: dialer, done: make(chan error, 1)}
	session.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionDisabled})
		if err != nil {
			session.done <- err
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(2 * 1024 * 1024)
		_, first, err := ReadOpenAIWSClientMessage(ctx, conn, 10*time.Second, coderws.StatusPolicyViolation, "benchmark first frame")
		if err != nil {
			session.done <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.Clone(ctx)
		c.Request.Header = r.Header.Clone()
		c.Set("api_key", &APIKey{ID: 1, UserID: 1, User: &User{ID: 1}})
		session.done <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "synthetic-benchmark-only", first, nil)
	}))
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()
	client, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(session.server.URL, "http"), &coderws.DialOptions{HTTPHeader: headers.Clone(), CompressionMode: coderws.CompressionDisabled})
	if err != nil {
		session.server.Close()
		cancel()
		b.Fatal(err)
	}
	client.SetReadLimit(2 * 1024 * 1024)
	session.client = client
	return session
}
func (s *seededPassthroughBenchmarkSession) write(b *testing.B, raw []byte) {
	b.Helper()
	if err := s.client.Write(s.ctx, coderws.MessageText, raw); err != nil {
		b.Fatal(err)
	}
}
func (s *seededPassthroughBenchmarkSession) receive(b *testing.B) []byte {
	b.Helper()
	select {
	case raw := <-s.upstream.writes:
		return raw
	case err := <-s.done:
		b.Fatalf("production path ended before upstream write: %v", err)
	case <-time.After(10 * time.Second):
		b.Fatal("production path produced no upstream frame")
	}
	return nil
}
func (s *seededPassthroughBenchmarkSession) checkAndComplete(b *testing.B, raw []byte) {
	b.Helper()
	if gjson.GetBytes(raw, "type").String() != "response.create" {
		b.Fatal("upstream did not receive response.create")
	}
	installation := gjson.GetBytes(raw, "client_metadata.x-codex-installation-id").String()
	if installation == "" || installation == seededPassthroughBenchmarkInstallation {
		b.Fatal("production identity transform did not run")
	}
	session := gjson.GetBytes(raw, "client_metadata.session_id").String()
	if session == "" || session == seededPassthroughBenchmarkSessionID || session != gjson.GetBytes(raw, "prompt_cache_key").String() {
		b.Fatal("production session/cache projection did not run")
	}
	if s.dialer.calls.Load() != 1 {
		b.Fatal("unexpected upstream dial count")
	}
	s.acknowledged++
	id := fmt.Sprintf("resp_bench_%d", s.acknowledged)
	s.upstream.events <- []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress"}}`, id))
	s.upstream.events <- []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}}`, id))
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	for {
		_, event, err := s.client.Read(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if gjson.GetBytes(event, "type").String() == "response.completed" {
			if gjson.GetBytes(event, "response.id").String() != id {
				b.Fatal("wrong completed response")
			}
			break
		}
	}
}
func (s *seededPassthroughBenchmarkSession) close(b *testing.B) {
	b.Helper()
	if s.upstream.writeCount.Load() != s.acknowledged {
		b.Fatal("unacknowledged actual upstream write")
	}
	s.cancel()
	_ = s.client.CloseNow()
	_ = s.upstream.Close()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		b.Fatal("production relay did not stop")
	}
	s.server.Close()
}
