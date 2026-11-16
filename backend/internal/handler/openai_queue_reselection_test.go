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
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// The scheduler sees occupied slots with room to queue. Admission gets a newer
// view, reproducing another request filling that queue after selection.
type openAIQueueSchedulerCache struct{ fakeConcurrencyCache }

func (*openAIQueueSchedulerCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	return false, nil
}

func (*openAIQueueSchedulerCache) GetAccountsLoadBatch(_ context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	loads := make(map[int64]*service.AccountLoadInfo, len(accounts))
	for _, account := range accounts {
		loads[account.ID] = &service.AccountLoadInfo{AccountID: account.ID, CurrentConcurrency: account.MaxConcurrency, LoadRate: 100}
	}
	return loads, nil
}

type openAIQueueAdmissionCache struct {
	fakeConcurrencyCache
	mu           sync.Mutex
	availableID  int64
	canWait      bool
	waitAccounts []int64
	released     int
	decremented  int
}

func (c *openAIQueueAdmissionCache) AcquireAccountSlot(_ context.Context, accountID int64, _ int, _ string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return accountID == c.availableID, nil
}

func (c *openAIQueueAdmissionCache) IncrementAccountWaitCount(_ context.Context, accountID int64, _ int) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waitAccounts = append(c.waitAccounts, accountID)
	return c.canWait, nil
}

func (c *openAIQueueAdmissionCache) DecrementAccountWaitCount(context.Context, int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.decremented++
	return nil
}

func (c *openAIQueueAdmissionCache) ReleaseAccountSlot(context.Context, int64, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released++
	return nil
}

func TestOpenAIQueueAdmissionLegacyAndOptIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name    string
		optIn   bool
		canWait bool
	}{
		{name: "legacy writes queue full"},
		{name: "opt in defers queue full", optIn: true},
		{name: "opt in still writes wait timeout", optIn: true, canWait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := &openAIQueueAdmissionCache{canWait: tc.canWait}
			h := &OpenAIGatewayHandler{
				gatewayService:    &service.OpenAIGatewayService{},
				concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0),
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			selection := &service.AccountSelectionResult{
				Account: &service.Account{ID: 1, Concurrency: 1},
				WaitPlan: &service.AccountWaitPlan{AccountID: 1, MaxConcurrency: 1,
					MaxWaiting: 3, Timeout: time.Millisecond},
			}
			streamStarted := false
			var release func()
			var result openAISlotAcquireResult
			if tc.optIn {
				release, result = h.acquireOpenAIAccountSlotWithQueueReselection(c, nil, "", selection, false, &streamStarted, zap.NewNop(), nil, true)
			} else {
				release, result = h.acquireResponsesAccountSlot(c, nil, "", selection, false, &streamStarted, zap.NewNop())
			}
			require.Nil(t, release)
			require.Equal(t, []int64{1}, cache.waitAccounts)
			require.Zero(t, cache.released, "a rejected queue never owned a slot")
			if tc.optIn && !tc.canWait {
				require.Equal(t, openAISlotAcquireQueueFull, result)
				require.False(t, c.Writer.Written(), "the outer handler must still be able to select another account")
				require.Empty(t, rec.Body.String())
				require.Zero(t, cache.decremented, "a rejected queue did not increment the counter")
				return
			}
			require.Equal(t, openAISlotAcquireFailed, result)
			require.Equal(t, http.StatusTooManyRequests, rec.Code)
			if tc.canWait {
				require.Equal(t, gatewayConcurrencyLimitCode, gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
				require.Equal(t, 1, cache.decremented, "timeout must release its wait count exactly once")
			} else {
				require.Equal(t, gatewayQueueFullCode, gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
			}
		})
	}
}

type openAIQueueSuccessUpstream struct {
	service.HTTPUpstream
	accountIDs []int64
}

func (u *openAIQueueSuccessUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.accountIDs = append(u.accountIDs, accountID)
	requestBody, _ := io.ReadAll(req.Body)
	body := `{"id":"resp_queue_success","object":"response","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	contentType := "application/json"
	if strings.Contains(req.URL.Path, "chat/completions") {
		body = `{"id":"chatcmpl_queue_success","object":"chat.completion","model":"gpt-5.1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	} else if gjson.GetBytes(requestBody, "stream").Bool() {
		body = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func newOpenAIQueueReselectionHandler(t *testing.T, count int, admission *openAIQueueAdmissionCache) (*OpenAIGatewayHandler, *openAIQueueSuccessUpstream) {
	t.Helper()
	accounts := make([]service.Account, count)
	for i := range accounts {
		accounts[i] = service.Account{
			ID: int64(i + 1), Name: fmt.Sprintf("queue-account-%d", i+1), Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
			Concurrency: 1, Priority: i, Credentials: map[string]any{"api_key": "test-key", "base_url": "https://api.openai.com"},
		}
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{
		LoadBatchEnabled: true, StickySessionMaxWaiting: 3, StickySessionWaitTimeout: time.Millisecond,
		FallbackMaxWaiting: 100, FallbackWaitTimeout: time.Millisecond,
	}
	upstream := &openAIQueueSuccessUpstream{}
	gw := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts}, nil, nil, nil, nil, nil, nil, cfg, nil,
		service.NewConcurrencyService(&openAIQueueSchedulerCache{}), nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	gw.DisableOpenAITiboRouteForTest()
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := NewOpenAIGatewayHandler(gw, service.NewConcurrencyService(admission), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 0 // Queue admission retries have their own budget.
	return h, upstream
}

func openAIQueueRequestContext(t *testing.T, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	require.True(t, ok)
	apiKey.Group.AllowMessagesDispatch = true
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func TestOpenAITextHandlersQueueReselection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []struct {
		name string
		path string
		body string
		run  func(*OpenAIGatewayHandler, *gin.Context)
	}{
		{"responses", "/v1/responses", `{"model":"gpt-5.1","stream":false,"input":"hello"}`, (*OpenAIGatewayHandler).Responses},
		{"messages", "/v1/messages", `{"model":"gpt-5.1","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, (*OpenAIGatewayHandler).Messages},
		{"chat", "/v1/chat/completions", `{"model":"gpt-5.1","stream":false,"messages":[{"role":"user","content":"hello"}]}`, (*OpenAIGatewayHandler).ChatCompletions},
	} {
		for _, tc := range []struct {
			name        string
			count       int
			availableID int64
			status      int
			waitIDs     []int64
		}{
			{"recovers without upstream replay", 2, 2, http.StatusOK, []int64{1}},
			{"exhaustion preserves queue full", 2, 0, http.StatusTooManyRequests, []int64{1, 2}},
			{"bounded even with more accounts", 6, 6, http.StatusTooManyRequests, []int64{1, 2, 3, 4}},
			{"wait timeout is terminal", 2, 2, http.StatusTooManyRequests, []int64{1}},
		} {
			t.Run(endpoint.name+"/"+tc.name, func(t *testing.T) {
				cache := &openAIQueueAdmissionCache{availableID: tc.availableID, canWait: tc.name == "wait timeout is terminal"}
				h, upstream := newOpenAIQueueReselectionHandler(t, tc.count, cache)
				c, rec := openAIQueueRequestContext(t, endpoint.path, endpoint.body)
				endpoint.run(h, c)
				require.Equal(t, tc.status, rec.Code, rec.Body.String())
				require.Equal(t, tc.waitIDs, cache.waitAccounts)
				if tc.status == http.StatusOK {
					require.Equal(t, []int64{2}, upstream.accountIDs, "only the admitted account may make an upstream request")
					require.Contains(t, rec.Body.String(), "ok")
					require.Equal(t, 1, cache.released)
				} else {
					require.Empty(t, upstream.accountIDs)
					wantCode := gatewayQueueFullCode
					if cache.canWait {
						wantCode = gatewayConcurrencyLimitCode
					}
					require.Equal(t, wantCode, gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
				}
			})
		}
	}
}

func TestOpenAIResponsesQueueFullPreservesPreviousResponseAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &openAIQueueAdmissionCache{availableID: 1}
	h, upstream := newOpenAIQueueReselectionHandler(t, 2, cache)
	c, rec := openAIQueueRequestContext(t, "/v1/responses", `{"model":"gpt-5.1","input":"hello"}`)
	h.Responses(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []int64{1}, upstream.accountIDs)

	cache.availableID = 2
	c, rec = openAIQueueRequestContext(t, "/v1/responses", `{"model":"gpt-5.1","previous_response_id":"resp_queue_success","input":"continue"}`)
	h.Responses(c)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.Equal(t, gatewayQueueFullCode, gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Equal(t, []int64{1}, cache.waitAccounts, "the continuation must remain on its original account")
	require.Equal(t, []int64{1}, upstream.accountIDs, "a full owner queue cannot move the continuation to account 2")
}

func TestOpenAIQueueReselectionDoesNotMaskUnrelatedSelectionFailure(t *testing.T) {
	r := openAIQueueReselection{enabled: true, retries: 1}
	require.True(t, r.exhausted(fmt.Errorf("selection exhausted: %w", service.ErrNoAvailableAccounts)))
	require.False(t, r.exhausted(context.DeadlineExceeded))
	r.enabled = false
	require.False(t, r.exhausted(service.ErrNoAvailableAccounts), "upstream failover must retain its own terminal error")
	require.False(t, openAITextQueueReselectionAllowed(service.PlatformOpenAI, "codex-auto-review"))
	require.False(t, openAITextQueueReselectionAllowed(service.PlatformGrok, "grok-4"))
}
