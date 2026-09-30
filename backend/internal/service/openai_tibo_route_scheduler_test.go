package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func tiboSchedulerAccount(id int64, priority int, groupID int64) Account {
	return Account{ID: id, Name: "tibo", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Concurrency: 4, Priority: priority, GroupIDs: []int64{groupID},
		Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "chatgpt"}}
}

func newTiboSchedulerService(accounts []Account, cache *schedulerTestGatewayCache, prefer bool) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Gateway.OpenAICodexTicket.Enabled = true
	cfg.Gateway.OpenAICodexTicket.Mode = openAICookieWSMode
	cfg.Gateway.OpenAITiboRoute.SchedulerPreferHealthyRoute = prefer
	cfg.Gateway.OpenAIWS.LBTopK = 3
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
}

func seedTiboSchedulerVerdict(svc *OpenAIGatewayService, accountID int64, verdict openAITiboVerdict) {
	now := time.Now()
	state := svc.openAITiboAccountState(accountID)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.http.verdict, state.http.checkedAt, state.http.lastSampleAt = verdict, now, now
}

func TestTiboSchedulerTier(t *testing.T) {
	groupID := int64(7001)
	account := tiboSchedulerAccount(7101, 1, groupID)
	svc := newTiboSchedulerService([]Account{account}, &schedulerTestGatewayCache{}, true)
	now := time.Now()
	require.Equal(t, 1, svc.openAITiboSchedulerTier(&account, "gpt-5.1", now), "no verdict yet")
	seedTiboSchedulerVerdict(svc, account.ID, openAITiboHealthy)
	require.Equal(t, 0, svc.openAITiboSchedulerTier(&account, "gpt-5.1", now))
	seedTiboSchedulerVerdict(svc, account.ID, openAITiboDegraded)
	require.Equal(t, 2, svc.openAITiboSchedulerTier(&account, "gpt-5.1", now), "every route degraded")

	svc.openaiCookieWSTickets.Store(openAICodexTicketKey(account.ID, openAICodexTicketDefaultModel), cookieWSTestTicket(account.ID, now.Add(-time.Minute)))
	require.Equal(t, 0, svc.openAITiboSchedulerTier(&account, openAICodexTicketDefaultModel, now), "ready Cookie WS for astra")
	require.Equal(t, 2, svc.openAITiboSchedulerTier(&account, "gpt-5.1", now), "Cookie WS serves astra only")

	withBPS := account
	withBPS.Extra = map[string]any{"openai_excel_bps": true}
	require.Equal(t, 0, svc.openAITiboSchedulerTier(&withBPS, "gpt-5.1", now), "off/shadow BPS counts as healthy")
	svc.cfg.Gateway.OpenAITiboRoute.BPSProbeMode = config.OpenAITiboBPSProbeEnforce
	require.Equal(t, 1, svc.openAITiboSchedulerTier(&withBPS, "gpt-5.1", now), "enforce uses the BPS verdict (none yet)")

	apiKey := Account{ID: 7199, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.Equal(t, 1, svc.openAITiboSchedulerTier(&apiKey, "gpt-5.1", now), "non Cookie WS accounts are unknown")
}

func TestTiboSchedulerPrefersHealthyRouteAccount(t *testing.T) {
	groupID := int64(7002)
	healthy, unknown, degraded := tiboSchedulerAccount(7201, 9, groupID), tiboSchedulerAccount(7202, 1, groupID), tiboSchedulerAccount(7203, 0, groupID)
	svc := newTiboSchedulerService([]Account{degraded, unknown, healthy}, &schedulerTestGatewayCache{}, true)
	seedTiboSchedulerVerdict(svc, healthy.ID, openAITiboHealthy)
	seedTiboSchedulerVerdict(svc, degraded.ID, openAITiboDegraded)
	pick := func(svc *OpenAIGatewayService) int64 {
		selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return selection.Account.ID
	}
	for i := 0; i < 20; i++ {
		require.Equal(t, healthy.ID, pick(svc), "T0 first despite the lowest priority")
	}
	// Control: without tiering the low-priority healthy account does not
	// always win (weighted top-K), so the assertion above is meaningful.
	untiered := newTiboSchedulerService([]Account{degraded, unknown, healthy}, &schedulerTestGatewayCache{}, false)
	seedTiboSchedulerVerdict(untiered, healthy.ID, openAITiboHealthy)
	seedTiboSchedulerVerdict(untiered, degraded.ID, openAITiboDegraded)
	others := 0
	for i := 0; i < 20; i++ {
		if pick(untiered) != healthy.ID {
			others++
		}
	}
	require.Positive(t, others)

	// Soft ordering, not filtering: with only degraded accounts left, they still serve.
	only := newTiboSchedulerService([]Account{degraded}, &schedulerTestGatewayCache{}, true)
	seedTiboSchedulerVerdict(only, degraded.ID, openAITiboDegraded)
	selection, _, err := only.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.Equal(t, degraded.ID, selection.Account.ID)
}

func TestTiboSchedulerPartitionOffOrSingleTier(t *testing.T) {
	groupID := int64(7003)
	a, b := tiboSchedulerAccount(7301, 1, groupID), tiboSchedulerAccount(7302, 1, groupID)
	pool := []openAIAccountCandidateScore{{account: &a}, {account: &b}}
	off := newTiboSchedulerService([]Account{a, b}, &schedulerTestGatewayCache{}, false)
	seedTiboSchedulerVerdict(off, a.ID, openAITiboHealthy)
	offScheduler := &defaultOpenAIAccountScheduler{service: off}
	require.Nil(t, offScheduler.partitionOpenAITiboRouteTiers(OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.1"}, pool), "switch off")

	on := newTiboSchedulerService([]Account{a, b}, &schedulerTestGatewayCache{}, true)
	onScheduler := &defaultOpenAIAccountScheduler{service: on}
	require.Nil(t, onScheduler.partitionOpenAITiboRouteTiers(OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.1"}, pool), "one tier: existing order")
	seedTiboSchedulerVerdict(on, a.ID, openAITiboDegraded)
	tiers := onScheduler.partitionOpenAITiboRouteTiers(OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.1"}, pool)
	require.Len(t, tiers[1], 1)
	require.Len(t, tiers[2], 1)

	// The previous-response account keeps the top tier.
	tiers = onScheduler.partitionOpenAITiboRouteTiers(OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.1", PreviousResponseID: "resp_1", StickyPreviousAccountID: a.ID}, pool)
	require.Equal(t, a.ID, tiers[0][0].account.ID)
}

func TestTiboSchedulerReleasesDegradedSticky(t *testing.T) {
	groupID := int64(7004)
	healthy, degraded := tiboSchedulerAccount(7401, 1, groupID), tiboSchedulerAccount(7402, 1, groupID)
	for _, prefer := range []bool{true, false} {
		cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:tibo_session": degraded.ID}}
		svc := newTiboSchedulerService([]Account{healthy, degraded}, cache, prefer)
		seedTiboSchedulerVerdict(svc, healthy.ID, openAITiboHealthy)
		seedTiboSchedulerVerdict(svc, degraded.ID, openAITiboDegraded)
		selection, decision, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "tibo_session", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		if !prefer {
			require.Equal(t, degraded.ID, selection.Account.ID, "switch off keeps sticky")
			require.True(t, decision.StickySessionHit)
			continue
		}
		require.Equal(t, healthy.ID, selection.Account.ID)
		require.False(t, decision.StickySessionHit)
		require.Equal(t, healthy.ID, cache.sessionBindings["openai:tibo_session"], "the session is rebound")
	}
}

func TestTiboSchedulerKeepsStickyForContinuationOrWithoutHealthyAlternative(t *testing.T) {
	groupID := int64(7005)
	unknown, degraded := tiboSchedulerAccount(7501, 1, groupID), tiboSchedulerAccount(7502, 1, groupID)
	svc := newTiboSchedulerService([]Account{unknown, degraded}, &schedulerTestGatewayCache{}, true)
	seedTiboSchedulerVerdict(svc, degraded.ID, openAITiboDegraded)
	scheduler := &defaultOpenAIAccountScheduler{service: svc}
	req := OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-5.1"}
	require.False(t, scheduler.shouldReleaseOpenAITiboDegradedSticky(context.Background(), req, &degraded), "only T1 alternatives")

	seedTiboSchedulerVerdict(svc, unknown.ID, openAITiboHealthy)
	require.True(t, scheduler.shouldReleaseOpenAITiboDegradedSticky(context.Background(), req, &degraded))
	req.PreviousResponseID = "resp_continuation"
	require.False(t, scheduler.shouldReleaseOpenAITiboDegradedSticky(context.Background(), req, &degraded), "continuations stay put")
	req.PreviousResponseID = ""
	req.ExcludedIDs = map[int64]struct{}{unknown.ID: {}}
	require.False(t, scheduler.shouldReleaseOpenAITiboDegradedSticky(context.Background(), req, &degraded), "excluded accounts do not count")
}
