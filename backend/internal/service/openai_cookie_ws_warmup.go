package service

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const openAICookieWSMinimumConnections = 3

// Count existing verified idle and leased sockets, then replenish only the
// deficit. This sends a probe only on a newly created socket, never a periodic
// ping or a request on an idle business session.
func (s *OpenAIGatewayService) maintainOpenAICookieWSMinimum(ctx context.Context, accountID int64) {
	if s == nil || ctx.Err() != nil {
		return
	}
	_, _, _ = s.openaiCookieWSWarmupFlight.Do(strconv.FormatInt(accountID, 10), func() (any, error) {
		if next, ok := s.openaiCookieWSWarmupRetry.Load(accountID); ok && time.Now().Before(next.(time.Time)) {
			return nil, nil
		}
		pool := s.getOpenAIWSConnPool()
		attempts, rejected := 0, 0
		for attempts < 12 && ctx.Err() == nil {
			account, err := s.latestOpenAICookieWSAccount(ctx, accountID)
			if err != nil {
				return nil, nil
			}
			counts := pool.CookieVerifiedCounts(accountID)
			if counts[0]+counts[1]+counts[2] >= openAICookieWSMinimumConnections {
				s.openaiCookieWSWarmupRetry.Delete(accountID)
				return nil, nil
			}
			var tickets [openAICookieWSSlotCount]*openAICookieWSTicket
			for slot := range tickets {
				tickets[slot] = s.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, slot)
			}
			// Each independent Cookie group owns at most one verified socket.
			// Missing groups remain unavailable until their own Cookie qualifies.
			planned := counts
			slots := make([]int, 0, 4)
			for len(slots) < 4 && planned[0]+planned[1]+planned[2] < openAICookieWSMinimumConnections && attempts+len(slots) < 12 {
				chosen := -1
				for slot, ticket := range tickets {
					if ticket.ready(time.Now()) && planned[slot] < 1 && (chosen < 0 || planned[slot] < planned[chosen]) {
						chosen = slot
					}
				}
				if chosen < 0 {
					break
				}
				slots = append(slots, chosen)
				planned[chosen]++
			}
			if len(slots) == 0 {
				return nil, nil
			}
			outcomes := make(chan bool, len(slots))
			var wg sync.WaitGroup
			for _, slot := range slots {
				wg.Add(1)
				go func(slot int) {
					defer wg.Done()
					outcomes <- s.warmOpenAICookieWSConnection(ctx, accountID, slot)
				}(slot)
			}
			wg.Wait()
			close(outcomes)
			for passed := range outcomes {
				attempts++
				if !passed {
					rejected++
				}
			}
			if rejected > 0 {
				break
			}
		}
		counts := pool.CookieVerifiedCounts(accountID)
		if counts[0]+counts[1]+counts[2] < openAICookieWSMinimumConnections {
			s.openaiCookieWSWarmupRetry.Store(accountID, time.Now().Add(15*time.Second))
		}
		logger.L().Info("openai_cookie_ws minimum pool", zap.Int64("account_id", accountID),
			zap.Int("slot_0_verified", counts[0]), zap.Int("slot_1_verified", counts[1]), zap.Int("slot_2_verified", counts[2]),
			zap.Int("minimum", openAICookieWSMinimumConnections), zap.Int("attempts", attempts), zap.Int("rejected", rejected))
		return nil, nil
	})
}

func (s *OpenAIGatewayService) warmOpenAICookieWSConnection(ctx context.Context, accountID int64, slot int) bool {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	account, err := s.latestOpenAICookieWSAccount(ctx, accountID)
	if err != nil {
		return false
	}
	ticket := s.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, slot)
	if !ticket.ready(time.Now()) {
		return false
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil || token == "" {
		return false
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	applyOpenAICookieWSTicketHeaders(headers, ticket)
	if resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, headers, account) != nil {
		return false
	}
	lease, err := s.getOpenAIWSConnPool().Acquire(ctx, openAIWSAcquireRequest{
		Account: account, WSURL: strings.Replace(chatgptCodexURL, "https://", "wss://", 1),
		Headers: headers, ProxyURL: "", CookieWarmup: true,
	})
	if err != nil {
		return false
	}
	lease.Release()
	return true
}

// The summary reads only process-local verified connections, never persisted
// readiness. It can be used by the admin API without exposing Cookie material.
func (s *AccountTestService) OpenAICookieWSVerifiedCounts(accountID int64) [openAICookieWSSlotCount]int {
	if s == nil || s.openaiGatewayService == nil {
		return [openAICookieWSSlotCount]int{}
	}
	return s.openaiGatewayService.getOpenAIWSConnPool().CookieVerifiedCounts(accountID)
}
