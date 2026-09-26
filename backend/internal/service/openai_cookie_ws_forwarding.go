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

const (
	openAICookieWSHTTPFallbackReason                      = "cookie_ws_unavailable_http"
	openAICookieWSPayloadTooLargeHTTPFallbackReason       = "cookie_ws_payload_too_large_http"
	openAICookieWSUnavailableMessage                      = "No verified Cookie websocket is available for this account"
	openAICookieWSHTTPFallbackThresholdBytesDefault int64 = 256 * 1024
)

// openAICookieWSPayloadTooLargeError 表示即将写出的 Cookie WS JSON 已达到
// 配置阈值，应在抢槽/拨号前改走 OAuth HTTP /responses。
type openAICookieWSPayloadTooLargeError struct {
	PayloadBytes   int
	ThresholdBytes int64
}

func (e *openAICookieWSPayloadTooLargeError) Error() string {
	if e == nil {
		return "cookie ws payload too large"
	}
	return fmt.Sprintf("cookie ws payload too large: payload_bytes=%d threshold_bytes=%d", e.PayloadBytes, e.ThresholdBytes)
}

type openAICookieWSSlotContextKey struct{}

type openAICookieWSSlotReservations struct {
	mu      sync.Mutex
	active  [openAICookieWSSlotCount]int
	changed chan struct{}
	last    int
}

func (s *OpenAIGatewayService) reserveOpenAICookieWSSlot(ctx context.Context, account *Account, model, preferredConnID string) (int, func(), error) {
	ready := [openAICookieWSSlotCount]bool{}
	preferred := -1
	if preferredConnID != "" {
		if slot, ok := s.getOpenAIWSConnPool().ConnCookieSlot(account.ID, preferredConnID); ok {
			preferred = slot
		}
	}
	raw, _ := s.openaiCookieWSSlots.LoadOrStore(account.ID, &openAICookieWSSlotReservations{changed: make(chan struct{}), last: openAICookieWSSlotCount - 1})
	state := raw.(*openAICookieWSSlotReservations)
	for {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		anyReady := false
		for slot := range ready {
			ready[slot] = s.lookupOpenAICookieWSTicketSlot(account, model, slot).ready(time.Now())
			anyReady = anyReady || ready[slot]
		}
		if (preferred >= 0 && !ready[preferred]) || !anyReady {
			return 0, nil, openAICookieWSUnavailableFailover(ErrOpenAICodexTicketUnavailable)
		}
		state.mu.Lock()
		chosen := -1
		if preferred >= 0 {
			if state.active[preferred] < 1 {
				chosen = preferred
			}
		} else {
			for offset := 1; offset <= openAICookieWSSlotCount; offset++ {
				slot := (state.last + offset) % openAICookieWSSlotCount
				if ready[slot] && state.active[slot] < 1 && (chosen < 0 || state.active[slot] < state.active[chosen]) {
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
		// A background refresh can make another slot available while all
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
	return slot, ok && slot >= 0 && slot < openAICookieWSSlotCount
}

func (s *OpenAIGatewayService) applySelectedOpenAICookieWSHeaders(ctx context.Context, account *Account, model string, headers http.Header) error {
	if slot, ok := openAICookieWSSlotFromContext(ctx); ok {
		return s.applyOpenAICookieWSHeadersForSlot(ctx, account, model, headers, slot)
	}
	return s.applyOpenAICookieWSHeaders(ctx, account, model, headers)
}

func (s *OpenAIGatewayService) resolveOpenAICookieWSDecision(account *Account, model string, compact bool, decision OpenAIWSProtocolDecision) (OpenAIWSProtocolDecision, bool) {
	if compact || account == nil {
		return decision, false
	}
	upstreamModel := resolveOpenAIAccountUpstreamModelForRequest(account, model, false)
	if s.openAICookieWSHTTPOnlyModel(account, upstreamModel) {
		return openAIWSHTTPDecision("cookie_ws_non_astra_http"), false
	}
	if !s.openAICookieWSEnabledForModel(account, upstreamModel) {
		return decision, false
	}
	// Persisted Cookie tickets deliberately keep processVerified=false after a
	// restart. They are not evidence that this process has a usable websocket;
	// route the request through the normal Responses HTTP path until a fresh
	// websocket validation publishes a ready ticket.
	if !s.openAICookieWSHasReadyTicket(account, upstreamModel) {
		return openAIWSHTTPDecision(openAICookieWSHTTPFallbackReason), false
	}
	return OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2, Reason: "cookie_ws"}, true
}

func (s *OpenAIGatewayService) openAICookieWSHasReadyTicket(account *Account, model string) bool {
	if s == nil || account == nil {
		return false
	}
	now := time.Now()
	for slot := 0; slot < openAICookieWSSlotCount; slot++ {
		if ticket := s.lookupOpenAICookieWSTicketSlot(account, model, slot); ticket != nil && ticket.ready(now) {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) openAICookieWSHTTPOnlyModel(account *Account, upstreamModel string) bool {
	return account != nil && account.Platform == PlatformOpenAI && s.openAICookieWSModeConfigured() &&
		s.openAICodexTicketEnabled() && strings.TrimSpace(upstreamModel) != openAICodexTicketDefaultModel
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
	if !errors.Is(err, ErrOpenAICodexTicketUnavailable) && !errors.Is(err, errOpenAIWSCookieExpired) && !errors.Is(err, errOpenAIWSCookieRetired) &&
		!errors.Is(err, errOpenAICookieWSAccountUnavailable) && !errors.Is(err, errOpenAIWSCookieValidatorMissing) {
		return err
	}
	return &UpstreamFailoverError{
		StatusCode:       http.StatusServiceUnavailable,
		ResponseBody:     []byte(`{"error":{"type":"upstream_unavailable","message":"No verified Cookie websocket is available for this account"}}`),
		ClientStatusCode: http.StatusServiceUnavailable,
		ClientMessage:    openAICookieWSUnavailableMessage,
	}
}

func isOpenAICookieWSUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errOpenAIWSCookieExpired) ||
		errors.Is(err, errOpenAIWSCookieRetired) ||
		errors.Is(err, errOpenAICookieWSAccountUnavailable) ||
		errors.Is(err, errOpenAIWSCookieValidatorMissing) ||
		errors.Is(err, errOpenAIWSConnQueueFull) {
		return true
	}
	var failoverErr *UpstreamFailoverError
	return errors.As(err, &failoverErr) && failoverErr != nil &&
		strings.TrimSpace(failoverErr.ClientMessage) == openAICookieWSUnavailableMessage
}

func isOpenAIWSMessageTooBigError(err error) bool {
	var fallbackErr *openAIWSFallbackError
	if !errors.As(err, &fallbackErr) || fallbackErr == nil {
		return false
	}
	reason := strings.TrimPrefix(strings.TrimSpace(fallbackErr.Reason), "prewarm_")
	return reason == "message_too_big"
}

func shouldOpenAICookieWSHTTPFallback(err error) bool {
	if err == nil {
		return false
	}
	if isOpenAICookieWSUnavailableError(err) || isOpenAIWSMessageTooBigError(err) {
		return true
	}
	var tooLarge *openAICookieWSPayloadTooLargeError
	if errors.As(err, &tooLarge) {
		return true
	}
	return isOpenAICookieWSUnusableProbeError(err)
}

func isOpenAICookieWSUnusableProbeError(err error) bool {
	var testErr *openAICookieWSTestError
	if !errors.As(err, &testErr) || testErr == nil {
		return false
	}
	if errors.Is(testErr.cause, context.Canceled) || testErr.Code == "cookie_ws_cancelled" {
		return false
	}
	return true
}

func isOpenAICookieWSHTTPTransportReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case openAICookieWSHTTPFallbackReason, openAICookieWSPayloadTooLargeHTTPFallbackReason:
		return true
	default:
		return false
	}
}

func cookieWSHTTPFallbackReasonFor(err error) string {
	if isOpenAIWSMessageTooBigError(err) {
		return openAICookieWSPayloadTooLargeHTTPFallbackReason
	}
	var tooLarge *openAICookieWSPayloadTooLargeError
	if errors.As(err, &tooLarge) {
		return openAICookieWSPayloadTooLargeHTTPFallbackReason
	}
	return openAICookieWSHTTPFallbackReason
}

func cookieWSFallbackPayloadStats(err error) (payloadBytes int, thresholdBytes int64) {
	var tooLarge *openAICookieWSPayloadTooLargeError
	if errors.As(err, &tooLarge) && tooLarge != nil {
		return tooLarge.PayloadBytes, tooLarge.ThresholdBytes
	}
	return -1, -1
}

func (s *OpenAIGatewayService) openAICookieWSHTTPFallbackThresholdBytes() int64 {
	if s == nil || s.cfg == nil {
		return openAICookieWSHTTPFallbackThresholdBytesDefault
	}
	if s.cfg.Gateway.OpenAIWS.CookieWSHTTPFallbackThresholdBytes < 0 {
		return openAICookieWSHTTPFallbackThresholdBytesDefault
	}
	return s.cfg.Gateway.OpenAIWS.CookieWSHTTPFallbackThresholdBytes
}

func (s *OpenAIGatewayService) cookieWSPayloadTooLargeError(payloadBytes int) *openAICookieWSPayloadTooLargeError {
	threshold := s.openAICookieWSHTTPFallbackThresholdBytes()
	if threshold <= 0 || int64(payloadBytes) < threshold {
		return nil
	}
	return &openAICookieWSPayloadTooLargeError{PayloadBytes: payloadBytes, ThresholdBytes: threshold}
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
