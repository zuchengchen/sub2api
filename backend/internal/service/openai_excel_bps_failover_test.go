package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// excelBPSWireStep scripts one upstream round trip together with the httptrace
// events a real transport emits for it (see the repository WroteRequest test).
type excelBPSWireStep struct {
	headersWritten bool  // fire WroteHeaders
	wrote          bool  // fire WroteRequest
	writeErr       error // WroteRequest error; nil means the request was written
	during         func()
	err            error
	resp           *http.Response
}

type excelBPSTraceUpstream struct {
	steps    []excelBPSWireStep
	requests []*http.Request
	bodies   [][]byte
}

func (u *excelBPSTraceUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
		u.bodies = append(u.bodies, body)
	}
	u.requests = append(u.requests, req)
	if len(u.steps) == 0 {
		return nil, errors.New("unexpected upstream request")
	}
	step := u.steps[0]
	u.steps = u.steps[1:]
	if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
		if step.headersWritten && trace.WroteHeaders != nil {
			trace.WroteHeaders()
		}
		if step.wrote && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{Err: step.writeErr})
		}
	}
	if step.during != nil {
		step.during()
	}
	if step.err != nil {
		return nil, step.err
	}
	return step.resp, nil
}

func (u *excelBPSTraceUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *excelBPSTraceUpstream) hosts() []string {
	hosts := make([]string, 0, len(u.requests))
	for _, req := range u.requests {
		hosts = append(hosts, req.URL.Host)
	}
	return hosts
}

type excelBPSForwardOutcome struct {
	result *OpenAIForwardResult
	err    error
	rec    *httptest.ResponseRecorder
	c      *gin.Context
}

func excelBPSForwardTrace(t *testing.T, ctx context.Context, upstream *excelBPSTraceUpstream, stream bool) excelBPSForwardOutcome {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := openAIClientToolsTestService(&httpUpstreamRecorder{})
	svc.httpUpstream = upstream
	body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"hello"}`, stream))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	result, err := svc.Forward(ctx, c, excelAccount(), body)
	return excelBPSForwardOutcome{result: result, err: err, rec: rec, c: c}
}

func excelBPSSSE(events ...string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"bps-request"}},
		Body: io.NopCloser(strings.NewReader(strings.Join(events, "")))}
}

// requireExcelBPSAccountSwitch asserts the uncertain class: exactly one BPS
// attempt, nothing written to the client, and a failover that only another
// account may retry.
func requireExcelBPSAccountSwitch(t *testing.T, got excelBPSForwardOutcome, upstream *excelBPSTraceUpstream, code string) *OpsUpstreamErrorEvent {
	t.Helper()
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, got.err, &failoverErr)
	require.Nil(t, got.result)
	require.Equal(t, []string{"bps.openai.com"}, upstream.hosts(), "a possibly executed BPS request must not be replayed on the same account")
	require.Empty(t, got.rec.Body.String())
	require.False(t, got.c.Writer.Written())
	require.False(t, IsResponseCommitted(got.c))
	require.Empty(t, got.rec.Header().Get("X-Codex2API-Basispoints-Bypass"))

	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.False(t, failoverErr.RequestScopedTransient)
	require.False(t, failoverErr.SafeToFailoverAfterWrite)
	require.False(t, failoverErr.IsCredentialFailure())
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.True(t, failoverErr.ShouldReportAccountScheduleFailure())
	require.Equal(t, GatewayFailureScopeAccount, failoverErr.Scope)
	require.Equal(t, excelBPSUncertainFailoverReason, failoverErr.Reason)
	require.Equal(t, http.StatusBadGateway, failoverErr.ClientStatusCode)
	require.NotEmpty(t, failoverErr.ClientMessage)
	require.Equal(t, "upstream_error", gjson.GetBytes(failoverErr.ResponseBody, "error.type").String())
	require.Equal(t, code, gjson.GetBytes(failoverErr.ResponseBody, "error.code").String())
	require.NotContains(t, string(failoverErr.ResponseBody), "replayed")
	require.NotContains(t, string(failoverErr.ResponseBody), "test-token")

	raw, exists := got.c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	attempts, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, attempts, 1)
	require.Equal(t, "failover", attempts[0].Kind)
	require.Equal(t, string(excelBPSUncertainFailoverReason), attempts[0].Reason)
	require.Equal(t, basispoints.ResponsesURL, attempts[0].UpstreamURL)
	require.Equal(t, int64(300), attempts[0].AccountID)
	require.NotContains(t, attempts[0].Detail, "test-token")
	return attempts[0]
}

func TestExcelBPSTransportFailureClassification(t *testing.T) {
	cases := []struct {
		name     string
		step     excelBPSWireStep
		failover bool
	}{
		{"dial refused before anything was written", excelBPSWireStep{err: syscall.ECONNREFUSED}, false},
		{"request write failed", excelBPSWireStep{headersWritten: true, wrote: true, writeErr: syscall.EPIPE, err: syscall.EPIPE}, false},
		{"header write failed", excelBPSWireStep{headersWritten: true, err: errors.New("http2: client connection lost")}, false},
		{"cancelled before headers", excelBPSWireStep{err: context.Canceled}, false},
		{"reset after the request was written", excelBPSWireStep{headersWritten: true, wrote: true, err: syscall.ECONNRESET}, true},
		{"EOF after the request was written", excelBPSWireStep{headersWritten: true, wrote: true, err: io.EOF}, true},
		{"response header timeout after write", excelBPSWireStep{headersWritten: true, wrote: true, err: errors.New("net/http: timeout awaiting response headers")}, true},
		{"HTTP/2 cancel before WroteRequest", excelBPSWireStep{headersWritten: true, err: context.Canceled}, true},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				upstream := &excelBPSTraceUpstream{steps: []excelBPSWireStep{tc.step, {resp: excelBPSCodexHTTPSuccessResponse()}}}
				got := excelBPSForwardTrace(t, context.Background(), upstream, stream)
				if tc.failover {
					attempt := requireExcelBPSAccountSwitch(t, got, upstream, "basispoints_transport_error")
					require.Zero(t, attempt.UpstreamStatusCode, "no upstream response was received")
					require.Contains(t, attempt.Detail, "after the request was written")
					return
				}
				// Definitely not executed: next route on the same account.
				require.NoError(t, got.err)
				require.NotNil(t, got.result)
				require.Equal(t, []string{"bps.openai.com", "chatgpt.com"}, upstream.hosts())
				require.Equal(t, excelBPSHTTPFallbackReason, got.rec.Header().Get("X-Codex2API-Basispoints-Bypass"))
				require.Contains(t, got.rec.Body.String(), "ok")
				_, exists := got.c.Get(OpsUpstreamErrorsKey)
				require.False(t, exists)
			})
		}
	}
}

func TestExcelBPSClientCancelDuringSendIsNotRetried(t *testing.T) {
	for _, written := range []bool{false, true} {
		t.Run(fmt.Sprintf("written=%t", written), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			upstream := &excelBPSTraceUpstream{steps: []excelBPSWireStep{{headersWritten: written, wrote: written, during: cancel, err: context.Canceled}}}
			got := excelBPSForwardTrace(t, ctx, upstream, true)
			require.ErrorIs(t, got.err, context.Canceled)
			var failoverErr *UpstreamFailoverError
			require.NotErrorAs(t, got.err, &failoverErr)
			require.Nil(t, got.result)
			require.Equal(t, []string{"bps.openai.com"}, upstream.hosts())
			require.Empty(t, got.rec.Body.String())
			_, exists := got.c.Get(OpsUpstreamErrorsKey)
			require.False(t, exists)
		})
	}
}

func TestExcelBPSStreamEndingBeforeTerminalSwitchesAccount(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cut\",\"status\":\"in_progress\"}}\n\n"
	cases := []struct {
		name    string
		body    func() io.Reader
		streams []bool
	}{
		{"empty 200 body", func() io.Reader { return strings.NewReader("") }, []bool{false, true}},
		{"only upstream keepalives", func() io.Reader { return strings.NewReader(": keepalive\n\n") }, []bool{false, true}},
		{"connection reset while reading", func() io.Reader {
			return io.MultiReader(strings.NewReader(": keepalive\n\n"), &passthroughErrReadCloser{err: syscall.ECONNRESET})
		}, []bool{false, true}},
		// A streaming client would already have received response.created.
		{"events then EOF", func() io.Reader { return strings.NewReader(created) }, []bool{false}},
		{"event cut mid-JSON", func() io.Reader {
			return strings.NewReader(created + "data: {\"type\":\"response.output_text.delta\",\"del")
		}, []bool{false}},
	}
	for _, tc := range cases {
		for _, stream := range tc.streams {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"bps-request"}}, Body: io.NopCloser(tc.body())}
				upstream := &excelBPSTraceUpstream{steps: []excelBPSWireStep{{headersWritten: true, wrote: true, resp: resp}}}
				got := excelBPSForwardTrace(t, context.Background(), upstream, stream)
				code := "basispoints_stream_incomplete"
				if tc.name == "event cut mid-JSON" {
					// The bridge turns an undecodable event into response.failed.
					code = "basispoints_protocol_error"
				}
				attempt := requireExcelBPSAccountSwitch(t, got, upstream, code)
				require.Equal(t, http.StatusBadGateway, attempt.UpstreamStatusCode)
				require.Equal(t, "bps-request", attempt.UpstreamRequestID)
			})
		}
	}
}

func TestExcelBPSStreamInterruptedAfterOutputStaysCommitted(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cut\",\"status\":\"in_progress\"}}\n\n"
	upstream := &excelBPSTraceUpstream{steps: []excelBPSWireStep{{headersWritten: true, wrote: true, resp: excelBPSSSE(created)}}}
	got := excelBPSForwardTrace(t, context.Background(), upstream, true)
	require.Error(t, got.err)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, got.err, &failoverErr)
	require.Equal(t, []string{"bps.openai.com"}, upstream.hosts())
	require.True(t, IsResponseCommitted(got.c))
	require.Contains(t, got.rec.Body.String(), "response.created")
	require.Contains(t, got.rec.Body.String(), "basispoints_stream_incomplete")
}

func TestExcelBPSTerminalFailureClassification(t *testing.T) {
	failed := func(errorObject string) string {
		return "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"output\":[],\"error\":" + errorObject + "}}\n\n"
	}
	cases := []struct {
		name     string
		wire     string
		terminal string
		rejected bool
		status   int
		code     string
		detail   string
	}{
		{name: "generic server error", wire: failed(`{"type":"server_error","code":"server_error","message":"The server had an error while processing your request."}`),
			terminal: "response.failed", detail: "terminal=response.failed code=server_error"},
		{name: "rate limited", wire: failed(`{"code":"rate_limit_exceeded","message":"Rate limit reached, please retry later"}`),
			terminal: "response.failed", detail: "code=rate_limit_exceeded"},
		{name: "incomplete without error", wire: "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_inc\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[]}}\n\n",
			terminal: "response.incomplete", detail: "terminal=response.incomplete reason=max_output_tokens"},
		{name: "flat error event", wire: "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"Upstream worker restarted\"}\n\n",
			terminal: "error", detail: "terminal=error code=server_error"},
		{name: "context window exceeded", wire: failed(`{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}`),
			terminal: "response.failed", rejected: true, status: http.StatusBadRequest, code: "context_length_exceeded"},
		{name: "invalid prompt error event", wire: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_prompt\",\"message\":\"Invalid prompt: token test-token was rejected.\"}}\n\n",
			terminal: "error", rejected: true, status: http.StatusBadRequest, code: "invalid_prompt"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				upstream := &excelBPSTraceUpstream{steps: []excelBPSWireStep{{headersWritten: true, wrote: true, resp: excelBPSSSE(tc.wire)}}}
				got := excelBPSForwardTrace(t, context.Background(), upstream, stream)
				if stream {
					// The terminal event already reached the client: keep it.
					require.Error(t, got.err)
					var failoverErr *UpstreamFailoverError
					require.NotErrorAs(t, got.err, &failoverErr)
					require.NotNil(t, got.result)
					require.Equal(t, tc.terminal, got.result.UpstreamTerminalEvent)
					require.Equal(t, []string{"bps.openai.com"}, upstream.hosts())
					require.True(t, IsResponseCommitted(got.c))
					require.Contains(t, got.rec.Body.String(), "event: "+tc.terminal)
					return
				}
				if !tc.rejected {
					attempt := requireExcelBPSAccountSwitch(t, got, upstream, "basispoints_protocol_error")
					require.Contains(t, attempt.Detail, tc.detail)
					return
				}
				// Request-scoped rejection: returned to the client, never retried.
				require.Error(t, got.err)
				var failoverErr *UpstreamFailoverError
				require.NotErrorAs(t, got.err, &failoverErr)
				require.NotErrorIs(t, got.err, errExcelBPSHTTPFallback)
				require.Nil(t, got.result)
				require.Equal(t, []string{"bps.openai.com"}, upstream.hosts())
				require.True(t, IsResponseCommitted(got.c))
				require.Equal(t, tc.status, got.rec.Code)
				require.Equal(t, "invalid_request_error", gjson.Get(got.rec.Body.String(), "error.type").String())
				require.Equal(t, tc.code, gjson.Get(got.rec.Body.String(), "error.code").String())
				require.NotEmpty(t, gjson.Get(got.rec.Body.String(), "error.message").String())
				require.NotContains(t, got.rec.Body.String(), "test-token")
			})
		}
	}
}

func TestExcelBPSExplicitRejectionAfterWriteStaysOnAccount(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			rejection := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"denied"}}`))}
			upstream := &excelBPSTraceUpstream{steps: []excelBPSWireStep{
				{headersWritten: true, wrote: true, resp: rejection},
				{resp: excelBPSCodexHTTPSuccessResponse()},
			}}
			got := excelBPSForwardTrace(t, context.Background(), upstream, false)
			require.NoError(t, got.err)
			require.Equal(t, []string{"bps.openai.com", "chatgpt.com"}, upstream.hosts(), "an explicit status means BPS did not execute the request")
			require.Equal(t, excelBPSHTTPFallbackReason, got.rec.Header().Get("X-Codex2API-Basispoints-Bypass"))
			require.Contains(t, got.rec.Body.String(), "ok")
		})
	}
}

func TestExcelBPSUncertainFailureAfterCompactKeepaliveStaysCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &excelBPSTraceUpstream{steps: []excelBPSWireStep{{headersWritten: true, wrote: true, err: syscall.ECONNRESET}}}
	svc := openAIClientToolsTestService(&httpUpstreamRecorder{})
	svc.httpUpstream = upstream
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	MarkOpenAICompactClientStream(c)
	stop := StartOpenAICompactSSEKeepalive(c, time.Hour)
	defer stop()
	value, exists := c.Get(openAICompactSSEKeepaliveKey)
	require.True(t, exists)
	keepalive, ok := value.(*openAICompactSSEKeepalive)
	require.True(t, ok)
	require.True(t, keepalive.beat())
	_, err := svc.Forward(context.Background(), c, excelAccount(), []byte(`{"model":"gpt-6-astra","input":"continue"}`))
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr)
	require.Equal(t, []string{"bps.openai.com"}, upstream.hosts())
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed\n"))
	streamError, exists := GetOpsStreamError(c)
	require.True(t, exists)
	require.Equal(t, "basispoints_transport_error", streamError.ErrType)
}
