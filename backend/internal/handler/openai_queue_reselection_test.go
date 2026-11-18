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
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// The scheduler sees occupied slots with room to queue. Admission gets a newer
// view, reproducing another request filling that queue after selection.
type openAIQueueSchedulerCache struct {
	fakeConcurrencyCache
	// Tests can make a different account available only after admission queues.
	available func(int64) bool
	released  []int64
}

func (c *openAIQueueSchedulerCache) AcquireAccountSlot(_ context.Context, id int64, _ int, _ string) (bool, error) {
	return c.available != nil && c.available(id), nil
}

func (c *openAIQueueSchedulerCache) ReleaseAccountSlot(_ context.Context, id int64, _ string) error {
	c.released = append(c.released, id)
	return nil
}

func (c *openAIQueueSchedulerCache) GetAccountsLoadBatch(_ context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	loads := make(map[int64]*service.AccountLoadInfo, len(accounts))
	for _, account := range accounts {
		loads[account.ID] = &service.AccountLoadInfo{AccountID: account.ID, CurrentConcurrency: account.MaxConcurrency, LoadRate: 100}
		if c.available != nil && c.available(account.ID) {
			loads[account.ID] = &service.AccountLoadInfo{AccountID: account.ID}
		}
	}
	return loads, nil
}

type openAIQueueAdmissionCache struct {
	fakeConcurrencyCache
	mu              sync.Mutex
	availableID     int64
	opensOnQueue    int64
	canWait         bool
	waitAccounts    []int64
	released        int
	leftQueue       []int64
	cancelOnDequeue context.CancelFunc
}

func (c *openAIQueueAdmissionCache) AcquireAccountSlot(_ context.Context, accountID int64, _ int, _ string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return accountID == c.availableID || (accountID == c.opensOnQueue && len(c.waitAccounts) > 0), nil
}

func (c *openAIQueueAdmissionCache) IncrementAccountWaitCount(_ context.Context, accountID int64, _ int) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waitAccounts = append(c.waitAccounts, accountID)
	return c.canWait, nil
}

func (c *openAIQueueAdmissionCache) DecrementAccountWaitCount(_ context.Context, accountID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leftQueue = append(c.leftQueue, accountID)
	if c.cancelOnDequeue != nil {
		c.cancelOnDequeue()
	}
	return nil
}

// The other account becomes available while the original queue still owns
// the request. Leaving the original queue before probing loses that slot.
func (c *openAIQueueAdmissionCache) otherSlotOpen(accountID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return accountID == 2 && len(c.waitAccounts) == 1 && c.waitAccounts[0] == 1 && len(c.leftQueue) == 0
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
				sessionHash := ""
				release, result = h.acquireOpenAIAccountSlotWithQueueReselection(c, nil, &sessionHash, selection, false, &streamStarted, zap.NewNop(), nil, true)
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
				require.Empty(t, cache.leftQueue, "a rejected queue did not increment the counter")
				return
			}
			require.Equal(t, openAISlotAcquireFailed, result)
			require.Equal(t, http.StatusTooManyRequests, rec.Code)
			if tc.canWait {
				require.Equal(t, gatewayConcurrencyLimitCode, gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
				require.Equal(t, []int64{1}, cache.leftQueue, "timeout must release its wait count exactly once")
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
	h, upstream, _ := newOpenAIQueueHandlerWithScheduler(t, count, admission, &openAIQueueSchedulerCache{}, time.Millisecond, nil)
	return h, upstream
}

func newOpenAIQueueHandlerWithScheduler(
	t *testing.T, count int,
	admission *openAIQueueAdmissionCache, scheduler *openAIQueueSchedulerCache,
	timeout time.Duration, bindings service.GatewayCache,
) (*OpenAIGatewayHandler, *openAIQueueSuccessUpstream, *openAIWSUsageHandlerUsageLogRepoStub) {
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
		LoadBatchEnabled: true, StickySessionMaxWaiting: 3, StickySessionWaitTimeout: timeout,
		FallbackMaxWaiting: 100, FallbackWaitTimeout: timeout,
	}
	upstream := &openAIQueueSuccessUpstream{}
	usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gw := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts}, usage, nil, nil, nil, nil, bindings, cfg, nil,
		service.NewConcurrencyService(scheduler), service.NewBillingService(cfg, nil), nil, billing, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	gw.StopOpenAICodexTicketHarvester()
	gw.DisableOpenAITiboRouteForTest()
	h := NewOpenAIGatewayHandler(gw, service.NewConcurrencyService(admission), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 0 // Queue admission retries have their own budget.
	return h, upstream, usage
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
			{"pool stays full until timeout", 2, 0, http.StatusTooManyRequests, []int64{1}},
		} {
			t.Run(endpoint.name+"/"+tc.name, func(t *testing.T) {
				cache := &openAIQueueAdmissionCache{availableID: tc.availableID, canWait: tc.name == "pool stays full until timeout"}
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
		t.Run(endpoint.name+"/sibling_slot_opens_while_queued", func(t *testing.T) {
			cache := &openAIQueueAdmissionCache{canWait: true}
			scheduler := &openAIQueueSchedulerCache{available: cache.otherSlotOpen}
			h, upstream, usage := newOpenAIQueueHandlerWithScheduler(t, 2, cache, scheduler, 700*time.Millisecond, nil)
			c, rec := openAIQueueRequestContext(t, endpoint.path, endpoint.body)
			endpoint.run(h, c)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, []int64{2}, upstream.accountIDs)
			require.Equal(t, int64(2), c.GetInt64(opsAccountIDKey))
			require.Len(t, usage.created, 1)
			entry := <-usage.created
			require.Equal(t, int64(2), entry.AccountID)
			require.Equal(t, []int64{1}, cache.waitAccounts)
			require.Equal(t, []int64{1}, cache.leftQueue)
			require.Zero(t, cache.released)
			require.Equal(t, []int64{2}, scheduler.released)
		})
	}
}

func TestOpenAIQueueWaitOriginalAccountOpens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &openAIQueueAdmissionCache{canWait: true, opensOnQueue: 1}
	scheduler := &openAIQueueSchedulerCache{available: cache.otherSlotOpen}
	h, upstream, _ := newOpenAIQueueHandlerWithScheduler(t, 2, cache, scheduler, time.Second, nil)
	c, rec := openAIQueueRequestContext(t, "/v1/responses", `{"model":"gpt-5.1","input":"hello"}`)
	h.Responses(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []int64{1}, upstream.accountIDs)
	require.Equal(t, int64(1), c.GetInt64(opsAccountIDKey))
	require.Equal(t, []int64{1}, cache.waitAccounts)
	require.Equal(t, []int64{1}, cache.leftQueue)
	require.Equal(t, 1, cache.released)
	require.Empty(t, scheduler.released)
}

func TestOpenAIQueueWaitStickyBindingAfterAlternativeAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name     string
		existing bool
		want     int64
	}{
		{"new session binds to the serving account", false, 2},
		{"existing session keeps its original account", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bindings := testutil.NewRedisGatewayCache(t)
			cache := &openAIQueueAdmissionCache{canWait: true}
			scheduler := &openAIQueueSchedulerCache{available: cache.otherSlotOpen}
			h, upstream, _ := newOpenAIQueueHandlerWithScheduler(t, 2, cache, scheduler, 700*time.Millisecond, bindings)
			c, rec := openAIQueueRequestContext(t, "/v1/responses", `{"model":"gpt-5.1","input":"hello"}`)
			c.Request.Header.Set("Session-Id", "queue-binding")
			hash := h.gatewayService.GenerateSessionHash(c, nil)
			apiKey, ok := middleware2.GetAPIKeyFromContext(c)
			require.True(t, ok)
			if tc.existing {
				require.NoError(t, h.gatewayService.BindStickySession(c.Request.Context(), apiKey.GroupID, hash, 1))
			}

			h.Responses(c)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, []int64{2}, upstream.accountIDs)
			bound, err := bindings.GetSessionAccountID(c.Request.Context(), *apiKey.GroupID, "openai:"+hash)
			require.NoError(t, err)
			require.Equal(t, tc.want, bound)
		})
	}
}

func TestOpenAIQueueWaitStreamingResponseAfterHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &openAIQueueAdmissionCache{canWait: true}
	c, rec := openAIQueueRequestContext(t, "/v1/responses", `{"model":"gpt-5.1","stream":true,"input":"hello"}`)
	scheduler := &openAIQueueSchedulerCache{available: func(id int64) bool {
		return c.GetInt(gatewayStreamHeartbeatBytesKey) > 0 && cache.otherSlotOpen(id)
	}}
	h, upstream, _ := newOpenAIQueueHandlerWithScheduler(t, 2, cache, scheduler, 2*time.Second, nil)
	h.concurrencyHelper.pingInterval = 5 * time.Millisecond
	h.Responses(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, strings.HasPrefix(rec.Body.String(), string(SSEPingFormatComment)), rec.Body.String())
	require.Contains(t, rec.Body.String(), "response.completed")
	require.Equal(t, []int64{2}, upstream.accountIDs)
	require.Equal(t, []int64{2}, scheduler.released)
	require.Equal(t, []int64{1}, cache.leftQueue)
}

func TestOpenAIQueueWaitClientDisconnectsAfterAlternativeAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := &openAIQueueAdmissionCache{canWait: true, cancelOnDequeue: cancel}
	scheduler := &openAIQueueSchedulerCache{available: cache.otherSlotOpen}
	h, upstream, usage := newOpenAIQueueHandlerWithScheduler(t, 2, cache, scheduler, 700*time.Millisecond, nil)
	c, rec := openAIQueueRequestContext(t, "/v1/responses", `{"model":"gpt-5.1","input":"hello"}`)
	c.Request = c.Request.WithContext(ctx)
	h.Responses(c)

	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Empty(t, upstream.accountIDs)
	require.Empty(t, usage.created)
	require.Equal(t, statusClientClosedRequest, c.Writer.Status())
	require.Empty(t, rec.Body.String())
	require.Equal(t, []int64{1}, cache.waitAccounts)
	require.Equal(t, []int64{1}, cache.leftQueue)
	require.Equal(t, []int64{2}, scheduler.released)
	require.Zero(t, cache.released)
}

func TestOpenAIQueueWaitPreservesPreviousResponseAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &openAIQueueAdmissionCache{availableID: 1, canWait: true}
	scheduler := &openAIQueueSchedulerCache{available: cache.otherSlotOpen}
	h, upstream, _ := newOpenAIQueueHandlerWithScheduler(t, 2, cache, scheduler, 400*time.Millisecond, nil)
	c, rec := openAIQueueRequestContext(t, "/v1/responses", `{"model":"gpt-5.1","input":"hello"}`)
	h.Responses(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	cache.availableID = 0
	c, rec = openAIQueueRequestContext(t, "/v1/responses", `{"model":"gpt-5.1","previous_response_id":"resp_queue_success","input":"continue"}`)
	h.Responses(c)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.Equal(t, gatewayConcurrencyLimitCode, gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Equal(t, []int64{1}, upstream.accountIDs, "only the first turn may reach upstream")
	require.Equal(t, int64(1), c.GetInt64(opsAccountIDKey))
	require.Equal(t, []int64{1}, cache.waitAccounts)
	require.Equal(t, []int64{1}, cache.leftQueue)
	require.Empty(t, scheduler.released)
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
