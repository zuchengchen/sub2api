package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// This local compatibility key must never be sent to the upstream. Cookie
// identity is deliberately shared within an account, while connection history
// still belongs to the originating API key and client execution scope.
const openAICookieWSScopeHeader = "x-sub2api-cookie-ws-scope"

type openAICookieWSSlotContextKey struct{}

type openAICookieWSSlotReservations struct {
	mu      sync.Mutex
	active  [2]int
	changed chan struct{}
	last    int
}

func (s *OpenAIGatewayService) reserveOpenAICookieWSSlot(ctx context.Context, account *Account, model, preferredConnID string) (int, func(), error) {
	ready := [2]bool{}
	preferred := -1
	if preferredConnID != "" {
		if slot, ok := s.getOpenAIWSConnPool().ConnCookieSlot(account.ID, preferredConnID); ok {
			preferred = slot
		}
	}
	raw, _ := s.openaiCookieWSSlots.LoadOrStore(account.ID, &openAICookieWSSlotReservations{changed: make(chan struct{}), last: 1})
	state := raw.(*openAICookieWSSlotReservations)
	for {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		for slot := range ready {
			ready[slot] = s.lookupOpenAICookieWSTicketSlot(account, model, slot).ready(time.Now())
		}
		if (preferred >= 0 && !ready[preferred]) || (!ready[0] && !ready[1]) {
			return 0, nil, openAICookieWSUnavailableFailover(ErrOpenAICodexTicketUnavailable)
		}
		state.mu.Lock()
		chosen := -1
		if preferred >= 0 {
			if state.active[preferred] < 10 {
				chosen = preferred
			}
		} else {
			for offset := 1; offset <= 2; offset++ {
				slot := (state.last + offset) % 2
				if ready[slot] && state.active[slot] < 10 && (chosen < 0 || state.active[slot] < state.active[chosen]) {
					chosen = slot
				}
			}
		}
		if chosen >= 0 {
			state.active[chosen]++
			state.last = chosen
			state.mu.Unlock()
			var once sync.Once
			return chosen, func() {
				once.Do(func() {
					state.mu.Lock()
					state.active[chosen]--
					close(state.changed)
					state.changed = make(chan struct{})
					state.mu.Unlock()
				})
			}, nil
		}
		changed := state.changed
		state.mu.Unlock()
		// A background refresh can make the second slot available while all
		// current reservations remain active; recheck without sending probes.
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, nil, ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func openAICookieWSSlotFromContext(ctx context.Context) (int, bool) {
	if ctx == nil {
		return 0, false
	}
	slot, ok := ctx.Value(openAICookieWSSlotContextKey{}).(int)
	return slot, ok && slot >= 0 && slot < 2
}

func (s *OpenAIGatewayService) applySelectedOpenAICookieWSHeaders(ctx context.Context, account *Account, model string, headers http.Header) error {
	if slot, ok := openAICookieWSSlotFromContext(ctx); ok {
		return s.applyOpenAICookieWSHeadersForSlot(ctx, account, model, headers, slot)
	}
	return s.applyOpenAICookieWSHeaders(ctx, account, model, headers)
}

func (s *OpenAIGatewayService) resolveOpenAICookieWSDecision(account *Account, model string, compact bool, decision OpenAIWSProtocolDecision) (OpenAIWSProtocolDecision, bool) {
	if compact || account == nil || !s.openAICookieWSEnabledForModel(account, resolveOpenAIAccountUpstreamModelForRequest(account, model, false)) {
		return decision, false
	}
	return OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2, Reason: "cookie_ws"}, true
}

func setOpenAICookieWSExecutionScope(headers http.Header, c *gin.Context, scope string) {
	seed := fmt.Sprintf("cookie_ws:%d:%s", getAPIKeyIDFromContext(c), strings.TrimSpace(scope))
	digest := sha256.Sum256([]byte(seed))
	headers.Set(openAICookieWSScopeHeader, hex.EncodeToString(digest[:]))
}

func applyOpenAICookieWSMetadataRaw(headers http.Header, payload []byte) ([]byte, error) {
	// Existing payload metadata is optional in the Codex wire format. Preserve
	// that shape instead of manufacturing a new object for ordinary callers.
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, err
	}
	applyOpenAICookieWSMetadataFromHeaders(headers, decoded)
	delete(decoded, "stream")
	return json.Marshal(decoded)
}

func openAICookieWSProxyURL(account *Account, cookieWS bool) string {
	if !cookieWS && account != nil && account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func openAICookieWSUnavailableFailover(err error) error {
	if !errors.Is(err, ErrOpenAICodexTicketUnavailable) && !errors.Is(err, errOpenAIWSCookieExpired) && !errors.Is(err, errOpenAIWSCookieRetired) {
		return err
	}
	return &UpstreamFailoverError{
		StatusCode:       http.StatusServiceUnavailable,
		ResponseBody:     []byte(`{"error":{"type":"upstream_unavailable","message":"No verified Cookie websocket is available for this account"}}`),
		ClientStatusCode: http.StatusServiceUnavailable,
		ClientMessage:    "No verified Cookie websocket is available for this account",
	}
}

func wrapOpenAICookieWSCurrentTurnFailover(err error, originalPayload []byte, replayInput []json.RawMessage, replayExists bool, originalModel string) error {
	var failover *UpstreamFailoverError
	if !errors.As(err, &failover) {
		return err
	}
	payload, safe, buildErr := buildOpenAIWSCurrentTurnRetryPayload(originalPayload, replayInput, replayExists, originalModel)
	if buildErr != nil || !safe {
		payload = nil
	}
	return newOpenAIWSCurrentTurnFailoverError(err, payload)
}
