package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// Tibo routing for Cookie WS accounts: probe the account's own OAuth HTTP
// path once with the fixed astra/low Tibo question. True keeps the request on
// HTTP. Otherwise prefer Excel/BPS (if enabled), then the Tibo-verified Cookie
// WS, and finally HTTP, so the request is always sent.
const (
	openAITiboHTTPOKReason = "tibo_http_ok"
	openAITiboVerdictTTL   = 10 * time.Minute
	openAITiboUnknownTTL   = time.Minute
	openAITiboProbeTimeout = 25 * time.Second
	// A cold account waits at most this long for its first probe.
	openAITiboProbeWait = 8 * time.Second
)

type openAITiboVerdict string

const (
	openAITiboUnknown  openAITiboVerdict = "unknown"
	openAITiboHealthy  openAITiboVerdict = "healthy"
	openAITiboDegraded openAITiboVerdict = "degraded"
)

type openAITiboHTTPState struct {
	verdict   openAITiboVerdict
	checkedAt time.Time
}

func (s *openAITiboHTTPState) fresh(now time.Time) bool {
	ttl := openAITiboVerdictTTL
	if s.verdict == openAITiboUnknown {
		ttl = openAITiboUnknownTTL
	}
	return now.Before(s.checkedAt.Add(ttl))
}

func (s *OpenAIGatewayService) openAITiboRouteApplies(account *Account) bool {
	return s != nil && !s.tiboRouteDisabled && s.openAICookieWSAccountEnabled(account)
}

func (s *OpenAIGatewayService) loadOpenAITiboHTTPState(accountID int64) *openAITiboHTTPState {
	raw, ok := s.openaiTiboHTTP.Load(accountID)
	if !ok {
		return nil
	}
	state, _ := raw.(*openAITiboHTTPState)
	return state
}

// openAITiboHTTPHealthy reports whether the account's HTTP path last answered
// the Tibo probe True. A stale verdict is served while a background probe
// refreshes it; only an account with no verdict yet waits for its first probe.
// Unknown (probe failed or timed out) is not healthy.
func (s *OpenAIGatewayService) openAITiboHTTPHealthy(ctx context.Context, account *Account) bool {
	state := s.loadOpenAITiboHTTPState(account.ID)
	if state != nil && state.fresh(time.Now()) {
		return state.verdict == openAITiboHealthy
	}
	done := s.refreshOpenAITiboHTTP(account)
	if state == nil {
		timer := time.NewTimer(openAITiboProbeWait)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		case <-ctx.Done():
		}
		state = s.loadOpenAITiboHTTPState(account.ID)
	}
	return state != nil && state.verdict == openAITiboHealthy
}

func (s *OpenAIGatewayService) refreshOpenAITiboHTTP(account *Account) <-chan singleflight.Result {
	snapshot := *account
	snapshot.Extra = maps.Clone(account.Extra)
	snapshot.Credentials = maps.Clone(account.Credentials)
	return s.openaiTiboHTTPFlight.DoChan(strconv.FormatInt(account.ID, 10), func() (any, error) {
		// Detached from the caller: a cancelled request must not discard the verdict.
		ctx, cancel := context.WithTimeout(context.Background(), openAITiboProbeTimeout)
		defer cancel()
		verdict, status, answerClass := s.probeOpenAITiboHTTP(ctx, &snapshot)
		previous := s.loadOpenAITiboHTTPState(snapshot.ID)
		s.openaiTiboHTTP.Store(snapshot.ID, &openAITiboHTTPState{verdict: verdict, checkedAt: time.Now()})
		if previous == nil || previous.verdict != verdict {
			logger.L().Info("openai_tibo http verdict", zap.Int64("account_id", snapshot.ID),
				zap.String("verdict", string(verdict)), zap.Int("http", status), zap.String("answer_class", answerClass))
		}
		return verdict, nil
	})
}

// probeOpenAITiboHTTP sends the Tibo probe once over the account's own proxy
// and token (not the Cookie harvest proxy). It never changes account state.
func (s *OpenAIGatewayService) probeOpenAITiboHTTP(ctx context.Context, account *Account) (openAITiboVerdict, int, string) {
	if s.httpUpstream == nil {
		return openAITiboUnknown, 0, ""
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil || token == "" {
		return openAITiboUnknown, 0, ""
	}
	body, err := json.Marshal(openAICookieWSProbePayload(false))
	if err != nil {
		return openAITiboUnknown, 0, ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return openAITiboUnknown, 0, ""
	}
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	newOpenAICookieWSIdentity().apply(req.Header)
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, req.Header, account); err != nil {
		return openAITiboUnknown, 0, ""
	}
	resp, err := s.httpUpstream.Do(req, openAICookieWSProxyURL(account, false), account.ID, account.Concurrency)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil || resp == nil || resp.Body == nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return openAITiboUnknown, status, ""
	}
	if resp.StatusCode != http.StatusOK {
		return openAITiboUnknown, resp.StatusCode, ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, openAICodexTicketProbeBodyLimit+1))
	if err != nil {
		return openAITiboUnknown, resp.StatusCode, ""
	}
	observation := observeOpenAICookieWSHTTPProbe(data)
	switch {
	case observation.completed && observation.modelMatch && observation.trueAnswer && !observation.failed:
		return openAITiboHealthy, resp.StatusCode, observation.answerClass
	case observation.completed && observation.successfulCompletion:
		// Completed but not True, or served by another model: degraded.
		return openAITiboDegraded, resp.StatusCode, observation.answerClass
	default:
		return openAITiboUnknown, resp.StatusCode, observation.answerClass
	}
}
