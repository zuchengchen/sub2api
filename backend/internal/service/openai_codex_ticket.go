package service

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	openAICodexTicketExtraKeyPrefix        = "codex_turn_ticket:"
	openAICodexTicketRevokedExtraKeyPrefix = "codex_turn_ticket_revoked:"
	openAICookieWSExtraKeyPrefix           = "codex_cookie_ws:"
	openAICodexTicketDefaultModel          = "gpt-6-astra"
	openAICodexTicketDefaultSolModel       = "gpt-5.6-sol"
	openAICookieWSSlotCount                = 3
	openAICookieWSGenerationHeader         = "x-sub2api-cookie-generation"
	openAICookieWSExpiresHeader            = "x-sub2api-cookie-expires-at"
	openAICookieWSSlotHeader               = "x-sub2api-cookie-slot"
	openAICookieWSProbeHeader              = "x-sub2api-cookie-probe"
	openAICookieWSScopeHeader              = "x-sub2api-cookie-ws-scope"
)

type openAICodexSharedTicketState struct {
	mu      sync.Mutex
	tickets []*struct{}
}

type openAICodexTicketWatch struct{}

func (o *upstreamResponseModelObserver) noteOpenAICodexTicketCompletion([]byte, string) {}

func (o *upstreamResponseModelObserver) adoptOpenAICodexTicketWatch(*gin.Context) {}

func (s *AccountTestService) OpenAICookieWSVerifiedCounts(int64) [openAICookieWSSlotCount]int {
	return [openAICookieWSSlotCount]int{}
}

func (s *OpenAIGatewayService) validateOpenAICookieWSBusinessConn(context.Context, *Account, *openAIWSConnLease) error {
	return nil
}

const openAICookieWSUnavailableMessage = "No verified Cookie websocket is available for this account"

func extractOpenAICodexTicketModel(body []byte) string {
	return strings.TrimSpace(gjson.GetBytes(body, "model").String())
}

func openAICodexTicketInjected(http.Header, int) bool { return false }

func (s *OpenAIGatewayService) openAICookieWSModeConfigured() bool { return false }

func (s *OpenAIGatewayService) observeOpenAICodexTicketResponseHeader(*gin.Context, http.Header) {}

func (s *OpenAIGatewayService) observeOpenAICodexTicketEvent(*gin.Context, string, []byte) {}

func openAICookieWSUnavailableFailover(err error) error { return err }

func cookieWSHTTPFallbackReasonFor(error) string { return "" }

func cookieWSFallbackPayloadStats(error) (int, int64) { return 0, 0 }

func restoreOpenAICodexTicketIdentity(*gin.Context, http.Header) {}

func isOpenAICodexTicketHarvest(context.Context) bool { return false }

func (s *OpenAIGatewayService) latestOpenAICookieWSAccount(context.Context, int64) (*Account, error) {
	return nil, errOpenAITiboAccountUnavailable
}

func (s *OpenAIGatewayService) openAICookieWSEnabledForModel(*Account, string) bool { return false }

func (s *OpenAIGatewayService) openAICookieWSHTTPOnlyModel(*Account, string) bool { return false }

type openAICookieWSSlotContextKey struct{}

type openAICookieWSProbeObserver struct{ enabled bool }

func (o *openAICookieWSProbeObserver) observe(*openAIWSConnLease, []byte, string) {}

func setOpenAICookieWSExecutionScope(http.Header, *gin.Context, string) {}

func openAICookieWSProxyURL(*Account, bool) string { return "" }

func applyOpenAICookieWSMetadataRaw(http.Header, []byte) ([]byte, error) {
	return nil, nil
}

func wrapOpenAICookieWSCurrentTurnFailover(err error, _ []byte, _ []json.RawMessage, _ bool, _ string) error {
	return err
}

func (s *OpenAIGatewayService) reserveOpenAICookieWSSlotWithin(context.Context, *Account, string, string, time.Duration) (int, func(), error) {
	return 0, func() {}, errOpenAITiboAccountUnavailable
}

func (s *OpenAIGatewayService) applySelectedOpenAICookieWSHeaders(context.Context, *Account, string, http.Header) error {
	return nil
}

func applyOpenAICookieWSMetadataFromHeaders(http.Header, map[string]any) {}

func openAICookieWSIsProbePayload(map[string]any) bool { return false }

type openAICookieWSPayloadTooLargeError struct {
	PayloadBytes   int
	ThresholdBytes int64
}

func (e *openAICookieWSPayloadTooLargeError) Error() string { return "cookie ws payload too large" }

func (s *OpenAIGatewayService) cookieWSPayloadTooLargeError(int) *openAICookieWSPayloadTooLargeError {
	return nil
}

func normalizeOpenAICodexTicketModel(model string) string {
	return strings.TrimSpace(model)
}

func (s *OpenAIGatewayService) openAICodexTicketConfig() config.OpenAICodexTicketConfig {
	if s == nil || s.cfg == nil {
		return config.OpenAICodexTicketConfig{}
	}
	return s.cfg.Gateway.OpenAICodexTicket
}

// OpenAICodexTicketStatus is the admin summary. Ticket harvest and Cookie WS
// are gone; this only carries Tibo HTTP route state.
type OpenAICodexTicketStatus struct {
	Model      string                  `json:"model"`
	TiboRoutes []OpenAITiboRouteStatus `json:"tibo_routes,omitempty"`
}

func OpenAICodexTicketStatuses(account *Account, _ config.OpenAICodexTicketConfig, _ time.Time) []OpenAICodexTicketStatus {
	if !isOpenAICodexTicketAccount(account) {
		return nil
	}
	return []OpenAICodexTicketStatus{{Model: openAICodexTicketDefaultModel}}
}

func (s *AccountTestService) OpenAICodexTicketStatuses(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) []OpenAICodexTicketStatus {
	statuses := OpenAICodexTicketStatuses(account, cfg, now)
	if s == nil || s.openaiGatewayService == nil || len(statuses) == 0 {
		return statuses
	}
	statuses[0].TiboRoutes = s.openaiGatewayService.openAITiboRouteStatuses(account, now)
	return statuses
}

func (s *OpenAIGatewayService) applyOpenAICodexTicket(_ context.Context, _ *Account, _ string, _ http.Header) error {
	return nil
}

func (s *OpenAIGatewayService) applyOpenAICodexTicketForRequest(_ context.Context, _ *gin.Context, _ *Account, _ string, _ http.Header) error {
	return nil
}

func (s *OpenAIGatewayService) openAICodexTicketBlocksAccount(_ *Account, _ string) bool {
	return false
}

func (s *OpenAIGatewayService) StartOpenAICodexTicketHarvester() {
	if s == nil {
		return
	}
	s.openaiCodexTicketLifecycleMu.Lock()
	defer s.openaiCodexTicketLifecycleMu.Unlock()
	if s.openaiCodexTicketStopped || s.openaiCodexTicketDone != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.openaiCodexTicketCancel = cancel
	s.openaiCodexTicketDone = done
	go func() {
		defer close(done)
		s.openAICodexTicketHarvestLoop(ctx)
	}()
	logger.L().Info("openai_tibo probe loop started")
}

func (s *OpenAIGatewayService) StopOpenAICodexTicketHarvester() {
	if s == nil {
		return
	}
	s.openaiCodexTicketLifecycleMu.Lock()
	s.openaiCodexTicketStopped = true
	cancel, done := s.openaiCodexTicketCancel, s.openaiCodexTicketDone
	s.openaiCodexTicketLifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.probeOpenAITiboRoutes(ctx)
			interval := 6 * time.Second
			if seconds := s.openAICodexTicketConfig().HarvestProbeIntervalSeconds; seconds > 0 {
				interval = time.Duration(seconds) * time.Second
			}
			timer.Reset(interval)
		}
	}
}

func openAICodexSuccessfulCompletionModel(payload []byte, eventType string) (string, bool) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return "", false
	}
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		eventType = strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	}
	if eventType == "response.completed" {
		if !openAICodexTicketJSONValueEmpty(payload, "error") || !openAICodexTicketJSONValueEmpty(payload, "response.error") {
			return "", false
		}
		status := strings.TrimSpace(gjson.GetBytes(payload, "response.status").String())
		if status != "" && status != "completed" {
			return "", false
		}
		model := strings.TrimSpace(gjson.GetBytes(payload, "response.model").String())
		if model == "" {
			return "", false
		}
		return model, true
	}
	return "", false
}

func openAICodexTicketJSONValueEmpty(payload []byte, path string) bool {
	value := gjson.GetBytes(payload, path)
	return !value.Exists() || value.Type == gjson.Null
}

func IsOpenAICodexTicketExtraKey(key string) bool {
	return strings.HasPrefix(key, openAICodexTicketExtraKeyPrefix) || strings.HasPrefix(key, openAICodexTicketRevokedExtraKeyPrefix) ||
		strings.HasPrefix(key, openAICookieWSExtraKeyPrefix) || strings.HasPrefix(key, openAITiboVerdictExtraKeyPrefix)
}

func MergeOpenAICodexTicketExtra(extra, current map[string]any) map[string]any {
	result := maps.Clone(extra)
	for key := range result {
		if IsOpenAICodexTicketExtraKey(key) {
			delete(result, key)
		}
	}
	for key, value := range current {
		if strings.HasPrefix(key, openAITiboVerdictExtraKeyPrefix) {
			if result == nil {
				result = make(map[string]any)
			}
			result[key] = value
		}
	}
	return result
}

func isOpenAICodexTicketAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike() && !account.IsShadow()
}

func IsOpenAICodexTicketPrivateExtraKey(key string) bool {
	return IsOpenAICodexTicketExtraKey(key) || key == "codex_harvest_proxy_url"
}

func MaskProxyURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	return "***"
}

func IsMaskedProxyURL(raw string) bool {
	return strings.Contains(raw, "***")
}

func RedactOpenAICodexTicketExtra(extra map[string]any) map[string]any {
	redacted := maps.Clone(extra)
	for key := range redacted {
		if IsOpenAICodexTicketPrivateExtraKey(key) {
			delete(redacted, key)
		}
	}
	return redacted
}


