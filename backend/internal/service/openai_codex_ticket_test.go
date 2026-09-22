package service

import (
	"context"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

const openAICodexTicketTestCookies = "__cf_bm=bm; __cflb=lb; __oailb=ol"

func fakeCodexTicketState(n int) string {
	if n < len(openAICodexTicketStatePrefix) {
		return strings.Repeat("A", n)
	}
	return openAICodexTicketStatePrefix + strings.Repeat("B", n-len(openAICodexTicketStatePrefix))
}

func stampCodexTicketSetCookies(h http.Header) {
	h.Add("Set-Cookie", "__cf_bm=bm; Path=/; HttpOnly")
	h.Add("Set-Cookie", "__cflb=lb; Path=/")
	h.Add("Set-Cookie", "__oailb=ol; Path=/")
}

func ticketTestAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"access_token": "tok", "chatgpt_account_id": "acc-1"},
	}
}

func releaseCodexTicketHarvestRetry(svc *OpenAIGatewayService, accountID int64, model string) {
	svc.openaiCodexTicketHarvestBackoff.Store(openAICodexTicketKey(accountID, model), &openAICodexTicketHarvestBackoff{
		nextProbeAt: time.Now().Add(-time.Second),
	})
}

func ticketTestService(t *testing.T, cfg config.OpenAICodexTicketConfig, upstream HTTPUpstream) *OpenAIGatewayService {
	t.Helper()
	return &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{OpenAICodexTicket: cfg},
		},
		httpUpstream: upstream,
	}
}

func TestApplyOpenAICodexTicket_ReplacesHeader(t *testing.T) {
	state := fakeCodexTicketState(292)
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:      true,
		TargetLength: 292,
		TTLSeconds:   3600,
		FailClosed:   true,
	}, nil)
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      state,
		Length:     292,
		Cookies:    openAICodexTicketTestCookies,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(312))
	err := svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h)
	require.NoError(t, err)
	require.Equal(t, state, h.Get(openAICodexTurnStateHeader))
	require.Equal(t, 292, len(h.Get(openAICodexTurnStateHeader)))
}

func TestApplyOpenAICodexTicket_DoesNotReuseOtherModelOrAccount(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		TargetLength:    292,
		TTLSeconds:      3600,
		FailClosed:      true,
		HarvestProxyURL: "socks5h://harvest",
	}, &httpUpstreamRecorder{err: io.EOF})
	a := ticketTestAccount(41)
	b := ticketTestAccount(42)
	astra := fakeCodexTicketState(292)
	svc.storeOpenAICodexTicket(context.Background(), a, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      astra,
		Length:     292,
		Cookies:    openAICodexTicketTestCookies,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "keep-ungated")
	err := svc.applyOpenAICodexTicket(context.Background(), a, "gpt-5.5", h)
	require.NoError(t, err)
	require.Equal(t, "keep-ungated", h.Get(openAICodexTurnStateHeader))
	require.False(t, svc.openAICodexTicketBlocksAccount(a, "gpt-5.5"))
	require.True(t, svc.openAICodexTicketBlocksAccount(b, "gpt-6-astra"))
	require.False(t, svc.openAICodexTicketBlocksAccount(a, "gpt-6-astra"))

	h = http.Header{}
	err = svc.applyOpenAICodexTicket(context.Background(), b, "gpt-6-astra", h)
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

func TestLookupOpenAICodexTicket_PrefersNewerExtra(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600}, nil)
	account := ticketTestAccount(41)
	oldState := fakeCodexTicketState(292)
	newState := openAICodexTicketStatePrefix + strings.Repeat("C", 286)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      oldState,
		Length:     292,
		CapturedAt: time.Now().Add(-30 * time.Minute),
		ExpiresAt:  time.Now().Add(-time.Minute),
	})
	account.Extra = map[string]any{openAICodexTicketExtraKey("gpt-6-astra"): &openAICodexTicket{
		Model:      "gpt-6-astra",
		State:      newState,
		Length:     292,
		Cookies:    openAICodexTicketTestCookies,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	},
	}
	got := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, got)
	require.Equal(t, newState, got.State)
	require.True(t, got.valid(time.Now(), 292))
}

func TestApplyOpenAICodexTicket_ExpiredNotInjected(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		TargetLength:    292,
		TTLSeconds:      3600,
		FailClosed:      true,
		HarvestProxyURL: "socks5h://harvest",
	}, &httpUpstreamRecorder{err: io.EOF})
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      fakeCodexTicketState(292),
		Length:     292,
		CapturedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt:  time.Now().Add(-time.Minute),
	})
	h := http.Header{}
	err := svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h)
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

func TestApplyOpenAICodexTicket_WrongLengthNotInjected(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:      true,
		TargetLength: 292,
		TTLSeconds:   3600,
		FailClosed:   true,
	}, nil)
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      fakeCodexTicketState(312),
		Length:     312,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	h := http.Header{}
	err := svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h)
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

func TestApplyOpenAICodexTicket_FailOpenSkipsInject(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:    true,
		FailClosed: false,
	}, nil)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	err := svc.applyOpenAICodexTicket(context.Background(), ticketTestAccount(41), "gpt-6-astra", h)
	require.NoError(t, err)
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
	require.False(t, svc.openAICodexTicketBlocksAccount(ticketTestAccount(41), "gpt-6-astra"))
}

func TestApplyOpenAICodexTicket_DisabledNoop(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: false, FailClosed: true}, nil)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	err := svc.applyOpenAICodexTicket(context.Background(), ticketTestAccount(41), "gpt-6-astra", h)
	require.NoError(t, err)
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
}

func TestHarvestOpenAICodexTicket_Stores292AndUsesHarvestProxy(t *testing.T) {
	state312 := fakeCodexTicketState(312)
	state292 := fakeCodexTicketState(292)
	header312 := http.Header{}
	header312.Set(openAICodexTurnStateHeader, state312)
	header292 := http.Header{}
	header292.Set(openAICodexTurnStateHeader, state292)
	stampCodexTicketSetCookies(header292)
	upstream := &httpUpstreamRecorder{
		responses: []*http.Response{
			{
				StatusCode: http.StatusOK,
				Header:     header312,
				Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
			},
			{
				StatusCode: http.StatusOK,
				Header:     header292,
				Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
			},
		},
	}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:                      true,
		TargetLength:                 292,
		TTLSeconds:                   3600,
		HarvestProxyURL:              "socks5h://user:pass@harvest.example:31",
		HarvestAttemptTimeoutSeconds: 5,
		FailClosed:                   true,
	}, upstream)
	account := ticketTestAccount(41)

	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))
	releaseCodexTicketHarvestRetry(svc, account.ID, "gpt-6-astra")
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	ticket := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, ticket)
	require.Equal(t, 292, ticket.Length)
	require.Equal(t, state292, ticket.State)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "stale")
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, state292, h.Get(openAICodexTurnStateHeader))
	require.Equal(t, openAICodexTicketTestCookies, h.Get("Cookie"))
	require.Equal(t, openAICodexTicketTestCookies, ticket.Cookies)
	require.Equal(t, "socks5h://user:pass@harvest.example:31", upstream.lastProxyURL)
	require.Len(t, upstream.requests, 2)
	require.Empty(t, upstream.requests[0].Header.Get(openAICodexTurnStateHeader))
	require.Equal(t, HTTPUpstreamProfileOpenAIHarvest, HTTPUpstreamProfileFromContext(upstream.requests[0].Context()))
	require.True(t, upstream.requests[0].Close)
}

func TestHarvestOpenAICodexTicket_IdentityFollowsCanonicalVersion(t *testing.T) {
	SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent("0.155.1") })
	t.Cleanup(func() { SetCodexCanonicalUserAgentResolver(nil) })
	header292 := http.Header{}
	header292.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	stampCodexTicketSetCookies(header292)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{{StatusCode: 200, Header: header292, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:                      true,
		TargetLength:                 292,
		HarvestProxyURL:              "socks5h://harvest.example:31",
		HarvestAttemptTimeoutSeconds: 5,
	}, upstream)
	account := ticketTestAccount(41)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Equal(t, "0.155.1", upstream.requests[0].Header.Get("version"))
	require.Contains(t, upstream.requests[0].Header.Get("user-agent"), "0.155.1")
	got := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, got)
	require.Equal(t, 292, got.Length)
}

func TestHarvestOpenAICodexTicket_Keeps292WhenProbeReturns312(t *testing.T) {
	state292 := fakeCodexTicketState(292)
	state312 := fakeCodexTicketState(312)
	header292 := http.Header{}
	header292.Set(openAICodexTurnStateHeader, state292)
	stampCodexTicketSetCookies(header292)
	header312 := http.Header{}
	header312.Set(openAICodexTurnStateHeader, state312)
	upstream := &httpUpstreamRecorder{
		responses: []*http.Response{
			{StatusCode: 200, Header: header292, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))},
			{StatusCode: 200, Header: header312, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))},
		},
	}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:                      true,
		TargetLength:                 292,
		TTLSeconds:                   3600,
		HarvestProxyURL:              "socks5h://harvest.example:31",
		HarvestAttemptTimeoutSeconds: 5,
		FailClosed:                   true,
	}, upstream)
	account := ticketTestAccount(41)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	got := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.Equal(t, 292, got.Length)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	got = svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.Equal(t, 292, got.Length)
	h := http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, state292, h.Get(openAICodexTurnStateHeader))
}

func TestHarvestOpenAICodexTicket_HTTP503DoesNotAbortHunt(t *testing.T) {
	state292 := fakeCodexTicketState(292)
	header503 := http.Header{}
	header292 := http.Header{}
	header292.Set(openAICodexTurnStateHeader, state292)
	stampCodexTicketSetCookies(header292)
	responses := make([]*http.Response, 0, 3)
	responses = append(responses, &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     header503,
		Body:       io.NopCloser(strings.NewReader(`{"error":"overloaded"}`)),
	})
	responses = append(responses, &http.Response{
		StatusCode: http.StatusOK,
		Header:     header292,
		Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
	})
	upstream := &httpUpstreamRecorder{responses: responses}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:                      true,
		TargetLength:                 292,
		TTLSeconds:                   3600,
		HarvestProxyURL:              "socks5h://harvest.example:31",
		HarvestAttemptTimeoutSeconds: 5,
		FailClosed:                   true,
	}, upstream)
	account := ticketTestAccount(41)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))
	releaseCodexTicketHarvestRetry(svc, account.ID, "gpt-6-astra")
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	ticket := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, ticket)
	require.Equal(t, state292, ticket.State)
	require.Len(t, upstream.requests, 2)
}

func TestApplyOpenAICodexTicket_MergesHarvestCookies(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 180, FailClosed: true}, nil)
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      fakeCodexTicketState(292),
		Length:     292,
		Cookies:    "__cf_bm=bm; __cflb=lb",
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	h := http.Header{}
	h.Set("Cookie", "session=keep; __cf_bm=old")
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, "session=keep; __cf_bm=bm; __cflb=lb", h.Get("Cookie"))
}

func TestHarvestOpenAICodexTicket_292WithoutCookiesIsMiss(t *testing.T) {
	header := http.Header{}
	header.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	upstream := &httpUpstreamRecorder{responses: []*http.Response{{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
	}}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, TargetLength: 292, TTLSeconds: 180, FailClosed: true,
		HarvestProxyURL: "socks5h://harvest.example:31", HarvestAttemptTimeoutSeconds: 5,
	}, upstream)
	account := ticketTestAccount(41)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))
}

func TestOpenAICodexTicketClampExpiry_ShortensStoredHour(t *testing.T) {
	now := time.Now()
	ticket := &openAICodexTicket{
		State:      fakeCodexTicketState(292),
		Length:     292,
		CapturedAt: now.Add(-200 * time.Second),
		ExpiresAt:  now.Add(time.Hour),
	}
	ticket.clampExpiry(180 * time.Second)
	require.True(t, ticket.needsRefresh(now, 0))
	require.False(t, ticket.valid(now, 292))
}

func TestLookupOpenAICodexTicket_HydratesFromExtra(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600}, nil)
	state := fakeCodexTicketState(292)
	account := ticketTestAccount(9)
	account.Extra = map[string]any{
		openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{
			"state":       state,
			"length":      292,
			"model":       "gpt-6-astra",
			"cookies":     openAICodexTicketTestCookies,
			"captured_at": time.Now().Add(-time.Minute),
			"expires_at":  time.Now().Add(time.Hour),
		},
	}
	got := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, got)
	require.Equal(t, state, got.State)
	require.True(t, got.valid(time.Now(), 292))
}

func TestOpenAICodexTicketStatuses_ReportsRemainingTTL(t *testing.T) {
	account := ticketTestAccount(41)
	account.Extra = map[string]any{
		openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{
			"state":       fakeCodexTicketState(292),
			"length":      292,
			"model":       "gpt-6-astra",
			"cookies":     openAICodexTicketTestCookies,
			"captured_at": time.Now().Add(-10 * time.Minute),
			"expires_at":  time.Now().Add(50 * time.Minute),
		},
	}
	now := time.Now()
	got := OpenAICodexTicketStatuses(account, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, now)
	require.Len(t, got, 2)
	require.Equal(t, "gpt-6-astra", got[0].Model)
	require.True(t, got[0].Ready)
	require.Greater(t, got[0].RemainingSeconds, int64(40*60))
	require.LessOrEqual(t, got[0].RemainingSeconds, int64(50*60))
	require.Equal(t, "gpt-5.6-sol", got[1].Model)
	require.False(t, got[1].Ready)
}

func TestExtractOpenAICodexTicketModel(t *testing.T) {
	require.Equal(t, "gpt-6-astra", extractOpenAICodexTicketModel([]byte(`{"model":"gpt-6-astra"}`)))
	require.Empty(t, extractOpenAICodexTicketModel([]byte(`{}`)))
}

// These stubs exercise the real continuous refresh path with both default models
// completing together. Run under -race to catch writes to the shared account maps.
type codexTicketRefreshRepo struct {
	AccountRepository
	accounts []Account
	mu       sync.Mutex
	updates  map[string]any
}

func (r *codexTicketRefreshRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	return r.accounts, nil
}
func (r *codexTicketRefreshRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updates == nil {
		r.updates = make(map[string]any)
	}
	for k, v := range updates {
		r.updates[k] = v
	}
	for i := range r.accounts {
		if r.accounts[i].ID != id {
			continue
		}
		extra := maps.Clone(r.accounts[i].Extra)
		if extra == nil {
			extra = make(map[string]any, len(updates))
		}
		for k, v := range updates {
			extra[k] = v
		}
		r.accounts[i].Extra = extra
	}
	return nil
}

type codexTicketConcurrentUpstream struct {
	HTTPUpstream
	started atomic.Int64
	ready   chan struct{}
}

func (u *codexTicketConcurrentUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if u.started.Add(1) == 2 {
		close(u.ready)
	}
	select {
	case <-u.ready:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}, nil
}
func TestRefreshOpenAICodexTickets_ConcurrentModelsPreserveAccountSnapshot(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	account.Extra = map[string]any{"existing": true}
	repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
	upstream := &codexTicketConcurrentUpstream{ready: make(chan struct{})}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "socks5h://proxy.example.com:1080"}, upstream)
	svc.accountRepo = repo
	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, int64(2), upstream.started.Load())
	require.Equal(t, map[string]any{"existing": true}, account.Extra)
	require.Len(t, repo.updates, 2)
	for _, model := range []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel} {
		ticket := svc.lookupOpenAICodexTicket(account, model)
		require.NotNil(t, ticket)
		require.True(t, ticket.valid(time.Now(), 292))
		require.Equal(t, 292, ticket.Length)
	}
	// Valid 292 tickets do not produce another probe on the next cycle.
	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, int64(2), upstream.started.Load())
}
func TestOpenAICodexTicketStatuses_RespectRuntimeConfiguration(t *testing.T) {
	account := ticketTestAccount(41)
	require.Empty(t, OpenAICodexTicketStatuses(account, config.OpenAICodexTicketConfig{}, time.Now()))
	cfg := config.OpenAICodexTicketConfig{Enabled: true, Models: []string{"custom-model"}}
	status := OpenAICodexTicketStatuses(account, cfg, time.Now())
	require.Len(t, status, 1)
	require.Equal(t, "custom-model", status[0].Model)
	require.False(t, status[0].Blocked)
	cfg.FailClosed = true
	require.True(t, OpenAICodexTicketStatuses(account, cfg, time.Now())[0].Blocked)
}
func TestProbeOpenAICodexTicket_RejectsInvalidState(t *testing.T) {
	for _, state := range []string{fakeCodexTicketState(312), strings.Repeat("X", 292), ""} {
		h := http.Header{}
		h.Set(openAICodexTurnStateHeader, state)
		upstream := &httpUpstreamRecorder{responses: []*http.Response{{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(""))}}}
		svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080"}, upstream)
		account := ticketTestAccount(41)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
		require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))
	}
}
func TestOpenAICodexTicket_RequiresActualLengthAndExpiry(t *testing.T) {
	ticket := &openAICodexTicket{State: fakeCodexTicketState(312), Length: 292, ExpiresAt: time.Now().Add(time.Hour)}
	require.False(t, ticket.valid(time.Now(), 292))
	ticket.State = fakeCodexTicketState(292)
	ticket.Length = 292
	ticket.ExpiresAt = time.Time{}
	require.False(t, ticket.valid(time.Now(), 292))
	ticket.State = fakeCodexTicketState(312)
	ticket.Length = 312
	ticket.ExpiresAt = time.Now().Add(time.Hour)
	require.False(t, ticket.valid(time.Now(), 292))
}

// /responses/compact 的出站模型被 Forward 改写为 gateway.openai_compact_model
// （默认非空），门票门控必须按该出站模型判定。否则对门控模型发 compact 请求时，
// 所有无票账号都会被 fail_closed 误判为不可调度，而这些请求实际不需要票。
func TestOpenAICodexTicketGate_CompactRequestUsesForwardOutboundModel(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAICompactModel: "gpt-5.5",
		OpenAICodexTicket: config.OpenAICodexTicketConfig{
			Enabled:      true,
			TargetLength: 292,
			TTLSeconds:   3600,
			FailClosed:   true,
			Models:       []string{"gpt-6-astra"},
		},
	}}}
	account := ticketTestAccount(41) // 无票

	// 出站模型预测必须与 Forward 的解析链一致。
	require.Equal(t, "gpt-6-astra", svc.openAICodexTicketOutboundModel(account, "gpt-6-astra", false))
	require.Equal(t, "gpt-5.5", svc.openAICodexTicketOutboundModel(account, "gpt-6-astra", true))

	// 普通请求：出站仍是门控模型且无票 → fail_closed 必须拦号。
	require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra", false))

	// compact 请求：出站已被改写成非门控的 gpt-5.5 → 不得拦号。
	require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra", true))

	// 回归锚点：按客户端原始模型判定（旧实现的口径）在 compact 下必然误拦。
	require.True(t, svc.openAICodexTicketBlocksAccount(account, canonicalOpenAIAccountSchedulingModel(account, "gpt-6-astra")))
}

func TestOpenAICodexTicketHarvestSkipReason(t *testing.T) {
	now := time.Now()
	fresh := now.Format(time.RFC3339)
	quota := func(window string, used float64, reset time.Time, updated string) map[string]any {
		return map[string]any{
			"codex_" + window + "_used_percent": used,
			"codex_" + window + "_reset_at":     reset.Format(time.RFC3339),
			"codex_usage_updated_at":            updated,
		}
	}

	t.Run("usable_account_keeps_harvesting", func(t *testing.T) {
		require.Empty(t, openAICodexTicketHarvestSkipReason(ticketTestAccount(1), now))
	})
	t.Run("rate_limit_without_full_quota_is_not_skipped", func(t *testing.T) {
		account := ticketTestAccount(1)
		reset := now.Add(2 * time.Hour)
		account.RateLimitResetAt = &reset
		account.Extra = quota("5h", 50, now.Add(3*time.Hour), fresh)
		require.Empty(t, openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("overload_is_not_skipped", func(t *testing.T) {
		account := ticketTestAccount(1)
		until := now.Add(time.Hour)
		account.OverloadUntil = &until
		require.Empty(t, openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("health_temp_unschedulable_is_not_skipped", func(t *testing.T) {
		account := ticketTestAccount(1)
		until := now.Add(time.Hour)
		account.TempUnschedulableUntil = &until
		account.TempUnschedulableReason = "health:auto err_rate=50.0%"
		require.Empty(t, openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("quota_7d_100_skips", func(t *testing.T) {
		account := ticketTestAccount(1)
		account.Extra = quota("7d", 100, now.Add(48*time.Hour), fresh)
		require.Equal(t, "quota_7d", openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("quota_5h_100_skips", func(t *testing.T) {
		account := ticketTestAccount(1)
		account.Extra = quota("5h", 100, now.Add(2*time.Hour), fresh)
		require.Equal(t, "quota_5h", openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("quota_100_after_reset_does_not_skip", func(t *testing.T) {
		account := ticketTestAccount(1)
		account.Extra = quota("7d", 100, now.Add(-time.Minute), fresh)
		require.Empty(t, openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("stale_quota_snapshot_does_not_skip", func(t *testing.T) {
		account := ticketTestAccount(1)
		account.Extra = quota("7d", 100, now.Add(48*time.Hour), now.Add(-3*time.Hour).Format(time.RFC3339))
		require.Empty(t, openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("auth_cooldown_skips", func(t *testing.T) {
		account := ticketTestAccount(1)
		until := now.Add(10 * time.Minute)
		account.TempUnschedulableUntil = &until
		account.TempUnschedulableReason = "OAuth 401: invalid or expired credentials"
		require.Equal(t, "auth", openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("error_status_skips", func(t *testing.T) {
		account := ticketTestAccount(1)
		account.Status = StatusError
		require.Equal(t, "not_active", openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("manual_unschedulable_skips", func(t *testing.T) {
		account := ticketTestAccount(1)
		account.Schedulable = false
		require.Equal(t, "manual_unschedulable", openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("expired_account_skips", func(t *testing.T) {
		account := ticketTestAccount(1)
		account.AutoPauseOnExpired = true
		expired := now.Add(-time.Minute)
		account.ExpiresAt = &expired
		require.Equal(t, "expired", openAICodexTicketHarvestSkipReason(account, now))
	})
	t.Run("missing_ticket_is_not_a_skip", func(t *testing.T) {
		account := ticketTestAccount(1)
		require.Empty(t, openAICodexTicketHarvestSkipReason(account, now))
		require.True(t, ticketTestService(t, config.OpenAICodexTicketConfig{
			Enabled: true, FailClosed: true, TargetLength: 292,
		}, nil).openAICodexTicketBlocksAccount(account, "gpt-6-astra"))
	})
}

func TestRefreshOpenAICodexTickets_SkipsExhaustedQuotaButNotRateLimitCooldown(t *testing.T) {
	now := time.Now()
	exhausted := ticketTestAccount(68)
	exhausted.Extra = map[string]any{
		"codex_7d_used_percent":  100.0,
		"codex_7d_reset_at":      now.Add(48 * time.Hour).Format(time.RFC3339),
		"codex_usage_updated_at": now.Format(time.RFC3339),
		"codex_5h_used_percent":  0.0,
	}
	limited := ticketTestAccount(73)
	reset := now.Add(3 * time.Hour)
	limited.RateLimitResetAt = &reset
	limited.Extra = map[string]any{
		"codex_5h_used_percent":  50.0,
		"codex_5h_reset_at":      now.Add(4 * time.Hour).Format(time.RFC3339),
		"codex_usage_updated_at": now.Format(time.RFC3339),
	}
	upstream := &httpUpstreamRecorder{resp: func() *http.Response {
		h := http.Header{}
		h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}
	}()}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		HarvestProxyURL: "socks5h://harvest.example:31",
		Models:          []string{"gpt-6-astra"},
	}, upstream)
	svc.accountRepo = &codexTicketRefreshRepo{accounts: []Account{*exhausted, *limited}}
	svc.refreshOpenAICodexTickets(context.Background())
	require.Len(t, upstream.requests, 1)
	require.NotNil(t, svc.lookupOpenAICodexTicket(limited, "gpt-6-astra"))
	require.Nil(t, svc.lookupOpenAICodexTicket(exhausted, "gpt-6-astra"))
}

func TestProbeOpenAICodexTicket_UsageLimitReachedPersistsQuotaSnapshot(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	body := `{"error":{"type":"usage_limit_reached","message":"limit reached"}}`
	upstream := &httpUpstreamRecorder{responses: []*http.Response{{
		StatusCode: http.StatusTooManyRequests,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
	}}}
	account := ticketTestAccount(68)
	repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		HarvestProxyURL: "socks5h://harvest.example:31",
		Models:          []string{"gpt-6-astra"},
	}, upstream)
	svc.accountRepo = repo
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))
	require.Equal(t, 100.0, repo.updates["codex_7d_used_percent"])
	require.Equal(t, "quota_7d", openAICodexTicketHarvestSkipReason(account, time.Now()))
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Len(t, upstream.requests, 1)
}

func TestProbeOpenAICodexTicket_NonQuota429KeepsHunting(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "37")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", "12")
	headers.Set("x-codex-secondary-reset-after-seconds", "18000")
	headers.Set("x-codex-secondary-window-minutes", "300")
	header292 := http.Header{}
	header292.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: http.StatusTooManyRequests, Header: headers, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_exceeded"}}`))},
		{StatusCode: http.StatusOK, Header: header292, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))},
	}}
	account := ticketTestAccount(73)
	repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		HarvestProxyURL: "socks5h://harvest.example:31",
	}, upstream)
	svc.accountRepo = repo
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))
	require.Empty(t, openAICodexTicketHarvestSkipReason(account, time.Now()))
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	ticket := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, ticket)
	require.Len(t, upstream.requests, 2)
}

func TestHarvestOpenAICodexTicket_RetriesMissingTicketOnceAMinute(t *testing.T) {
	miss := http.Header{}
	miss.Set(openAICodexTurnStateHeader, fakeCodexTicketState(312))
	stampCodexTicketSetCookies(miss)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     miss,
		Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:                      true,
		TargetLength:                 292,
		TTLSeconds:                   180,
		FailClosed:                   false,
		HarvestProxyURL:              "socks5h://harvest.example:31",
		HarvestAttemptTimeoutSeconds: 5,
		Models:                       []string{"gpt-6-astra", "gpt-5.6-sol"},
	}, upstream)
	account := ticketTestAccount(41)
	ctx := context.Background()

	svc.probeOnceOpenAICodexTicket(ctx, account, "gpt-6-astra")
	require.Len(t, upstream.requests, 1)
	require.True(t, svc.openAICodexTicketHarvestRetryWaiting(account.ID, "gpt-6-astra", time.Now()))
	require.Equal(t, StatusActive, account.Status)
	require.True(t, account.Schedulable)

	svc.probeOnceOpenAICodexTicket(ctx, account, "gpt-6-astra")
	require.Len(t, upstream.requests, 1)
	svc.probeOnceOpenAICodexTicket(ctx, account, "gpt-5.6-sol")
	require.Len(t, upstream.requests, 2)
	require.True(t, svc.openAICodexTicketHarvestRetryWaiting(account.ID, "gpt-5.6-sol", time.Now()))

	repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
	svc.accountRepo = repo
	svc.cfg.Gateway.OpenAICodexTicket.Models = []string{"gpt-6-astra"}
	svc.refreshOpenAICodexTickets(ctx)
	require.Len(t, upstream.requests, 2)

	key := openAICodexTicketKey(account.ID, "gpt-6-astra")
	raw, ok := svc.openaiCodexTicketHarvestBackoff.Load(key)
	require.True(t, ok)
	waiting := *raw.(*openAICodexTicketHarvestBackoff)
	require.True(t, waiting.nextProbeAt.After(time.Now().Add(30*time.Second)))
	waiting.nextProbeAt = time.Now().Add(-time.Second)
	svc.openaiCodexTicketHarvestBackoff.Store(key, &waiting)

	good := http.Header{}
	good.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	stampCodexTicketSetCookies(good)
	upstream.resp = &http.Response{
		StatusCode: http.StatusOK,
		Header:     good,
		Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
	}
	svc.probeOnceOpenAICodexTicket(ctx, account, "gpt-6-astra")
	require.Len(t, upstream.requests, 3)
	require.False(t, svc.openAICodexTicketHarvestRetryWaiting(account.ID, "gpt-6-astra", time.Now()))
	svc.refreshOpenAICodexTickets(ctx)
	require.Len(t, upstream.requests, 3)
}

func TestApplyOpenAICodexTicket_UsesExpiredTicket(t *testing.T) {
	state := fakeCodexTicketState(292)
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, TargetLength: 292, TTLSeconds: 180, FailClosed: true,
		HarvestProxyURL: "socks5h://harvest.example:31",
		Models:          []string{"gpt-6-astra"},
	}, nil)
	account := ticketTestAccount(41)
	expired := time.Now().Add(-time.Minute)
	account.Extra = map[string]any{
		openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{
			"state":       state,
			"length":      292,
			"model":       "gpt-6-astra",
			"cookies":     openAICodexTicketTestCookies,
			"captured_at": expired.Add(-time.Minute),
			"expires_at":  expired,
		},
	}
	h := http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, state, h.Get(openAICodexTurnStateHeader))
	require.Equal(t, openAICodexTicketTestCookies, h.Get("Cookie"))
	require.False(t, svc.openAICodexTicketBlocksAccount(account, "gpt-6-astra"))
	repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
	svc.accountRepo = repo
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(""))}}
	svc.httpUpstream = upstream
	svc.refreshOpenAICodexTickets(context.Background())
	require.Len(t, upstream.requests, 1)
	require.Equal(t, state, h.Get(openAICodexTurnStateHeader))
	svc.refreshOpenAICodexTickets(context.Background())
	require.Len(t, upstream.requests, 1)
}

func TestOpenAICodexTicketHarvestQuotaUpdatesFromUsageLimitBody(t *testing.T) {
	now := time.Now()
	fiveHourReset := now.Add(90 * time.Minute).Unix()
	updates := openAICodexTicketHarvestQuotaUpdatesFromProbe(nil, []byte(
		`{"error":{"type":"usage_limit_reached","message":"limit reached","resets_at":`+strconv.FormatInt(fiveHourReset, 10)+`}}`,
	), now)
	require.Equal(t, 100.0, updates["codex_5h_used_percent"])
	require.NotContains(t, updates, "codex_7d_used_percent")
}
