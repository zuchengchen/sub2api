//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityContractNativeWSReadFailureRetainsActualUsage(t *testing.T) {
	cases := []struct {
		name       string
		events     []string
		wantInputs int
		wantDials  int
		wantErr    bool
	}{
		{name: "no_work_can_retry", wantInputs: 9, wantDials: 2},
		{name: "metadata_already_written", events: []string{`{"type":"response.created","response":{"id":"resp_contract_native","model":"gpt-6-astra"}}`}, wantDials: 1, wantErr: true},
		{name: "reasoning_no_estimate", events: []string{`{"type":"response.reasoning_text.delta","delta":"synthetic reasoning"}`}, wantDials: 1, wantErr: true},
		{name: "tool_no_estimate", events: []string{`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_contract","call_id":"call_contract","name":"synthetic_tool"}}`}, wantDials: 1, wantErr: true},
		{name: "metered_metadata", events: []string{`{"type":"response.in_progress","response":{"id":"resp_contract_native","model":"gpt-6-astra","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`}, wantInputs: 11, wantDials: 1, wantErr: true},
		{name: "later_zero_cannot_erase_usage", events: []string{`{"type":"response.in_progress","response":{"id":"resp_contract_native","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`, `{"type":"response.in_progress","response":{"id":"resp_contract_native","usage":{"input_tokens":0,"output_tokens":0}}}`}, wantInputs: 11, wantDials: 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := &openAIWSCaptureConn{}
			for _, event := range tc.events {
				first.events = append(first.events, []byte(event))
			}
			second := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_contract_retry","model":"gpt-6-astra","status":"completed","usage":{"input_tokens":9,"output_tokens":2}}}`)}}
			result, turnErr, dials := oauthReliabilityNativeBoundaryTurn(t, []openAIWSClientConn{first, second}, false, nil)
			require.Equal(t, tc.wantDials, dials)
			if tc.wantErr {
				require.Error(t, turnErr)
			} else {
				require.NoError(t, turnErr)
			}
			if tc.wantInputs == 0 {
				require.Nil(t, result, "unmetered progress must not become estimated usage")
			} else {
				require.NotNil(t, result)
				require.Equal(t, tc.wantInputs, result.Usage.InputTokens)
				if tc.wantErr {
					require.Equal(t, 3, result.Usage.OutputTokens)
					require.Equal(t, 2, result.Usage.CacheReadInputTokens)
					require.Empty(t, result.UpstreamTerminalEvent, "transport failure must not fabricate an upstream terminal")
					require.False(t, result.wsReplayInputExists, "partial work must not publish reusable continuation items")
				}
			}
		})
	}
}

func TestOAuthReliabilityContractNativeWSRecoveryRequiresUnstartedTurn(t *testing.T) {
	for _, code := range []string{"previous_response_not_found", "rate_limit_exceeded"} {
		for _, metered := range []bool{false, true} {
			name := code + "/unmetered"
			if metered {
				name = code + "/metered"
			}
			t.Run(name, func(t *testing.T) {
				errType := "invalid_request_error"
				if code == "rate_limit_exceeded" {
					errType = "rate_limit_error"
				}
				usage := `"usage":{"input_tokens":0,"output_tokens":0}`
				if metered {
					usage = `"usage":{"input_tokens":11,"output_tokens":3}`
				}
				first := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"error","error":{"type":"` + errType + `","code":"` + code + `","message":"synthetic rejection"},` + usage + `}`)}}
				second := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_contract_retry","model":"gpt-6-astra","status":"completed","usage":{"input_tokens":9,"output_tokens":2}}}`)}}
				result, turnErr, dials := oauthReliabilityNativeBoundaryTurn(t, []openAIWSClientConn{first, second}, code == "previous_response_not_found", nil)
				if metered {
					require.Equal(t, 1, dials, "metered rejection cannot replay this turn")
					require.Error(t, turnErr)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(turnErr, &failover), "metered rate error cannot switch accounts")
					require.NotNil(t, result)
					require.Equal(t, 11, result.Usage.InputTokens)
					require.Equal(t, 3, result.Usage.OutputTokens)
				} else if code == "previous_response_not_found" {
					require.NoError(t, turnErr)
					require.Equal(t, 2, dials, "existing unused-anchor recovery remains available")
					require.NotNil(t, result)
				} else {
					var failover *UpstreamFailoverError
					require.ErrorAs(t, turnErr, &failover, "an unmetered rate rejection retains existing account failover")
					require.Nil(t, result)
					require.Equal(t, 1, dials)
				}
			})
		}
	}
}

// The gin request can be canceled independently while the WS lifecycle remains
// writable. Cancellation before an upstream error must veto both field/anchor
// recovery and account failover without timing-dependent client sleeps.
func TestOAuthReliabilityContractNativeWSCanceledTurnDoesNotRecover(t *testing.T) {
	for _, code := range []string{"previous_response_not_found", "rate_limit_exceeded"} {
		t.Run(code, func(t *testing.T) {
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			first := &oauthReliabilityCancelNativeConn{openAIWSCaptureConn: &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"error","error":{"type":"invalid_request_error","code":"` + code + `","message":"synthetic rejection"}}`)}}, cancel: cancelRequest}
			_, turnErr, dials := oauthReliabilityNativeBoundaryTurn(t, []openAIWSClientConn{first}, code == "previous_response_not_found", requestCtx)
			require.Error(t, turnErr)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(turnErr, &failover))
			require.Equal(t, 1, dials)
			first.mu.Lock()
			writes := len(first.writes)
			first.mu.Unlock()
			wantWrites := 1
			if code == "previous_response_not_found" {
				wantWrites++ // the successful turn establishing connection affinity
			}
			require.Equal(t, wantWrites, writes)
		})
	}
}

type oauthReliabilityCancelNativeConn struct {
	*openAIWSCaptureConn
	cancel context.CancelFunc
}

func (c *oauthReliabilityCancelNativeConn) ReadMessage(ctx context.Context) ([]byte, error) {
	body, err := c.openAIWSCaptureConn.ReadMessage(ctx)
	if err == nil && gjson.GetBytes(body, "type").String() == "error" {
		c.cancel()
	}
	return body, err
}

func oauthReliabilityNativeBoundaryTurn(t *testing.T, conns []openAIWSClientConn, previous bool, requestCtx context.Context) (*OpenAIForwardResult, error, int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if previous {
		// OAuth continuation requires an actual response on the same leased
		// connection. Establish that successful turn instead of bypassing the
		// production strict-affinity gate with a fabricated initial anchor.
		var first *openAIWSCaptureConn
		switch conn := conns[0].(type) {
		case *openAIWSCaptureConn:
			first = conn
		case *oauthReliabilityCancelNativeConn:
			first = conn.openAIWSCaptureConn
		default:
			t.Fatal("unsupported native fixture connection")
		}
		first.events = append([][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_contract_anchor","model":"gpt-6-astra","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)}, first.events...)
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.IngressPreviousResponseRecoveryEnabled = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 2
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 2
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	dialer := &openAIWSQueueDialer{conns: conns}
	pool.setClientDialerForTest(dialer)
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool, tiboRouteDisabled: true}
	account := newOpenAIOAuthNamespaceTestAccount()
	account.Extra["responses_websockets_v2_enabled"] = true
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
	type outcome struct {
		result *OpenAIForwardResult
		err    error
	}
	outcomes := make(chan outcome, 4)
	serverDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_, body, err := conn.Read(ctx)
		if err != nil {
			serverDone <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		if requestCtx == nil {
			c.Request = r.WithContext(ctx)
		} else {
			c.Request = r.WithContext(requestCtx)
		}
		serverDone <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "synthetic-token", body, &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, turnErr error) { outcomes <- outcome{result, turnErr} }})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	body := `{"type":"response.create","model":"gpt-6-astra","stream":true,"input":[{"type":"message","role":"user","content":"synthetic native boundary request"}]`
	body += `}`
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(body)))
	anchorEstablished := false
	// Drain every visible event. Success needs an explicit client close to end
	// the session; failures close it themselves. No sleep decides replay safety.
	for {
		_, event, readErr := client.Read(ctx)
		if readErr != nil {
			break
		}
		if gjson.GetBytes(event, "type").String() == "response.completed" {
			if previous && !anchorEstablished {
				require.Equal(t, "resp_contract_anchor", gjson.GetBytes(event, "response.id").String())
				anchorEstablished = true
				next := `{"type":"response.create","model":"gpt-6-astra","stream":true,"previous_response_id":"resp_contract_anchor","input":[{"type":"message","role":"user","content":"synthetic next turn"}]}`
				require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(next)))
				continue
			}
			_ = client.Close(coderws.StatusNormalClosure, "done")
			break
		}
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("native forwarding did not settle")
	}
	if previous {
		require.True(t, anchorEstablished, "continuation fixture must finish a real prior turn")
		select {
		case primed := <-outcomes:
			require.NoError(t, primed.err)
			require.NotNil(t, primed.result)
			require.Equal(t, "resp_contract_anchor", primed.result.RequestID)
		default:
			t.Fatal("anchor-establishing turn did not settle")
		}
	}
	select {
	case out := <-outcomes:
		require.Empty(t, outcomes, "one logical turn must settle once")
		return out.result, out.err, dialer.DialCount()
	default:
		t.Fatal("native forwarding never invoked AfterTurn")
		return nil, nil, 0
	}
}
