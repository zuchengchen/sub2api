package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func capacitySchedulerAccount(id, groupID int64, concurrency, priority int) Account {
	return Account{
		ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: concurrency,
		Priority: priority, GroupIDs: []int64{groupID},
	}
}

func capacitySchedulerConfig(topK int) *config.Config {
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.OpenAIWS.LBTopK = topK
	// Make the preferred account deterministic even when it is already busy.
	// Capacity fallback must not depend on changing administrator score weights.
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights = config.GatewayOpenAIWSSchedulerScoreWeights{Priority: 10}
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 120 * time.Second
	cfg.Gateway.Scheduling.FallbackMaxWaiting = 100
	cfg.Gateway.Scheduling.FallbackWaitTimeout = 30 * time.Second
	return cfg
}

func TestOpenAIAccountSchedulerCapacity_UsesAvailableAccountOutsideTopK(t *testing.T) {
	for _, knownFull := range []bool{true, false} {
		name := "slot_taken_after_load_snapshot"
		if knownFull {
			name = "preferred_account_known_full"
		}
		t.Run(name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
			groupID := int64(9201)
			preferred := capacitySchedulerAccount(92011, groupID, 1, 0)
			available := capacitySchedulerAccount(92012, groupID, 1, 100)
			preferredLoad := &AccountLoadInfo{AccountID: preferred.ID}
			if knownFull {
				preferredLoad.CurrentConcurrency = 1
				preferredLoad.LoadRate = 100
			}
			acquiredIDs := []int64{}
			svc := &OpenAIGatewayService{
				accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{preferred, available}},
				cfg:              capacitySchedulerConfig(1),
				rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
					loadMap: map[int64]*AccountLoadInfo{
						preferred.ID: preferredLoad,
						available.ID: {AccountID: available.ID},
					},
					acquireResults: map[int64]bool{preferred.ID: false, available.ID: true},
					acquiredIDs:    &acquiredIDs,
				}),
			}

			selection, decision, err := svc.SelectAccountWithScheduler(
				context.Background(), &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
			)
			require.NoError(t, err)
			require.NotNil(t, selection)
			if selection.ReleaseFunc != nil {
				t.Cleanup(selection.ReleaseFunc)
			}
			require.True(t, selection.Acquired, "an available account must be admitted before returning a wait plan")
			require.Equal(t, available.ID, selection.Account.ID)
			require.Nil(t, selection.WaitPlan)
			require.Equal(t, 1, decision.TopK, "top-k remains the preferred shortlist, not the entire capacity pool")
			require.Contains(t, acquiredIDs, available.ID)
			if knownFull {
				require.NotContains(t, acquiredIDs, preferred.ID)
			}
		})
	}
}

func TestOpenAIAccountSchedulerCapacity_WeightedStickyFullQueueUsesAnotherAccount(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	groupID := int64(9202)
	sticky := capacitySchedulerAccount(92021, groupID, 1, 0)
	other := capacitySchedulerAccount(92022, groupID, 1, 100)
	sessions := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:capacity-sticky": sticky.ID}}
	svc := &OpenAIGatewayService{
		accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{sticky, other}},
		cache:            sessions,
		cfg:              capacitySchedulerConfig(1),
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true", "true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				sticky.ID: {AccountID: sticky.ID, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: 3},
				other.ID:  {AccountID: other.ID, CurrentConcurrency: 1, LoadRate: 100},
			},
			acquireResults: map[int64]bool{sticky.ID: false, other.ID: false},
			waitCounts:     map[int64]int{sticky.ID: 3, other.ID: 0},
		}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(
		context.Background(), &groupID, "", "capacity-sticky", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, other.ID, selection.Account.ID, "a full sticky queue must not reject requests while another account can accept a waiter")
	require.Equal(t, other.ID, selection.WaitPlan.AccountID)
	require.Equal(t, sticky.ID, sessions.sessionBindings["openai:capacity-sticky"], "selection must not publish a new binding before admission")
}

func TestOpenAIAccountSchedulerCapacity_WaitsAtLowerQueuePressure(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	groupID := int64(9203)
	small := capacitySchedulerAccount(92031, groupID, 1, 0)
	large := capacitySchedulerAccount(92032, groupID, 10, 100)
	svc := &OpenAIGatewayService{
		accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{small, large}},
		cfg:              capacitySchedulerConfig(1),
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				small.ID: {AccountID: small.ID, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: 2},
				large.ID: {AccountID: large.ID, CurrentConcurrency: 10, LoadRate: 100, WaitingCount: 3},
			},
			acquireResults: map[int64]bool{small.ID: false, large.ID: false},
			waitCounts:     map[int64]int{small.ID: 2, large.ID: 3},
		}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(
		context.Background(), &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, large.ID, selection.Account.ID, "three waiters sharing ten slots should be preferred to two waiters sharing one slot")
	require.Equal(t, large.Concurrency, selection.WaitPlan.MaxConcurrency)
}

func TestOpenAIAccountSchedulerCapacity_HardContinuationKeepsBusyOwner(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	ctx := context.Background()
	groupID := int64(9204)
	owner := capacitySchedulerAccount(92041, groupID, 1, 100)
	available := capacitySchedulerAccount(92042, groupID, 1, 0)
	owner.Extra = map[string]any{"openai_apikey_responses_websockets_v2_enabled": true}
	available.Extra = map[string]any{"openai_apikey_responses_websockets_v2_enabled": true}
	acquiredIDs := []int64{}
	svc := &OpenAIGatewayService{
		accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{owner, available}},
		cfg:              capacitySchedulerConfig(1),
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true", "true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				owner.ID:     {AccountID: owner.ID, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: 3},
				available.ID: {AccountID: available.ID},
			},
			acquireResults: map[int64]bool{owner.ID: false, available.ID: true},
			waitCounts:     map[int64]int{owner.ID: 3},
			acquiredIDs:    &acquiredIDs,
		}),
	}
	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_capacity_owner", owner.ID, time.Hour))

	selection, decision, err := svc.SelectAccountWithSchedulerForCapability(
		ctx, &groupID, "resp_capacity_owner", "", "gpt-5.1", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, true, PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, owner.ID, selection.Account.ID)
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, openAIAccountScheduleLayerPreviousResponse, decision.Layer)
	require.True(t, decision.StickyPreviousHit)
	require.NotContains(t, acquiredIDs, available.ID, "an unavailable owner does not authorize moving a hard continuation")
}

func TestOpenAIAccountSchedulerCapacity_AllQueuesFullKeepsStickyWaitLimit(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	groupID := int64(9205)
	sticky := capacitySchedulerAccount(92051, groupID, 1, 0)
	other := capacitySchedulerAccount(92052, groupID, 1, 100)
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{sticky, other}},
		cache: &schedulerTestGatewayCache{sessionBindings: map[string]int64{
			"openai:capacity-all-full": sticky.ID,
		}},
		cfg:              capacitySchedulerConfig(1),
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true", "true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				sticky.ID: {AccountID: sticky.ID, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: 3},
				other.ID:  {AccountID: other.ID, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: 100},
			},
			acquireResults: map[int64]bool{sticky.ID: false, other.ID: false},
			waitCounts:     map[int64]int{sticky.ID: 3, other.ID: 100},
		}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(
		context.Background(), &groupID, "", "capacity-all-full", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, sticky.ID, selection.Account.ID)
	require.Equal(t, 3, selection.WaitPlan.MaxWaiting, "full sticky admission must retain its 429 limit, not silently expand to the fallback queue")
}

func TestOpenAIAccountSchedulerCapacity_SkipsFullFallbackQueue(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	groupID := int64(9206)
	full := capacitySchedulerAccount(92061, groupID, 1000, 0)
	waitable := capacitySchedulerAccount(92062, groupID, 1, 100)
	svc := &OpenAIGatewayService{
		accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{full, waitable}},
		cfg:              capacitySchedulerConfig(1),
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				full.ID:     {AccountID: full.ID, CurrentConcurrency: 1000, LoadRate: 100, WaitingCount: 100},
				waitable.ID: {AccountID: waitable.ID, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: 1},
			},
			acquireResults: map[int64]bool{full.ID: false, waitable.ID: false},
			waitCounts:     map[int64]int{full.ID: 100, waitable.ID: 1},
		}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(
		context.Background(), &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, waitable.ID, selection.Account.ID, "a queue at its hard limit is ineligible even when its normalized pressure is smaller")
}

func TestOpenAIAccountSchedulerCapacity_CanceledRequestDoesNotAcquire(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	groupID := int64(9207)
	account := capacitySchedulerAccount(92071, groupID, 1, 0)
	acquiredIDs := []int64{}
	svc := &OpenAIGatewayService{
		accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{account}},
		cfg:              capacitySchedulerConfig(1),
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquiredIDs: &acquiredIDs,
		}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	selection, _, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	if selection != nil && selection.ReleaseFunc != nil {
		t.Cleanup(selection.ReleaseFunc)
	}
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, selection)
	require.Empty(t, acquiredIDs)
}

func TestOpenAIAccountSchedulerCapacity_NonCostOverflowHasBoundedProbes(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	groupID := int64(9208)
	accounts := make([]Account, 0, 2*openAIAccountSelectionProbeLimit)
	acquireResults := make(map[int64]bool)
	for i := 0; i < cap(accounts); i++ {
		account := capacitySchedulerAccount(int64(92100+i), groupID, 1, i)
		accounts = append(accounts, account)
		acquireResults[account.ID] = false
	}
	acquiredIDs := []int64{}
	svc := &OpenAIGatewayService{
		accountRepo:      schedulerTestOpenAIAccountRepo{accounts: accounts},
		cfg:              capacitySchedulerConfig(1),
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquireResults: acquireResults,
			acquiredIDs:    &acquiredIDs,
		}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(
		context.Background(), &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	probed := make(map[int64]struct{})
	for _, id := range acquiredIDs {
		probed[id] = struct{}{}
	}
	require.Greater(t, len(probed), 1, "the scheduler must actually try capacity outside the one-account shortlist")
	require.LessOrEqual(t, len(acquiredIDs), openAIAccountSelectionProbeLimit, "initial and refreshed attempts must share one bounded request budget")
}

func TestOpenAIAccountSchedulerCapacity_RecheckBudgetKeepsValidatedFullQueue(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	groupID := int64(9209)
	sticky := capacitySchedulerAccount(92300, groupID, 1, 0)
	snapshots := []*Account{&sticky}
	snapshotByID := map[int64]*Account{sticky.ID: &sticky}
	latestByID := map[int64]*Account{sticky.ID: &sticky}
	loadMap := map[int64]*AccountLoadInfo{
		sticky.ID: {AccountID: sticky.ID, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: 3},
	}
	acquireResults := map[int64]bool{sticky.ID: false}
	// These accounts were eligible in the snapshot, but became disabled before
	// admission. Rechecking them must not erase a validated queue-full outcome
	// when the request reaches its DB-work budget.
	for i := 1; i <= openAIAccountSelectionProbeLimit; i++ {
		cached := capacitySchedulerAccount(sticky.ID+int64(i), groupID, 1, 100+i)
		latest := cached
		latest.Status = StatusDisabled
		snapshots = append(snapshots, &cached)
		snapshotByID[cached.ID] = &cached
		latestByID[cached.ID] = &latest
		loadMap[cached.ID] = &AccountLoadInfo{AccountID: cached.ID, CurrentConcurrency: 1, LoadRate: 100}
		acquireResults[cached.ID] = false
	}
	repo := &upstreamCostCountingAccountRepo{accounts: latestByID}
	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache: &schedulerTestGatewayCache{sessionBindings: map[string]int64{
			"openai:capacity-recheck-budget": sticky.ID,
		}},
		cfg:              capacitySchedulerConfig(1),
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true", "true"),
		schedulerSnapshot: &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{
			snapshotAccounts: snapshots,
			accountsByID:     snapshotByID,
		}},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap:        loadMap,
			acquireResults: acquireResults,
			waitCounts:     map[int64]int{sticky.ID: 3},
		}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(
		context.Background(), &groupID, "", "capacity-recheck-budget", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err, "bounded discovery must retain the original admission outcome instead of returning model unavailability")
	require.NotNil(t, selection)
	require.False(t, selection.Acquired)
	require.Equal(t, sticky.ID, selection.Account.ID)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, 3, selection.WaitPlan.MaxWaiting)
	require.Equal(t, openAIAccountSelectionProbeLimit, repo.calls(), "the fixture must reach the shared DB-recheck limit")
}

func TestOpenAIAccountSchedulerCapacity_BothStickyQueuesKeepTheirLimits(t *testing.T) {
	for _, allFull := range []bool{false, true} {
		name := "ordinary_account_can_wait"
		if allFull {
			name = "all_queues_full"
		}
		t.Run(name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
			ctx := context.Background()
			groupID := int64(9210)
			previous := capacitySchedulerAccount(92401, groupID, 1, 0)
			session := capacitySchedulerAccount(92402, groupID, 1, 1)
			ordinary := capacitySchedulerAccount(92403, groupID, 1, 100)
			previous.Extra = map[string]any{"openai_apikey_responses_websockets_v2_enabled": true}
			otherWaiting := 0
			if allFull {
				otherWaiting = 100
			}
			svc := &OpenAIGatewayService{
				accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{previous, session, ordinary}},
				cache: &schedulerTestGatewayCache{sessionBindings: map[string]int64{
					"openai:capacity-two-sticky": session.ID,
				}},
				cfg:              capacitySchedulerConfig(1),
				rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true", "true"),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
					loadMap: map[int64]*AccountLoadInfo{
						previous.ID: {AccountID: previous.ID, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: 3},
						session.ID:  {AccountID: session.ID, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: 3},
						ordinary.ID: {AccountID: ordinary.ID, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: otherWaiting},
					},
					acquireResults: map[int64]bool{previous.ID: false, session.ID: false, ordinary.ID: false},
					waitCounts:     map[int64]int{previous.ID: 3, session.ID: 3, ordinary.ID: otherWaiting},
				}),
			}
			store := svc.getOpenAIWSStateStore()
			require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_capacity_two_sticky", previous.ID, time.Hour))

			selection, _, err := svc.SelectAccountWithSchedulerForCapability(
				ctx, &groupID, "resp_capacity_two_sticky", "capacity-two-sticky", "gpt-5.1", nil,
				OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
				false, true, true, PlatformOpenAI,
			)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.False(t, selection.Acquired)
			require.NotNil(t, selection.WaitPlan)
			if allFull {
				require.Equal(t, previous.ID, selection.Account.ID)
				require.Equal(t, 3, selection.WaitPlan.MaxWaiting)
			} else {
				require.Equal(t, ordinary.ID, selection.Account.ID, "neither full sticky queue may be widened to the ordinary queue limit")
				require.Equal(t, 100, selection.WaitPlan.MaxWaiting)
			}
		})
	}
}
