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

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The failure is tied to a completed, valid source-body read, not an assumed
// number of selection/configuration repository calls. Each case owns its state.
type oauthReliabilityModelsMergePhase struct {
	mu               sync.Mutex
	sourceBodyClosed bool
	sourceCloses     int
	beforeReads      int
	afterReads       int
	upstreamIDs      []int64
	mode             string
	cancel           context.CancelFunc
}

type oauthReliabilityModelsMergePhaseRepo struct {
	codexModelsFailoverAccountRepo
	phase *oauthReliabilityModelsMergePhase
}

var _ service.AccountRepository = (*oauthReliabilityModelsMergePhaseRepo)(nil)

func (r *oauthReliabilityModelsMergePhaseRepo) ListSchedulableByGroupID(ctx context.Context, groupID int64) ([]service.Account, error) {
	r.phase.mu.Lock()
	after := r.phase.sourceBodyClosed
	if after {
		r.phase.afterReads++
	} else {
		r.phase.beforeReads++
	}
	mode, cancel := r.phase.mode, r.phase.cancel
	r.phase.mu.Unlock()
	if after {
		switch mode {
		case "repository_error":
			return nil, errors.New("synthetic-private-repository-detail")
		case "canceled":
			cancel()
			return nil, fmt.Errorf("synthetic merge query: %w", ctx.Err())
		}
	}
	return r.codexModelsFailoverAccountRepo.ListSchedulableByGroupID(ctx, groupID)
}

type oauthReliabilityModelsMergePhaseHTTP struct {
	service.HTTPUpstream
	phase *oauthReliabilityModelsMergePhase
}

var _ service.HTTPUpstream = (*oauthReliabilityModelsMergePhaseHTTP)(nil)

func (u *oauthReliabilityModelsMergePhaseHTTP) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.phase.mu.Lock()
	u.phase.upstreamIDs = append(u.phase.upstreamIDs, accountID)
	u.phase.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Etag": {`"synthetic-source-etag"`}},
		Body: &oauthReliabilityModelsMergePhaseBody{
			Reader: strings.NewReader(`{"models":[{"slug":"synthetic-model","upstream_marker":"retained-source"}]}`),
			phase:  u.phase,
		},
	}, nil
}

type oauthReliabilityModelsMergePhaseBody struct {
	io.Reader
	phase *oauthReliabilityModelsMergePhase
}

func (b *oauthReliabilityModelsMergePhaseBody) Close() error {
	b.phase.mu.Lock()
	b.phase.sourceBodyClosed = true
	b.phase.sourceCloses++
	b.phase.mu.Unlock()
	return nil
}

func oauthReliabilityExerciseModelsPostFetchMerge(t *testing.T, pinned bool, mode string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	phase := &oauthReliabilityModelsMergePhase{mode: mode, cancel: cancel}
	accounts := oauthReliabilityModelsAccounts(2)
	repo := &oauthReliabilityModelsMergePhaseRepo{
		codexModelsFailoverAccountRepo: codexModelsFailoverAccountRepo{accounts: accounts},
		phase:                          phase,
	}
	h := oauthReliabilityModelsHandler(t, repo, &oauthReliabilityModelsMergePhaseHTTP{phase: phase}, 3)
	group := &service.Group{ID: 880, Platform: service.PlatformOpenAI}
	if pinned {
		group.CodexModelsManifestConfig = service.GroupCodexModelsManifestConfig{
			Enabled: true, AccountIDs: []int64{1}, FallbackToScheduler: true,
		}
	}
	core, logs := observer.New(zap.WarnLevel)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.144.0", nil).
		WithContext(logger.IntoContext(ctx, zap.New(core)))
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{GroupID: &group.ID, Group: group})

	h.CodexModels(c)

	phase.mu.Lock()
	before, after, closed := phase.beforeReads, phase.afterReads, phase.sourceCloses
	phase.mu.Unlock()
	require.Equal(t, 1, after, "exercise the final merge, after one successful source fetch")
	require.Equal(t, 1, closed)
	if pinned {
		require.Zero(t, before, "pinned membership uses ListByGroup, not configured manifest generation")
	} else {
		require.GreaterOrEqual(t, before, 1, "initial configured manifest check must succeed before selection")
	}
	require.Equal(t, int64(1), c.GetInt64(opsAccountIDKey), "retain attribution to the account that fetched the catalog")

	// A local final-merge failure must not invalidate the successfully fetched
	// source or replay the upstream request. Read through the real service cache
	// using an uncanceled observer context and the same public request identity.
	cacheCtx := service.WithCodexModelsClientContext(context.Background(), "", "")
	cached, err := h.gatewayService.FetchCodexModelsManifest(cacheCtx, &accounts[0], "0.144.0", "")
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.False(t, cached.NotModified)
	var envelope struct {
		Models []struct {
			Slug   string `json:"slug"`
			Marker string `json:"upstream_marker"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(cached.Body, &envelope))
	require.Len(t, envelope.Models, 1)
	require.Equal(t, "synthetic-model", envelope.Models[0].Slug)
	require.Equal(t, "retained-source", envelope.Models[0].Marker)
	phase.mu.Lock()
	ids := append([]int64(nil), phase.upstreamIDs...)
	phase.mu.Unlock()
	require.Equal(t, []int64{1}, ids, "neither the failed final merge nor the cache read may add an upstream attempt")

	errType, code := service.OpsLocalErrorType(c)
	events := logs.FilterMessage("codex.models_manifest.failed").All()
	switch mode {
	case "success":
		require.NoError(t, ctx.Err())
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Equal(t, []string{"synthetic-model"}, codexHandlerManifestSlugs(t, recorder))
		require.NotEmpty(t, recorder.Header().Get("ETag"))
		require.Empty(t, errType)
		require.Empty(t, code)
		require.Empty(t, events)
	case "repository_error":
		require.NoError(t, ctx.Err(), "a genuine repository failure must not be treated as a canceled request")
		require.Equal(t, http.StatusInternalServerError, recorder.Code)
		require.Equal(t, `{"error":{"message":"Failed to build Codex models manifest","type":"api_error"}}`, recorder.Body.String())
		require.Equal(t, "api_error", errType)
		require.Equal(t, "codex_models_group_account_read_repository", code)
		require.Len(t, events, 1)
		require.Equal(t, map[string]any{"manifest_failure_stage": "group_account_read", "manifest_failure_class": "repository"}, events[0].ContextMap())
		require.NotContains(t, fmt.Sprintf("%+v", events), "synthetic-private")
	case "canceled":
		require.ErrorIs(t, ctx.Err(), context.Canceled)
		require.Empty(t, errType)
		require.Empty(t, code)
		require.Empty(t, events)
		require.False(t, c.Writer.Written(), "post-fetch cancellation must honor the same no-response contract as cancellation before entry; status=%d body=%s", recorder.Code, recorder.Body.String())
		require.Empty(t, recorder.Body.String())
	default:
		t.Fatalf("unknown synthetic mode %q", mode)
	}
}

func TestOAuthReliabilityCodexModelsPostFetchMergeSuccess(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned_%t", pinned), func(t *testing.T) {
			oauthReliabilityExerciseModelsPostFetchMerge(t, pinned, "success")
		})
	}
}

func TestOAuthReliabilityCodexModelsPostFetchMergeRepositoryFailure(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned_%t", pinned), func(t *testing.T) {
			oauthReliabilityExerciseModelsPostFetchMerge(t, pinned, "repository_error")
		})
	}
}

func TestOAuthReliabilityCodexModelsPostFetchMergeCancellation(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned_%t", pinned), func(t *testing.T) {
			oauthReliabilityExerciseModelsPostFetchMerge(t, pinned, "canceled")
		})
	}
}
