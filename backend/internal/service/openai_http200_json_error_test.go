package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const oauthReliabilityJSONSuccess = `{"id":"resp_ok","object":"response","status":"completed","error":null,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":2,"output_tokens":1}}`

func oauthReliabilityHTTP200JSONForward(t *testing.T, passthrough bool, request []byte, bodies ...string) (*gin.Context, *httptest.ResponseRecorder, *httpUpstreamRecorder, *OpenAIForwardResult, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(request))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Accept", "application/json")
	c.Request.Header.Set("User-Agent", "curl/8.0")
	upstream := &httpUpstreamRecorder{}
	for _, body := range bodies {
		upstream.responses = append(upstream.responses, newOpenAIRejectedFieldTestResponse(http.StatusOK, body))
	}
	account := newOpenAIOAuthNamespaceTestAccount()
	account.Extra["openai_passthrough"] = passthrough
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeOff
	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, account, request)
	return c, rec, upstream, result, err
}

func TestOAuthReliabilityHTTP200JSONStructuredFailure(t *testing.T) {
	request := []byte(`{"model":"gpt-5.6-sol","instructions":"test","input":"hello","stream":false}`)
	tests := []struct {
		name, body string
		status     int
	}{
		{"parameter", `{"error":{"type":"invalid_request_error","code":"invalid_value","param":"temperature","message":"Unsupported temperature"}}`, 400},
		{"bare", `{"type":"error","code":"unknown_parameter","param":"temperature","status":400,"message":"Unsupported temperature"}`, 400},
		{"generic400", `{"error":{"status":400,"message":"Bad Request"}}`, 400},
		{"unknown", `{"error":{"message":"upstream rejected request"}}`, 502},
		{"server", `{"error":{"type":"server_error","message":"Temporary upstream failure, please retry"}}`, 502},
		{"contradictory", `{"object":"response","status":"completed","error":{"type":"invalid_request_error","code":"invalid_value","message":"bad"},"usage":{"input_tokens":12}}`, 502},
		{"duplicate", `{"error":{"type":"invalid_request_error","type":"server_error","message":"bad"},"usage":{"input_tokens":12}}`, 502},
		{"hidden_duplicate", `{"error":null,"error":{"type":"invalid_request_error","message":"bad"},"usage":{"input_tokens":12}}`, 502},
	}
	for _, passthrough := range []bool{false, true} {
		for _, tt := range tests {
			t.Run(tt.name+map[bool]string{true: "/passthrough", false: "/native"}[passthrough], func(t *testing.T) {
				c, rec, upstream, result, err := oauthReliabilityHTTP200JSONForward(t, passthrough, request, tt.body)
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "HTTP 200 JSON cannot grant generic failover")
				require.Nil(t, result, "no proven usage means no billable partial result")
				require.Len(t, upstream.bodies, 1)
				require.Equal(t, tt.status, rec.Code)
				require.Equal(t, tt.status, c.GetInt(OpsUpstreamStatusCodeKey))
				events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
				require.Equal(t, tt.status, events[len(events)-1].UpstreamStatusCode)
				if tt.name == "parameter" || tt.name == "bare" {
					require.Equal(t, "temperature", gjson.GetBytes(rec.Body.Bytes(), "error.param").String())
					require.NotEmpty(t, gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
				}
			})
		}
	}
}

func TestOAuthReliabilityHTTP200JSONPreservesSuccessAndQuotedErrors(t *testing.T) {
	request := []byte(`{"model":"gpt-5.6-sol","instructions":"test","input":"hello","stream":false}`)
	quoted, err := json.Marshal(`{"error":{"type":"invalid_request_error","message":"Bad Request"}}`)
	require.NoError(t, err)
	for _, passthrough := range []bool{false, true} {
		for _, body := range []string{oauthReliabilityJSONSuccess, strings.Replace(oauthReliabilityJSONSuccess, `"text":"OK"`, `"text":`+string(quoted), 1)} {
			c, rec, upstream, result, err := oauthReliabilityHTTP200JSONForward(t, passthrough, request, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, OpenAIUsage{InputTokens: 2, OutputTokens: 1}, result.Usage)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, "completed", gjson.GetBytes(rec.Body.Bytes(), "status").String())
			require.Equal(t, gjson.Get(body, "output").Raw, gjson.GetBytes(rec.Body.Bytes(), "output").Raw)
			require.Zero(t, c.GetInt(OpsUpstreamStatusCodeKey))
		}
	}
}

func TestOAuthReliabilityHTTP200JSONPreservesOnlyActualUsage(t *testing.T) {
	request := rejectedMessageIDHistory(t, 15, 0)
	object := oauthReliabilityIDObject(t, 15, "item_BAD", "", "", true)
	for _, passthrough := range []bool{false, true} {
		body := `{"object":"response","id":"resp_failed","status":"failed","error":` + object + `,"usage":{"input_tokens":9,"output_tokens":2}}`
		_, rec, upstream, result, err := oauthReliabilityHTTP200JSONForward(t, passthrough, request, body)
		require.Error(t, err)
		require.NotNil(t, result)
		require.Equal(t, OpenAIUsage{InputTokens: 9, OutputTokens: 2}, result.Usage)
		require.Equal(t, "resp_failed", result.ResponseID)
		require.Len(t, upstream.bodies, 1, "real usage forbids targeted repair/replay")
		require.Equal(t, http.StatusBadRequest, rec.Code)
	}
}

func TestOAuthReliabilityHTTP200JSONExactRepairAndContradictoryControl(t *testing.T) {
	request := rejectedMessageIDHistory(t, 15, 0)
	object := oauthReliabilityIDObject(t, 15, "item_BAD", "", "", true)
	for _, passthrough := range []bool{false, true} {
		_, rec, upstream, result, err := oauthReliabilityHTTP200JSONForward(t, passthrough, request, `{"error":`+object+`}`, oauthReliabilityJSONSuccess)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Len(t, upstream.bodies, 2)
		require.Equal(t, "item_BAD", gjson.GetBytes(upstream.bodies[0], "input.15.id").String())
		require.False(t, gjson.GetBytes(upstream.bodies[1], "input.15.id").Exists())
		_, rec, upstream, result, err = oauthReliabilityHTTP200JSONForward(t, passthrough, request,
			`{"object":"response","status":"completed","error":`+object+`}`)
		require.Error(t, err)
		require.Nil(t, result)
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Len(t, upstream.bodies, 1, "completed+error is ambiguous and cannot authorize repair")
	}
}

func TestOAuthReliabilityHTTP200JSONAfterHeadersKeepsWireStatus(t *testing.T) {
	c, rec := newOpenAIUpstreamErrorTestContext(t)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	_, err := c.Writer.Write([]byte(": ping\n\n"))
	require.NoError(t, err)
	c.Writer.Flush()
	body := []byte(`{"error":{"type":"invalid_request_error","code":"invalid_value","param":"temperature","message":"bad"}}`)
	_, err, handled := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{}).handleOpenAIHTTP200JSONFailure(c,
		newOpenAIRejectedFieldTestResponse(http.StatusOK, string(body)), newOpenAIOAuthNamespaceTestAccount(), body, false, "gpt-5.6-sol")
	require.True(t, handled)
	require.Error(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "event: response.failed")
	streamError, ok := GetOpsStreamError(c)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, streamError.IntendedStatus)
	require.Contains(t, rec.Body.String(), `"param":"temperature"`)
}

func TestOAuthReliabilityWSErrorFieldsAndLogicalStatus(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte(`{"type":"error","code":"invalid_value","param":"input[15].id","message":"bad","status":400}`),
		[]byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"invalid_value","param":"input[15].id","message":"bad"}}}`),
	} {
		code, _, message := parseOpenAIWSErrorEventFields(payload)
		require.Equal(t, "invalid_value", code)
		require.Equal(t, "bad", message)
		c, _ := newOpenAIUpstreamErrorTestContext(t)
		markOpenAIWSClientVisibleFailure(c, gjson.GetBytes(payload, "type").String(), payload)
		streamError, ok := GetOpsStreamError(c)
		require.True(t, ok)
		require.Equal(t, http.StatusBadRequest, streamError.IntendedStatus)
		require.Equal(t, "invalid_value", streamError.Code)
	}
	code, typ, message := parseOpenAIWSErrorEventFields([]byte(`{"type":"error","code":"invalid_value","code":"server_error","message":"ambiguous"}`))
	require.Empty(t, code)
	require.Empty(t, typ)
	require.Empty(t, message)
}

func TestOAuthReliabilityWSSyntheticFailedPreservesBareFields(t *testing.T) {
	source := []byte(`{"type":"error","code":"invalid_value","param":"input[15].id","message":"bad","status":400,"sequence_number":7}`)
	failed := buildOpenAIResponseFailedEvent("resp_keep", "gpt-5.6-sol", 123, source, "fallback")
	require.Equal(t, "invalid_value", gjson.GetBytes(failed, "response.error.code").String())
	require.Equal(t, "input[15].id", gjson.GetBytes(failed, "response.error.param").String())
	require.Equal(t, "bad", gjson.GetBytes(failed, "response.error.message").String())
	require.Equal(t, int64(400), gjson.GetBytes(failed, "response.error.status").Int())
	require.Equal(t, http.StatusBadRequest, openAIStreamFailureStatus(failed, ""))
	bad := []byte(`{"type":"error","code":"invalid_value","code":"server_error","param":"input[15].id"}`)
	failed = buildOpenAIResponseFailedEvent("resp_keep", "gpt-5.6-sol", 123, bad, "fallback")
	require.Equal(t, "upstream_error", gjson.GetBytes(failed, "response.error.code").String())
	require.False(t, gjson.GetBytes(failed, "response.error.param").Exists())
}
