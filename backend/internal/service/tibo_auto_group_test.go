package service

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type tiboAutoGroupRecorder struct {
	*tiboAccountRepo
	mu      sync.Mutex
	samples []TiboAutoGroupSample
}

func (r *tiboAutoGroupRecorder) PersistTiboProbeResult(ctx context.Context, id int64, updates map[string]any, sample TiboAutoGroupSample) error {
	r.mu.Lock()
	r.samples = append(r.samples, sample)
	r.mu.Unlock()
	return r.tiboAccountRepo.UpdateExtra(ctx, id, updates)
}

func TestTiboAutoGroupOnlyFreshDefinitiveProbe(t *testing.T) {
	for _, verdict := range []openAITiboVerdict{openAITiboHealthy, openAITiboDegraded} {
		t.Run(string(verdict), func(t *testing.T) {
			tc := newTiboRouteCase(t, false, false)
			repo := &tiboAutoGroupRecorder{tiboAccountRepo: tc.svc.accountRepo.(*tiboAccountRepo)}
			tc.svc.accountRepo = repo
			state := tc.svc.openAITiboAccountState(tc.account.ID)
			state.http.verdict = openAITiboHealthy
			state.http.checkedAt = time.Now()
			start := time.Now().Add(-time.Second)
			tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account, openAITiboProbeSample{verdict: verdict, startedAt: start})
			require.Len(t, repo.samples, 1)
			require.Equal(t, string(verdict), repo.samples[0].Verdict)
			require.Equal(t, start, repo.samples[0].StartedAt)
			require.Equal(t, openAITiboHealthy, state.http.verdict, "a single red does not change confirmed state, but must drive grouping")
			// A failed persistence may leave historical points dirty. Unknown
			// retries their storage but must never authorize a membership move.
			state.http.persistedSampleAt = time.Time{}
			tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account, openAITiboProbeSample{verdict: openAITiboUnknown, status: http.StatusTooManyRequests})
			require.Len(t, repo.samples, 1)
			tc.svc.seedOpenAITiboStateLocked(tc.svc.openAITiboAccountState(999), tc.account.Extra, time.Now(), tc.svc.openAITiboRouteConfig())
			require.Len(t, repo.samples, 1)
			// Passive business-response degradation persists without group authority.
			tc.svc.persistOpenAITiboVerdict(tc.account.ID, openAITiboRouteHTTP, openAITiboVerdictRecordFor(&state.http), state.http.recent)
			require.Len(t, repo.samples, 1)
		})
	}
}

func TestTiboAutoGroupCredentialScope(t *testing.T) {
	for _, kind := range []string{AccountTypeAPIKey, AccountTypeSetupToken} {
		tc := newTiboRouteCase(t, false, false)
		tc.account.Type = kind
		repo := &tiboAutoGroupRecorder{tiboAccountRepo: tc.svc.accountRepo.(*tiboAccountRepo)}
		tc.svc.accountRepo = repo
		tc.svc.recordOpenAITiboSample(tc.svc.openAITiboAccountState(tc.account.ID), openAITiboRouteHTTP, tc.account, tiboSample(openAITiboDegraded))
		require.Empty(t, repo.samples)
	}
}

func TestTiboAutoGroupScheduledProbeRecordsStart(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("False"))
	repo := &tiboAutoGroupRecorder{tiboAccountRepo: tc.svc.accountRepo.(*tiboAccountRepo)}
	tc.svc.accountRepo = repo
	tc.svc.openaiTiboLoaded.Store(true)
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.http.verdict = openAITiboHealthy
	state.http.checkedAt = time.Now()
	before := time.Now()
	tc.svc.probeOpenAITiboRoutes(context.Background())
	tc.svc.openaiTiboProbeWG.Wait()
	require.Len(t, repo.samples, 1)
	require.Equal(t, "degraded", repo.samples[0].Verdict)
	require.False(t, repo.samples[0].StartedAt.Before(before))
	require.False(t, repo.samples[0].CompletedAt.Before(repo.samples[0].StartedAt))
	require.Equal(t, openAITiboHealthy, state.http.verdict)
}
