//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/userfacing"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const oauthManifestSelectionRepositorySentinel = "synthetic-private-manifest-selection-repository"

type oauthManifestSelectionState struct {
	mu             sync.Mutex
	mode           string
	cancel         context.CancelFunc
	selectionReads int
	upstreamCalls  int
	unscopedReads  int
}

type oauthManifestSelectionRepo struct {
	codexModelsFailoverAccountRepo
	state *oauthManifestSelectionState
}

// Configuration/catalog reads succeed without explicit mappings; only the
// subsequent scheduler query fails. This exercises the final selection branch.
func (r *oauthManifestSelectionRepo) ListSchedulableByGroupID(context.Context, int64) ([]service.Account, error) {
	return nil, nil
}

func (r *oauthManifestSelectionRepo) ListByGroup(ctx context.Context, groupID int64) ([]service.Account, error) {
	if r.state.mode == "pinned_repository" {
		return nil, errors.New(oauthManifestSelectionRepositorySentinel)
	}
	return r.codexModelsFailoverAccountRepo.ListByGroup(ctx, groupID)
}

func (r *oauthManifestSelectionRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, _ string) ([]service.Account, error) {
	r.state.mu.Lock()
	r.state.selectionReads++
	calls, mode, cancel := r.state.upstreamCalls, r.state.mode, r.state.cancel
	r.state.mu.Unlock()
	switch mode {
	case "wrapped_no_accounts":
		return nil, fmt.Errorf("synthetic scheduler: %w", service.ErrNoAvailableAccounts)
	case "wrapped_no_compact_accounts":
		return nil, fmt.Errorf("synthetic scheduler: %w", service.ErrNoAvailableCompactAccounts)
	case "repository_error", "pinned_upstream_then_repository":
		return nil, errors.New(oauthManifestSelectionRepositorySentinel)
	case "repository_same_message":
		return nil, errors.New(userfacing.NoAvailableOpenAIAccounts)
	case "repository_cancel_value":
		// A canceled dependency does not prove the live client request ended.
		return nil, fmt.Errorf("synthetic repository: %w", context.Canceled)
	case "repository_deadline_value":
		return nil, fmt.Errorf("synthetic repository: %w", context.DeadlineExceeded)
	case "cancel_during_selection":
		cancel()
		return nil, fmt.Errorf("synthetic repository: %w", ctx.Err())
	case "upstream_then_empty":
		if calls == 0 {
			return append([]service.Account(nil), r.accounts...), nil
		}
	}
	// The real selector creates the typed no-available result for an empty set.
	return nil, nil
}

func (r *oauthManifestSelectionRepo) ListSchedulableByPlatform(context.Context, string) ([]service.Account, error) {
	r.state.mu.Lock()
	r.state.unscopedReads++
	r.state.mu.Unlock()
	return nil, errors.New("synthetic fixture forbids unscoped selection")
}

// Every upstream operation ends in this in-memory fake. The body intentionally
// uses the same public message as the local selection failure.
type oauthManifestSelectionHTTP struct {
	service.HTTPUpstream
	state *oauthManifestSelectionState
}

func (u *oauthManifestSelectionHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.state.mu.Lock()
	u.state.upstreamCalls++
	u.state.mu.Unlock()
	body, err := json.Marshal(map[string]any{"error": map[string]any{
		"type": "upstream_error", "message": userfacing.NoAvailableOpenAIAccounts,
	}})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Status:     "503 Service Unavailable",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Request:    req,
	}, nil
}

func oauthManifestSelectionFixture(t *testing.T, mode string, cancel context.CancelFunc) (*OpenAIGatewayHandler, *oauthManifestSelectionState) {
	t.Helper()
	state := &oauthManifestSelectionState{mode: mode, cancel: cancel}
	repo := &oauthManifestSelectionRepo{
		codexModelsFailoverAccountRepo: codexModelsFailoverAccountRepo{accounts: oauthReliabilityModelsAccounts(1)},
		state:                          state,
	}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	disabled := false
	cfg.Gateway.OpenAITiboRoute.Enabled = &disabled
	svc := service.NewOpenAIGatewayService(repo,
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		&oauthManifestSelectionHTTP{state: state}, nil, nil, nil, nil, nil, nil, nil, nil)
	svc.StopOpenAICodexTicketHarvester()
	return &OpenAIGatewayHandler{gatewayService: svc, maxAccountSwitches: 1}, state
}

func oauthManifestSelectionRequest(t *testing.T, h *OpenAIGatewayHandler, group *service.Group, path string, ctx context.Context) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	written := false
	router.GET(path, func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{GroupID: &group.ID, Group: group})
		h.CodexModels(c)
		written = c.Writer.Written()
	})
	requestPath := path
	if path != "/backend-api/codex/models" {
		requestPath += "?client_version=0.144.0"
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestPath, nil).WithContext(ctx))
	return recorder, written
}

func TestOAuthReliabilityManifestSelectionLocalOps(t *testing.T) {
	for _, path := range []string{"/v1/models", "/models", "/backend-api/codex/models"} {
		for _, tc := range []struct {
			mode            string
			businessLimited bool
		}{
			{mode: "empty", businessLimited: true},
			{mode: "wrapped_no_accounts", businessLimited: true},
			{mode: "wrapped_no_compact_accounts", businessLimited: true},
			{mode: "repository_error"},
			{mode: "repository_same_message"},
			{mode: "repository_cancel_value"},
			{mode: "repository_deadline_value"},
		} {
			t.Run(path+"/"+tc.mode, func(t *testing.T) {
				setupOpsErrorLogTestQueue(t, 2)
				h, state := oauthManifestSelectionFixture(t, tc.mode, nil)
				group := &service.Group{ID: 918, Platform: service.PlatformOpenAI}
				recorder, written := oauthManifestSelectionRequest(t, h, group, path, context.Background())
				require.True(t, written)
				require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
				require.Equal(t, userfacing.NoAvailableOpenAIAccounts, gjson.Get(recorder.Body.String(), "error.message").String())
				require.NotContains(t, recorder.Body.String(), oauthManifestSelectionRepositorySentinel)
				state.mu.Lock()
				reads, calls, unscoped := state.selectionReads, state.upstreamCalls, state.unscopedReads
				state.mu.Unlock()
				require.GreaterOrEqual(t, reads, 1, "near-limit preference can read the same scheduler repository before final selection")
				require.Zero(t, calls, "selection failure must occur before any upstream request")
				require.Zero(t, unscoped)
				require.Equal(t, int64(1), OpsErrorLogQueueLength())
				entry := (<-opsErrorLogQueue).entry
				require.Equal(t, http.StatusServiceUnavailable, entry.StatusCode)
				require.Equal(t, "routing", entry.ErrorPhase)
				require.Equal(t, "platform", entry.ErrorOwner)
				require.Equal(t, "gateway", entry.ErrorSource)
				require.Equal(t, "api_error", entry.ErrorType)
				require.Equal(t, "api_error", gjson.Get(recorder.Body.String(), "error.type").String())
				require.Equal(t, tc.businessLimited, entry.IsBusinessLimited)
				require.Nil(t, entry.AccountID)
				require.Nil(t, entry.UpstreamStatusCode)
				require.Nil(t, entry.UpstreamErrorMessage)
				require.Empty(t, entry.UpstreamErrors)
				require.Nil(t, entry.UpstreamErrorsJSON)
				encoded, err := json.Marshal(entry)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), oauthManifestSelectionRepositorySentinel)
				// Direct test-channel reads bypass the worker-maintained length gauge.
				require.Empty(t, opsErrorLogQueue)
				require.Equal(t, int64(1), OpsErrorLogEnqueuedTotal())
			})
		}
	}
}

func TestOAuthReliabilityManifestSelectionCanceledOps(t *testing.T) {
	for _, path := range []string{"/v1/models", "/models", "/backend-api/codex/models"} {
		for _, mode := range []string{"cancel_before_entry", "cancel_during_selection"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				setupOpsErrorLogTestQueue(t, 2)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				h, state := oauthManifestSelectionFixture(t, mode, cancel)
				if mode == "cancel_before_entry" {
					cancel()
				}
				group := &service.Group{ID: 918, Platform: service.PlatformOpenAI}
				recorder, written := oauthManifestSelectionRequest(t, h, group, path, ctx)
				require.False(t, written, "handler must not commit a response after cancellation")
				require.Empty(t, recorder.Body.String())
				require.Zero(t, OpsErrorLogQueueLength())
				require.Zero(t, OpsErrorLogEnqueuedTotal())
				state.mu.Lock()
				reads, calls := state.selectionReads, state.upstreamCalls
				state.mu.Unlock()
				if mode == "cancel_before_entry" {
					require.Zero(t, reads)
				} else {
					require.GreaterOrEqual(t, reads, 1, "near-limit preference can read the same scheduler repository before final selection")
				}
				require.Zero(t, calls)
			})
		}
	}
}

func TestOAuthReliabilityManifestSelectionPreservesPriorFailureOps(t *testing.T) {
	for _, path := range []string{"/v1/models", "/models", "/backend-api/codex/models"} {
		for _, tc := range []struct {
			mode       string
			pinned     bool
			wantStatus int
			wantCalls  int
		}{
			{mode: "upstream_then_empty", wantStatus: http.StatusBadGateway, wantCalls: 1},
			{mode: "pinned_upstream_then_empty", pinned: true, wantStatus: http.StatusServiceUnavailable, wantCalls: 1},
			{mode: "pinned_upstream_then_repository", pinned: true, wantStatus: http.StatusServiceUnavailable, wantCalls: 1},
			{mode: "pinned_repository", pinned: true, wantStatus: http.StatusServiceUnavailable},
			{mode: "pinned_empty", pinned: true, wantStatus: http.StatusServiceUnavailable},
		} {
			t.Run(path+"/"+tc.mode, func(t *testing.T) {
				setupOpsErrorLogTestQueue(t, 2)
				h, state := oauthManifestSelectionFixture(t, tc.mode, nil)
				group := &service.Group{ID: 918, Platform: service.PlatformOpenAI}
				if tc.pinned {
					group.CodexModelsManifestConfig = service.GroupCodexModelsManifestConfig{
						Enabled: true, AccountIDs: []int64{1}, FallbackToScheduler: true,
					}
					if tc.mode == "pinned_empty" {
						group.CodexModelsManifestConfig.AccountIDs = []int64{2}
					}
				}
				recorder, written := oauthManifestSelectionRequest(t, h, group, path, context.Background())
				require.True(t, written)
				require.Equal(t, tc.wantStatus, recorder.Code)
				require.Equal(t, "upstream_error", gjson.Get(recorder.Body.String(), "error.type").String())
				if tc.pinned {
					require.Equal(t, userfacing.NoAvailableOpenAIAccounts, gjson.Get(recorder.Body.String(), "error.message").String())
				} else {
					require.Contains(t, gjson.Get(recorder.Body.String(), "error.message").String(), userfacing.NoAvailableOpenAIAccounts)
				}
				require.NotContains(t, recorder.Body.String(), oauthManifestSelectionRepositorySentinel)
				state.mu.Lock()
				reads, calls, unscoped := state.selectionReads, state.upstreamCalls, state.unscopedReads
				state.mu.Unlock()
				require.GreaterOrEqual(t, reads, 1)
				require.Equal(t, tc.wantCalls, calls)
				require.Zero(t, unscoped)
				require.Equal(t, int64(1), OpsErrorLogQueueLength())
				entry := (<-opsErrorLogQueue).entry
				require.Equal(t, tc.wantStatus, entry.StatusCode)
				require.Equal(t, "upstream_error", entry.ErrorType)
				require.Equal(t, "upstream", entry.ErrorPhase)
				require.Equal(t, "provider", entry.ErrorOwner)
				require.Equal(t, "upstream_http", entry.ErrorSource)
				require.False(t, entry.IsBusinessLimited, "a prior failure must not become local capacity by matching the final message")
				if !tc.pinned {
					require.NotNil(t, entry.AccountID)
				}
				// The worker decrements its gauge; this fixture reads the channel directly.
				require.Empty(t, opsErrorLogQueue)
				require.Equal(t, int64(1), OpsErrorLogEnqueuedTotal())
			})
		}
	}
}
