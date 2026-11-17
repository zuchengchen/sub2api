//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type oauthReliabilityModelsHTTP struct {
	service.HTTPUpstream
	mu       sync.Mutex
	calls    []int64
	allCold  bool
	etagSeen []string
}

func (u *oauthReliabilityModelsHTTP) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, id)
	u.etagSeen = append(u.etagSeen, req.Header.Get("If-None-Match"))
	u.mu.Unlock()
	if id == 1 || u.allCold {
		return &http.Response{StatusCode: http.StatusNotModified, Header: http.Header{"Etag": {`"source-etag"`}}, Body: http.NoBody}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": {`"source-etag"`}}, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"allowed"},{"slug":"hidden"}]}`))}, nil
}

func oauthReliabilityModelsHandler(t *testing.T, repo service.AccountRepository, upstream service.HTTPUpstream, maxSwitches int) *OpenAIGatewayHandler {
	t.Helper()
	return &OpenAIGatewayHandler{
		gatewayService: service.NewOpenAIGatewayService(repo,
			nil, nil, nil, nil, nil, nil, &config.Config{RunMode: config.RunModeSimple}, nil, nil, nil, nil, nil,
			upstream, nil, nil, nil, nil, nil, nil, nil, nil),
		maxAccountSwitches: maxSwitches,
	}
}

func oauthReliabilityModelsAccounts(count int) []service.Account {
	accounts := make([]service.Account, 0, count)
	for id := 1; id <= count; id++ {
		accounts = append(accounts, service.Account{
			ID: int64(id), Name: "synthetic", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Priority: id - 1, Concurrency: 1,
			Credentials: map[string]any{"api_key": "synthetic-key", "base_url": fmt.Sprintf("https://synthetic-%d.example/v1", id)},
		})
	}
	return accounts
}

func oauthReliabilityModelsRequest(t *testing.T, h *OpenAIGatewayHandler, group *service.Group) (*httptest.ResponseRecorder, *gin.Context, *observer.ObservedLogs) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	core, logs := observer.New(zap.WarnLevel)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.144.0", nil).WithContext(logger.IntoContext(context.Background(), zap.New(core)))
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{GroupID: &group.ID, Group: group})
	h.CodexModels(c)
	return recorder, c, logs
}

func TestOAuthReliabilityCodexModelsCold304FailsOverAndFilters(t *testing.T) {
	upstream := &oauthReliabilityModelsHTTP{}
	handler := oauthReliabilityModelsHandler(t, codexModelsFailoverAccountRepo{accounts: oauthReliabilityModelsAccounts(2)}, upstream, 3)
	// Pinned discovery fails on account1 and then falls back to the scheduler.
	// It ensures configured local model lists cannot bypass the upstream test.
	group := &service.Group{ID: 752, Platform: service.PlatformOpenAI,
		CodexModelsManifestConfig: service.GroupCodexModelsManifestConfig{Enabled: true, AccountIDs: []int64{1}, FallbackToScheduler: true},
		ModelAllowlist:            service.GroupModelAllowlist{Enabled: true, Models: []string{"allowed"}},
	}
	recorder, c, logs := oauthReliabilityModelsRequest(t, handler, group)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, []string{"allowed"}, codexHandlerManifestSlugs(t, recorder))
	require.NotEqual(t, `"source-etag"`, recorder.Header().Get("ETag"))
	upstream.mu.Lock()
	calls, etags := append([]int64(nil), upstream.calls...), append([]string(nil), upstream.etagSeen...)
	upstream.mu.Unlock()
	require.Equal(t, []int64{1, 1, 2}, calls)
	require.Equal(t, []string{"", "", ""}, etags)
	_, code := service.OpsLocalErrorType(c)
	require.Empty(t, code, "recovered cold304 must not leave a final failure marker")
	require.Empty(t, logs.FilterMessage("codex.models_manifest.failed").All())
}

func TestOAuthReliabilityCodexModelsCold304ExhaustionIsUpstreamFailure(t *testing.T) {
	upstream := &oauthReliabilityModelsHTTP{allCold: true}
	handler := oauthReliabilityModelsHandler(t, codexModelsFailoverAccountRepo{accounts: oauthReliabilityModelsAccounts(4)}, upstream, 2)
	group := &service.Group{ID: 753, Platform: service.PlatformComposite}
	recorder, c, logs := oauthReliabilityModelsRequest(t, handler, group)
	require.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), `"type":"upstream_error"`)
	require.Contains(t, recorder.Body.String(), "304 without a cached catalog")
	upstream.mu.Lock()
	calls := append([]int64(nil), upstream.calls...)
	upstream.mu.Unlock()
	require.Equal(t, []int64{1, 2, 3}, calls)
	_, code := service.OpsLocalErrorType(c)
	require.Equal(t, "codex_models_cache_revalidation_upstream_protocol", code)
	events := logs.FilterMessage("codex.models_manifest.failed").All()
	require.Len(t, events, 1)
	require.Equal(t, "cache_revalidation", events[0].ContextMap()["manifest_failure_stage"])
	require.Equal(t, "upstream_protocol", events[0].ContextMap()["manifest_failure_class"])
}

type oauthReliabilityModelsFailingRepo struct{ service.AccountRepository }

func (oauthReliabilityModelsFailingRepo) ListSchedulableByGroupID(context.Context, int64) ([]service.Account, error) {
	return nil, errors.New("synthetic-secret-provider-body")
}

func TestOAuthReliabilityCodexModelsBuildFailureRetainsOnlySafeStage(t *testing.T) {
	handler := oauthReliabilityModelsHandler(t, oauthReliabilityModelsFailingRepo{}, nil, 2)
	recorder, c, logs := oauthReliabilityModelsRequest(t, handler, &service.Group{ID: 754, Platform: service.PlatformOpenAI})
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Contains(t, recorder.Body.String(), "Failed to build Codex models manifest")
	require.NotContains(t, recorder.Body.String(), "synthetic-secret")
	errType, code := service.OpsLocalErrorType(c)
	require.Equal(t, "api_error", errType)
	require.Equal(t, "codex_models_group_account_read_repository", code)
	events := logs.FilterMessage("codex.models_manifest.failed").All()
	require.Len(t, events, 1)
	require.Equal(t, map[string]any{"manifest_failure_stage": "group_account_read", "manifest_failure_class": "repository"}, events[0].ContextMap())
	require.NotContains(t, fmt.Sprintf("%+v", events), "synthetic-secret")
}
