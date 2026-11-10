package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const seededWSExpectedInstallation = "3a479c33-60a4-4c91-8479-62da0fecf605"
const seededWSRawSession = "01a1c2c0-1012-7141-88a7-2b5cbe3269ef"
const seededWSRawTurn = "01a1c2c0-11b5-7d2c-8665-4c940b5e694c"

type seededWSObservedConn struct{ *stagedPassthroughConn }

func (c *seededWSObservedConn) WriteJSON(ctx context.Context, value any) error {
	raw, e := json.Marshal(value)
	if e != nil {
		return e
	}
	return c.WriteFrame(ctx, coderws.MessageText, raw)
}

type seededWSObservedDialer struct {
	conn    openAIWSClientConn
	headers chan http.Header
}

func (d *seededWSObservedDialer) Dial(_ context.Context, _ string, h http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	d.headers <- h.Clone()
	return d.conn, 101, http.Header{}, nil
}
func seededWSMetadata(installation string, prewarm bool) map[string]any {
	turn := seededWSRawTurn
	kind := "turn"
	if prewarm {
		turn = ""
		kind = "prewarm"
	}
	embedded := map[string]any{"installation_id": installation, "session_id": seededWSRawSession, "thread_id": seededWSRawSession, "turn_id": turn, "window_id": seededWSRawSession + ":0", "window_number": 0, "request_kind": kind, "agent_name": "/root", "sandbox": "seccomp", "sandbox_mode": "read-only"}
	if !prewarm {
		embedded["root_turn_id"] = turn
		embedded["turn_started_at_unix_ms"] = int64(1794268729796)
	}
	encoded, _ := json.Marshal(embedded)
	cm := map[string]any{"x-codex-installation-id": installation, "session_id": seededWSRawSession, "thread_id": seededWSRawSession, "turn_id": turn, "x-codex-window-id": seededWSRawSession + ":0", "x-codex-turn-metadata": string(encoded), "keep_unknown": "unchanged"}
	if !prewarm {
		cm["root_turn_id"] = turn
	}
	return cm
}
func runSeededWSIdentitySession(t *testing.T, mode, installation string, apiKeyID int64, convergence bool) (http.Header, [][]byte) {
	t.Helper()
	cfg := newOpenAIWSExecutionScopeTestConfig()
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	conn := &seededWSObservedConn{newStagedPassthroughConn()}
	dialer := &seededWSObservedDialer{conn: conn, headers: make(chan http.Header, 4)}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)
	defer pool.Close()
	svc := &OpenAIGatewayService{cfg: cfg, tiboRouteDisabled: true, openaiWSPool: pool, openaiWSPassthroughDialer: dialer, toolCorrector: NewCodexToolCorrector()}
	account := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 2, Credentials: map[string]any{"access_token": "synthetic-only", "chatgpt_account_id": "acct-golden", "chatgpt_user_id": "user-golden"}, Extra: map[string]any{"codex_fingerprint_mode": "full", "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111", "codex_experimental_fingerprint_convergence": convergence, "openai_oauth_responses_websockets_v2_enabled": true, "openai_oauth_responses_websockets_v2_mode": mode}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
		c.Set("api_key", &APIKey{ID: apiKeyID, UserID: apiKeyID, User: &User{ID: apiKeyID}})
		return nil
	})
	defer server.Close()
	firstCM := seededWSMetadata(installation, true)
	headers := http.Header{"User-Agent": {"codex_cli_rs/0.0.0"}, "Session-Id": {seededWSRawSession}, "Thread-Id": {seededWSRawSession}, "X-Client-Request-Id": {seededWSRawSession}, "X-Codex-Window-Id": {seededWSRawSession + ":0"}, "X-Codex-Turn-Metadata": {firstCM["x-codex-turn-metadata"].(string)}}
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{HTTPHeader: headers})
	require.NoError(t, err)
	defer client.CloseNow()
	var sent [][]byte
	for turn := 0; turn < 2; turn++ {
		cm := seededWSMetadata(installation, turn == 0)
		payload := map[string]any{"type": "response.create", "model": "gpt-5.5", "instructions": "preserve instructions", "input": []any{}, "store": false, "prompt_cache_key": seededWSRawSession, "client_metadata": cm}
		if turn == 0 {
			payload["generate"] = false
		} else {
			payload["input"] = []any{map[string]any{"role": "user", "content": "preserve user content"}}
		}
		raw, e := json.Marshal(payload)
		require.NoError(t, e)
		require.NoError(t, client.Write(ctx, coderws.MessageText, raw))
		select {
		case actual := <-conn.writes:
			sent = append(sent, actual)
		case <-ctx.Done():
			t.Fatal("no actual upstream request")
		}
		id := fmt.Sprintf("resp_seeded_%d", turn)
		conn.Send(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress"}}`, id))
		conn.Send(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}}`, id))
		for {
			_, event, e := client.Read(ctx)
			require.NoError(t, e)
			if gjson.GetBytes(event, "type").String() == "response.completed" {
				break
			}
		}
	}
	_ = client.Close(coderws.StatusNormalClosure, "done")
	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("relay did not close")
	}
	var actualHeaders http.Header
	select {
	case actualHeaders = <-dialer.headers:
	default:
		t.Fatal("no actual upstream handshake")
	}
	return actualHeaders, sent
}
func TestCodexOAuthWSSeededFullNativeIngress(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			sessions := map[int64]string{}
			for _, key := range []int64{1, 2} {
				for _, installation := range []string{"a1e48840-2a1a-44ad-8890-bfa930340c78", "22222222-2222-4222-8222-222222222222"} {
					t.Run(fmt.Sprintf("key_%d_install_%s", key, installation[:8]), func(t *testing.T) {
						headers, frames := runSeededWSIdentitySession(t, mode, installation, key, true)
						require.Len(t, frames, 2)
						require.Empty(t, headers.Get("x-codex-installation-id"), "official Responses carries device inside metadata")
						require.Equal(t, seededWSExpectedInstallation, gjson.Get(headers.Get("x-codex-turn-metadata"), "installation_id").String(), "native Upgrade must use the configured full-mode seed")
						session := gjson.GetBytes(frames[0], "client_metadata.session_id").String()
						if previous, ok := sessions[key]; ok {
							require.Equal(t, previous, session, "installation changes cannot split an explicit session")
						}
						sessions[key] = session
						for _, frame := range frames {
							cm := gjson.GetBytes(frame, "client_metadata")
							embedded := gjson.Parse(cm.Get("x-codex-turn-metadata").String())
							require.Equal(t, seededWSExpectedInstallation, cm.Get("x-codex-installation-id").String())
							require.Equal(t, seededWSExpectedInstallation, embedded.Get("installation_id").String())
							require.Equal(t, session, cm.Get("session_id").String())
							require.Equal(t, session, cm.Get("thread_id").String())
							require.Equal(t, session, headers.Get("session-id"))
							require.Equal(t, session, headers.Get("thread-id"))
							require.Equal(t, session, headers.Get("x-client-request-id"))
							require.Equal(t, session, gjson.GetBytes(frame, "prompt_cache_key").String())
							require.Equal(t, session+":0", cm.Get("x-codex-window-id").String())
							require.Equal(t, "unchanged", cm.Get("keep_unknown").String())
							require.Equal(t, "preserve instructions", gjson.GetBytes(frame, "instructions").String())
						}
						require.Contains(t, string(frames[1]), "preserve user content")
						secondCM := gjson.GetBytes(frames[1], "client_metadata")
						require.Equal(t, secondCM.Get("turn_id").String(), secondCM.Get("root_turn_id").String(), "turn relationship must remain exact")
						require.NotEqual(t, seededWSRawTurn, secondCM.Get("turn_id").String())
					})
				}
			}
			require.NotEqual(t, sessions[1], sessions[2], "full installation convergence must retain API-key conversation isolation")
		})
	}
}
func TestCodexOAuthWSExplicitConvergenceOffKeepsAccountIsolation(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			headers, frames := runSeededWSIdentitySession(t, mode, "a1e48840-2a1a-44ad-8890-bfa930340c78", 1, false)
			require.Equal(t, "1d8d77d2-7eaa-404d-ab92-8d58b4dca591", gjson.Get(headers.Get("x-codex-turn-metadata"), "installation_id").String())
			for _, frame := range frames {
				require.Equal(t, "1d8d77d2-7eaa-404d-ab92-8d58b4dca591", gjson.GetBytes(frame, "client_metadata.x-codex-installation-id").String())
			}
		})
	}
}
