package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexFingerprintPrewarmHeaderDoesNotClassifyCurrentBody(t *testing.T) {
	first := seededWSMetadata("a1e48840-2a1a-44ad-8890-bfa930340c78", true)
	headers := http.Header{"X-Codex-Turn-Metadata": {first[openAIWSTurnMetadataHeader].(string)}, "Session-Id": {seededWSRawSession}}
	for _, kind := range []string{"turn", "", "legacy_prewarm"} {
		for _, carrier := range []string{"raw", "map"} {
			t.Run(fmt.Sprintf("kind=%s/%s", kind, carrier), func(t *testing.T) {
				body := map[string]any{"input": []any{map[string]any{"role": "user", "content": "start a real turn"}}}
				if kind == "legacy_prewarm" {
					body["generate"] = false
				} else if kind != "" {
					body["client_metadata"] = map[string]any{"x-codex-turn-metadata": `{"request_kind":"turn"}`}
				}
				account := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "acct-golden", "chatgpt_user_id": "user-golden"}, Extra: map[string]any{"codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111", "codex_fingerprint_mode": "full", "codex_experimental_fingerprint_convergence": true}}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/responses", nil)
				c.Request.Header = headers.Clone()
				c.Set("api_key", &APIKey{ID: 1})
				var ids *codexFingerprintIDs
				if carrier == "raw" {
					raw, err := json.Marshal(body)
					require.NoError(t, err)
					ids = resolveCodexFingerprintIDsForRawBody(c, account, nil, raw)
				} else {
					ids = resolveCodexFingerprintIDsForDecodedBody(c, account, nil, body)
				}
				require.NotNil(t, ids)
				if kind == "legacy_prewarm" {
					require.Empty(t, ids.turnID, "explicit generate=false allows legacy header-only prewarm")
					require.Zero(t, ids.turnStartedAtUnixMs)
				} else {
					require.NotEmpty(t, ids.turnID, "stale Upgrade prewarm must not suppress this body's turn")
					require.Positive(t, ids.turnStartedAtUnixMs)
				}
			})
		}
	}
	ev := collectCodexTurnEvidence(nil, headers, gjson.Result{})
	require.Equal(t, "prewarm", ev.RequestKind, "header-only evidence remains supported")
	raw := gjson.Parse(`{"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"prewarm\",\"turn_id\":\"\"}"},"generate":false}`)
	require.Equal(t, "prewarm", collectCodexTurnEvidence(raw.Get("client_metadata"), headers, raw).RequestKind)
}

func TestCodexOAuthWSPrewarmHeaderThenGenericTurns(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			upstream := &seededWSObservedConn{newStagedPassthroughConn()}
			dialer := &seededWSObservedDialer{conn: upstream, headers: make(chan http.Header, 4)}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			svc := &OpenAIGatewayService{cfg: cfg, tiboRouteDisabled: true, openaiWSPool: pool, openaiWSPassthroughDialer: dialer, toolCorrector: NewCodexToolCorrector()}
			account := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 2, Credentials: map[string]any{"access_token": "synthetic-only", "chatgpt_account_id": "acct-golden", "chatgpt_user_id": "user-golden"}, Extra: map[string]any{"codex_fingerprint_mode": "full", "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111", "codex_experimental_fingerprint_convergence": true, "openai_oauth_responses_websockets_v2_enabled": true, "openai_oauth_responses_websockets_v2_mode": mode}}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			server, done := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
				c.Set("api_key", &APIKey{ID: 1, UserID: 1, User: &User{ID: 1}})
				return nil
			})
			defer server.Close()
			cm := seededWSMetadata("a1e48840-2a1a-44ad-8890-bfa930340c78", true)
			header := http.Header{"User-Agent": {"codex_cli_rs/0.0.0"}, "Session-Id": {seededWSRawSession}, "X-Codex-Turn-Metadata": {cm[openAIWSTurnMetadataHeader].(string)}}
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{HTTPHeader: header})
			require.NoError(t, err)
			defer client.CloseNow()
			var observed [][]byte
			for index, kind := range []string{"prewarm", "turn", ""} {
				body := map[string]any{"type": "response.create", "model": "gpt-5.5", "instructions": "synthetic-only", "store": false, "input": []any{map[string]any{"role": "user", "content": fmt.Sprintf("generic turn %d", index)}}, "prompt_cache_key": seededWSRawSession}
				if index == 0 {
					body["client_metadata"] = cm
					body["generate"] = false
					body["input"] = []any{}
				} else if kind != "" {
					body["client_metadata"] = map[string]any{"x-codex-turn-metadata": `{"request_kind":"turn"}`}
				}
				raw, e := json.Marshal(body)
				require.NoError(t, e)
				require.NoError(t, client.Write(ctx, coderws.MessageText, raw))
				select {
				case out := <-upstream.writes:
					observed = append(observed, out)
				case <-ctx.Done():
					t.Fatal("missing actual upstream write")
				}
				id := fmt.Sprintf("resp_prewarm_priority_%d", index)
				upstream.Send(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress"}}`, id))
				upstream.Send(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}}`, id))
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
			case err := <-done:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("relay did not close")
			}
			require.Empty(t, gjson.GetBytes(observed[0], "client_metadata.turn_id").String())
			for i := 1; i < 3; i++ {
				require.NotEmpty(t, gjson.GetBytes(observed[i], "client_metadata.turn_id").String(), "current frame %d must get its own turn despite stale prewarm Upgrade", i)
			}
			require.NotEqual(t, gjson.GetBytes(observed[1], "client_metadata.turn_id").String(), gjson.GetBytes(observed[2], "client_metadata.turn_id").String(), "different generic user submissions need distinct turns")
		})
	}
}
