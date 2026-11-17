//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type oauthReliabilityModelPolicyRepo struct {
	openAIImagesFailoverAccountRepo
	mu                   sync.Mutex
	modelAccountID       []int64
	modelKeys            []string
	accountBlocks        int
	stateContextErrors   []error
	stateWithoutDeadline int
}

func (r *oauthReliabilityModelPolicyRepo) SetModelRateLimit(ctx context.Context, id int64, model string, _ time.Time, _ ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Match the production repository's ExecContext behavior: a canceled
	// request cannot persist state unless the caller supplies a bounded context.
	if err := ctx.Err(); err != nil {
		r.stateContextErrors = append(r.stateContextErrors, err)
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		r.stateWithoutDeadline++
		return fmt.Errorf("model policy update has no deadline")
	}
	r.modelAccountID = append(r.modelAccountID, id)
	r.modelKeys = append(r.modelKeys, model)
	return nil
}

func (r *oauthReliabilityModelPolicyRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accountBlocks++
	return nil
}

func (r *oauthReliabilityModelPolicyRepo) SetError(context.Context, int64, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accountBlocks++
	return nil
}

func oauthReliabilityModelPolicyHandler(t *testing.T, upstream service.HTTPUpstream, status int, route string) (*OpenAIGatewayHandler, *openAIWSUsageHandlerUsageLogRepoStub, *oauthReliabilityModelPolicyRepo) {
	t.Helper()
	accounts := []service.Account{
		{ID: 1, Name: "synthetic-model-policy", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
			Credentials: map[string]any{"access_token": "synthetic-token", "temp_unschedulable_enabled": true, "temp_unschedulable_rules": []any{map[string]any{
				"error_code": float64(status), "keywords": []any{"model_not_found"}, "duration_minutes": float64(1), "scope": "model",
			}}}, Extra: astraHandlerExtra("healthy")},
		{ID: 2, Name: "synthetic-model-policy-next", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 10,
			Credentials: map[string]any{"api_key": "synthetic-key", "base_url": "https://upstream.example"}, Extra: astraHandlerExtra("healthy")},
	}
	for i := range accounts {
		accounts[i].Extra[openai_compat.ExtraKeyResponsesMode] = string(openai_compat.ResponsesSupportModeForceResponses)
	}
	if route == "passthrough" {
		accounts[0].Extra["openai_passthrough"] = true
	}
	if route == "responses_chat" {
		accounts[0].Type = service.AccountTypeAPIKey
		accounts[0].Credentials["api_key"] = "synthetic-key"
		accounts[0].Credentials["base_url"] = "https://upstream.example"
		accounts[0].Extra[openai_compat.ExtraKeyResponsesMode] = string(openai_compat.ResponsesSupportModeForceChatCompletions)
	}
	repo := &oauthReliabilityModelPolicyRepo{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: accounts}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxAccountSwitches = 1
	cfg.Gateway.OpenAIWS.ForceHTTP = true
	usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	policy := service.NewRateLimitService(repo, nil, cfg, nil)
	svc := service.NewOpenAIGatewayService(repo, usage, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), policy, billingCache, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	svc.DisableOpenAITiboRouteForTest()
	svc.StopOpenAICodexTicketHarvester()
	t.Cleanup(svc.CloseOpenAIWSPool)
	h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(nil), billingCache, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 1
	return h, usage, repo
}

func TestOAuthReliabilityModelUnavailablePolicyCannotRestoreReplay(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "responses_chat", "chat", "messages"} {
		for _, status := range []int{400, 502} {
			for _, stream := range []bool{false, true} {
				for _, evidence := range []string{"usage", "tool_output", "canceled_usage", "canceled_no_progress", "no_progress"} {
					t.Run(fmt.Sprintf("%s/status=%d/stream=%t/%s", route, status, stream, evidence), func(t *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						body := oauthReliabilityHandlerModelError
						switch evidence {
						case "usage", "canceled_usage":
							body = strings.TrimSuffix(body, "}") + `,"usage":{"input_tokens":11,"input_tokens_details":{"cached_tokens":4},"output_tokens":3}}`
						case "tool_output":
							body = strings.TrimSuffix(body, "}") + `,"output":[{"type":"function_call","call_id":"call_executed","name":"work","arguments":"{}"}]}`
						}
						upstream := &oauthReliabilityModelHandlerUpstream{status: status, body: body, succeedSecond: true}
						if strings.HasPrefix(evidence, "canceled_") {
							upstream.onFirst = cancel
						}
						h, usage, repo := oauthReliabilityModelPolicyHandler(t, upstream, status, route)
						c, recorder := oauthReliabilityModelRequest(t, route, stream, ctx)
						oauthReliabilityRunModelHandler(h, c, route)
						ids, _ := upstream.calls()
						if evidence == "no_progress" {
							require.Equal(t, []int64{1, 2}, ids, "configured policy retains safe account failover")
							require.Equal(t, http.StatusOK, recorder.Code)
						} else {
							require.Equal(t, []int64{1}, ids, "policy state cannot override execution or cancellation evidence")
							if !strings.HasPrefix(evidence, "canceled_") {
								require.Equal(t, http.StatusBadRequest, recorder.Code)
								require.Equal(t, "model_not_found", gjson.Get(recorder.Body.String(), "error.code").String())
								require.Equal(t, "model", gjson.Get(recorder.Body.String(), "error.param").String())
								require.Equal(t, status, c.GetInt(service.OpsUpstreamStatusCodeKey))
							}
						}
						if evidence == "tool_output" || evidence == "canceled_no_progress" {
							require.Empty(t, usage.created, "output or cancellation evidence must not fabricate token usage")
						} else {
							require.Len(t, usage.created, 1)
							log := <-usage.created
							if evidence == "no_progress" {
								require.Equal(t, int64(2), log.AccountID)
								require.Equal(t, 7, log.InputTokens+log.CacheReadTokens)
								require.Equal(t, 2, log.OutputTokens)
							} else {
								require.Equal(t, int64(1), log.AccountID)
								require.Equal(t, 11, log.InputTokens+log.CacheReadTokens)
								require.Equal(t, 4, log.CacheReadTokens)
								require.Equal(t, 3, log.OutputTokens)
							}
						}
						repo.mu.Lock()
						require.Equal(t, []int64{1}, repo.modelAccountID, "the existing configured model policy still records its hit")
						require.Equal(t, []string{"gpt-5.6-sol"}, repo.modelKeys)
						require.Zero(t, repo.accountBlocks, "a model-specific refusal cannot disable the whole account")
						require.Empty(t, repo.stateContextErrors, "policy persistence must survive request cancellation")
						require.Zero(t, repo.stateWithoutDeadline, "policy persistence remains bounded")
						repo.mu.Unlock()
					})
				}
			}
		}
	}
}
