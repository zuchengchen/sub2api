package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const oauthReliabilityModelUnavailableSentence = "The 'gpt-5.4-mini' model is not supported when using Codex with a ChatGPT account."
const oauthReliabilityModelUnavailableDetail = `{"detail":"` + oauthReliabilityModelUnavailableSentence + `"}`
const oauthReliabilityModelUnavailableStructured = `{"error":{"type":"invalid_request_error","code":"model_not_found","param":"model","message":"Unavailable model"}}`

func TestOAuthReliabilityModelUnavailableStrictEvidence(t *testing.T) {
	for _, body := range []string{
		oauthReliabilityModelUnavailableDetail,
		oauthReliabilityModelUnavailableStructured,
		`{"error":{"code":"model_not_found","param":null,"message":"Unavailable"}}`,
		`{"error":{"type":"invalid_request_error","code":"unsupported_model","param":"model","message":"Unavailable"}}`,
		`{"error":{"type":"invalid_request_error","code":"model_not_available","param":"model","message":"Unavailable"}}`,
		`{"error":{"type":"invalid_request_error","code":"invalid_model","param":"model","message":"Unavailable"}}`,
		`{"error":{"type":"invalid_request_error","code":"invalid_value","param":"model","message":"` + oauthReliabilityModelUnavailableSentence + `"}}`,
		`{"type":"error","code":"model_not_found","param":"model","message":"Unavailable","status":400}`,
		`{"type":"response.failed","response":{"status":"failed","error":{"type":"invalid_request_error","code":"model_not_found","param":"model"}}}`,
		`{"detail":"` + oauthReliabilityModelUnavailableSentence + `","status":400,"status_code":"400"}`,
		`{"error":{"type":"invalid_request_error","message":"The 'future-model-2050' model is not supported when using Codex with a ChatGPT account."}}`,
	} {
		for _, status := range []int{http.StatusBadRequest, http.StatusBadGateway} {
			require.True(t, isOpenAIModelUnavailableError(status, []byte(body)), "status=%d body=%s", status, body)
		}
		for _, status := range []int{200, 401, 403, 404, 413, 422, 429, 500, 503} {
			require.False(t, isOpenAIModelUnavailableError(status, []byte(body)), "status=%d", status)
		}
	}
}

func TestOAuthReliabilityModelUnavailableRejectsQuotedAndConflictingEvidence(t *testing.T) {
	quoted, err := json.Marshal(oauthReliabilityModelUnavailableStructured)
	require.NoError(t, err)
	for _, body := range []string{
		`{"error":{"type":"invalid_request_error","message":"Bad Request"}}`,
		`{"error":{"message":"Unknown model in an example"}}`,
		`{"error":{"message":` + string(quoted) + `}}`,
		`{"error":{"message":"Example: ` + oauthReliabilityModelUnavailableSentence + `"}}`,
		`{"error":{"message":"` + oauthReliabilityModelUnavailableSentence + ` This is only an example."}}`,
		`{"error":{"code":"model_not_found","param":"input","message":"Invalid input"}}`,
		`{"error":{"code":"model_not_found","type":"server_error","message":"Unavailable"}}`,
		`{"error":{"code":"model_not_found","status":503}}`,
		`{"error":{"code":"model_not_found","status":400},"status_code":502}`,
		`{"error":{"code":"model_not_found"},"code":"server_is_overloaded"}`,
		`{"error":{"code":"model_not_found"},"detail":"A conflicting rejection"}`,
		`{"error":{"code":"invalid_api_key","param":"model","message":"` + oauthReliabilityModelUnavailableSentence + `"}}`,
		`{"error":{"code":"invalid_model","message":"The model data are invalid"}}`,
		`{"error":{"code":"model_not_found","code":"invalid_value"}}`,
		`{"error":{"code":"invalid_value","c\u006fde":"model_not_found"}}`,
		`{"error":{"code":"model_not_found"},"extra":{"nested":{"x":1,"x":2}}}`,
		`{"error":null,"error":{"code":"model_not_found"}}`,
		`{"type":"response.completed","response":{"status":"completed","error":{"code":"model_not_found"}}}`,
		`{"object":"response","status":"completed","error":{"code":"model_not_found"}}`,
		`{"error":{"code":"model_not_found"},"response":{"status":"completed"}}`,
		`{"error":{"code":"model_not_found"},"response":"completed"}`,
		`{"detail":"` + oauthReliabilityModelUnavailableSentence + `","error":{"code":"invalid_value"}}`,
		`{"detail":"` + oauthReliabilityModelUnavailableSentence + `","output":[{"type":"message"}]}`,
		`{"detail":"` + oauthReliabilityModelUnavailableSentence + `","request":{"model":"missing"}}`,
		`{"detail":"` + oauthReliabilityModelUnavailableSentence + `","status":400,"status_code":502}`,
		`{"detail":["` + oauthReliabilityModelUnavailableSentence + `"]}`,
		`{"detail":"The '' model is not supported when using Codex with a ChatGPT account."}`,
		`{"detail":"The 'quoted model' model is not supported when using Codex with a ChatGPT account."}`,
		`{"detail":"The 'a' model is not supported when using Codex with a ChatGPT account.\n"}`,
		`{"detail":"The '` + strings.Repeat("a", 129) + `' model is not supported when using Codex with a ChatGPT account."}`,
		`{"request":{"error":{"code":"model_not_found","param":"model"}}}`,
		oauthReliabilityModelUnavailableSentence,
		string(quoted),
		`{"error":`,
		`[]`,
	} {
		for _, status := range []int{400, 502} {
			require.False(t, isOpenAIModelUnavailableError(status, []byte(body)), "status=%d body=%s", status, body)
		}
	}
}

func TestOAuthReliabilityModelUnavailableBoundedParsing(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"error":{"code":"model_not_found"},"x":"` + strings.Repeat("x", maxOpenAIErrorNormalizationBytes) + `"}`),
		[]byte(`{"error":{"code":"model_not_found"},"x":` + strings.Repeat("[", maxOpenAIErrorJSONDepth+1) + `0` + strings.Repeat("]", maxOpenAIErrorJSONDepth+1) + `}`),
		[]byte(`{"error":{"code":"model_not_found"},"x":[` + strings.Repeat("0,", maxOpenAIErrorJSONFields) + `0]}`),
		append([]byte(`{"error":{"code":"model_not_found"},"x":"`), 0xff, '"', '}'),
	} {
		before := bytes.Clone(body)
		require.False(t, isOpenAIModelUnavailableError(http.StatusBadRequest, body))
		require.Equal(t, before, body, "declining classification must not truncate or rewrite the payload")
	}
}

func TestOAuthReliabilityModelUnavailableFailoverPreservesTransport(t *testing.T) {
	for _, status := range []int{400, 502} {
		for _, body := range []string{oauthReliabilityModelUnavailableDetail, oauthReliabilityModelUnavailableStructured} {
			headers := http.Header{"X-Request-Id": []string{"synthetic-model-rejection"}}
			err := newOpenAIUpstreamFailoverError(status, headers, []byte(body), "servers are overloaded", true)
			require.True(t, IsOpenAIModelUnavailableFailover(err))
			require.Equal(t, status, err.StatusCode)
			require.Equal(t, []byte(body), err.ResponseBody)
			require.Equal(t, http.StatusBadRequest, err.ClientStatusCode)
			require.Equal(t, OpenAIModelUnavailableClientMessage, err.ClientMessage)
			require.Equal(t, GatewayFailureScopeAccount, err.Scope)
			require.True(t, err.ShouldRetryNextAccount())
			require.False(t, err.RetryableOnSameAccount)
			require.False(t, err.RequestScopedTransient)
			require.Zero(t, err.SameAccountRetryDelay)
			require.False(t, err.IsCredentialFailure())
			headers.Set("X-Request-Id", "changed")
			require.Equal(t, "synthetic-model-rejection", err.ResponseHeaders.Get("X-Request-Id"))
		}
	}
	for _, body := range []string{`{"error":{"message":"Bad Request"}}`, `{"error":{"code":"server_error","message":"The unknown model is not supported"}}`} {
		err := newOpenAIUpstreamFailoverError(http.StatusBadGateway, nil, []byte(body), "", false)
		require.False(t, IsOpenAIModelUnavailableFailover(err))
		require.Zero(t, err.ClientStatusCode)
	}
	require.False(t, IsOpenAIModelUnavailableFailover(nil))
	require.False(t, IsOpenAIModelUnavailableFailover(&UpstreamFailoverError{ClientStatusCode: http.StatusBadRequest}))
	clientBody := OpenAIModelUnavailableClientErrorBody()
	require.True(t, json.Valid(clientBody))
	require.Equal(t, "model", gjson.GetBytes(clientBody, "error.param").String())
	require.Equal(t, "model_not_found", gjson.GetBytes(clientBody, "error.code").String())
	require.Equal(t, "invalid_request_error", gjson.GetBytes(clientBody, "error.type").String())
	require.NotContains(t, string(clientBody), "gpt-5.4-mini")
}

func TestOAuthReliabilityModelUnavailableRoutingScopeAndProgress(t *testing.T) {
	managed := &OpenAIGatewayService{accountRepo: &modelNotFoundManagedAccountRepo{}}
	unmanaged := &OpenAIGatewayService{}
	for _, body := range []string{oauthReliabilityModelUnavailableDetail, oauthReliabilityModelUnavailableStructured,
		`{"error":{"type":"invalid_request_error","code":"unsupported_model","param":"model"}}`} {
		for _, account := range []*Account{
			{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			{Platform: PlatformDeepseek, Type: AccountTypeAPIKey},
		} {
			require.True(t, managed.shouldFailoverOpenAIUpstreamResponse(account, 400, "", []byte(body)))
			require.False(t, unmanaged.shouldFailoverOpenAIUpstreamResponse(account, 400, "", []byte(body)))
		}
		require.False(t, managed.shouldFailoverOpenAIUpstreamResponse(&Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}, 400, "", []byte(body)))
		require.False(t, managed.shouldFailoverOpenAIUpstreamResponse(nil, 400, "", []byte(body)))
	}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, extra := range []string{
		`"usage":{"input_tokens":1}`,
		`"usage":{"output_tokens":2}`,
		`"output":[{"type":"function_call","call_id":"call_once","name":"work","arguments":"{}"}]`,
		`"tool_usage":{"web_search":{"count":1}}`,
	} {
		body := []byte(strings.TrimSuffix(oauthReliabilityModelUnavailableStructured, "}") + "," + extra + "}")
		for _, status := range []int{400, 502} {
			require.True(t, isOpenAIModelUnavailableError(status, body), "classification remains available for the final error")
			require.False(t, managed.shouldFailoverOpenAIUpstreamResponse(account, status, "", body), "progress cannot grant replay: %s", extra)
		}
	}
	for _, body := range []string{
		`{"error":{"type":"invalid_request_error","message":"Bad Request"}}`,
		`{"error":{"code":"invalid_value","param":"input","message":"Bad Request"},"request":{"text":"` + oauthReliabilityModelUnavailableSentence + `"}}`,
	} {
		require.False(t, managed.shouldFailoverOpenAIUpstreamResponse(account, 400, "", []byte(body)))
	}
}

func oauthReliabilityModelUnavailableForwardFixture(t *testing.T, oauth, passthrough bool, responses ...*http.Response) (*OpenAIGatewayService, *Account, *gin.Context, *httptest.ResponseRecorder, *httpUpstreamRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "curl/8.0")
	account := newOpenAIRejectedFieldTestAccount()
	if oauth {
		account = newOpenAIOAuthNamespaceTestAccount()
	}
	account.Extra["openai_passthrough"] = passthrough
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeOff
	account.Credentials["pool_mode"] = true
	account.Credentials["pool_mode_retry_status_codes"] = []any{float64(400), float64(502)}
	upstream := &httpUpstreamRecorder{responses: responses}
	svc := newOpenAIRejectedFieldTestService(upstream)
	svc.accountRepo = &modelNotFoundManagedAccountRepo{}
	return svc, account, c, rec, upstream
}

func TestOAuthReliabilityModelUnavailableForwardKeepsAccountRouting(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4-mini","instructions":"test","input":"hello","stream":false}`)
	for _, oauth := range []bool{false, true} {
		for _, passthrough := range []bool{false, true} {
			for _, status := range []int{400, 502} {
				for index, body := range []string{oauthReliabilityModelUnavailableDetail, oauthReliabilityModelUnavailableStructured} {
					t.Run(fmt.Sprintf("oauth_%t/pass_%t/status_%d/shape_%d", oauth, passthrough, status, index), func(t *testing.T) {
						svc, account, c, rec, upstream := oauthReliabilityModelUnavailableForwardFixture(t, oauth, passthrough, newOpenAIRejectedFieldTestResponse(status, body))
						result, err := svc.Forward(context.Background(), c, account, request)
						require.Nil(t, result, "rejection without usage must not fabricate billing")
						var failover *UpstreamFailoverError
						require.ErrorAs(t, err, &failover)
						require.True(t, IsOpenAIModelUnavailableFailover(failover))
						require.Equal(t, status, failover.StatusCode)
						require.Equal(t, http.StatusBadRequest, failover.ClientStatusCode)
						require.True(t, failover.ShouldRetryNextAccount())
						require.False(t, failover.RetryableOnSameAccount)
						require.Len(t, upstream.bodies, 1, "same-account rejection is deterministic")
						require.False(t, c.Writer.Written())
						require.Empty(t, rec.Body.String())
						require.Equal(t, "gpt-5.4-mini", gjson.GetBytes(upstream.lastBody, "model").String())
						events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
						require.Equal(t, status, events[len(events)-1].UpstreamStatusCode, "diagnostics retain actual transport")
					})
				}
			}
		}
	}
}

func TestOAuthReliabilityModelUnavailableNextAccountCanSucceed(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4-mini","instructions":"test","input":"hello","stream":false}`)
	const success = `{"id":"resp_ok","object":"response","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}`
	for _, passthrough := range []bool{false, true} {
		svc, account, c, rec, upstream := oauthReliabilityModelUnavailableForwardFixture(t, true, passthrough,
			newOpenAIRejectedFieldTestResponse(400, oauthReliabilityModelUnavailableDetail), newOpenAIRejectedFieldTestResponse(200, success))
		result, err := svc.Forward(context.Background(), c, account, request)
		require.Nil(t, result)
		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover)
		require.True(t, failover.ShouldRetryNextAccount())
		require.False(t, c.Writer.Written())
		// Model availability is per account: the caller can select another
		// credential class without changing the request's model or reasoning.
		apiAccount := newOpenAIRejectedFieldTestAccount()
		apiAccount.Extra["openai_passthrough"] = passthrough
		result, err = svc.Forward(context.Background(), c, apiAccount, request)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, OpenAIUsage{InputTokens: 2, OutputTokens: 1}, result.Usage)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Len(t, upstream.bodies, 2)
		for _, body := range upstream.bodies {
			require.Equal(t, "gpt-5.4-mini", gjson.GetBytes(body, "model").String())
			require.False(t, gjson.GetBytes(body, "reasoning.effort").Exists())
		}
	}
}

func TestOAuthReliabilityModelUnavailableUnknown400IsTerminal(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4-mini","instructions":"test","input":"hello","stream":false}`)
	for _, passthrough := range []bool{false, true} {
		svc, account, c, rec, upstream := oauthReliabilityModelUnavailableForwardFixture(t, false, passthrough,
			newOpenAIRejectedFieldTestResponse(400, `{"error":{"type":"invalid_request_error","code":"invalid_value","param":"input","message":"Bad Request"},"request":{"text":"`+oauthReliabilityModelUnavailableSentence+`"}}`))
		// No explicit pool-mode retry override in the unknown-error control.
		delete(account.Credentials, "pool_mode")
		delete(account.Credentials, "pool_mode_retry_status_codes")
		result, err := svc.Forward(context.Background(), c, account, request)
		require.Nil(t, result)
		require.Error(t, err)
		var failover *UpstreamFailoverError
		require.False(t, errors.As(err, &failover))
		require.Len(t, upstream.bodies, 1)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.NotEqual(t, "model_not_found", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
		if !passthrough {
			require.Equal(t, "input", gjson.GetBytes(rec.Body.Bytes(), "error.param").String())
			require.Equal(t, "invalid_value", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
		}
	}
}
