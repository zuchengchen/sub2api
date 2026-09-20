package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newIntelligentOpenAITestContext(prompt string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/intelligent-tests/run", nil)
	if prompt != "" {
		req = req.WithContext(context.WithValue(req.Context(), intelligentRunKey{}, &intelligentRunContext{prompt: prompt}))
	}
	c.Request = req
	return c, rec
}

func TestIntelligentOpenAIOAuthTestInjectsCodex292Ticket(t *testing.T) {
	state := fakeCodexTicketState(292)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n")),
	}
	upstream := &httpUpstreamRecorder{resp: resp}
	gateway := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:      true,
		TargetLength: 292,
		TTLSeconds:   3600,
		FailClosed:   true,
	}, nil)
	account := ticketTestAccount(89)
	account.Credentials = map[string]any{"access_token": "test-token"}
	gateway.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  account.ID,
		Model:      "gpt-6-astra",
		State:      state,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	svc := &AccountTestService{httpUpstream: upstream, openaiGatewayService: gateway}
	c, _ := newIntelligentOpenAITestContext("pelican")

	err := svc.testOpenAIAccountConnection(c, account, "gpt-6-astra", "pelican", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, state, upstream.requests[0].Header.Get(openAICodexTurnStateHeader))
	require.Equal(t, 292, len(upstream.requests[0].Header.Get(openAICodexTurnStateHeader)))
}

func TestOpenAIOAuthConnectivityProbeDoesNotInjectCodexTicket(t *testing.T) {
	state := fakeCodexTicketState(292)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n")),
	}
	upstream := &httpUpstreamRecorder{resp: resp}
	gateway := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:      true,
		TargetLength: 292,
		TTLSeconds:   3600,
		FailClosed:   true,
	}, nil)
	account := ticketTestAccount(89)
	account.Credentials = map[string]any{"access_token": "test-token"}
	gateway.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  account.ID,
		Model:      "gpt-6-astra",
		State:      state,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	svc := &AccountTestService{httpUpstream: upstream, openaiGatewayService: gateway}
	c, _ := newIntelligentOpenAITestContext("")

	err := svc.testOpenAIAccountConnection(c, account, "gpt-6-astra", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Empty(t, upstream.requests[0].Header.Get(openAICodexTurnStateHeader))
}

func TestIntelligentOpenAIOAuthTestFailClosedWithoutTicket(t *testing.T) {
	upstream := &httpUpstreamRecorder{}
	gateway := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:      true,
		TargetLength: 292,
		FailClosed:   true,
	}, nil)
	account := ticketTestAccount(89)
	account.Credentials = map[string]any{"access_token": "test-token"}
	svc := &AccountTestService{httpUpstream: upstream, openaiGatewayService: gateway}
	c, rec := newIntelligentOpenAITestContext("pelican")

	err := svc.testOpenAIAccountConnection(c, account, "gpt-6-astra", "pelican", "")
	require.EqualError(t, err, ErrOpenAICodexTicketUnavailable.Error())
	require.Empty(t, upstream.requests)
	require.Contains(t, rec.Body.String(), ErrOpenAICodexTicketUnavailable.Error())
}
