//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOAuthReliabilityContractSchemaTypedErrorAcrossHTTPStreams(t *testing.T) {
	const schema = `{"type":"object","properties":{"values":{"type":"array","items":{"type":"string","pattern":"^[a-z]+$"},"uniqueItems":true}},"required":["values"],"additionalProperties":false}`
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, errorType := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/type_only=%t", route, stream, errorType), func(t *testing.T) {
					object := `{"code":"invalid_json_schema","param":"text.format.schema","message":"Invalid schema for response_format 'synthetic': uniqueItems is not permitted when strict is true"}`
					if errorType {
						object = strings.Replace(object, `"code":`, `"type":`, 1)
					}
					upstream := &httpUpstreamRecorder{responses: []*http.Response{oauthReliabilityContractSSE(oauthReliabilityContractFailed(object, ""))}}
					svc := newOpenAIRejectedFieldTestService(upstream)
					svc.toolCorrector = NewCodexToolCorrector()
					account := newOpenAIOAuthNamespaceTestAccount()
					account.Extra["openai_passthrough"] = route == "passthrough"
					body := oauthReliabilityContractBody(route, stream)
					var err error
					switch route {
					case "chat":
						body, err = sjson.SetRawBytes(body, "response_format", []byte(`{"type":"json_schema","json_schema":{"name":"synthetic","strict":true,"schema":`+schema+`}}`))
					case "messages":
						body, err = sjson.SetRawBytes(body, "output_config.format", []byte(`{"type":"json_schema","name":"synthetic","strict":true,"schema":`+schema+`}`))
					default:
						body, err = sjson.SetRawBytes(body, "text.format", []byte(`{"type":"json_schema","name":"synthetic","strict":true,"schema":`+schema+`}`))
					}
					require.NoError(t, err)
					c, rec := oauthReliabilityContractContext("/v1/"+route, body)
					_, err = oauthReliabilityContractForward(svc, c, account, route, body)
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.NotErrorAs(t, err, &failover)
					require.Len(t, upstream.bodies, 1, "schema rejection cannot remove constraints and retry")
					require.True(t, gjson.GetBytes(upstream.bodies[0], "text.format.strict").Bool())
					require.Equal(t, oauthContractSchemaValue(t, schema), oauthContractSchemaValue(t, gjson.GetBytes(upstream.bodies[0], "text.format.schema").Raw))
					if rec.Code == http.StatusOK {
						require.True(t, stream)
						require.Equal(t, http.StatusBadRequest, c.GetInt(OpsUpstreamStatusCodeKey))
					} else {
						require.Equal(t, http.StatusBadRequest, rec.Code)
					}
					require.Contains(t, rec.Body.String(), "uniqueItems")
					require.False(t, errors.As(err, &failover))
				})
			}
		}
	}
}

func TestOAuthReliabilityContractSchemaErrorDoesNotCaptureDedicatedWSFamilies(t *testing.T) {
	for _, object := range []string{
		`{"type":"invalid_request_error","code":"model_not_found","param":"model","message":"The model does not exist"}`,
		`{"type":"invalid_request_error","code":"previous_response_not_found","message":"previous response not found"}`,
		`{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"encrypted content could not be verified"}`,
		`{"type":"invalid_request_error","code":"context_length_exceeded","message":"maximum context length exceeded"}`,
		`{"type":"invalid_request_error","code":"cyber_policy","message":"blocked by cyber policy"}`,
		`{"type":"image_generation_user_error","code":"invalid_value","param":"output_format","message":"format unavailable"}`,
		`{"code":"opaque_provider_failure","param":"text.format.schema","message":"Invalid schema for response_format 'synthetic': uniqueItems is not permitted when strict is true"}`,
		`{"code":"invalid_json_schema","code":"opaque_provider_failure","message":"ambiguous"}`,
	} {
		t.Run(gjson.Get(object, "code").String(), func(t *testing.T) {
			for _, event := range []string{`{"type":"error","error":` + object + `}`, oauthReliabilityContractFailed(object, "")} {
				require.False(t, openAIWSKnownParameterError([]byte(event)), "dedicated families and unknown/ambiguous errors keep their existing policy")
			}
		})
	}
}

func oauthContractSchemaValue(t *testing.T, raw string) any {
	t.Helper()
	require.True(t, json.Valid([]byte(raw)), "actual schema must remain JSON")
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	require.NoError(t, decoder.Decode(&value))
	return value
}

type oauthContractSchemaSettlementConn struct {
	*openAIWSCaptureConn
	cancel            context.CancelFunc
	observedDeadlines []time.Time
	reads             int
}

func (c *oauthContractSchemaSettlementConn) ReadMessage(ctx context.Context) ([]byte, error) {
	c.reads++
	if c.reads > 1 {
		if deadline, ok := ctx.Deadline(); ok {
			c.observedDeadlines = append(c.observedDeadlines, deadline)
		}
	}
	if c.reads <= 3 {
		return c.openAIWSCaptureConn.ReadMessage(ctx)
	}
	if c.cancel != nil {
		c.cancel()
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestOAuthReliabilityContractPrewarmSchemaSettlementDeadlineAndCancel(t *testing.T) {
	for _, mode := range []string{"absolute_settlement_limit", "short_request_deadline", "canceled_after_actual_usage"} {
		t.Run(mode, func(t *testing.T) {
			bare := `{"type":"error","code":"invalid_json_schema","param":"text.format.schema","message":"Synthetic strict schema rejection"}`
			actual := `{"type":"response.in_progress","response":{"id":"resp_schema_deadline","usage":{"input_tokens":5,"output_tokens":1}}}`
			metadata := `{"type":"response.in_progress","response":{"id":"resp_schema_deadline"}}`
			conn := &oauthContractSchemaSettlementConn{openAIWSCaptureConn: &openAIWSCaptureConn{events: [][]byte{[]byte(bare), []byte(actual), []byte(metadata)}}}
			svc, account, httpUpstream := httpBridgeFixture(t, "http://127.0.0.1:1")
			svc.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = true
			svc.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 900
			svc.openaiWSPool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: conn})
			c, rec := httpBridgeContext("schema-settlement-" + mode)
			var ctx context.Context
			var cancel context.CancelFunc
			if mode == "short_request_deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 80*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			if mode == "canceled_after_actual_usage" {
				conn.cancel = cancel
			}
			c.Request = c.Request.WithContext(ctx)
			c.Request.Header.Set("User-Agent", "synthetic-schema-settlement")
			body := []byte(`{"model":"gpt-6-astra","stream":false,"input":"synthetic request","text":{"format":{"type":"json_schema","name":"synthetic","strict":true,"schema":{"type":"object","properties":{"values":{"type":"array","items":{"type":"string"},"uniqueItems":true}},"required":["values"],"additionalProperties":false}}}}`)
			started := time.Now()
			result, err := svc.Forward(ctx, c, account, body)
			elapsed := time.Since(started)
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.NotNil(t, result)
			require.Equal(t, 5, result.Usage.InputTokens)
			require.Equal(t, 1, result.Usage.OutputTokens)
			require.Len(t, conn.writes, 1, "observed work must never be replayed after timeout or cancellation")
			require.Zero(t, httpUpstream.callCount)
			require.Len(t, conn.observedDeadlines, 3)
			require.Equal(t, conn.observedDeadlines[0], conn.observedDeadlines[1], "metadata cannot refresh settlement deadline")
			require.Equal(t, conn.observedDeadlines[0], conn.observedDeadlines[2])
			require.LessOrEqual(t, conn.observedDeadlines[0].Sub(started), 2100*time.Millisecond, "900-second inference timeout is not the error-settlement budget")
			require.Less(t, elapsed, 4*time.Second)
			switch mode {
			case "absolute_settlement_limit":
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.Nil(t, ctx.Err())
				require.GreaterOrEqual(t, elapsed, 1800*time.Millisecond)
			case "short_request_deadline":
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Less(t, elapsed, time.Second)
				require.False(t, c.Writer.Written())
				require.Nil(t, c.Keys[OpsUpstreamErrorsKey], "cancellation must not invent a new parameter Ops event")
			case "canceled_after_actual_usage":
				require.ErrorIs(t, err, context.Canceled)
				require.False(t, c.Writer.Written())
				require.Nil(t, c.Keys[OpsUpstreamErrorsKey])
			}
		})
	}
}
