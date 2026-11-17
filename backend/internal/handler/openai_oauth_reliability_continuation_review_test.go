//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type oauthReliabilityReviewContinuationUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	bodies     [][]byte
	failure    bool
}

func (u *oauthReliabilityReviewContinuationUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	if u.failure {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","code":"overloaded","message":"synthetic temporary failure"}}`)), Request: req}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + `{"type":"response.completed","response":{"id":"resp_chat_owner","object":"response","model":"gpt-5.1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n")), Request: req}, nil
}

func oauthReliabilityReviewContinuationAccount(id int64) service.Account {
	return service.Account{ID: id, Name: "synthetic-continuation", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: int(id),
		Credentials: map[string]any{"api_key": fmt.Sprintf("synthetic-key-%d", id), "base_url": "https://upstream.example"},
		Extra:       map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses)},
	}
}

func oauthReliabilityReviewContinuationHandler(t *testing.T, repo service.AccountRepository, upstream service.HTTPUpstream) *OpenAIGatewayHandler {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 4)}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	svc := service.NewOpenAIGatewayService(repo, usage, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	svc.DisableOpenAITiboRouteForTest()
	svc.StopOpenAICodexTicketHarvester()
	t.Cleanup(svc.CloseOpenAIWSPool)
	h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(nil), billing, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 1
	return h
}

func TestOAuthReliabilityReviewContinuationChatCannotBypassKnownOwner(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			repo := &openAIWSUsageHandlerAccountRepoStub{account: oauthReliabilityReviewContinuationAccount(1)}
			upstream := &oauthReliabilityReviewContinuationUpstream{}
			h := oauthReliabilityReviewContinuationHandler(t, repo, upstream)
			first, firstRecorder := newOpenAIResponsesFailoverTestContext(t, context.Background())
			h.Responses(first)
			require.Equal(t, http.StatusOK, firstRecorder.Code)
			require.Equal(t, "resp_chat_owner", gjson.Get(firstRecorder.Body.String(), "id").String())
			repo.account.Credentials["api_key"] = "synthetic-rotated-owner"
			second, recorder := newOpenAIResponsesFailoverTestContext(t, context.Background())
			second.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"previous_response_id":"resp_chat_owner","input":[{"role":"user","content":"native delta"}]}`, stream)))
			second.Request.Header.Set("Content-Type", "application/json")
			h.ChatCompletions(second)
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			require.Equal(t, []int64{1}, upstream.accountIDs, "Responses-shaped Chat must enforce the same known credential origin")
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Equal(t, "previous_response_id", gjson.Get(recorder.Body.String(), "error.param").String())
		})
	}
}

func TestOAuthReliabilityReviewContinuationChatFailoverKeepsExternalAnchor(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			repo := openAIImagesFailoverAccountRepo{accounts: []service.Account{oauthReliabilityReviewContinuationAccount(1), oauthReliabilityReviewContinuationAccount(2)}}
			upstream := &oauthReliabilityReviewContinuationUpstream{failure: true}
			h := oauthReliabilityReviewContinuationHandler(t, repo, upstream)
			c, recorder := newOpenAIResponsesFailoverTestContext(t, context.Background())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"previous_response_id":"resp_external","input":[{"role":"user","content":"native delta"}]}`, stream)))
			c.Request.Header.Set("Content-Type", "application/json")
			h.ChatCompletions(c)
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			require.Equal(t, []int64{1}, upstream.accountIDs, "a text-only delta is not complete history and cannot move to a new credential")
			require.Equal(t, "resp_external", gjson.GetBytes(upstream.bodies[0], "previous_response_id").String())
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Equal(t, "continuation_context_missing", gjson.Get(recorder.Body.String(), "error.code").String())
		})
	}
}
