package service

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type cookieWarmupTestDialer struct {
	mu     sync.Mutex
	answer string
	conns  []*cookieWSProbeConn
}

func (d *cookieWarmupTestDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := &cookieWSProbeConn{answers: [][]byte{cookieWSCompletion(openAICodexTicketDefaultModel, d.answer)}}
	d.conns = append(d.conns, c)
	return c, 101, http.Header{}, nil
}

func cookieWarmupFixture(t *testing.T, slots int, answer string) (*OpenAIGatewayService, *Account, *cookieWarmupTestDialer) {
	t.Helper()
	s, _ := cookieWSTestService(t, &httpUpstreamRecorder{})
	a := ticketTestAccount(41)
	s.accountRepo = &cookieWSLifecycleRepo{accounts: []Account{*a}}
	d := &cookieWarmupTestDialer{answer: answer}
	p := s.getOpenAIWSConnPool()
	p.setClientDialerForTest(d)
	p.SetCookieEligibility(func(ctx context.Context, a *Account) error {
		_, err := s.latestOpenAICookieWSAccount(ctx, a.ID)
		return err
	})
	p.SetCookieValidator(s.validateOpenAICookieWSBusinessConn)
	for slot := 0; slot < slots; slot++ {
		ticket := cookieWSTestTicket(a.ID, time.Now())
		ticket.Slot = slot
		ticket.Generation += string(rune('a' + slot))
		s.openaiCookieWSTickets.Store(openAICookieWSKeySlot(a.ID, ticket.Model, slot), ticket)
		p.RotateCookieSlot(a.ID, slot, ticket.Generation)
	}
	return s, a, d
}

func TestCookieWSMinimumBuildsThreeAndDoesNotProbeAgain(t *testing.T) {
	for _, slots := range []int{1, 3} {
		s, a, d := cookieWarmupFixture(t, slots, "True")
		s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
		want := [3]int{1, 0, 0}
		if slots == 3 {
			want = [3]int{1, 1, 1}
		}
		require.Equal(t, want, s.getOpenAIWSConnPool().CookieVerifiedCounts(a.ID))
		require.Len(t, d.conns, slots)
		for _, c := range d.conns {
			require.Len(t, c.writes, 1)
			require.Zero(t, c.pingCount.Load())
		}
		s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
		require.Len(t, d.conns, slots, "healthy established sockets are not periodically probed")
		// A failed socket is removed and replaced; it cannot pad the target.
		s.getOpenAIWSConnPool().accounts.Range(func(_, v any) bool {
			ap := v.(*openAIWSAccountPool)
			ap.mu.Lock()
			var id string
			for key := range ap.conns {
				id = key
				break
			}
			ap.mu.Unlock()
			s.getOpenAIWSConnPool().evictConn(a.ID, id)
			return false
		})
		s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
		counts := s.getOpenAIWSConnPool().CookieVerifiedCounts(a.ID)
		require.Equal(t, slots, counts[0]+counts[1]+counts[2])
		require.Len(t, d.conns, slots+1)
	}
}

func TestCookieWSMinimumFalseClosesAndBacksOff(t *testing.T) {
	s, a, d := cookieWarmupFixture(t, 3, "False")
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	require.Equal(t, [3]int{}, s.getOpenAIWSConnPool().CookieVerifiedCounts(a.ID))
	require.Len(t, d.conns, 3)
	for _, c := range d.conns {
		require.True(t, c.closed.Load())
	}
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	require.Len(t, d.conns, 3, "failed pool fill waits before retrying")
}

func TestCookieWSMinimumSkipsCurrentRateLimitedAccount(t *testing.T) {
	s, a, d := cookieWarmupFixture(t, 3, "True")
	repo := s.accountRepo.(*cookieWSLifecycleRepo)
	until := time.Now().Add(time.Hour)
	repo.accounts[0].RateLimitResetAt = &until
	s.maintainOpenAICookieWSMinimum(context.Background(), a.ID)
	require.Empty(t, d.conns)
}
