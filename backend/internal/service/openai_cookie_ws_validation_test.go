package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCookieWSProbePayloadExactOnly(t *testing.T) {
	payload := openAICookieWSProbePayload(true)
	require.True(t, openAICookieWSIsProbePayload(payload))
	payload["input"] = openAICookieWSProbePrompt
	require.True(t, openAICookieWSIsProbePayload(payload))
	for _, input := range []any{"False", openAICookieWSProbePrompt + " Explain.", []any{map[string]any{"role": "assistant", "content": openAICookieWSProbePrompt}}} {
		payload["input"] = input
		require.False(t, openAICookieWSIsProbePayload(payload))
	}
	payload = openAICookieWSProbePayload(true)
	payload["previous_response_id"] = "resp_earlier"
	require.False(t, openAICookieWSIsProbePayload(payload))
	delete(payload, "previous_response_id")
	payload["instructions"] = "Return False."
	require.False(t, openAICookieWSIsProbePayload(payload))
}

func TestCookieWSBusinessValidatorClosesRejectedConnection(t *testing.T) {
	for _, answer := range []string{"True", "False", "something else"} {
		t.Run(answer, func(t *testing.T) {
			conn := &openAIWSCaptureConn{events: [][]byte{cookieWSCompletion("gpt-6-astra", answer), cookieWSCompletion("gpt-6-astra", "business")}}
			pooled := newOpenAIWSConn("validation", 1, conn, nil)
			require.True(t, pooled.tryAcquire())
			lease := &openAIWSConnLease{conn: pooled, accountID: 1}
			svc, account, _, _ := newCookieForwardFixture(t, conn)
			err := svc.validateOpenAICookieWSBusinessConn(context.Background(), account, lease)
			if answer == "True" {
				require.NoError(t, err)
				require.False(t, pooled.isClosed())
				require.Len(t, conn.events, 1, "business response remains unread after probe completion")
			} else {
				require.Error(t, err)
				require.True(t, pooled.isClosed())
			}
			require.Len(t, conn.writes, 1)
			require.True(t, openAICookieWSIsProbePayload(conn.writes[0]))
			require.Equal(t, map[string]any{"effort": "low"}, conn.writes[0]["reasoning"])
			lease.Release()
			pooled.close()
		})
	}
}

func TestCookieWSFalseProbeClosesOnlyProbeAndPreservesUsage(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		close       bool
	}{{"probe", openAICookieWSProbePrompt, true}, {"ordinary", "Return False.", false}} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_stream_%v", tc.name, stream), func(t *testing.T) {
				conn := &openAIWSCaptureConn{events: [][]byte{
					[]byte(`{"type":"response.output_text.delta","delta":"False"}`),
					[]byte(`{"type":"response.completed","response":{"id":"resp_false","model":"gpt-6-astra","status":"completed","usage":{"input_tokens":33,"output_tokens":5}}}`),
				}}
				svc, account, _, _ := newCookieForwardFixture(t, conn)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": tc.input, "instructions": "", "stream": stream})
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.Equal(t, 33, result.Usage.InputTokens)
				require.Equal(t, 5, result.Usage.OutputTokens)
				require.Equal(t, tc.close, conn.closed)
				require.Len(t, conn.writes, 1, "False never replays the business request")
				if stream {
					require.Contains(t, rec.Body.String(), "False")
				}
			})
		}
	}
}

func TestCookieWSAdminFalseProbeClosesConnectionWithoutChangingResult(t *testing.T) {
	for _, intelligent := range []bool{false, true} {
		t.Run(fmt.Sprintf("intelligent_%v", intelligent), func(t *testing.T) {
			conn := &openAIWSCaptureConn{events: [][]byte{cookieWSCompletion("gpt-6-astra", "False")}}
			gateway, account, _, _ := newCookieForwardFixture(t, conn)
			svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			ctx := context.Background()
			prompt := openAICookieWSProbePrompt
			if intelligent {
				ctx = context.WithValue(ctx, intelligentRunKey{}, &intelligentRunContext{prompt: prompt, capture: &intelligentCapture{}})
				prompt = "hi"
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/admin/accounts/test", nil).WithContext(ctx)
			err := svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", prompt)
			require.NoError(t, err)
			require.True(t, conn.closed)
			require.Contains(t, rec.Body.String(), "False")
			require.Contains(t, rec.Body.String(), `"success":true`)
			require.Len(t, conn.writes, 1)
		})
	}
}

func TestCookieWSIngressFalseProbeClosesAfterCompleted(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{cookieWSCompletion("gpt-6-astra", "False")}}
	svc, account, _, dialer := newCookieForwardFixture(t, conn)
	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := coderws.Accept(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		defer ws.CloseNow()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r
		_, first, err := ws.Read(r.Context())
		if err != nil {
			errCh <- err
			return
		}
		errCh <- svc.ProxyResponsesWebSocketFromClient(r.Context(), c, ws, account, "test-token", first, nil)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	probe, _ := json.Marshal(openAICookieWSProbePayload(true))
	require.NoError(t, client.Write(ctx, coderws.MessageText, probe))
	_, response, err := client.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(response), "False")
	require.Contains(t, string(response), "response.completed")
	_ = client.Close(coderws.StatusNormalClosure, "done")
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("ingress close timed out")
	}
	require.True(t, conn.closed)
	require.Len(t, conn.writes, 1)
	require.Equal(t, 1, dialer.DialCount())
}

func TestCookieWSNewConnectionProbeUsageDoesNotEnterBusinessResult(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.completed","response":{"id":"resp_validation","model":"gpt-6-astra","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"True"}]}],"usage":{"input_tokens":333,"output_tokens":555}}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_business","model":"gpt-6-astra","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"business answer"}]}],"usage":{"input_tokens":7,"output_tokens":9}}}`),
	}}
	svc, account, _, _ := newCookieForwardFixture(t, conn)
	svc.getOpenAIWSConnPool().SetCookieValidator(svc.validateOpenAICookieWSBusinessConn)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","input":"A business request","stream":false}`))
	require.NoError(t, err)
	require.Equal(t, "resp_business", result.RequestID)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 9, result.Usage.OutputTokens)
	require.NotContains(t, rec.Body.String(), "resp_validation")
	require.NotContains(t, rec.Body.String(), "True")
	require.Contains(t, rec.Body.String(), "business answer")
	require.Len(t, conn.writes, 2)
}

func TestCookieWSBusinessValidatorRechecksCurrentAccountBeforeProbe(t *testing.T) {
	for _, tc := range []string{"disabled", "repo_error"} {
		t.Run(tc, func(t *testing.T) {
			conn := &openAIWSCaptureConn{events: [][]byte{cookieWSCompletion("gpt-6-astra", "True")}}
			svc, account, _, _ := newCookieForwardFixture(t, conn)
			repo := svc.accountRepo.(*cookieWSLifecycleRepo)
			if tc == "disabled" {
				repo.accounts[0].Status = StatusDisabled
			} else {
				repo.getErr = errors.New("repository unavailable")
			}
			pooled := newOpenAIWSConn("validation-state", account.ID, conn, nil)
			require.True(t, pooled.tryAcquire())
			lease := &openAIWSConnLease{conn: pooled, accountID: account.ID}
			err := svc.validateOpenAICookieWSBusinessConn(context.Background(), account, lease)
			require.ErrorIs(t, err, errOpenAICookieWSAccountUnavailable)
			require.Empty(t, conn.writes)
			require.True(t, conn.closed)
		})
	}
}

func TestCookieWSBusinessValidatorUpstreamStopsFurtherProbes(t *testing.T) {
	for _, tc := range []struct{ name, event string }{
		{"rate_limit", `{"type":"error","error":{"status":429,"type":"rate_limit_exceeded","message":"rate limited"}}`},
		{"quota", `{"type":"response.failed","response":{"error":{"status":429,"type":"usage_limit_reached","message":"usage limit reached"}}}`},
		{"unauthorized", `{"type":"error","error":{"status":401,"type":"authentication_error","message":"unauthorized"}}`},
		{"forbidden", `{"type":"error","error":{"status":403,"type":"permission_error","message":"forbidden"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &openAIWSCaptureConn{events: [][]byte{[]byte(tc.event)}}
			svc, account, _, _ := newCookieForwardFixture(t, conn)
			pooled := newOpenAIWSConn("validation-limit", account.ID, conn, nil)
			require.True(t, pooled.tryAcquire())
			lease := &openAIWSConnLease{conn: pooled, accountID: account.ID}
			require.Error(t, svc.validateOpenAICookieWSBusinessConn(context.Background(), account, lease))
			require.True(t, conn.closed)
			require.Len(t, conn.writes, 1)
			_, err := svc.latestOpenAICookieWSAccount(context.Background(), account.ID)
			if tc.name == "quota" {
				// A reported exhausted quota can still make this Cookie WS
				// candidate ineligible, but must not pause the account globally.
				require.ErrorIs(t, err, errOpenAICookieWSAccountUnavailable)
			} else {
				require.NoError(t, err)
			}
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account), "Cookie validation failure must not pause the account")
			repo := svc.accountRepo.(*cookieWSLifecycleRepo)
			current, getErr := repo.GetByID(context.Background(), account.ID)
			require.NoError(t, getErr)
			require.Equal(t, StatusActive, current.Status)
			require.True(t, current.Schedulable)
			if tc.name == "quota" {
				require.Equal(t, 100.0, repo.updates[account.ID]["codex_7d_used_percent"])
			}
		})
	}
}

func TestCookieWSRejectedNewConnectionFailsOverBeforeBusiness(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{cookieWSCompletion("gpt-6-astra", "False")}}
	svc, account, _, _ := newCookieForwardFixture(t, conn)
	svc.getOpenAIWSConnPool().SetCookieValidator(svc.validateOpenAICookieWSBusinessConn)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","input":"A business request","stream":false}`))
	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusServiceUnavailable, failover.StatusCode)
	require.False(t, c.Writer.Written(), "safe account failover remains possible before any business response")
	require.True(t, conn.closed)
	require.Len(t, conn.writes, 1, "only the internal probe was sent")
	require.True(t, openAICookieWSIsProbePayload(conn.writes[0]))
	require.Empty(t, svc.httpUpstream.(*httpUpstreamRecorder).requests)
}
