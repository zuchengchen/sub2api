//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type astraPolicySnapshotCache struct {
	SchedulerCache
	account atomic.Pointer[Account]
}

func (c *astraPolicySnapshotCache) GetAccount(context.Context, int64) (*Account, error) {
	return c.account.Load(), nil
}

func TestAstraOAuthModelPolicyQueuedForwardRechecksSharedSnapshot(t *testing.T) {
	stale := astraPolicyTestAccount("healthy")
	cache := &astraPolicySnapshotCache{}
	cache.account.Store(astraPolicyTestAccount("degraded"))
	svc := &OpenAIGatewayService{cfg: &config.Config{}, schedulerSnapshot: NewSchedulerSnapshotService(cache, nil, nil, nil, nil)}
	require.Equal(t, "healthy", stale.OpenAIOAuthModelPolicy().Verdict, "shared request snapshots are immutable")
	require.NoError(t, svc.CheckOpenAIAstraModelPolicy(context.Background(), stale, "gpt-6-astra"))
	require.NoError(t, svc.CheckOpenAIAstraModelPolicy(context.Background(), stale, "gpt-6-luna"))
}

func TestAstraOAuthModelPolicyWebSocketSameModelNextTurn(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {

			gin.SetMode(gin.TestMode)
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
			upstream := newStagedPassthroughConn()
			svc := newPassthroughLifecycleService(cfg, upstream)
			svc.tiboRouteDisabled = true
			if mode == OpenAIWSIngressModeCtxPool {
				cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
				cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
				cfg.Gateway.OpenAIWS.QueueLimitPerConn = 2
				pool := newOpenAIWSConnPool(cfg)
				pool.setClientDialerForTest(&stagedPassthroughDialer{conn: &astraPolicyNativeConn{upstream}})
				defer pool.Close()
				svc.openaiWSPool = pool
			}
			green := astraPolicyTestAccount("healthy")
			green.Credentials["access_token"] = "test-token"
			green.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
			cache := &astraPolicySnapshotCache{}
			cache.account.Store(green)
			svc.schedulerSnapshot = NewSchedulerSnapshotService(cache, nil, nil, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			turnDone := make(chan struct{}, 1)
			server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, green, func(*gin.Context) *OpenAIWSIngressHooks {
				return &OpenAIWSIngressHooks{AfterTurn: func(turn int, _ *OpenAIForwardResult, err error) {
					if turn == 1 && err == nil {
						turnDone <- struct{}{}
					}
				}}
			})
			defer server.Close()
			payload := `{"type":"response.create","model":"gpt-6-astra","input":"hello"}`
			client := dialPassthroughLifecycleClientWithPayload(t, server, payload)
			defer client.CloseNow()
			var first []byte
			select {
			case first = <-upstream.writes:
			case err := <-serverErr:
				t.Fatalf("first websocket turn: %v", err)
			case <-ctx.Done():
				t.Fatal("first frame did not reach upstream")
			}
			require.Equal(t, "gpt-6-astra", gjson.GetBytes(first, "model").String())
			upstream.Send(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}}`)
			_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
			require.NoError(t, err)
			select {
			case <-turnDone:
			case <-ctx.Done():
				t.Fatal("first turn was not completed")
			}
			red := astraPolicyTestAccount("degraded")
			red.Credentials["access_token"] = "test-token"
			red.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
			cache.account.Store(red)
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(payload)))
			select {
			case second := <-upstream.writes:
				require.Equal(t, "gpt-6-astra", gjson.GetBytes(second, "model").String())
			case err := <-serverErr:
				t.Fatalf("second websocket turn: %v", err)
			case <-ctx.Done():
				t.Fatal("second frame did not reach upstream")
			}
			require.Equal(t, "healthy", green.OpenAIOAuthModelPolicy().Verdict)

		})
	}
}

func TestAstraOAuthModelPolicyImageRoutesUseInternalDriver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SUB2API_IMAGES_MAIN_MODEL", "")
	for _, verdict := range []string{"healthy", "degraded", "unknown"} {
		for _, endpoint := range []string{"images", "responses"} {
			t.Run(verdict+"/"+endpoint, func(t *testing.T) {
				account := astraPolicyTestAccount(verdict)
				account.Credentials["access_token"] = "test-token"
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_image\",\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n"))}}
				svc := newOpenAIImagesTestService(upstream)
				svc.tiboRouteDisabled = true
				body := []byte(`{"model":"gpt-image-1","prompt":"draw a cat","input":"draw a cat","stream":false}`)
				c, rec := newOpenAIImagesTestContext(t, body)
				c.Set("api_key", &APIKey{ID: 42, Group: &Group{AllowImageGeneration: true}})
				var result *OpenAIForwardResult
				var err error
				if endpoint == "images" {
					var parsed *OpenAIImagesRequest
					parsed, err = svc.ParseOpenAIImagesRequest(c, body)
					require.NoError(t, err)
					result, err = svc.ForwardImages(context.Background(), c, account, body, parsed, "")
				} else {
					c.Request.URL.Path = "/v1/responses"
					result, err = svc.Forward(context.Background(), c, account, body)
				}
				require.NoError(t, err, rec.Body.String())
				require.NotNil(t, result)
				require.Equal(t, "gpt-5.6-luna", gjson.GetBytes(upstream.lastBody, "model").String())
				require.Equal(t, "gpt-image-1", gjson.GetBytes(upstream.lastBody, "tools.0.model").String())
				require.Contains(t, rec.Body.String(), "aW1hZ2U=")
			})
		}
	}
}

type astraPolicyFailingRepo struct {
	*tiboAccountRepo
	fail bool
}

func (r *astraPolicyFailingRepo) UpdateExtra(ctx context.Context, id int64, update map[string]any) error {
	if r.fail {
		return errors.New("scheduler cache unavailable")
	}
	return r.tiboAccountRepo.UpdateExtra(ctx, id, update)
}

func TestAstraOAuthModelPolicyProbePersistenceRetriesUnknownAndRecovers(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	repo := &astraPolicyFailingRepo{tiboAccountRepo: tc.svc.accountRepo.(*tiboAccountRepo), fail: true}
	tc.svc.accountRepo = repo
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account, tiboSample(openAITiboDegraded))
	require.True(t, state.http.persistedSampleAt.IsZero())
	repo.fail = false
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account, tiboSample(openAITiboUnknown))
	stored, err := repo.GetByID(context.Background(), tc.account.ID)
	require.NoError(t, err)
	require.Equal(t, "degraded", stored.OpenAIOAuthModelPolicy().Verdict)
	require.False(t, state.http.persistedSampleAt.IsZero())
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account, tiboSample(openAITiboHealthy))
	stored, err = repo.GetByID(context.Background(), tc.account.ID)
	require.NoError(t, err)
	require.Equal(t, "healthy", stored.OpenAIOAuthModelPolicy().Verdict, "one valid green sample wins while confirmed state is red")
	require.Equal(t, openAITiboDegraded, state.http.verdict)
	restarted := newTiboRouteCase(t, false, false)
	resumed := restarted.svc.openAITiboAccountState(tc.account.ID)
	restarted.svc.seedOpenAITiboStateLocked(resumed, stored.Extra, time.Now(), restarted.svc.openAITiboRouteConfig())
	require.Equal(t, openAITiboHealthy, resumed.http.recent[len(resumed.http.recent)-1].Verdict)
	tc.svc.forceOpenAITiboDegraded(tc.account, openAITiboRouteHTTP)
	stored, err = repo.GetByID(context.Background(), tc.account.ID)
	require.NoError(t, err)
	require.Equal(t, "degraded", stored.OpenAIOAuthModelPolicy().Verdict, "hard evidence is persisted even if confirmed verdict was already red")
}

func TestAstraOAuthModelPolicyAdminEditPreservesOriginalConfiguration(t *testing.T) {
	account := astraPolicyTestAccount("degraded")
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	original := account.Credentials["model_mapping"]
	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Name: "renamed", Credentials: map[string]any{"model_mapping": map[string]any{"gpt-6-astra": "gpt-6-astra"}, "unrelated": "keep"}, Extra: map[string]any{"note": "keep", OpenAIAstraPolicyExtraKey: astraPolicyTestHistory("healthy", time.Now())}})
	require.NoError(t, err)
	require.Equal(t, "renamed", updated.Name)
	require.Equal(t, original, updated.Credentials["model_mapping"])
	require.Equal(t, "keep", updated.Credentials["unrelated"])
	require.Equal(t, "keep", updated.Extra["note"])
	require.Equal(t, "degraded", updated.OpenAIOAuthModelPolicy().Verdict, "ordinary edits cannot inject probe state")
	require.True(t, updated.IsModelSupported("gpt-6-astra"))
}

func TestAstraOAuthModelPolicySchedulersDropStickyAndContinueOnEligibleAccount(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
			ctx := context.Background()
			groupID := int64(78)
			green := astraPolicyTestAccount("healthy")
			green.ID = 1
			green.GroupIDs = []int64{groupID}
			green.Concurrency = 2
			green.Extra["openai_oauth_responses_websockets_v2_enabled"] = true
			other := astraPolicyTestAccount("healthy")
			other.ID = 2
			other.GroupIDs = []int64{groupID}
			other.Concurrency = 2
			accounts := []Account{*green, *other}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session": green.ID}}
			svc := newTiboSchedulerService(accounts, cache, false)
			svc.cfg = newSchedulerTestOpenAIWSV2Config()
			svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService(advanced)
			require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, groupID, "resp_previous", green.ID, time.Hour))
			selectModel := func(model string, exclude map[int64]struct{}) (*AccountSelectionResult, error) {
				selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "resp_previous", "session", model, exclude, OpenAIUpstreamTransportAny, false)
				if selection != nil && selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				return selection, err
			}
			accounts[0].Extra = astraPolicyTestAccount("degraded").Extra
			selected, err := selectModel("gpt-6-astra", nil)
			require.NoError(t, err)
			require.Equal(t, green.ID, selected.Account.ID)
			selected, err = selectModel("gpt-6-luna", nil)
			require.NoError(t, err)
			require.Equal(t, green.ID, selected.Account.ID)
			accounts[0].Extra = astraPolicyTestAccount("healthy").Extra
			accounts[1].Schedulable = false
			selected, err = selectModel("gpt-6-astra", nil)
			require.NoError(t, err)
			require.Equal(t, green.ID, selected.Account.ID)
			selected, err = selectModel("gpt-6-luna", nil)
			require.NoError(t, err)
			require.Equal(t, green.ID, selected.Account.ID)
			accounts[0].Schedulable = false
			selected, err = selectModel("gpt-6-astra", nil)
			require.Error(t, err)
			require.Nil(t, selected, "green does not lift pause")
			accounts[0].Schedulable = true
			accounts[0].GroupIDs = []int64{999}
			svc.accountRepo = schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}}
			selected, err = selectModel("gpt-6-astra", nil)
			require.Error(t, err)
			require.Nil(t, selected, "green does not cross groups")
		})
	}
}

type astraPolicyNativeConn struct{ *stagedPassthroughConn }

func (c *astraPolicyNativeConn) WriteJSON(ctx context.Context, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, raw)
}

func TestAstraOAuthModelPolicyCatalogETagChangesWithLatestPoint(t *testing.T) {
	raw := []byte(`{"models":[{"slug":"gpt-6-astra"},{"slug":"gpt-6-luna"},{"slug":"gpt-5.6-luna"},{"slug":"gpt-image-2"}],"unknown_top":true}`)
	previousTag := ""
	for _, verdict := range []string{"healthy", "degraded", "healthy"} {
		account := astraPolicyTestAccount(verdict)
		response := &OpenAIModelsResponse{Body: raw, ETag: CodexModelsManifestETag(raw)}
		require.NoError(t, ApplyPinnedCodexModelsMapping(response, account, nil))
		require.True(t, containsManifestModel(response.Body, "gpt-6-astra"), verdict)
		require.True(t, containsManifestModel(response.Body, "gpt-6-luna"), verdict)
		require.True(t, gjson.GetBytes(response.Body, "unknown_top").Bool())
		if previousTag != "" {
			require.Equal(t, previousTag, response.ETag)
		}
		previousTag = response.ETag
	}
	require.True(t, containsManifestModel(raw, "gpt-6-astra"), "shared raw catalog is immutable")
	require.True(t, containsManifestModel(raw, "gpt-6-luna"))
}

func TestAstraOAuthModelPolicySeedRejectsForeignAndFutureHistory(t *testing.T) {
	svc := &OpenAIGatewayService{}
	for _, kind := range []string{"foreign", "future", "missing_time"} {
		raw := astraPolicyTestHistory("healthy", time.Now().Add(-time.Minute))
		if kind == "foreign" {
			raw["model"] = "gpt-5.6-sol"
		}
		if kind == "future" {
			raw["recent_samples"] = []map[string]any{{"verdict": "healthy", "at": time.Now().Add(time.Hour).Format(time.RFC3339Nano)}}
		}
		if kind == "missing_time" {
			raw["recent_samples"] = []map[string]any{{"verdict": "healthy"}}
		}
		state := &openAITiboAccountState{}
		svc.seedOpenAITiboStateLocked(state, map[string]any{OpenAIAstraPolicyExtraKey: raw}, time.Now(), svc.openAITiboRouteConfig())
		require.Empty(t, state.http.recent, kind)
	}
}
