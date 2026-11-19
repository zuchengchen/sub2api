//go:build unit

package service

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openAIAccessProbeTestStore struct {
	AccountRepository
	mu           sync.Mutex
	accounts     map[int64]*Account
	clearFailure error
	clearCalls   int
}

func (s *openAIAccessProbeTestStore) GetByID(_ context.Context, id int64) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[id]
	if a == nil {
		return nil, ErrAccountNotFound
	}
	return snapshotOAuthRefreshAccount(a), nil
}

func (s *openAIAccessProbeTestStore) ClearOpenAIOAuthRefreshErrorIfCredentialsUnchanged(_ context.Context, id int64, creds map[string]any, proxy *int64, msg string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearCalls++
	if s.clearFailure != nil {
		return false, s.clearFailure
	}
	a := s.accounts[id]
	if a.Status != StatusError || !a.Schedulable || a.ErrorMessage != msg || !reflect.DeepEqual(a.ProxyID, proxy) || !reflect.DeepEqual(a.Credentials, creds) {
		return false, nil
	}
	a.Status = StatusActive
	a.ErrorMessage = ""
	return true, nil
}

func verifiedOpenAIAccessProbe(_ context.Context, a *Account) openAIAccessProbeResult {
	return openAIAccessProbeResult{outcome: openAIAccessProbeVerified, authorization: "Bearer " + a.GetOpenAIAccessToken()}
}

func TestOpenAIAccessProbeWorkerCoalescesBurstAndBoundsRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _, _, _ := newOpenAIRecoveryFixture()
		s := &openAIAccessProbeTestStore{accounts: map[int64]*Account{}}
		api := NewOAuthRefreshAPI(s, nil)
		denial := &openai.RefreshTokenError{Permanent: true}
		for id := int64(1); id <= 8; id++ {
			current := snapshotOAuthRefreshAccount(a)
			current.ID = id
			s.accounts[id] = current
			for range 20 {
				api.recordOpenAIRefreshFailure(current, denial)
			}
		}
		calls := map[int64][]time.Time{}
		var starts []time.Time
		api.startOpenAIAccessVerification(func(ctx context.Context, account *Account) openAIAccessProbeResult {
			calls[account.ID] = append(calls[account.ID], time.Now())
			starts = append(starts, time.Now())
			switch account.ID {
			case 1:
				return openAIAccessProbeResult{outcome: openAIAccessProbeRetry}
			case 2:
				if len(calls[2]) < 2 {
					return openAIAccessProbeResult{outcome: openAIAccessProbeRetry}
				}
			}
			return verifiedOpenAIAccessProbe(ctx, account)
		})
		defer api.StopOpenAIAccessVerification()
		api.startOpenAIAccessVerification(func(context.Context, *Account) openAIAccessProbeResult {
			panic("a second Start must not spawn a second worker")
		})
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		require.Len(t, calls[1], 3)
		require.Len(t, calls[2], 2)
		require.GreaterOrEqual(t, calls[1][1].Sub(calls[1][0]), openAIAccessProbeBackoff[0])
		require.GreaterOrEqual(t, calls[1][2].Sub(calls[1][1]), openAIAccessProbeBackoff[1])
		require.Len(t, starts, 11)
		for i := 1; i < len(starts); i++ {
			require.GreaterOrEqual(t, starts[i].Sub(starts[i-1]), openAIAccessProbeInterval)
		}
		for id, account := range s.accounts {
			state := api.openAIRejectionForAccount(account)
			require.Equal(t, id != 1, state.autoVerified)
			require.Equal(t, id != 1, state.accessVerifiedAt(account, time.Now()))
			if id > 2 {
				require.Len(t, calls[id], 1)
			}
			api.recordOpenAIRefreshFailure(account, denial)
			require.Equal(t, id != 1, api.openAIRejectionForAccount(account).autoVerified)
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		require.Len(t, starts, 11, "repeated denials for the same credential must not rearm expired retry budgets")
	})
}

func TestOpenAIAccessProbeWorkerDiscardsStaleResults(t *testing.T) {
	for _, change := range []string{"inference 401", "credentials", "proxy", "expired", "manual pause", "other error", "wrong bearer", "stop"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, repo, _, p := newOpenAIRecoveryFixture()
				api := p.refreshAPI
				entered, finish := make(chan struct{}), make(chan struct{})
				calls := 0
				api.startOpenAIAccessVerification(func(ctx context.Context, got *Account) openAIAccessProbeResult {
					calls++
					close(entered)
					select {
					case <-finish:
					case <-ctx.Done():
					}
					if change == "wrong bearer" {
						return openAIAccessProbeResult{outcome: openAIAccessProbeVerified, authorization: "Bearer another"}
					}
					return verifiedOpenAIAccessProbe(ctx, got)
				})
				defer api.StopOpenAIAccessVerification()
				api.recordOpenAIRefreshFailure(a, &openai.RefreshTokenError{Permanent: true})
				<-entered
				switch change {
				case "inference 401":
					api.rejectVerifiedOpenAIAccess(a, "Bearer "+a.GetOpenAIAccessToken())
				case "stop":
					api.StopOpenAIAccessVerification()
				default:
					repo.mu.Lock()
					switch change {
					case "credentials":
						repo.account.Credentials["refresh_token"] = "replacement"
					case "proxy":
						id := int64(91)
						repo.account.ProxyID = &id
					case "expired":
						repo.account.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
					case "manual pause":
						repo.account.Schedulable = false
					case "other error":
						repo.account.Status = StatusError
						repo.account.ErrorMessage = "separate administrative error"
					}
					repo.mu.Unlock()
				}
				close(finish)
				synctest.Wait()
				require.Equal(t, 1, calls)
				require.False(t, api.openAIRejectionForAccount(a).accessVerifiedAt(a, time.Now()))
				if change == "inference 401" {
					api.recordOpenAIRefreshFailure(a, &openai.RefreshTokenError{Permanent: true})
					time.Sleep(3 * time.Minute)
					synctest.Wait()
					require.Equal(t, 1, calls, "a later refresh-only failure does not supersede the inference 401")
				}
			})
		})
	}
}

func TestOpenAIAccessProbeWorkerManuallyPausedAndExpiredNeverGoUpstream(t *testing.T) {
	for _, state := range []string{"paused", "account expired", "token expired", "unknown token lifetime", "unrelated status", "after replay 401"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, repo, _, p := newOpenAIRecoveryFixture()
				now := time.Now()
				switch state {
				case "paused":
					a.Schedulable = false
				case "account expired":
					a.AutoPauseOnExpired = true
					a.ExpiresAt = &now
				case "token expired":
					a.Credentials["expires_at"] = now.Format(time.RFC3339)
				case "unknown token lifetime":
					delete(a.Credentials, "expires_at")
				case "unrelated status":
					a.Status = StatusError
					a.ErrorMessage = "manually held"
				}
				repo.account = snapshotOAuthRefreshAccount(a)
				probed := false
				p.refreshAPI.startOpenAIAccessVerification(func(ctx context.Context, got *Account) openAIAccessProbeResult {
					probed = true
					return verifiedOpenAIAccessProbe(ctx, got)
				})
				defer p.refreshAPI.StopOpenAIAccessVerification()
				if state == "after replay 401" {
					p.RecordUnauthorizedAfterRefresh(context.Background(), a, a.GetOpenAIAccessToken())
				}
				p.refreshAPI.recordOpenAIRefreshFailure(a, &openai.RefreshTokenError{Permanent: true})
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				require.False(t, probed)
				require.False(t, p.refreshAPI.openAIRejectionForAccount(a).autoVerified)
			})
		})
	}
}

func TestOpenAIAccessProbeUnauthorizedHonorsSentSnapshot(t *testing.T) {
	for _, tc := range []string{"same", "new proxy", "new credential document", "wrong bearer"} {
		t.Run(tc, func(t *testing.T) {
			a, repo, _, p := newOpenAIRecoveryFixture()
			api := p.refreshAPI
			api.recordOpenAIRefreshFailure(a, &openai.RefreshTokenError{Permanent: true})
			before := api.openAIRejectionForAccount(a)
			header := "Bearer " + a.GetOpenAIAccessToken()
			outcome, _ := api.runOpenAIAccessProbe(context.Background(), a.ID, before, func(ctx context.Context, _ *Account) openAIAccessProbeResult {
				switch tc {
				case "new proxy":
					proxy := int64(19)
					repo.account.ProxyID = &proxy
				case "new credential document":
					repo.account.Credentials["_token_version"] = "new"
				}
				latest, err := repo.GetByID(ctx, a.ID)
				require.NoError(t, err)
				require.True(t, api.verifyOpenAIAccessAfterTest(ctx, latest, before, header, nil))
				if tc == "wrong bearer" {
					return openAIAccessProbeResult{outcome: openAIAccessProbeUnauthorized, authorization: "Bearer different"}
				}
				return openAIAccessProbeResult{outcome: openAIAccessProbeUnauthorized, authorization: header}
			})
			require.Equal(t, tc == "same", outcome == openAIAccessProbeUnauthorized)
			require.Equal(t, tc != "same", api.openAIRejectionForAccount(a).accessVerifiedAt(a, time.Now()))
		})
	}
}

func TestOpenAIAccessProbeWorkerRecoversOnlyItsRefreshErrorAfterCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _, _, _ := newOpenAIRecoveryFixture()
		denial := &openai.RefreshTokenError{Permanent: true, Code: "invalid_grant"}
		reset := time.Now().Add(time.Minute)
		store := &openAIAccessProbeTestStore{accounts: map[int64]*Account{a.ID: snapshotOAuthRefreshAccount(a)}, clearFailure: errors.New("database unavailable")}
		api := NewOAuthRefreshAPI(store, nil)
		api.recordOpenAIRefreshFailure(a, denial)
		stored := store.accounts[a.ID]
		stored.Status, stored.ErrorMessage = StatusError, nonRetryableTokenRefreshMessage(denial)
		stored.RateLimitResetAt = &reset
		var calls atomic.Int32
		api.startOpenAIAccessVerification(func(ctx context.Context, got *Account) openAIAccessProbeResult {
			calls.Add(1)
			return verifiedOpenAIAccessProbe(ctx, got)
		})
		defer api.StopOpenAIAccessVerification()
		synctest.Wait()
		require.Zero(t, calls.Load(), "a new rejection cannot override an existing cooldown")
		time.Sleep(time.Minute)
		synctest.Wait()
		require.EqualValues(t, 1, calls.Load())
		store.mu.Lock()
		attempts, status := store.clearCalls, stored.Status
		store.clearFailure = nil
		store.mu.Unlock()
		require.Equal(t, 1, attempts)
		require.Equal(t, StatusError, status)
		require.False(t, api.openAIRejectionForAccount(a).accessVerifiedAt(a, time.Now()))
		time.Sleep(openAIAccessProbeBackoff[0])
		synctest.Wait()
		require.EqualValues(t, 2, calls.Load())
		recovered, err := store.GetByID(context.Background(), a.ID)
		require.NoError(t, err)
		require.Equal(t, StatusActive, recovered.Status)
		require.Empty(t, recovered.ErrorMessage)
		require.Equal(t, &reset, recovered.RateLimitResetAt)
		require.True(t, api.openAIRejectionForAccount(a).autoVerified)
		require.Same(t, denial, api.openAIRefreshFailure(a))
	})
}
