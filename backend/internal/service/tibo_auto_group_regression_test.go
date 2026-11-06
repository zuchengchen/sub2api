package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// This recorder deliberately accepts observations by StartedAt, never by arrival
// or completion time. The repository integration suite verifies the real SQL
// guard; this fixture proves that the production prober supplies the right key.
type tiboAutoGroupOrderedRecorder struct {
	*tiboAutoGroupRecorder
	applied []TiboAutoGroupSample
	latest  TiboAutoGroupSample
}

func (r *tiboAutoGroupOrderedRecorder) PersistTiboProbeResult(ctx context.Context, id int64, updates map[string]any, sample TiboAutoGroupSample) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, sample)
	if sample.StartedAt.IsZero() || sample.CompletedAt.Before(sample.StartedAt) {
		return fmt.Errorf("probe did not supply a valid start/completion interval")
	}
	if !r.latest.StartedAt.IsZero() && !sample.StartedAt.After(r.latest.StartedAt) {
		return nil
	}
	if err := r.tiboAccountRepo.UpdateExtra(ctx, id, updates); err != nil {
		return err
	}
	r.latest = sample
	r.applied = append(r.applied, sample)
	return nil
}

func (r *tiboAutoGroupOrderedRecorder) observations() ([]TiboAutoGroupSample, []TiboAutoGroupSample, TiboAutoGroupSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TiboAutoGroupSample(nil), r.samples...), append([]TiboAutoGroupSample(nil), r.applied...), r.latest
}

type tiboAutoGroupProbeCall struct {
	enteredAt time.Time
	body      []byte
}

type tiboAutoGroupGatedUpstream struct {
	answer  string
	entered chan tiboAutoGroupProbeCall
	release chan struct{}
	calls   atomic.Int32
}

func (u *tiboAutoGroupGatedUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if u.calls.Add(1) != 1 {
		return nil, fmt.Errorf("unexpected additional Tibo probe")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	call := tiboAutoGroupProbeCall{enteredAt: time.Now(), body: body}
	select {
	case u.entered <- call:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	select {
	case <-u.release:
		return cookieWSHTTPResponse(u.answer), nil
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

func (u *tiboAutoGroupGatedUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestTiboAutoGroupOverlappingProbesOrderByActualStart(t *testing.T) {
	for _, test := range []struct {
		name, olderAnswer, newerAnswer, olderVerdict, newerVerdict string
	}{
		{"newer_green_wins", "False", "True", "degraded", "healthy"},
		{"newer_red_wins", "True", "False", "healthy", "degraded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := newTiboRouteCase(t, false, false)
			second := newTiboRouteCase(t, false, false)
			repo := &tiboAutoGroupOrderedRecorder{
				tiboAutoGroupRecorder: &tiboAutoGroupRecorder{
					tiboAccountRepo: first.svc.accountRepo.(*tiboAccountRepo),
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			var probesDone []<-chan struct{}
			t.Cleanup(func() {
				cancel()
				for _, done := range probesDone {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("Tibo probe did not stop after cancellation")
					}
				}
			})

			makeUpstream := func(answer string) *tiboAutoGroupGatedUpstream {
				return &tiboAutoGroupGatedUpstream{
					answer: answer, entered: make(chan tiboAutoGroupProbeCall, 1), release: make(chan struct{}),
				}
			}
			firstUpstream, secondUpstream := makeUpstream(test.olderAnswer), makeUpstream(test.newerAnswer)
			first.svc.accountRepo, second.svc.accountRepo = repo, repo
			first.svc.httpUpstream, second.svc.httpUpstream = firstUpstream, secondUpstream
			start := func(svc *OpenAIGatewayService) <-chan struct{} {
				// Only bypass enrollment staggering. The harvester hook, account
				// reload, probe transport, sample recording and persistence are real.
				svc.openaiTiboLoaded.Store(true)
				svc.openAITiboAccountState(first.account.ID)
				svc.probeOpenAITiboRoutes(ctx)
				done := make(chan struct{})
				go func() {
					svc.openaiTiboProbeWG.Wait()
					close(done)
				}()
				probesDone = append(probesDone, done)
				return done
			}
			awaitCall := func(upstream *tiboAutoGroupGatedUpstream) tiboAutoGroupProbeCall {
				t.Helper()
				select {
				case call := <-upstream.entered:
					requireTiboProbe(t, call.body)
					return call
				case <-time.After(5 * time.Second):
					t.Fatal("scheduled Tibo probe never reached the controlled upstream")
					return tiboAutoGroupProbeCall{}
				}
			}
			awaitDone := func(done <-chan struct{}) {
				t.Helper()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("scheduled Tibo probe never completed")
				}
			}

			beforeFirst := time.Now()
			firstDone := start(first.svc)
			firstCall := awaitCall(firstUpstream)
			// S1 is already waiting on its HTTP response before S2 starts.
			beforeSecond := time.Now()
			secondDone := start(second.svc)
			secondCall := awaitCall(secondUpstream)
			select {
			case <-firstDone:
				t.Fatal("older probe completed before its response was released")
			default:
			}

			close(secondUpstream.release)
			awaitDone(secondDone)
			received, applied, winner := repo.observations()
			require.Len(t, received, 1)
			require.Len(t, applied, 1)
			require.Equal(t, test.newerVerdict, winner.Verdict)
			require.False(t, winner.StartedAt.Before(beforeSecond))
			require.False(t, winner.StartedAt.After(secondCall.enteredAt), "start must precede the upstream response")

			close(firstUpstream.release)
			awaitDone(firstDone)
			received, applied, finalWinner := repo.observations()
			require.Len(t, received, 2)
			newer, older := received[0], received[1]
			require.Equal(t, test.newerVerdict, newer.Verdict)
			require.Equal(t, test.olderVerdict, older.Verdict)
			require.False(t, older.StartedAt.Before(beforeFirst))
			require.False(t, older.StartedAt.After(firstCall.enteredAt), "late completion must retain the real earlier start")
			require.True(t, older.StartedAt.Before(newer.StartedAt))
			require.True(t, older.CompletedAt.After(newer.CompletedAt), "responses must actually finish in reverse start order")
			require.Len(t, applied, 1, "the late old probe must not replace the newer accepted observation")
			require.Equal(t, winner, finalWinner)
			require.EqualValues(t, 1, firstUpstream.calls.Load())
			require.EqualValues(t, 1, secondUpstream.calls.Load())
			update, ok := repo.updateFor(first.account.ID)
			require.True(t, ok)
			record, ok := parseOpenAITiboVerdictRecord(update[openAITiboVerdictExtraKey(openAITiboRouteHTTP)])
			require.True(t, ok)
			require.Equal(t, openAITiboVerdict(test.newerVerdict), record.Verdict)
		})
	}
}

type tiboAutoGroupSchedulingRepo struct {
	schedulerGroupAwareOpenAIAccountRepo
	mu    sync.Mutex
	reads map[int64]int
}

func cloneTiboAutoGroupSchedulingAccount(account *Account) *Account {
	cloned := cloneTiboAccount(account)
	cloned.GroupIDs = append([]int64(nil), account.GroupIDs...)
	cloned.AccountGroups = append([]AccountGroup(nil), account.AccountGroups...)
	return cloned
}

func (r *tiboAutoGroupSchedulingRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads[id]++
	account, err := r.schedulerGroupAwareOpenAIAccountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return cloneTiboAutoGroupSchedulingAccount(account), nil
}

func (r *tiboAutoGroupSchedulingRepo) readCount(id int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads[id]
}

func (r *tiboAutoGroupSchedulingRepo) replaceAccount(account Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.accounts {
		if r.accounts[i].ID == account.ID {
			r.accounts[i] = *cloneTiboAutoGroupSchedulingAccount(&account)
			return
		}
	}
}

// Keep old group snapshots as successful cache hits. An absent snapshot or a
// group-insensitive candidate fake could let a DB fallback hide the regression.
type tiboAutoGroupSchedulingSnapshot struct {
	*openAISnapshotCacheStub
	mu           sync.Mutex
	byGroup      map[int64][]*Account
	accountReads map[int64]int
	bucketReads  map[int64]int
}

func (c *tiboAutoGroupSchedulingSnapshot) GetSnapshot(_ context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bucketReads[bucket.GroupID]++
	accounts := c.byGroup[bucket.GroupID]
	out := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, cloneTiboAutoGroupSchedulingAccount(account))
	}
	return out, true, nil
}

func (c *tiboAutoGroupSchedulingSnapshot) GetAccount(ctx context.Context, id int64) (*Account, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accountReads[id]++
	account, err := c.openAISnapshotCacheStub.GetAccount(ctx, id)
	if err != nil || account == nil {
		return account, err
	}
	return cloneTiboAutoGroupSchedulingAccount(account), nil
}

func (c *tiboAutoGroupSchedulingSnapshot) readCounts(accountID, groupID int64) (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accountReads[accountID], c.bucketReads[groupID]
}

func (c *tiboAutoGroupSchedulingSnapshot) publish(account, backup *Account, oldGroup, newGroup, externalGroup int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accountsByID[account.ID] = cloneTiboAutoGroupSchedulingAccount(account)
	c.byGroup[oldGroup] = []*Account{cloneTiboAutoGroupSchedulingAccount(backup)}
	c.byGroup[newGroup] = []*Account{cloneTiboAutoGroupSchedulingAccount(account)}
	c.byGroup[externalGroup] = []*Account{cloneTiboAutoGroupSchedulingAccount(account)}
}

func TestTiboAutoGroupMovedAccountSessionStickyUsesFreshMembership(t *testing.T) {
	runTiboAutoGroupMovedAccountMatrix(t, false)
}

func TestTiboAutoGroupMovedAccountPreviousResponseUsesFreshMembership(t *testing.T) {
	runTiboAutoGroupMovedAccountMatrix(t, true)
}

func runTiboAutoGroupMovedAccountMatrix(t *testing.T, previousResponse bool) {
	t.Helper()
	for _, mode := range []struct {
		name, enabled string
	}{
		{"legacy", "false"},
		{"advanced", "true"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
			ctx := context.Background()
			oldGroup, newGroup, externalGroup := int64(57101), int64(57102), int64(57103)
			moving := tiboSchedulerAccount(57201, 0, oldGroup)
			moving.GroupIDs = []int64{oldGroup, externalGroup}
			moving.AccountGroups = []AccountGroup{
				{AccountID: moving.ID, GroupID: oldGroup, Priority: 7},
				{AccountID: moving.ID, GroupID: externalGroup, Priority: 83, CreatedAt: time.Unix(1700000000, 0)},
			}
			moving.Extra["openai_oauth_responses_websockets_v2_enabled"] = true
			backup := tiboSchedulerAccount(57202, 20, oldGroup)
			repo := &tiboAutoGroupSchedulingRepo{
				schedulerGroupAwareOpenAIAccountRepo: schedulerGroupAwareOpenAIAccountRepo{
					schedulerTestOpenAIAccountRepo{accounts: []Account{
						*cloneTiboAutoGroupSchedulingAccount(&moving), *cloneTiboAutoGroupSchedulingAccount(&backup),
					}},
				},
				reads: make(map[int64]int),
			}
			cachedMoving, cachedBackup := cloneTiboAutoGroupSchedulingAccount(&moving), cloneTiboAutoGroupSchedulingAccount(&backup)
			snapshots := &tiboAutoGroupSchedulingSnapshot{
				openAISnapshotCacheStub: &openAISnapshotCacheStub{
					accountsByID: map[int64]*Account{moving.ID: cachedMoving, backup.ID: cachedBackup},
				},
				byGroup: map[int64][]*Account{
					oldGroup: {cachedMoving, cachedBackup}, newGroup: {}, externalGroup: {cachedMoving},
				},
				accountReads: make(map[int64]int),
				bucketReads:  make(map[int64]int),
			}
			cache := &schedulerTestGatewayCache{}
			acquired, released := []int64{}, []int64{}
			cfg := newSchedulerTestOpenAIWSV2Config()
			cfg.RunMode = config.RunModeStandard
			cfg.Gateway.Scheduling.LoadBatchEnabled = true
			cfg.Gateway.OpenAIWS.LBTopK = 2
			cfg.Gateway.OpenAITiboRoute.SchedulerPreferHealthyRoute = false
			svc := &OpenAIGatewayService{
				accountRepo: repo,
				cache:       cache,
				cfg:         cfg,
				schedulerSnapshot: &SchedulerSnapshotService{
					cache: snapshots, accountRepo: repo, cfg: cfg,
				},
				rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(mode.enabled),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
					acquiredIDs: &acquired, releasedIDs: &released,
				}),
			}
			require.Equal(t, mode.enabled == "true", svc.isOpenAIAdvancedSchedulerEnabled(ctx))
			require.Equal(t, OpenAIUpstreamTransportResponsesWebsocketV2, svc.getOpenAIWSProtocolResolver().Resolve(&moving).Transport)

			sessionHash, responseID := "tibo-auto-group-stale-session", ""
			if previousResponse {
				sessionHash, responseID = "", "resp_tibo_auto_group_stale"
				require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, oldGroup, responseID, moving.ID, time.Hour))
			} else {
				require.NoError(t, cache.SetSessionAccountID(ctx, oldGroup, "openai:"+sessionHash, moving.ID, time.Hour))
			}
			selectAccount := func(groupID int64, previous, session string, wantID int64) OpenAIAccountScheduleDecision {
				t.Helper()
				selection, decision, err := svc.SelectAccountWithScheduler(
					ctx, &groupID, previous, session, "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false,
				)
				if selection != nil && selection.ReleaseFunc != nil {
					defer selection.ReleaseFunc()
				}
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.NotNil(t, selection.Account)
				require.Equal(t, wantID, selection.Account.ID)
				require.True(t, selection.Acquired)
				require.True(t, openAIStickyAccountMatchesGroup(selection.Account, &groupID))
				return decision
			}
			beforeMove := selectAccount(oldGroup, responseID, sessionHash, moving.ID)
			if previousResponse {
				require.True(t, beforeMove.StickyPreviousHit, "the continuation must genuinely hit before the move")
			} else {
				require.True(t, beforeMove.StickySessionHit, "the session must genuinely hit before the move")
			}

			// Simulate the committed membership change while event propagation is
			// delayed. Neither cached account nor old-group bucket is refreshed.
			moved := *cloneTiboAutoGroupSchedulingAccount(&moving)
			moved.GroupIDs = []int64{newGroup, externalGroup}
			moved.AccountGroups[0].GroupID = newGroup
			repo.replaceAccount(moved)
			require.Contains(t, cachedMoving.GroupIDs, oldGroup)
			require.NotContains(t, cachedMoving.GroupIDs, newGroup)
			require.Equal(t, oldGroup, cachedMoving.AccountGroups[0].GroupID)
			dbReadsBefore := repo.readCount(moving.ID)
			cacheReadsBefore, bucketReadsBefore := snapshots.readCounts(moving.ID, oldGroup)
			if previousResponse {
				slotsBefore := len(acquired)
				stale, err := svc.SelectAccountByPreviousResponseID(ctx, &oldGroup, responseID, "gpt-6-astra", nil, false)
				if stale != nil && stale.ReleaseFunc != nil {
					stale.ReleaseFunc()
				}
				require.NoError(t, err)
				require.Nil(t, stale, "the public continuation selector must reject the moved account before taking a slot")
				require.Len(t, acquired, slotsBefore)
			}
			afterMove := selectAccount(oldGroup, responseID, sessionHash, backup.ID)
			require.False(t, afterMove.StickyPreviousHit)
			require.False(t, afterMove.StickySessionHit)
			require.Greater(t, repo.readCount(moving.ID), dbReadsBefore, "reject based on a fresh authoritative account read")
			cacheReadsAfter, bucketReadsAfter := snapshots.readCounts(moving.ID, oldGroup)
			require.Greater(t, cacheReadsAfter, cacheReadsBefore, "the stale account snapshot must actually be consulted")
			require.Greater(t, bucketReadsAfter, bucketReadsBefore, "fallback must use the old cached group bucket")
			if !previousResponse {
				require.Positive(t, cache.deletedSessions["openai:"+sessionHash])
				require.NotEqual(t, moving.ID, cache.sessionBindings["openai:"+sessionHash])
			}
			require.ElementsMatch(t, acquired, released, "all rejected and successful selection slots must be released")

			// A preserved external membership still works even during cache lag.
			selectAccount(externalGroup, "", "", moving.ID)
			// The repository/outbox integration test verifies real publication;
			// here exercise the selector again after that publication is visible.
			snapshots.publish(&moved, &backup, oldGroup, newGroup, externalGroup)
			selectAccount(newGroup, "", "", moving.ID)
			selectAccount(externalGroup, "", "", moving.ID)
			again := selectAccount(oldGroup, responseID, "", backup.ID)
			require.False(t, again.StickyPreviousHit)
			require.ElementsMatch(t, acquired, released)
		})
	}
}
