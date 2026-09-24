package service

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// A background pass may overlap account workers. Every HTTP request gets its
// own response body, and recording remains safe if the worker count changes.
type cookieReplenishmentHTTP struct {
	mu       sync.Mutex
	requests int
}

func (u *cookieReplenishmentHTTP) Do(*http.Request, string, int64, int) (*http.Response, error) {
	u.mu.Lock()
	u.requests++
	u.mu.Unlock()
	return cookieWSHTTPResponse("True"), nil
}

func (u *cookieReplenishmentHTTP) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func (u *cookieReplenishmentHTTP) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests
}

type cookieReplenishmentDialer struct {
	mu     sync.Mutex
	answer string
	conns  []*cookieWSProbeConn
}

func (d *cookieReplenishmentDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Candidate sockets require two answers; each business socket consumes
	// one answer before publication. The write counts distinguish both paths.
	conn := &cookieWSProbeConn{answers: [][]byte{
		cookieWSCompletion(openAICodexTicketDefaultModel, d.answer),
		cookieWSCompletion(openAICodexTicketDefaultModel, d.answer),
	}}
	d.conns = append(d.conns, conn)
	return conn, http.StatusSwitchingProtocols, http.Header{}, nil
}

func (d *cookieReplenishmentDialer) snapshot() []*cookieWSProbeConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*cookieWSProbeConn(nil), d.conns...)
}

func cookieReplenishmentFixture(t *testing.T, ready bool) (*OpenAIGatewayService, *Account, *cookieReplenishmentHTTP, *cookieReplenishmentDialer) {
	t.Helper()
	u := &cookieReplenishmentHTTP{}
	s, _ := cookieWSTestService(t, u)
	account := ticketTestAccount(41)
	s.accountRepo = &cookieWSLifecycleRepo{accounts: []Account{*account}}
	d := &cookieReplenishmentDialer{answer: "True"}
	pool := s.getOpenAIWSConnPool()
	pool.setClientDialerForTest(d)
	pool.SetCookieEligibility(func(ctx context.Context, account *Account) error {
		_, err := s.latestOpenAICookieWSAccount(ctx, account.ID)
		return err
	})
	if ready {
		for slot := 0; slot < openAICookieWSSlotCount; slot++ {
			ticket := cookieWSTestTicket(account.ID, time.Now())
			ticket.Slot = slot
			ticket.Generation += "-" + strconv.Itoa(slot)
			s.openaiCookieWSTickets.Store(openAICookieWSKeySlot(account.ID, ticket.Model, slot), ticket)
			pool.RotateCookieSlot(account.ID, slot, ticket.Generation)
		}
	}
	return s, account, u, d
}

func cookieReplenishmentSockets(t *testing.T, pool *openAIWSConnPool, accountID int64) [openAICookieWSSlotCount]*openAIWSConn {
	t.Helper()
	var result [openAICookieWSSlotCount]*openAIWSConn
	ap, ok := pool.getAccountPool(accountID)
	require.True(t, ok)
	ap.mu.Lock()
	defer ap.mu.Unlock()
	for _, conn := range ap.conns {
		if conn == nil || !conn.cookieVerified.Load() || conn.handshakeCompatibility.cookieProbe {
			continue
		}
		slot := conn.handshakeCompatibility.cookieSlot
		require.Nil(t, result[slot], "at most one business socket per Cookie group")
		result[slot] = conn
	}
	return result
}

func TestOpenAICookieWSReplenishmentFullPassRecoversOneLostSocketWithoutHTTP(t *testing.T) {
	s, account, upstream, dialer := cookieReplenishmentFixture(t, false)
	pool := s.getOpenAIWSConnPool()
	s.refreshOpenAICookieWSTickets(context.Background())
	require.Equal(t, [3]int{1, 1, 1}, pool.CookieVerifiedCounts(account.ID))
	require.Equal(t, 3, upstream.count(), "each independent missing Cookie is acquired once")
	initial := dialer.snapshot()
	require.Len(t, initial, 6, "three validation candidates and three verified business sockets")
	for i, conn := range initial {
		if i < 3 {
			require.Len(t, conn.writes, 2, "Cookie candidates pass both validations")
			require.True(t, conn.closed.Load(), "candidate validation sockets are retired")
		} else {
			require.Len(t, conn.writes, 1, "business sockets pass their own validation")
			require.False(t, conn.closed.Load())
		}
	}
	before := cookieReplenishmentSockets(t, pool, account.ID)
	var generations [3]string
	for slot := range generations {
		generations[slot] = s.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, slot).Generation
	}
	// Eviction is the pool's transport-failure notification. A later full
	// harvester pass must retain the healthy Cookie and the other sockets.
	pool.evictConn(account.ID, before[1].id)
	require.Equal(t, [3]int{1, 0, 1}, pool.CookieVerifiedCounts(account.ID))
	s.refreshOpenAICookieWSTickets(context.Background())
	require.Equal(t, [3]int{1, 1, 1}, pool.CookieVerifiedCounts(account.ID))
	require.Equal(t, 3, upstream.count(), "valid Cookies do not need another HTTP probe")
	after := cookieReplenishmentSockets(t, pool, account.ID)
	require.Same(t, before[0], after[0])
	require.NotSame(t, before[1], after[1])
	require.Same(t, before[2], after[2])
	for slot := range generations {
		require.Equal(t, generations[slot], s.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, slot).Generation)
	}
	conns := dialer.snapshot()
	require.Len(t, conns, 7, "only the missing business socket is rebuilt")
	require.Len(t, conns[6].writes, 1)
	s.refreshOpenAICookieWSTickets(context.Background())
	require.Len(t, dialer.snapshot(), 7, "healthy sockets are not repeatedly validated")
	require.Equal(t, 3, upstream.count())
	for _, conn := range dialer.snapshot() {
		require.Zero(t, conn.pingCount.Load())
	}
}

func TestOpenAICookieWSReplenishmentExpiredCookieIsReplacedBeforeBusinessSocket(t *testing.T) {
	s, account, upstream, dialer := cookieReplenishmentFixture(t, true)
	pool := s.getOpenAIWSConnPool()
	s.refreshOpenAICookieWSTickets(context.Background())
	require.Equal(t, [3]int{1, 1, 1}, pool.CookieVerifiedCounts(account.ID))
	require.Zero(t, upstream.count())
	before := cookieReplenishmentSockets(t, pool, account.ID)
	old := s.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, 2)
	expired := *old
	expired.CapturedAt = time.Now().Add(-61 * time.Minute)
	expired.RefreshAt = expired.CapturedAt.Add(openAICookieWSRefreshAge)
	expired.ExpiresAt = expired.CapturedAt.Add(openAICookieWSLifetime)
	s.openaiCookieWSTickets.Store(openAICookieWSKeySlot(account.ID, expired.Model, 2), &expired)
	s.refreshOpenAICookieWSTickets(context.Background())
	require.Equal(t, [3]int{1, 1, 1}, pool.CookieVerifiedCounts(account.ID))
	require.Equal(t, 1, upstream.count())
	renewed := s.lookupOpenAICookieWSTicketSlot(account, expired.Model, 2)
	require.True(t, renewed.ready(time.Now()))
	require.NotEqual(t, old.Generation, renewed.Generation)
	conns := dialer.snapshot()
	require.Len(t, conns, 5, "one new candidate followed by one new business socket")
	require.Len(t, conns[3].writes, 2)
	require.True(t, conns[3].closed.Load())
	require.Len(t, conns[4].writes, 1)
	require.False(t, conns[4].closed.Load())
	after := cookieReplenishmentSockets(t, pool, account.ID)
	require.Same(t, before[0], after[0])
	require.Same(t, before[1], after[1])
	require.NotSame(t, before[2], after[2])
	require.True(t, before[2].isClosed())
}

func TestOpenAICookieWSReplenishmentBackgroundSkipsUnavailableAccounts(t *testing.T) {
	for _, ready := range []bool{false, true} {
		for _, reason := range []string{"rate_limited", "unschedulable"} {
			t.Run(reason+"/ready="+strconv.FormatBool(ready), func(t *testing.T) {
				s, account, upstream, dialer := cookieReplenishmentFixture(t, ready)
				repo := s.accountRepo.(*cookieWSLifecycleRepo)
				repo.mu.Lock()
				if reason == "rate_limited" {
					until := time.Now().Add(time.Hour)
					repo.accounts[0].RateLimitResetAt = &until
				} else {
					repo.accounts[0].Schedulable = false
				}
				repo.mu.Unlock()
				s.refreshOpenAICookieWSTickets(context.Background())
				require.Zero(t, upstream.count(), "unavailable accounts do not acquire Cookies")
				require.Empty(t, dialer.snapshot(), "unavailable accounts do not replenish sockets")
				require.Equal(t, [3]int{}, s.getOpenAIWSConnPool().CookieVerifiedCounts(account.ID))
			})
		}
	}
}

func TestOpenAICookieWSReplenishmentFalseClosesAndDefersNextBackgroundPass(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run("ready="+strconv.FormatBool(ready), func(t *testing.T) {
			s, account, upstream, dialer := cookieReplenishmentFixture(t, ready)
			dialer.mu.Lock()
			dialer.answer = "False"
			dialer.mu.Unlock()
			s.refreshOpenAICookieWSTickets(context.Background())
			require.Equal(t, [3]int{}, s.getOpenAIWSConnPool().CookieVerifiedCounts(account.ID))
			conns := dialer.snapshot()
			require.Len(t, conns, 3)
			for _, conn := range conns {
				require.True(t, conn.closed.Load(), "False sockets cannot stay in the pool")
				require.Len(t, conn.writes, 1)
			}
			httpCount := 3
			if ready {
				httpCount = 0
			}
			require.Equal(t, httpCount, upstream.count())
			s.refreshOpenAICookieWSTickets(context.Background())
			require.Len(t, dialer.snapshot(), 3, "the next pass respects probe or warmup backoff")
			require.Equal(t, httpCount, upstream.count())
			for slot := 0; slot < 3; slot++ {
				ticket := s.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, slot)
				require.Equal(t, ready, ticket.ready(time.Now()), "business False preserves the valid Cookie; candidate False cannot publish it")
			}
			if ready {
				// Advance the retry deadline without sleeping. Once upstream
				// recovers, the retained Cookies can fill all three slots.
				s.openaiCookieWSWarmupRetry.Store(account.ID, time.Now().Add(-time.Second))
				dialer.mu.Lock()
				dialer.answer = "True"
				dialer.mu.Unlock()
				s.refreshOpenAICookieWSTickets(context.Background())
				require.Equal(t, [3]int{1, 1, 1}, s.getOpenAIWSConnPool().CookieVerifiedCounts(account.ID))
				require.Len(t, dialer.snapshot(), 6)
				require.Zero(t, upstream.count())
			}
		})
	}
}
