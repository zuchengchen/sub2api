package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNativeReasoningReplayWSInterruptState(t *testing.T) {
	for _, order := range []string{"ack_first", "terminal_first"} {
		t.Run(order, func(t *testing.T) {
			s := &openAIWSInterruptState{responseID: "resp_synthetic"}
			require.NoError(t, s.request([]byte(`{"type":"response.interrupt","response_id":"resp_synthetic","mode":"discard_partial_items"}`)))
			ack := []byte(`{"type":"response.interrupt.accepted","response_id":"resp_synthetic"}`)
			terminal := []byte(`{"type":"response.incomplete","response":{"id":"resp_synthetic","incomplete_details":{"reason":"interrupted"}}}`)
			if order == "ack_first" {
				_, err := s.observe(ack, "response.interrupt.accepted", "resp_synthetic")
				require.NoError(t, err)
				require.False(t, s.ready())
				_, err = s.observe(terminal, "response.incomplete", "resp_synthetic")
				require.NoError(t, err)
			} else {
				_, err := s.observe(terminal, "response.incomplete", "resp_synthetic")
				require.NoError(t, err)
				require.False(t, s.ready())
				_, err = s.observe(ack, "response.interrupt.accepted", "resp_synthetic")
				require.NoError(t, err)
			}
			require.True(t, s.ready())
			require.True(t, s.reusable())
			_, err := s.observe(ack, "response.interrupt.accepted", "resp_synthetic")
			require.Error(t, err)
			require.False(t, s.reusable(), "duplicate acknowledgements are ambiguous")
		})
	}
	for _, raw := range []string{`{"type":"response.interrupt","response_id":"other","mode":"discard_partial_items"}`, `{"type":"response.interrupt","response_id":"resp_synthetic","mode":"keep"}`, `{"type":"response.interrupt","response_id":"other","response_id":"resp_synthetic","mode":"discard_partial_items"}`} {
		require.Error(t, (&openAIWSInterruptState{responseID: "resp_synthetic"}).request([]byte(raw)))
	}
	s := &openAIWSInterruptState{responseID: "resp_synthetic"}
	_, err := s.observe([]byte(`{"type":"response.interrupt.accepted","response_id":"resp_synthetic"}`), "response.interrupt.accepted", "resp_synthetic")
	require.Error(t, err)
	require.False(t, s.reusable())
}

type nativeInterruptTestConn struct {
	events chan []byte
	writes chan []byte
	closed chan struct{}
	once   sync.Once
}

func newNativeInterruptTestConn() *nativeInterruptTestConn {
	return &nativeInterruptTestConn{events: make(chan []byte, 16), writes: make(chan []byte, 16), closed: make(chan struct{})}
}
func (c *nativeInterruptTestConn) RequiresReaderLoop() bool { return true }
func (c *nativeInterruptTestConn) ReadMessage(ctx context.Context) ([]byte, error) {
	select {
	case p := <-c.events:
		return p, nil
	case <-c.closed:
		return nil, errOpenAIWSConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *nativeInterruptTestConn) WriteJSON(ctx context.Context, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	select {
	case <-c.closed:
		return errOpenAIWSConnClosed
	default:
	}
	select {
	case c.writes <- raw:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *nativeInterruptTestConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	body, err := c.ReadMessage(ctx)
	return coderws.MessageText, body, err
}
func (c *nativeInterruptTestConn) WriteFrame(ctx context.Context, _ coderws.MessageType, body []byte) error {
	return c.WriteJSON(ctx, json.RawMessage(body))
}
func (c *nativeInterruptTestConn) Ping(context.Context) error { return nil }
func (c *nativeInterruptTestConn) Close() error               { c.once.Do(func() { close(c.closed) }); return nil }

type nativeInterruptTestOutcome struct {
	result *OpenAIForwardResult
	err    error
}
type nativeInterruptHarness struct {
	client        *coderws.Conn
	upstream      *nativeInterruptTestConn
	serverErr     chan error
	outcomes      chan nativeInterruptTestOutcome
	pool          *openAIWSConnPool
	cancelControl chan context.CancelCauseFunc
}

func newNativeInterruptHarness(t *testing.T) *nativeInterruptHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 1
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 1
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 1
	upstream := newNativeInterruptTestConn()
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: upstream})
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
	account := &Account{ID: 91991, Name: "synthetic-interrupt", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic-token"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
	h := &nativeInterruptHarness{upstream: upstream, serverErr: make(chan error, 1), outcomes: make(chan nativeInterruptTestOutcome, 4), pool: pool, cancelControl: make(chan context.CancelCauseFunc, 1)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			h.serverErr <- err
			return
		}
		defer conn.CloseNow()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r
		_, first, err := conn.Read(r.Context())
		if err != nil {
			h.serverErr <- err
			return
		}
		hooks := &OpenAIWSIngressHooks{ClientLifecycleContext: r.Context(), AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
			h.outcomes <- nativeInterruptTestOutcome{result, err}
		}}
		control, cancel := context.WithCancelCause(r.Context())
		defer cancel(context.Canceled)
		h.cancelControl <- cancel
		err = svc.ProxyResponsesWebSocketFromClient(control, c, conn, account, "synthetic-token", first, hooks)
		var closeErr *OpenAIWSClientCloseError
		if errors.As(err, &closeErr) {
			_ = conn.Close(closeErr.StatusCode(), closeErr.Reason())
		}
		h.serverErr <- err
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	h.client = client
	t.Cleanup(func() { _ = client.CloseNow(); _ = upstream.Close(); pool.Close(); server.Close() })
	return h
}
func (h *nativeInterruptHarness) write(t *testing.T, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, h.client.Write(ctx, coderws.MessageText, []byte(payload)))
}
func (h *nativeInterruptHarness) read(t *testing.T) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, raw, err := h.client.Read(ctx)
	require.NoError(t, err)
	return raw
}
func (h *nativeInterruptHarness) sent(t *testing.T) []byte {
	t.Helper()
	select {
	case raw := <-h.upstream.writes:
		return raw
	case <-time.After(2 * time.Second):
		t.Fatal("missing upstream frame")
		return nil
	}
}
func (h *nativeInterruptHarness) outcome(t *testing.T) nativeInterruptTestOutcome {
	t.Helper()
	select {
	case result := <-h.outcomes:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not settle")
		return nativeInterruptTestOutcome{}
	}
}
func (h *nativeInterruptHarness) begin(t *testing.T) {
	h.write(t, `{"type":"response.create","model":"gpt-5.5","stream":true,"store":false,"input":[{"role":"user","content":"synthetic interrupt"}]}`)
	require.Equal(t, "response.create", gjson.GetBytes(h.sent(t), "type").String())
	h.upstream.events <- []byte(`{"type":"response.created","response":{"id":"resp_interrupt","model":"gpt-5.5"}}`)
	require.Equal(t, "response.created", gjson.GetBytes(h.read(t), "type").String())
}
func (h *nativeInterruptHarness) interrupt(t *testing.T) {
	h.write(t, `{"type":"response.interrupt","response_id":"resp_interrupt","mode":"discard_partial_items"}`)
	sent := h.sent(t)
	require.Equal(t, "response.interrupt", gjson.GetBytes(sent, "type").String())
	require.Equal(t, "resp_interrupt", gjson.GetBytes(sent, "response_id").String())
	require.Equal(t, "discard_partial_items", gjson.GetBytes(sent, "mode").String())
}
func nativeInterruptTerminal() []byte {
	return []byte(`{"type":"response.incomplete","response":{"id":"resp_interrupt","model":"gpt-5.5","status":"incomplete","incomplete_details":{"reason":"interrupted"},"usage":{"input_tokens":7,"output_tokens":3}}}`)
}
func nativeInterruptACK(kind, id string) []byte {
	raw, _ := json.Marshal(map[string]string{"type": "response.interrupt." + kind, "response_id": id})
	return raw
}

func TestNativeReasoningReplayWSInterruptRejectsUnverifiedTerminal(t *testing.T) {
	for _, raw := range []string{
		`{"type":"response.completed","response":{"id":"resp_synthetic"}`,
		`{"type":"response.completed","response":{"id":"other","id":"resp_synthetic"}}`,
		`{"type":"response.completed","type":"response.completed","response":{"id":"resp_synthetic"}}`,
	} {
		state := &openAIWSInterruptState{responseID: "resp_synthetic"}
		require.NotEmpty(t, state.automaticRequest())
		_, err := state.observe([]byte(`{"type":"response.interrupt.accepted","response_id":"resp_synthetic"}`), "response.interrupt.accepted", "resp_synthetic")
		require.NoError(t, err)
		_, err = state.observe([]byte(raw), "response.completed", "resp_synthetic")
		require.Error(t, err)
		require.False(t, state.ready())
		require.False(t, state.reusable())
	}
}

func TestNativeReasoningReplayWSInterruptCtxPoolLifecycle(t *testing.T) {
	for _, order := range []string{"ack_first", "terminal_first"} {
		t.Run(order, func(t *testing.T) {
			h := newNativeInterruptHarness(t)
			h.begin(t)
			h.interrupt(t)
			if order == "ack_first" {
				h.upstream.events <- nativeInterruptACK("accepted", "resp_interrupt")
				require.Equal(t, "response.interrupt.accepted", gjson.GetBytes(h.read(t), "type").String())
				select {
				case <-h.outcomes:
					t.Fatal("ACK alone cannot complete a turn")
				default:
				}
				h.upstream.events <- nativeInterruptTerminal()
				require.Equal(t, "response.incomplete", gjson.GetBytes(h.read(t), "type").String())
			} else {
				h.upstream.events <- nativeInterruptTerminal()
				require.Equal(t, "response.incomplete", gjson.GetBytes(h.read(t), "type").String())
				select {
				case <-h.outcomes:
					t.Fatal("terminal alone cannot complete an interrupted turn")
				default:
				}
				h.upstream.events <- nativeInterruptACK("accepted", "resp_interrupt")
				require.Equal(t, "response.interrupt.accepted", gjson.GetBytes(h.read(t), "type").String())
			}
			out := h.outcome(t)
			require.NoError(t, out.err)
			require.NotNil(t, out.result)
			require.Equal(t, 7, out.result.Usage.InputTokens)
			require.Equal(t, 3, out.result.Usage.OutputTokens)
			require.False(t, out.result.wsReplayInputExists)
			require.True(t, out.result.ClientInterrupted)
			require.Equal(t, "response.incomplete", out.result.UpstreamTerminalEvent)
			require.False(t, out.result.SucceededForScheduling())
			select {
			case <-h.upstream.closed:
				t.Fatal("matching ACK plus terminal should permit reuse")
			default:
			}
			h.write(t, `{"type":"response.create","model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"new full input"}]}`)
			require.Equal(t, "response.create", gjson.GetBytes(h.sent(t), "type").String())
			h.upstream.events <- []byte(`{"type":"response.completed","response":{"id":"resp_after","model":"gpt-5.5","status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}`)
			require.Equal(t, "response.completed", gjson.GetBytes(h.read(t), "type").String())
			require.NoError(t, h.outcome(t).err)
		})
	}
}

func TestNativeReasoningReplayWSInterruptFailureKeepsTerminalUsage(t *testing.T) {
	for _, fault := range []string{"failed_ack", "wrong_id", "missing_ack"} {
		t.Run(fault, func(t *testing.T) {
			h := newNativeInterruptHarness(t)
			h.begin(t)
			h.interrupt(t)
			h.upstream.events <- nativeInterruptTerminal()
			require.Equal(t, "response.incomplete", gjson.GetBytes(h.read(t), "type").String())
			if fault == "failed_ack" {
				h.upstream.events <- nativeInterruptACK("failed", "resp_interrupt")
			} else if fault == "wrong_id" {
				h.upstream.events <- nativeInterruptACK("accepted", "other_response")
			}
			out := h.outcome(t)
			require.NoError(t, out.err, "confirmed usage is settled independently of control failure")
			require.NotNil(t, out.result)
			require.Equal(t, 7, out.result.Usage.InputTokens)
			require.False(t, out.result.wsReplayInputExists)
			require.False(t, out.result.ClientInterrupted)
			// Read the close handshake so the handler can finish promptly.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _, _ = h.client.Read(ctx)
			select {
			case err := <-h.serverErr:
				require.Error(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("failed interruption did not end the session")
			}
			select {
			case <-h.upstream.closed:
			default:
				t.Fatal("invalid interruption must retire upstream")
			}
			select {
			case extra := <-h.outcomes:
				t.Fatalf("duplicate settlement: %+v", extra)
			default:
			}
			select {
			case extra := <-h.upstream.writes:
				t.Fatalf("must not replay generation: %s", extra)
			default:
			}
		})
	}
}

func TestNativeReasoningReplayWSInterruptNoTerminalAndPrecreatedClose(t *testing.T) {
	t.Run("ack_without_terminal", func(t *testing.T) {
		h := newNativeInterruptHarness(t)
		h.begin(t)
		h.interrupt(t)
		h.upstream.events <- nativeInterruptACK("accepted", "resp_interrupt")
		_ = h.read(t)
		out := h.outcome(t)
		require.Error(t, out.err)
		require.Nil(t, out.result, "no terminal usage may be invented")
	})
	t.Run("close_before_created", func(t *testing.T) {
		h := newNativeInterruptHarness(t)
		h.write(t, `{"type":"response.create","model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"synthetic"}]}`)
		_ = h.sent(t)
		started := time.Now()
		_ = h.client.CloseNow()
		out := h.outcome(t)
		require.Less(t, time.Since(started), 2500*time.Millisecond)
		require.Nil(t, out.result)
		require.Error(t, out.err)
		select {
		case extra := <-h.upstream.writes:
			t.Fatalf("no generation replay or fabricated response ID: %s", extra)
		default:
		}
	})
}

func TestNativeReasoningReplayWSInterruptConcurrentCloseSettlesOnce(t *testing.T) {
	h := newNativeInterruptHarness(t)
	h.begin(t)
	h.interrupt(t)
	h.upstream.events <- nativeInterruptTerminal()
	require.Equal(t, "response.incomplete", gjson.GetBytes(h.read(t), "type").String())
	cancel := <-h.cancelControl
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = h.client.CloseNow() }()
	go func() { defer wg.Done(); cancel(errors.New("synthetic control close")) }()
	wg.Wait()
	out := h.outcome(t)
	require.NoError(t, out.err)
	require.NotNil(t, out.result)
	require.Equal(t, 7, out.result.Usage.InputTokens)
	require.False(t, out.result.ClientInterrupted)
	select {
	case <-h.serverErr:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent cancellation remained stuck")
	}
	select {
	case <-h.upstream.closed:
	default:
		t.Fatal("unacknowledged socket must not be reused")
	}
	select {
	case extra := <-h.outcomes:
		t.Fatalf("duplicate terminal billing: %+v", extra)
	default:
	}
}

func TestNativeReasoningReplayWSInterruptRacingCompletedTurn(t *testing.T) {
	h := newNativeInterruptHarness(t)
	h.begin(t)
	h.upstream.events <- []byte(`{"type":"response.completed","response":{"id":"resp_interrupt","model":"gpt-5.5","status":"completed","usage":{"input_tokens":7,"output_tokens":3}}}`)
	require.Equal(t, "response.completed", gjson.GetBytes(h.read(t), "type").String())
	require.NoError(t, h.outcome(t).err)
	// The old next-turn parser rejected this frame with 1008. It still belongs
	// to the just-completed response until the next response.create is admitted.
	h.interrupt(t)
	h.upstream.events <- nativeInterruptACK("accepted", "resp_interrupt")
	require.Equal(t, "response.interrupt.accepted", gjson.GetBytes(h.read(t), "type").String())
	select {
	case extra := <-h.outcomes:
		t.Fatalf("late ACK billed the same terminal twice: %+v", extra)
	default:
	}
	h.write(t, `{"type":"response.create","model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"new full input"}]}`)
	require.Equal(t, "response.create", gjson.GetBytes(h.sent(t), "type").String())
	h.upstream.events <- []byte(`{"type":"response.completed","response":{"id":"resp_new","model":"gpt-5.5","status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}`)
	_ = h.read(t)
	require.NoError(t, h.outcome(t).err)
}

type nativeInterruptFailoverDialer struct {
	mu    sync.Mutex
	calls int
	conn  openAIWSClientConn
}

func (d *nativeInterruptFailoverDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.calls == 1 {
		return nil, http.StatusForbidden, http.Header{}, errors.New("synthetic initial handshake denied")
	}
	return d.conn, 0, http.Header{}, nil
}

func TestNativeReasoningReplayWSInterruptReaderSurvivesPreoutputFailover(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.Enabled = false
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 1
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 1
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 1
			upstream := newNativeInterruptTestConn()
			pool := newOpenAIWSConnPool(cfg)
			dialer := &nativeInterruptFailoverDialer{conn: upstream}
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			svc := &OpenAIGatewayService{openaiWSPassthroughDialer: dialer, cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
			account := &Account{ID: 91993, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic-token"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
			done := make(chan error, 1)
			firstFailure := make(chan error, 1)
			sameReader := make(chan bool, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer conn.CloseNow()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r
				_, first, err := conn.Read(r.Context())
				if err != nil {
					done <- err
					return
				}
				err = svc.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, account, "synthetic-token", first, nil)
				firstFailure <- err
				old, _ := c.Get(openAIWSInterruptClientReaderKey)
				replacement := *account
				replacement.ID++
				replacement.Extra = map[string]any{"openai_apikey_responses_websockets_v2_mode": mode}
				err = svc.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, &replacement, "synthetic-token", first, nil)
				current, _ := c.Get(openAIWSInterruptClientReaderKey)
				sameReader <- old == current
				done <- err
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer client.CloseNow()
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"synthetic failover"}]}`)))
			select {
			case err := <-firstFailure:
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
			case <-ctx.Done():
				t.Fatal("first attempt did not fail")
			}
			select {
			case raw := <-upstream.writes:
				require.Equal(t, "response.create", gjson.GetBytes(raw, "type").String())
			case <-ctx.Done():
				t.Fatal("replacement attempt did not start")
			}
			upstream.events <- []byte(`{"type":"response.completed","response":{"id":"resp_recovered","model":"gpt-5.5","status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}`)
			_, payload, err := client.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(payload, "type").String())
			_ = client.Close(coderws.StatusNormalClosure, "done")
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("replacement attempt did not close")
			}
			require.True(t, <-sameReader, "the downstream reader must belong to the socket, not an account")
			dialer.mu.Lock()
			require.Equal(t, 2, dialer.calls)
			dialer.mu.Unlock()
		})
	}
}

func TestNativeReasoningReplayWSInterruptHTTPDrainAndReuse(t *testing.T) {
	for _, scenario := range []string{"ack_first", "terminal_first", "failed_ack", "wrong_id", "missing_ack", "no_terminal"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.Enabled = false
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 1
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 1
			upstream := newNativeInterruptTestConn()
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: upstream})
			defer pool.Close()
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
			account := &Account{ID: 91992, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic-token"}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeCtxPool}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writer := &cancelOnFirstWriteResponseWriter{cancel: cancel}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			resultCh := make(chan nativeInterruptTestOutcome, 1)
			go func() {
				result, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"synthetic cancel"}]}`))
				resultCh <- nativeInterruptTestOutcome{result, err}
			}()
			select {
			case raw := <-upstream.writes:
				require.Equal(t, "response.create", gjson.GetBytes(raw, "type").String())
			case early := <-resultCh:
				t.Fatalf("forward returned before WS request: result=%+v err=%v", early.result, early.err)
			case <-time.After(2 * time.Second):
				t.Fatal("missing generated request")
			}
			upstream.events <- []byte(`{"type":"response.created","response":{"id":"resp_interrupt","model":"gpt-5.5"}}`)
			upstream.events <- []byte(`{"type":"response.output_text.delta","response_id":"resp_interrupt","delta":"partial synthetic"}`)
			select {
			case raw := <-upstream.writes:
				require.Equal(t, "response.interrupt", gjson.GetBytes(raw, "type").String())
				require.Equal(t, "resp_interrupt", gjson.GetBytes(raw, "response_id").String())
			case <-time.After(2 * time.Second):
				t.Fatal("HTTP cancellation did not send interrupt")
			}
			switch scenario {
			case "ack_first":
				upstream.events <- nativeInterruptACK("accepted", "resp_interrupt")
				upstream.events <- nativeInterruptTerminal()
			case "terminal_first":
				upstream.events <- nativeInterruptTerminal()
				upstream.events <- nativeInterruptACK("accepted", "resp_interrupt")
			case "failed_ack":
				upstream.events <- nativeInterruptTerminal()
				upstream.events <- nativeInterruptACK("failed", "resp_interrupt")
			case "wrong_id":
				upstream.events <- nativeInterruptTerminal()
				upstream.events <- nativeInterruptACK("accepted", "foreign")
			case "missing_ack":
				upstream.events <- nativeInterruptTerminal()
			case "no_terminal":
				upstream.events <- nativeInterruptACK("accepted", "resp_interrupt")
			}
			var out nativeInterruptTestOutcome
			select {
			case out = <-resultCh:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP cancel drain exceeded configured bound")
			}
			require.NotNil(t, out.result)
			require.True(t, out.result.ClientDisconnect)
			if scenario == "no_terminal" {
				require.Error(t, out.err)
				require.Zero(t, out.result.Usage.InputTokens)
			} else {
				require.NoError(t, out.err)
				require.Equal(t, 7, out.result.Usage.InputTokens)
				require.Equal(t, 3, out.result.Usage.OutputTokens)
			}
			require.Equal(t, scenario == "ack_first" || scenario == "terminal_first", out.result.ClientInterrupted)
			if scenario == "ack_first" || scenario == "terminal_first" {
				require.Equal(t, "response.incomplete", out.result.UpstreamTerminalEvent)
				require.False(t, out.result.SucceededForScheduling())
				select {
				case <-upstream.closed:
					t.Fatal("valid interrupt completion should be reusable")
				default:
				}
			} else {
				select {
				case <-upstream.closed:
				default:
					t.Fatal("failed/unconfirmed interrupt must retire socket")
				}
			}
			select {
			case extra := <-upstream.writes:
				t.Fatalf("must not re-generate after cancellation: %s", extra)
			default:
			}
		})
	}
}
