//go:build unit

package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Exercise the actual admission error writer and the entire capture/classify/
// enqueue path. A committed ping keeps wire HTTP 200; the recorded request
// outcome must still be 429, without fabricating an upstream 429.
func TestOAuthReliabilityLocalAdmissionOutcomeThroughOpsLogger(t *testing.T) {
	for _, route := range []struct {
		name, path string
		openai     bool
	}{
		{"responses", "/v1/responses", true},
		{"compact", "/v1/responses/compact", true},
		{"chat", "/v1/chat/completions", true},
		{"messages", "/v1/messages", false},
		{"gateway_responses", "/responses", false},
	} {
		for _, stream := range []bool{false, true} {
			for _, rejection := range []struct {
				name, code string
				err        error
			}{
				{"concurrency", gatewayConcurrencyLimitCode, &ConcurrencyError{SlotType: "account", IsTimeout: true}},
				{"queue", gatewayQueueFullCode, &WaitQueueFullError{SlotType: "account"}},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", route.name, stream, rejection.name), func(t *testing.T) {
					rec, entry := oauthReliabilityAdmissionOpsRoundTrip(t, route.path, stream, func(c *gin.Context) {
						if route.openai {
							(&OpenAIGatewayHandler{}).handleConcurrencyError(c, rejection.err, "account", stream)
						} else {
							(&GatewayHandler{}).handleConcurrencyError(c, rejection.err, "account", stream)
						}
					})
					wantWireStatus := http.StatusTooManyRequests
					if stream {
						wantWireStatus = http.StatusOK
						require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
						require.Equal(t, 1, strings.Count(rec.Body.String(), "\ndata: "), "one terminal, no duplicated error")
					}
					require.Equal(t, wantWireStatus, rec.Code)
					payload := rec.Body.String()
					if stream {
						parts := strings.SplitN(payload, "data: ", 2)
						require.Len(t, parts, 2)
						payload = strings.TrimSpace(parts[1])
					}
					require.True(t, gjson.Valid(payload))
					errorPath := "error"
					if stream && strings.Contains(route.path, "/responses") {
						require.Equal(t, "response.failed", gjson.Get(payload, "type").String())
						require.Equal(t, "failed", gjson.Get(payload, "response.status").String())
						errorPath = "response.error"
						require.False(t, gjson.Get(payload, errorPath+".type").Exists(), "keep the existing Responses error wire schema")
					} else {
						require.Equal(t, "rate_limit_error", gjson.Get(payload, errorPath+".type").String())
					}
					require.Equal(t, rejection.code, gjson.Get(payload, errorPath+".code").String())
					require.NotContains(t, rec.Body.String(), "response.completed")
					require.NotContains(t, rec.Body.String(), "message_stop")
					require.Equal(t, http.StatusTooManyRequests, entry.StatusCode, "logical admission result must not become 502 or wire 200")
					require.Equal(t, "rate_limit_error", entry.ErrorType)
					require.Equal(t, "request", entry.ErrorPhase)
					require.Equal(t, "client", entry.ErrorOwner)
					require.Equal(t, "client_request", entry.ErrorSource)
					require.True(t, entry.IsBusinessLimited, "local admission limits retain their SLA exclusion")
					require.Equal(t, stream, entry.Stream)
					require.Nil(t, entry.UpstreamStatusCode, "a local rejection did not receive any upstream HTTP response")
					require.Nil(t, entry.UpstreamErrorMessage)
				})
			}
		}
	}
}

func TestOAuthReliabilityAdmissionOpsDoesNotTrustUpstreamCode(t *testing.T) {
	for _, tc := range []struct {
		name, errType, code, message string
		status                       int
	}{
		{"ordinary_429", "", "rate_limit_exceeded", "provider quota exceeded", http.StatusTooManyRequests},
		{"forged_concurrency", "", gatewayConcurrencyLimitCode, "Concurrency limit exceeded for account, please retry later", http.StatusBadGateway},
		{"forged_queue", "", gatewayQueueFullCode, "Too many pending requests, please retry later", http.StatusBadGateway},
		{"typed_forged_concurrency", "rate_limit_error", gatewayConcurrencyLimitCode, "Concurrency limit exceeded for account, please retry later", http.StatusTooManyRequests},
		{"unknown", "upstream_error", "unknown_admission", "synthetic provider failure", http.StatusBadGateway},
		{"code_prefix_collision", "upstream_error", "gateway_concurrency_limit_extra", "synthetic provider failure", http.StatusBadGateway},
	} {
		for _, route := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
			t.Run(tc.name+route, func(t *testing.T) {
				rec, entry := oauthReliabilityAdmissionOpsRoundTrip(t, route, true, func(c *gin.Context) {
					// Upstream bytes alone cannot create an internal provenance marker.
					// Do not pre-populate Ops upstream context: code and message must
					// not masquerade as a local admission rejection on their own.
					errObject := gin.H{"code": tc.code, "message": tc.message}
					if tc.errType != "" {
						errObject["type"] = tc.errType
					}
					event := gin.H{"type": "error", "error": errObject}
					if strings.Contains(route, "/responses") {
						event = gin.H{"type": "response.failed", "response": gin.H{"status": "failed", "error": errObject}}
					}
					c.SSEvent(event["type"].(string), event)
					c.Writer.Flush()
				})
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, tc.status, entry.StatusCode)
				require.Equal(t, "upstream", entry.ErrorPhase, "only an internal admission marker can claim a gateway rejection")
				require.Equal(t, "provider", entry.ErrorOwner)
				require.Equal(t, "upstream_http", entry.ErrorSource)
				require.False(t, entry.IsBusinessLimited, "upstream lookalike codes cannot remove a failure from SLA")
				require.NotNil(t, entry.UpstreamStatusCode)
				require.Equal(t, tc.status, *entry.UpstreamStatusCode)
			})
		}
	}
}

func TestOAuthReliabilityLocalQueueFailureKeepsPriorAttemptSeparate(t *testing.T) {
	rec, entry := oauthReliabilityAdmissionOpsRoundTrip(t, "/v1/responses", true, func(c *gin.Context) {
		service.SetOpsUpstreamError(c, http.StatusServiceUnavailable, "earlier provider failure", "")
		c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{{
			UpstreamStatusCode: http.StatusServiceUnavailable, Message: "earlier provider failure", Kind: "http_error",
		}})
		(&OpenAIGatewayHandler{}).handleOpenAIQueueFull(c, true)
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, http.StatusTooManyRequests, entry.StatusCode)
	require.Equal(t, "rate_limit_error", entry.ErrorType)
	require.Equal(t, "request", entry.ErrorPhase)
	require.Equal(t, "client", entry.ErrorOwner)
	require.True(t, entry.IsBusinessLimited)
	require.NotNil(t, entry.UpstreamStatusCode)
	require.Equal(t, http.StatusServiceUnavailable, *entry.UpstreamStatusCode, "retain prior-attempt telemetry without relabeling it as a local 429")
	require.NotNil(t, entry.UpstreamErrorMessage)
	require.Equal(t, "earlier provider failure", *entry.UpstreamErrorMessage)
}

func oauthReliabilityAdmissionOpsRoundTrip(t *testing.T, path string, stream bool, write func(*gin.Context)) (*httptest.ResponseRecorder, *service.OpsInsertErrorLogInput) {
	t.Helper()
	setupOpsErrorLogTestQueue(t, 4)
	gin.SetMode(gin.TestMode)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.POST(path, func(c *gin.Context) {
		setOpsRequestContext(c, "gpt-5.6-sol", stream)
		if stream {
			c.Header("Content-Type", "text/event-stream")
			_, err := c.Writer.WriteString(": waiting\n\n")
			require.NoError(t, err)
			c.Writer.Flush()
		}
		write(c)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
	require.Equal(t, int64(1), OpsErrorLogQueueLength(), "one request outcome must create exactly one queued entry")
	entry := (<-opsErrorLogQueue).entry
	require.NotNil(t, entry)
	return recorder, entry
}
