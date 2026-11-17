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
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const oauthModelsMembershipGroupID int64 = 801
const oauthModelsMembershipForeignGroupID int64 = 802
const oauthModelsMembershipPinnedID int64 = 11
const oauthModelsMembershipScheduledID int64 = 12
const oauthModelsMembershipSentinel = "synthetic-membership-repository-private-detail"

// The repository implements standard-mode group queries. A globally existing
// account is deliberately different from an account authorized for this group.
// No fixture uses the simple-mode global account list as a permission shortcut.
type oauthModelsMembershipRepo struct {
	service.AccountRepository
	mu              sync.Mutex
	accounts        map[int64]service.Account
	members         map[int64][]service.Account
	scheduled       map[int64][]service.Account
	membershipErr   error
	membershipReads []int64
	selectionReads  []int64
	getByIDReads    []int64
	unscopedReads   int
}

func (r *oauthModelsMembershipRepo) ListByGroup(_ context.Context, groupID int64) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.membershipReads = append(r.membershipReads, groupID)
	// Return both rows and the sentinel: callers must not consume partial rows
	// when the membership lookup itself failed.
	return append([]service.Account(nil), r.members[groupID]...), r.membershipErr
}

func (r *oauthModelsMembershipRepo) ListSchedulableByGroupID(_ context.Context, groupID int64) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]service.Account(nil), r.scheduled[groupID]...), nil
}

func (r *oauthModelsMembershipRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, groupID int64, platform string) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.selectionReads = append(r.selectionReads, groupID)
	var accounts []service.Account
	for _, account := range r.scheduled[groupID] {
		if account.Platform == platform {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func (r *oauthModelsMembershipRepo) ListSchedulableByPlatform(context.Context, string) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unscopedReads++
	return nil, errors.New("synthetic fixture forbids unscoped account selection")
}

func (r *oauthModelsMembershipRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

func (r *oauthModelsMembershipRepo) ListModelAvailabilityCandidates(_ context.Context, groupID *int64, _ []string, _ bool) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if groupID == nil {
		r.unscopedReads++
		return nil, errors.New("synthetic fixture requires catalog group identity")
	}
	return append([]service.Account(nil), r.members[*groupID]...), nil
}

func (r *oauthModelsMembershipRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getByIDReads = append(r.getByIDReads, id)
	if account, ok := r.accounts[id]; ok {
		return &account, nil
	}
	return nil, service.ErrNoAvailableAccounts
}

func (r *oauthModelsMembershipRepo) failMembership() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.membershipErr = errors.New(oauthModelsMembershipSentinel)
}

func (r *oauthModelsMembershipRepo) observations() (memberships, selections, lookups []int64, unscoped int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.membershipReads...), append([]int64(nil), r.selectionReads...), append([]int64(nil), r.getByIDReads...), r.unscopedReads
}

// Every network boundary terminates here. Unexpected account, method, or path
// calls are counted and rejected in memory; no real client or dialer is used.
type oauthModelsMembershipHTTP struct {
	service.HTTPUpstream
	mu         sync.Mutex
	allowed    map[int64]bool
	calls      []int64
	violations []string
}

func (u *oauthModelsMembershipHTTP) authorize(ids ...int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.allowed = make(map[int64]bool, len(ids))
	for _, id := range ids {
		u.allowed[id] = true
	}
}

func (u *oauthModelsMembershipHTTP) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, accountID)
	allowed := u.allowed[accountID] && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/models")
	if !allowed {
		u.violations = append(u.violations, fmt.Sprintf("account=%d method=%s path=%s", accountID, req.Method, req.URL.Path))
	}
	u.mu.Unlock()
	if !allowed {
		return nil, errors.New("synthetic fixture rejected an unauthorized model-list request")
	}
	body := `{"object":"list","data":[{"id":"g4-allowed","object":"model"},{"id":"g4-allowed-2","object":"model"},{"id":"g4-hidden","object":"model"}]}`
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}, "Etag": {`"synthetic-source-etag"`}},
		Body: io.NopCloser(strings.NewReader(body)), Request: req,
	}, nil
}

func (u *oauthModelsMembershipHTTP) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func (u *oauthModelsMembershipHTTP) observations() (calls []int64, violations []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.calls...), append([]string(nil), u.violations...)
}

func oauthModelsMembershipFixture(t *testing.T, pinnedInGroup bool) (*OpenAIGatewayHandler, *oauthModelsMembershipRepo, *oauthModelsMembershipHTTP) {
	t.Helper()
	pinnedGroup := oauthModelsMembershipGroupID
	if !pinnedInGroup {
		pinnedGroup = oauthModelsMembershipForeignGroupID
	}
	account := func(id, groupID int64) service.Account {
		return service.Account{
			ID: id, Name: "synthetic model discovery", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID},
			Credentials: map[string]any{"api_key": "synthetic-g4-key", "base_url": fmt.Sprintf("https://models-g4-%d.invalid/v1", id)},
		}
	}
	pinned := account(oauthModelsMembershipPinnedID, pinnedGroup)
	scheduled := account(oauthModelsMembershipScheduledID, oauthModelsMembershipGroupID)
	repo := &oauthModelsMembershipRepo{
		accounts:  map[int64]service.Account{pinned.ID: pinned, scheduled.ID: scheduled},
		members:   map[int64][]service.Account{oauthModelsMembershipGroupID: {scheduled}},
		scheduled: map[int64][]service.Account{oauthModelsMembershipGroupID: {scheduled}},
	}
	repo.members[pinnedGroup] = append(repo.members[pinnedGroup], pinned)
	if !pinnedInGroup {
		repo.scheduled[pinnedGroup] = []service.Account{pinned}
	}
	upstream := &oauthModelsMembershipHTTP{}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	disabled := false
	cfg.Gateway.OpenAITiboRoute.Enabled = &disabled
	svc := service.NewOpenAIGatewayService(repo,
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	svc.StopOpenAICodexTicketHarvester()
	return &OpenAIGatewayHandler{gatewayService: svc, maxAccountSwitches: 1}, repo, upstream
}

func oauthModelsMembershipGroup(id int64, fallback bool, models ...string) *service.Group {
	return &service.Group{
		ID: id, Platform: service.PlatformOpenAI,
		CodexModelsManifestConfig: service.GroupCodexModelsManifestConfig{
			Enabled: true, AccountIDs: []int64{oauthModelsMembershipPinnedID}, FallbackToScheduler: fallback,
		},
		ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: models},
	}
}

type oauthModelsMembershipResult struct {
	recorder *httptest.ResponseRecorder
	context  *gin.Context
	logs     *observer.ObservedLogs
}

// Invoke the actual two public handlers and the real Ops capture/queue path.
// Authentication is synthetic but group identity is passed exactly as in ingress.
func oauthModelsMembershipRequest(t *testing.T, codex *OpenAIGatewayHandler, entry string, group *service.Group, etag string) oauthModelsMembershipResult {
	t.Helper()
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.WarnLevel)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	ordinary := &GatewayHandler{openAIGatewayService: codex.gatewayService, maxAccountSwitches: 1}
	var captured *gin.Context
	router.GET("/v1/models", func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 8801, GroupID: &group.ID, Group: group})
		captured = c
		if entry == "codex_manifest" {
			codex.CodexModels(c)
		} else {
			ordinary.Models(c)
		}
	})
	path := "/v1/models"
	if entry == "codex_manifest" {
		path += "?client_version=0.144.0"
	}
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(logger.IntoContext(context.Background(), zap.New(core)))
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	require.NotNil(t, captured)
	return oauthModelsMembershipResult{recorder: recorder, context: captured, logs: logs}
}

func oauthModelsMembershipAssertSuccess(t *testing.T, entry string, result oauthModelsMembershipResult, want []string) {
	t.Helper()
	require.Equal(t, http.StatusOK, result.recorder.Code, result.recorder.Body.String())
	var ids []string
	field, idField := "data", "id"
	if entry == "codex_manifest" {
		field, idField = "models", "slug"
	}
	body := result.recorder.Body.String()
	require.True(t, gjson.Valid(body), body)
	for _, model := range gjson.Get(body, field).Array() {
		ids = append(ids, model.Get(idField).String())
	}
	require.Equal(t, want, ids, body)
	require.NotContains(t, body, "g4-hidden")
	require.NotEmpty(t, result.recorder.Header().Get("ETag"))
	require.NotEqual(t, `"synthetic-source-etag"`, result.recorder.Header().Get("ETag"))
	oauthModelsMembershipAssertNoFinalFailure(t, result)
}

func oauthModelsMembershipAssertNoFinalFailure(t *testing.T, result oauthModelsMembershipResult) {
	t.Helper()
	errType, code := service.OpsLocalErrorType(result.context)
	require.Empty(t, errType)
	require.Empty(t, code, "a recovered membership lookup is not a final failed request")
	require.Empty(t, result.logs.FilterMessage("codex.models_manifest.failed").All())
	require.Empty(t, service.GetOpsStreamErrors(result.context))
	require.Zero(t, OpsErrorLogQueueLength(), "successful discovery must not enter the final failure queue")
	require.Zero(t, OpsErrorLogEnqueuedTotal())
}

func oauthModelsMembershipAssertFailure(t *testing.T, entry string, result oauthModelsMembershipResult, status int) {
	t.Helper()
	require.Equal(t, status, result.recorder.Code, result.recorder.Body.String())
	require.Equal(t, "upstream_error", gjson.Get(result.recorder.Body.String(), "error.type").String())
	if status == http.StatusInternalServerError {
		require.JSONEq(t, `{"error":{"type":"upstream_error","message":"internal error"}}`, result.recorder.Body.String())
	}
	require.NotContains(t, result.recorder.Body.String(), oauthModelsMembershipSentinel)
	require.Empty(t, result.recorder.Header().Get("ETag"), "failure cannot validate an old cached catalog")
	if entry == "codex_manifest" {
		failures := result.logs.FilterMessage("codex.models_manifest.failed").All()
		require.Len(t, failures, 1)
		require.Equal(t, map[string]any{"manifest_failure_stage": "pinned_fetch", "manifest_failure_class": "unknown"}, failures[0].ContextMap())
	} else {
		require.Empty(t, result.logs.FilterMessage("codex.models_manifest.failed").All())
	}
	require.Equal(t, int64(1), OpsErrorLogQueueLength())
	job := <-opsErrorLogQueue
	require.Equal(t, status, job.entry.StatusCode)
	require.Equal(t, "upstream_error", job.entry.ErrorType)
	require.Nil(t, job.entry.AccountID, "membership failed before any account was selected")
	require.Nil(t, job.entry.UpstreamErrorsJSON, "a repository failure is not an upstream HTTP attempt")
	encoded, err := json.Marshal(job.entry)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), oauthModelsMembershipSentinel)
	require.NotContains(t, string(encoded), "synthetic-g4-key")
}

func TestOAuthReliabilityModelsPinnedMembershipRepositoryFailure(t *testing.T) {
	for _, entry := range []string{"codex_manifest", "ordinary_models"} {
		for _, fallback := range []bool{false, true} {
			for _, warm := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/fallback_%t/warm_%t", entry, fallback, warm), func(t *testing.T) {
					setupOpsErrorLogTestQueue(t, 4)
					h, repo, upstream := oauthModelsMembershipFixture(t, true)
					group := oauthModelsMembershipGroup(oauthModelsMembershipGroupID, fallback, "g4-allowed", "g4-allowed-2")
					var wantCalls []int64
					var wantReads []int64
					etag := ""
					if warm {
						upstream.authorize(oauthModelsMembershipPinnedID)
						prime := oauthModelsMembershipRequest(t, h, entry, group, "")
						oauthModelsMembershipAssertSuccess(t, entry, prime, []string{"g4-allowed", "g4-allowed-2"})
						etag = prime.recorder.Header().Get("ETag")
						wantCalls = append(wantCalls, oauthModelsMembershipPinnedID)
						wantReads = append(wantReads, group.ID)
					}
					repo.failMembership()
					group.ModelAllowlist.Models = []string{"g4-allowed-2"}
					upstream.authorize()
					if fallback {
						upstream.authorize(oauthModelsMembershipScheduledID)
						wantCalls = append(wantCalls, oauthModelsMembershipScheduledID)
					}
					result := oauthModelsMembershipRequest(t, h, entry, group, etag)
					if fallback {
						oauthModelsMembershipAssertSuccess(t, entry, result, []string{"g4-allowed-2"})
						require.NotEqual(t, etag, result.recorder.Header().Get("ETag"))
					} else {
						oauthModelsMembershipAssertFailure(t, entry, result, http.StatusInternalServerError)
					}
					calls, violations := upstream.observations()
					require.Equal(t, wantCalls, calls)
					require.Empty(t, violations)
					memberships, selections, _, unscoped := repo.observations()
					require.Equal(t, append(wantReads, group.ID), memberships, "membership is checked again even with a fresh cached body")
					if fallback {
						require.NotEmpty(t, selections)
						for _, selectedGroup := range selections {
							require.Equal(t, group.ID, selectedGroup, "all scheduler reads remain bound to the authenticated group")
						}
					} else {
						require.Empty(t, selections)
					}
					require.Zero(t, unscoped)
				})
			}
		}
	}
}

func TestOAuthReliabilityModelsPinnedMembershipRejectsUnboundAccount(t *testing.T) {
	for _, entry := range []string{"codex_manifest", "ordinary_models"} {
		for _, fallback := range []bool{false, true} {
			for _, warm := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/fallback_%t/warm_%t", entry, fallback, warm), func(t *testing.T) {
					setupOpsErrorLogTestQueue(t, 4)
					h, repo, upstream := oauthModelsMembershipFixture(t, false)
					var wantCalls []int64
					var wantReads []int64
					etag := ""
					if warm {
						owner := oauthModelsMembershipGroup(oauthModelsMembershipForeignGroupID, false, "g4-allowed", "g4-allowed-2")
						upstream.authorize(oauthModelsMembershipPinnedID)
						prime := oauthModelsMembershipRequest(t, h, entry, owner, "")
						oauthModelsMembershipAssertSuccess(t, entry, prime, []string{"g4-allowed", "g4-allowed-2"})
						etag = prime.recorder.Header().Get("ETag")
						wantCalls = append(wantCalls, oauthModelsMembershipPinnedID)
						wantReads = append(wantReads, owner.ID)
					}
					group := oauthModelsMembershipGroup(oauthModelsMembershipGroupID, fallback, "g4-allowed-2")
					upstream.authorize()
					if fallback {
						upstream.authorize(oauthModelsMembershipScheduledID)
						wantCalls = append(wantCalls, oauthModelsMembershipScheduledID)
					}
					result := oauthModelsMembershipRequest(t, h, entry, group, etag)
					if fallback {
						oauthModelsMembershipAssertSuccess(t, entry, result, []string{"g4-allowed-2"})
						require.NotEqual(t, etag, result.recorder.Header().Get("ETag"))
					} else {
						oauthModelsMembershipAssertFailure(t, entry, result, http.StatusServiceUnavailable)
					}
					calls, violations := upstream.observations()
					require.Equal(t, wantCalls, calls, "a foreign account's cache is not an authorization grant")
					require.Empty(t, violations)
					memberships, selections, lookups, unscoped := repo.observations()
					require.Equal(t, append(wantReads, group.ID), memberships)
					require.NotContains(t, lookups, oauthModelsMembershipPinnedID, "a global ID lookup must not replace group membership")
					if fallback {
						require.NotEmpty(t, selections)
						for _, selectedGroup := range selections {
							require.Equal(t, group.ID, selectedGroup, "all scheduler reads remain bound to the authenticated group")
						}
					} else {
						require.Empty(t, selections)
					}
					require.Zero(t, unscoped)
				})
			}
		}
	}
}

func TestOAuthReliabilityModelsPinnedMembershipAuthorizedCatalog(t *testing.T) {
	for _, entry := range []string{"codex_manifest", "ordinary_models"} {
		for _, fallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fallback_%t", entry, fallback), func(t *testing.T) {
				setupOpsErrorLogTestQueue(t, 4)
				h, repo, upstream := oauthModelsMembershipFixture(t, true)
				upstream.authorize(oauthModelsMembershipPinnedID)
				group := oauthModelsMembershipGroup(oauthModelsMembershipGroupID, fallback, "g4-allowed", "g4-allowed-2")
				first := oauthModelsMembershipRequest(t, h, entry, group, "")
				oauthModelsMembershipAssertSuccess(t, entry, first, []string{"g4-allowed", "g4-allowed-2"})
				firstETag, firstBody := first.recorder.Header().Get("ETag"), first.recorder.Body.String()

				// The raw account cache stays reusable, while every request applies
				// current group policy before comparing the client's old ETag.
				upstream.authorize()
				group.ModelAllowlist.Models = []string{"g4-allowed-2"}
				filtered := oauthModelsMembershipRequest(t, h, entry, group, firstETag)
				oauthModelsMembershipAssertSuccess(t, entry, filtered, []string{"g4-allowed-2"})
				require.NotEqual(t, firstETag, filtered.recorder.Header().Get("ETag"))
				unchanged := oauthModelsMembershipRequest(t, h, entry, group, filtered.recorder.Header().Get("ETag"))
				require.Equal(t, http.StatusNotModified, unchanged.recorder.Code)
				require.Empty(t, unchanged.recorder.Body.String())
				oauthModelsMembershipAssertNoFinalFailure(t, unchanged)

				group.ModelAllowlist.Models = []string{"g4-allowed", "g4-allowed-2"}
				restored := oauthModelsMembershipRequest(t, h, entry, group, "")
				oauthModelsMembershipAssertSuccess(t, entry, restored, []string{"g4-allowed", "g4-allowed-2"})
				require.Equal(t, firstBody, restored.recorder.Body.String(), "filtered catalogs cannot overwrite the shared raw account cache")
				require.Equal(t, firstETag, restored.recorder.Header().Get("ETag"))
				calls, violations := upstream.observations()
				require.Equal(t, []int64{oauthModelsMembershipPinnedID}, calls)
				require.Empty(t, violations)
				memberships, selections, _, unscoped := repo.observations()
				require.Equal(t, []int64{group.ID, group.ID, group.ID, group.ID}, memberships)
				require.Empty(t, selections, "successful pinned discovery never enters scheduler fallback")
				require.Zero(t, unscoped)
			})
		}
	}
}
