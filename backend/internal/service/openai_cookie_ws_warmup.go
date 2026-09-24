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
		pool := s.getOpenAIWSConnPool()
		// Business traffic may have filled a missing slot since warmup failed.
		// Observe that recovery once without probing or moving healthy clocks.
		for slot, count := range pool.CookieVerifiedCounts(accountID) {
			if count > 0 {
				if diagnostic := s.openAICookieWSRecoverySnapshot(accountID, slot, "warmup"); diagnostic != nil &&
					(diagnostic.Phase != "idle" || diagnostic.LastError != nil || diagnostic.NextAttemptAt != nil) {
					s.succeedOpenAICookieWSRecovery(accountID, slot, "warmup")
				}
			}
		}
		if next, ok := s.openaiCookieWSWarmupRetry.Load(accountID); ok && time.Now().Before(next.(time.Time)) {
			counts := s.getOpenAIWSConnPool().CookieVerifiedCounts(accountID)
			for slot, count := range counts {
				if count < 1 {
					value := next.(time.Time)
					s.phaseOpenAICookieWSRecovery(accountID, slot, "warmup", "backoff", &value)
				}
			}
			return nil, nil
		}
		attempts, rejected := 0, 0
		for attempts < 12 && ctx.Err() == nil {
			account, err := s.latestOpenAICookieWSAccount(ctx, accountID)
			if err != nil {
				counts := pool.CookieVerifiedCounts(accountID)
				for slot, count := range counts {
					if count < 1 {
						s.failOpenAICookieWSRecovery(accountID, slot, "warmup", "waiting", cookieWSRecoveryOperationError("account", err, 0, nil), nil)
					}
				}
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
				for slot, count := range counts {
					if count < 1 {
						s.phaseOpenAICookieWSRecovery(accountID, slot, "warmup", "waiting", nil)
					}
				}
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
			next := time.Now().Add(15 * time.Second)
			s.openaiCookieWSWarmupRetry.Store(accountID, next)
			for slot, count := range counts {
				if count < 1 {
					s.phaseOpenAICookieWSRecovery(accountID, slot, "warmup", "backoff", &next)
				}
			}
		}
		logger.L().Info("openai_cookie_ws minimum pool", zap.Int64("account_id", accountID),
			zap.Int("slot_0_verified", counts[0]), zap.Int("slot_1_verified", counts[1]), zap.Int("slot_2_verified", counts[2]),
			zap.Int("minimum", openAICookieWSMinimumConnections), zap.Int("attempts", attempts), zap.Int("rejected", rejected))
		return nil, nil
	})
}

func (s *OpenAIGatewayService) warmOpenAICookieWSConnection(ctx context.Context, accountID int64, slot int) bool {
	s.beginOpenAICookieWSRecovery(accountID, slot, "warmup", "warming")
	fail := func(failure *openAICookieWSRecoveryFailure) bool {
		s.failOpenAICookieWSRecovery(accountID, slot, "warmup", "waiting", failure, nil)
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	account, err := s.latestOpenAICookieWSAccount(ctx, accountID)
	if err != nil {
		return fail(cookieWSRecoveryOperationError("account", err, 0, nil))
	}
	ticket := s.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, slot)
	if !ticket.ready(time.Now()) {
		return fail(cookieWSRecoveryOperationError("ws_acquire", ErrOpenAICodexTicketUnavailable, 0, nil))
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil || token == "" {
		return fail(cookieWSRecoveryFailure("authentication", "cookie_ws_token_unavailable", "Cookie recovery access token is unavailable", 0, errOpenAICookieWSAccountUnavailable))
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	applyOpenAICookieWSTicketHeaders(headers, ticket)
	if resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, headers, account) != nil {
		return fail(cookieWSRecoveryFailure("account_headers", "cookie_ws_account_identity_unavailable", "Cookie recovery account identity could not be prepared", 0, nil))
	}
	lease, err := s.getOpenAIWSConnPool().Acquire(ctx, openAIWSAcquireRequest{
		Account: account, WSURL: strings.Replace(chatgptCodexURL, "https://", "wss://", 1),
		Headers: headers, ProxyURL: "", CookieWarmup: true,
	})
	if err != nil {
		return fail(cookieWSRecoveryOperationError("ws_acquire", err, 0, nil))
	}
	lease.Release()
	s.succeedOpenAICookieWSRecovery(accountID, slot, "warmup")
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
