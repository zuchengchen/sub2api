package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	openAICookieWSMode           = "cookie_ws"
	openAICookieWSExtraKeyPrefix = "codex_cookie_ws:"
	// Pool metadata only. The dialer removes these before the upstream handshake.
	openAICookieWSGenerationHeader = "x-sub2api-cookie-generation"
	openAICookieWSExpiresHeader    = "x-sub2api-cookie-expires-at"
	openAICookieWSSlotHeader       = "x-sub2api-cookie-slot"
	openAICookieWSProbeHeader      = "x-sub2api-cookie-probe"
	openAICookieWSSlotCount        = 3
	openAICookieWSLifetime         = 60 * time.Minute
	openAICookieWSRefreshAge       = 50 * time.Minute
	openAICookieWSProbePrompt      = "Tell me whether you know who Tibo is in OpenAI based on your knowledge without searching online. Only return True or  False."
)

type openAICookieWSIdentity struct {
	Version         string `json:"version"`
	UserAgent       string `json:"user_agent"`
	Originator      string `json:"originator"`
	InstallationID  string `json:"installation_id"`
	WindowID        string `json:"window_id"`
	SessionID       string `json:"session_id"`
	ThreadID        string `json:"thread_id"`
	ClientRequestID string `json:"client_request_id"`
	RoutingHint     string `json:"routing_hint"`
}

func newOpenAICookieWSIdentity() openAICookieWSIdentity {
	// The same coherent device profile used by the successful Kit experiment.
	version := "0.156.1"
	if canonical := CodexCanonicalClientVersion(); CompareVersions(canonical, version) > 0 {
		version = canonical
	}
	threadID := uuid.NewString()
	return openAICookieWSIdentity{
		Version: version, UserAgent: "codex_cli_rs/" + version + " (Mac OS 15.5.0; arm64) xterm-256color",
		Originator: "codex_cli_rs", InstallationID: uuid.NewString(), WindowID: uuid.NewString(),
		SessionID: uuid.NewString(), ThreadID: threadID, ClientRequestID: threadID,
		RoutingHint: "model=" + openAICodexTicketDefaultModel,
	}
}

func (i openAICookieWSIdentity) valid() bool {
	return i.Version != "" && i.UserAgent != "" && i.Originator != "" &&
		i.InstallationID != "" && i.WindowID != "" && i.SessionID != "" &&
		i.ThreadID != "" && i.ClientRequestID != "" && i.RoutingHint != ""
}

func (i openAICookieWSIdentity) apply(h http.Header) {
	// These belong to the caller's device, not the successful HTTP identity.
	for _, key := range []string{"session-id", "conversation_id", "x-codex-turn-metadata", "x-codex-parent-thread-id"} {
		h.Del(key)
	}
	for key, value := range map[string]string{
		"version": i.Version, "user-agent": i.UserAgent, "originator": i.Originator,
		"x-codex-installation-id": i.InstallationID, "x-codex-window-id": i.WindowID,
		"session_id": i.SessionID, "thread-id": i.ThreadID,
		"x-client-request-id": i.ClientRequestID, "x-codex-routing-hint": i.RoutingHint,
	} {
		h.Set(key, value)
	}
}

// Ticket values are immutable after publication, including processVerified.
// Persisted tickets retain their original absolute lifetime but must pass fresh
// direct WS verification after a process restart before serving requests.
type openAICookieWSTicket struct {
	AccountID       int64                  `json:"account_id"`
	Slot            int                    `json:"slot"`
	Model           string                 `json:"model"`
	Generation      string                 `json:"generation"`
	Cookies         string                 `json:"cookies"`
	Identity        openAICookieWSIdentity `json:"identity"`
	CapturedAt      time.Time              `json:"captured_at"`
	RefreshAt       time.Time              `json:"refresh_at"`
	ExpiresAt       time.Time              `json:"expires_at"`
	HTTPVerified    bool                   `json:"http_verified"`
	WSVerified      bool                   `json:"ws_verified"`
	processVerified bool
}

func (t *openAICookieWSTicket) valid(now time.Time) bool {
	return t != nil && t.AccountID > 0 && t.Model == openAICodexTicketDefaultModel &&
		t.Slot >= 0 && t.Slot < openAICookieWSSlotCount &&
		t.Generation != "" && t.Cookies != "" && t.Identity.valid() && t.HTTPVerified && t.WSVerified &&
		!t.CapturedAt.IsZero() && !now.Before(t.CapturedAt) &&
		now.Before(t.CapturedAt.Add(openAICookieWSLifetime)) && now.Before(t.ExpiresAt)
}

func (t *openAICookieWSTicket) ready(now time.Time) bool {
	return t.valid(now) && t.processVerified
}

func (t *openAICookieWSTicket) needsRefresh(now time.Time) bool {
	return t == nil || !t.ready(now) || !now.Before(t.CapturedAt.Add(openAICookieWSRefreshAge))
}

func openAICookieWSExtraKey(model string) string {
	return openAICookieWSExtraKeyPrefix + strings.TrimSpace(model)
}

func openAICookieWSExtraKeySlot(model string, slot int) string {
	if slot == 0 {
		return openAICookieWSExtraKey(model)
	}
	return openAICookieWSExtraKey(model) + ":slot:" + strconv.Itoa(slot)
}

func openAICookieWSKeySlot(accountID int64, model string, slot int) string {
	if slot == 0 {
		return openAICodexTicketKey(accountID, model)
	}
	return openAICodexTicketKey(accountID, model) + "\x00slot:" + strconv.Itoa(slot)
}

func openAICookieWSAccountConfigured(account *Account, cfg config.OpenAICodexTicketConfig) bool {
	if !isOpenAICodexTicketAccount(account) || account.Type != AccountTypeOAuth || account.IsOpenAIAgentIdentity() ||
		!strings.EqualFold(strings.TrimSpace(cfg.Mode), openAICookieWSMode) {
		return false
	}
	if len(cfg.CookieWSAccountIDs) == 0 {
		return true
	}
	for _, id := range cfg.CookieWSAccountIDs {
		if id == account.ID {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) openAICookieWSAccountEnabled(account *Account) bool {
	return s != nil && openAICookieWSAccountConfigured(account, s.openAICodexTicketConfig()) && s.openAICodexTicketEnabled()
}

func (s *OpenAIGatewayService) openAICookieWSModeConfigured() bool {
	return s != nil && strings.EqualFold(strings.TrimSpace(s.openAICodexTicketConfig().Mode), openAICookieWSMode)
}

func (s *OpenAIGatewayService) openAICookieWSEnabledForModel(account *Account, model string) bool {
	return strings.TrimSpace(model) == openAICodexTicketDefaultModel && s.openAICookieWSAccountEnabled(account)
}

func parseOpenAICookieWSTicket(accountID int64, model string, raw any) *openAICookieWSTicket {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var ticket openAICookieWSTicket
	if json.Unmarshal(b, &ticket) != nil || ticket.AccountID != accountID || ticket.Model != model {
		return nil
	}
	// A legacy or malformed deadline must never extend a captured cookie.
	deadline := ticket.CapturedAt.Add(openAICookieWSLifetime)
	if ticket.ExpiresAt.IsZero() || ticket.ExpiresAt.After(deadline) {
		ticket.ExpiresAt = deadline
	}
	ticket.RefreshAt = ticket.CapturedAt.Add(openAICookieWSRefreshAge)
	return &ticket
}

func (s *OpenAIGatewayService) lookupOpenAICookieWSTicket(account *Account, model string) *openAICookieWSTicket {
	var fallback *openAICookieWSTicket
	for slot := 0; slot < openAICookieWSSlotCount; slot++ {
		ticket := s.lookupOpenAICookieWSTicketSlot(account, model, slot)
		if ticket.ready(time.Now()) {
			return ticket
		}
		if fallback == nil {
			fallback = ticket
		}
	}
	return fallback
}

func (s *OpenAIGatewayService) lookupOpenAICookieWSTicketSlot(account *Account, model string, slot int) *openAICookieWSTicket {
	if s == nil || account == nil {
		return nil
	}
	key := openAICookieWSKeySlot(account.ID, model, slot)
	var current *openAICookieWSTicket
	if raw, ok := s.openaiCookieWSTickets.Load(key); ok {
		current, _ = raw.(*openAICookieWSTicket)
	}
	persisted := parseOpenAICookieWSTicket(account.ID, model, account.Extra[openAICookieWSExtraKeySlot(model, slot)])
	if persisted != nil && persisted.Slot != slot {
		persisted = nil
	}
	if persisted != nil && (current == nil || persisted.CapturedAt.After(current.CapturedAt)) {
		return persisted
	}
	return current
}

func applyOpenAICookieWSTicketHeaders(h http.Header, ticket *openAICookieWSTicket) {
	ticket.Identity.apply(h)
	h.Set("Cookie", ticket.Cookies)
	h.Set("OpenAI-Beta", openAIWSBetaV2Value)
	h.Del(openAICodexTurnStateHeader)
	h.Set(openAICookieWSGenerationHeader, ticket.Generation)
	h.Set(openAICookieWSSlotHeader, strconv.Itoa(ticket.Slot))
	h.Set(openAICookieWSExpiresHeader, strconv.FormatInt(ticket.ExpiresAt.Unix(), 10))
}

func (s *OpenAIGatewayService) applyOpenAICookieWSHeaders(ctx context.Context, account *Account, model string, h http.Header) error {
	if !s.openAICookieWSEnabledForModel(account, model) {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	ticket := s.lookupOpenAICookieWSTicket(account, strings.TrimSpace(model))
	if h == nil || !ticket.ready(time.Now()) {
		return ErrOpenAICodexTicketUnavailable
	}
	applyOpenAICookieWSTicketHeaders(h, ticket)
	return nil
}

func (s *OpenAIGatewayService) applyOpenAICookieWSHeadersForSlot(ctx context.Context, account *Account, model string, h http.Header, slot int) error {
	if !s.openAICookieWSEnabledForModel(account, model) {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	ticket := s.lookupOpenAICookieWSTicketSlot(account, strings.TrimSpace(model), slot)
	if h == nil || !ticket.ready(time.Now()) {
		return ErrOpenAICodexTicketUnavailable
	}
	applyOpenAICookieWSTicketHeaders(h, ticket)
	return nil
}

// Derive body identity from the chosen handshake, so a concurrent refresh cannot
// mix two generations. Like Kit, preserve absent metadata and unrelated fields.
func applyOpenAICookieWSMetadataFromHeaders(h http.Header, payload map[string]any) {
	if h.Get(openAICookieWSGenerationHeader) == "" {
		return
	}
	metadata, ok := payload["client_metadata"].(map[string]any)
	if !ok {
		return
	}
	metadata = maps.Clone(metadata)
	for _, key := range []string{"x-codex-installation-id", "session_id", "x-codex-window-id"} {
		metadata[key] = h.Get(key)
	}
	payload["client_metadata"] = metadata
}

func openAICookieWSProbePayload(websocket bool) map[string]any {
	payload := map[string]any{
		"model": openAICodexTicketDefaultModel, "store": false, "instructions": "",
		"reasoning": map[string]any{"effort": "low"}, "include": []string{"reasoning.encrypted_content"},
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": openAICookieWSProbePrompt}}}},
	}
	if websocket {
		payload["type"] = "response.create"
	} else {
		payload["stream"] = true
	}
	return payload
}

type openAICookieWSObservation struct {
	delta                strings.Builder
	completed            bool
	successfulCompletion bool
	failureStatus        int
	failed               bool
	trueAnswer           bool
	modelMatch           bool
	answerClass          string
}

func (o *openAICookieWSObservation) event(payload []byte, eventType string) {
	if !gjson.ValidBytes(payload) {
		return
	}
	if eventType == "" {
		eventType = gjson.GetBytes(payload, "type").String()
	}
	switch eventType {
	case "error", "response.failed", "response.incomplete", "response.cancelled":
		o.failed = true
		o.failureStatus = cookieWSTestPayloadStatus(payload)
	case "response.output_text.delta":
		o.delta.WriteString(gjson.GetBytes(payload, "delta").String())
	case "response.completed":
		if o.completed {
			o.failed = true
			return
		}
		o.completed = true
		model, success := openAICodexSuccessfulCompletionModel(payload, eventType)
		o.successfulCompletion = success
		o.failureStatus = cookieWSTestPayloadStatus(payload)
		o.modelMatch = success && model == openAICodexTicketDefaultModel
		if !o.modelMatch {
			o.failed = true
			return
		}
		var output strings.Builder
		for _, item := range gjson.GetBytes(payload, "response.output").Array() {
			for _, content := range item.Get("content").Array() {
				if content.Get("type").String() == "output_text" {
					output.WriteString(content.Get("text").String())
				}
				if content.Get("type").String() == "refusal" {
					o.failed = true
				}
			}
		}
		answer := output.String()
		if answer == "" {
			answer = o.delta.String()
		}
		switch strings.TrimSpace(answer) {
		case "True":
			o.answerClass = "True"
		case "False":
			o.answerClass = "False"
		case "":
			o.answerClass = "none"
		default:
			o.answerClass = "other"
		}
		o.trueAnswer = o.answerClass == "True"
	}
}

func openAICookieWSProbePassed(body []byte) bool {
	observation := observeOpenAICookieWSHTTPProbe(body)
	return observation.completed && observation.trueAnswer && !observation.failed
}

func observeOpenAICookieWSHTTPProbe(body []byte) *openAICookieWSObservation {
	observation := &openAICookieWSObservation{answerClass: "none"}
	if len(body) == 0 || len(body) > openAICodexTicketProbeBodyLimit {
		observation.failed = true
		return observation
	}
	forEachOpenAISSEFrame(string(body), func(eventType string, payload []byte) { observation.event(payload, eventType) })
	return observation
}

func (s *OpenAIGatewayService) doOpenAICookieWSHTTPProbe(ctx context.Context, account *Account, token, proxy string, identity openAICookieWSIdentity) (*openAICodexTicketProbeResult, error) {
	if account == nil {
		return nil, cookieWSRecoveryOperationError("account", errOpenAICookieWSAccountUnavailable, 0, nil)
	}
	current, err := s.latestOpenAICookieWSAccount(ctx, account.ID)
	if err != nil {
		return nil, cookieWSRecoveryOperationError("account", err, 0, nil)
	}
	account = current
	token, _, err = s.GetAccessToken(ctx, account)
	if err != nil || token == "" {
		return nil, cookieWSRecoveryFailure("authentication", "cookie_ws_token_unavailable", "Cookie recovery access token is unavailable", 0, errOpenAICookieWSAccountUnavailable)
	}
	body, _ := json.Marshal(openAICookieWSProbePayload(false))
	req, err := http.NewRequestWithContext(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAIHarvest), http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return nil, cookieWSRecoveryOperationError("http_request", err, 0, nil)
	}
	req.Close = true
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	identity.apply(req.Header)
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, req.Header, account); err != nil {
		return nil, cookieWSRecoveryFailure("account_headers", "cookie_ws_account_identity_unavailable", "Cookie recovery account identity could not be prepared", 0, nil)
	}
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		var result *openAICodexTicketProbeResult
		if resp != nil {
			result = &openAICodexTicketProbeResult{capturedAt: time.Now(), status: resp.StatusCode, headers: resp.Header.Clone()}
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			return result, cookieWSRecoveryOperationError("http_request", err, result.status, result.headers)
		}
		return nil, cookieWSRecoveryOperationError("http_request", err, 0, nil)
	}
	if resp == nil || resp.Body == nil {
		var result *openAICodexTicketProbeResult
		status := 0
		if resp != nil {
			status = resp.StatusCode
			result = &openAICodexTicketProbeResult{capturedAt: time.Now(), status: status, headers: resp.Header.Clone()}
		}
		return result, cookieWSRecoveryFailure("http_stream", "cookie_ws_http_body_missing", "Cookie recovery HTTP response had no body", status, nil)
	}
	capturedAt := time.Now()
	defer resp.Body.Close()
	limit := int64(openAICodexTicketHarvestErrorBodyLimit)
	if resp.StatusCode == http.StatusOK {
		limit = openAICodexTicketProbeBodyLimit + 1
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	result := &openAICodexTicketProbeResult{capturedAt: capturedAt, status: resp.StatusCode, headers: resp.Header.Clone(), body: data}
	if err != nil {
		return result, cookieWSRecoveryOperationError("http_stream", err, resp.StatusCode, resp.Header)
	}
	return result, nil
}

// validateOpenAICookieWSCandidate keeps a successful probe connection in the pool;
// its generation cannot be selected by business traffic until publication.
func (s *OpenAIGatewayService) validateOpenAICookieWSCandidate(ctx context.Context, account *Account, token string, ticket *openAICookieWSTicket) (*openAIWSConnLease, error) {
	if account == nil {
		return nil, cookieWSRecoveryOperationError("account", errOpenAICookieWSAccountUnavailable, 0, nil)
	}
	current, err := s.latestOpenAICookieWSAccount(ctx, account.ID)
	if err != nil {
		return nil, cookieWSRecoveryOperationError("account", err, 0, nil)
	}
	account = current
	token, _, err = s.GetAccessToken(ctx, account)
	if err != nil || token == "" {
		return nil, cookieWSRecoveryFailure("authentication", "cookie_ws_token_unavailable", "Cookie recovery access token is unavailable", 0, errOpenAICookieWSAccountUnavailable)
	}
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+token)
	applyOpenAICookieWSTicketHeaders(h, ticket)
	h.Set(openAICookieWSProbeHeader, "1")
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, h, account); err != nil {
		return nil, cookieWSRecoveryFailure("account_headers", "cookie_ws_account_identity_unavailable", "Cookie recovery account identity could not be prepared", 0, nil)
	}
	pool := s.getOpenAIWSConnPool()
	lease, err := pool.Acquire(ctx, openAIWSAcquireRequest{
		Account: account, WSURL: strings.Replace(chatgptCodexURL, "https://", "wss://", 1),
		Headers: h, ProxyURL: "", ForceNewConn: true,
	})
	if err != nil {
		return nil, cookieWSRecoveryOperationError("ws_acquire", err, 0, nil)
	}
	success := false
	defer func() {
		if !success {
			lease.MarkBroken()
			lease.Release()
		}
	}()
	for turn := 0; turn < 2; turn++ {
		if _, err := s.latestOpenAICookieWSAccount(ctx, account.ID); err != nil {
			return nil, cookieWSRecoveryOperationError("account", err, 0, nil)
		}
		if err := lease.WriteJSONContext(ctx, openAICookieWSProbePayload(true)); err != nil {
			return nil, cookieWSRecoveryOperationError("ws_write", err, 0, nil)
		}
		var observation openAICookieWSObservation
		total := 0
		for !observation.completed && !observation.failed {
			message, err := lease.ReadMessageContext(ctx)
			if err != nil {
				return nil, cookieWSRecoveryOperationError("ws_read", err, 0, nil)
			}
			total += len(message)
			if total > openAICodexTicketProbeBodyLimit {
				return nil, cookieWSRecoveryFailure("ws_validation", "cookie_ws_ws_validation_too_large", "Cookie recovery websocket validation response exceeded its size limit", 0, nil)
			}
			observation.event(message, "")
		}
		if !observation.completed || !observation.trueAnswer || observation.failed {
			return nil, cookieWSRecoveryObservationError("ws_validation", &observation, 0)
		}
	}
	success = true
	return lease, nil
}

type openAICookieWSRetryState struct {
	attempts      int
	nextAttemptAt time.Time
	reason        string
}

func (s *OpenAIGatewayService) noteOpenAICookieWSMiss(accountID int64, now time.Time, reason string, status int, headers http.Header) {
	s.noteOpenAICookieWSSlotMiss(accountID, 0, now, reason, status, headers)
}

func (s *OpenAIGatewayService) noteOpenAICookieWSSlotMiss(accountID int64, slot int, now time.Time, reason string, status int, headers http.Header) {
	key := openAICookieWSKeySlot(accountID, openAICodexTicketDefaultModel, slot)
	attempts := 1
	if raw, ok := s.openaiCookieWSRetry.Load(key); ok {
		if previous, ok := raw.(*openAICookieWSRetryState); ok {
			attempts = previous.attempts + 1
		}
	}
	delays := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}
	delay := delays[min(attempts-1, len(delays)-1)]
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if delay < time.Minute {
			delay = time.Minute
		}
	}
	next := now.Add(delay)
	if reset := parseRetryAfterResetTime(headers, now); reset != nil && reset.After(next) {
		next = *reset
	}
	s.openaiCookieWSRetry.Store(key, &openAICookieWSRetryState{attempts: attempts, nextAttemptAt: next, reason: reason})
	s.phaseOpenAICookieWSRecovery(accountID, slot, "refresh", "backoff", &next)
	logger.L().Info("openai_cookie_ws refresh deferred", zap.Int64("account_id", accountID), zap.Int("slot", slot), zap.String("reason", reason), zap.Int("http", status), zap.Time("next_attempt_at", next))
}

func (s *OpenAIGatewayService) openAICookieWSRetryWaiting(accountID int64, now time.Time) bool {
	return s.openAICookieWSSlotRetryWaiting(accountID, 0, now)
}

func (s *OpenAIGatewayService) openAICookieWSSlotRetryWaiting(accountID int64, slot int, now time.Time) bool {
	raw, ok := s.openaiCookieWSRetry.Load(openAICookieWSKeySlot(accountID, openAICodexTicketDefaultModel, slot))
	if !ok {
		return false
	}
	state, ok := raw.(*openAICookieWSRetryState)
	return ok && state != nil && now.Before(state.nextAttemptAt)
}

func (s *OpenAIGatewayService) refreshOpenAICookieWSTickets(ctx context.Context) {
	if s == nil || s.accountRepo == nil || !s.openAICodexTicketEnabledContext(ctx) || ctx.Err() != nil || !strings.EqualFold(strings.TrimSpace(s.openAICodexTicketConfig().Mode), openAICookieWSMode) {
		return
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		logger.L().Info("openai_cookie_ws recovery account scan failed", zap.String("stage", "account"), zap.String("code", "cookie_ws_account_list_unavailable"))
		return
	}
	// Bounded parallelism prevents one slow proxy from delaying every account.
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range accounts {
		account := accounts[i]
		if !s.openAICookieWSAccountEnabled(&account) || openAICookieWSAccountSkipReason(&account, time.Now()) != "" {
			continue
		}
		account.Extra = maps.Clone(account.Extra)
		account.Credentials = maps.Clone(account.Credentials)
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(account Account) {
			defer wg.Done()
			defer func() { <-sem }()
			for slot := 0; slot < openAICookieWSSlotCount; slot++ {
				s.refreshOpenAICookieWSSlot(ctx, &account, slot)
			}
			s.maintainOpenAICookieWSMinimum(ctx, account.ID)
		}(account)
	}
	wg.Wait()
}

func (s *OpenAIGatewayService) refreshOpenAICookieWSAccount(ctx context.Context, account *Account) {
	s.refreshOpenAICookieWSSlot(ctx, account, 0)
}

func (s *OpenAIGatewayService) refreshOpenAICookieWSSlot(ctx context.Context, account *Account, slot int) {
	if slot < 0 || slot >= openAICookieWSSlotCount || !s.openAICookieWSAccountEnabled(account) || ctx.Err() != nil {
		return
	}
	key := openAICookieWSKeySlot(account.ID, openAICodexTicketDefaultModel, slot)
	_, _, _ = s.openaiCookieWSFlight.Do(key, func() (any, error) {
		latest, err := s.latestOpenAICookieWSAccount(ctx, account.ID)
		if err != nil {
			s.failOpenAICookieWSRecovery(account.ID, slot, "refresh", "waiting", cookieWSRecoveryOperationError("account", err, 0, nil), nil)
			return nil, nil
		}
		account = latest
		now := time.Now()
		previous := s.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, slot)
		if !previous.needsRefresh(now) {
			s.phaseOpenAICookieWSRecovery(account.ID, slot, "refresh", "idle", nil)
			return nil, nil
		}
		if s.openAICookieWSSlotRetryWaiting(account.ID, slot, now) || openAICookieWSAccountSkipReason(account, now) != "" {
			return nil, nil
		}
		s.beginOpenAICookieWSRecovery(account.ID, slot, "refresh", "waiting")
		harvestCtx := withOpenAICodexTicketHarvest(ctx)
		token, _, err := s.GetAccessToken(harvestCtx, account)
		if err != nil || token == "" {
			s.deferOpenAICookieWSRecovery(account.ID, slot, "token_unavailable", cookieWSRecoveryFailure("authentication", "cookie_ws_token_unavailable", "Cookie recovery access token is unavailable", 0, errOpenAICookieWSAccountUnavailable))
			return nil, nil
		}
		timeout := time.Duration(s.openAICodexTicketConfig().HarvestAttemptTimeoutSeconds) * time.Second
		var candidate *openAICookieWSTicket
		if previous.valid(now) && !previous.processVerified && now.Before(previous.CapturedAt.Add(openAICookieWSRefreshAge)) {
			s.phaseOpenAICookieWSRecovery(account.ID, slot, "refresh", "restoring", nil)
			copy := *previous
			candidate = &copy
		} else {
			proxy := s.openAICodexTicketHarvestProxyURLContext(ctx)
			if proxy == "" || s.httpUpstream == nil {
				s.deferOpenAICookieWSRecovery(account.ID, slot, "harvest_proxy_unavailable", cookieWSRecoveryFailure("configuration", "cookie_ws_harvest_proxy_unavailable", "Cookie recovery HTTP harvester or proxy configuration is unavailable", 0, nil))
				return nil, nil
			}
			identity := newOpenAICookieWSIdentity()
			if previous != nil && previous.Identity.valid() {
				identity = previous.Identity
			}
			s.phaseOpenAICookieWSRecovery(account.ID, slot, "refresh", "harvesting", nil)
			probeCtx, cancel := context.WithTimeout(harvestCtx, timeout)
			result, probeErr := s.doOpenAICookieWSHTTPProbe(probeCtx, account, token, proxy, identity)
			cancel()
			if probeErr != nil {
				status := 0
				var headers http.Header
				if result != nil {
					status, headers = result.status, result.headers
				}
				s.deferOpenAICookieWSRecovery(account.ID, slot, "http_probe_failed", cookieWSRecoveryOperationError("http_request", probeErr, status, headers))
				return nil, nil
			}
			cookies := openAICodexTicketCookieHeader(result.headers)
			if result.status != http.StatusOK || cookies == "" || !openAICookieWSProbePassed(result.body) {
				s.applyOpenAICodexTicketHarvestProbeOutcome(harvestCtx, account, openAICodexTicketDefaultModel, result)
				reason := "http_probe_not_true"
				if result.status != http.StatusOK {
					reason = "http_probe_status"
				} else if cookies == "" {
					reason = "http_probe_missing_cookie"
				}
				observation := observeOpenAICookieWSHTTPProbe(result.body)
				failure := cookieWSRecoveryObservationError("http_validation", observation, result.status)
				if result.status != http.StatusOK {
					failure = cookieWSRecoveryStatusFailure("http_request", result.status)
				} else if cookies == "" {
					failure = cookieWSRecoveryFailure("http_validation", "cookie_ws_http_cookie_missing", "Cookie recovery HTTP response did not provide a usable Cookie", result.status, nil)
				} else if len(result.body) > openAICodexTicketProbeBodyLimit {
					failure = cookieWSRecoveryFailure("http_validation", "cookie_ws_http_validation_too_large", "Cookie recovery HTTP validation response exceeded its size limit", result.status, nil)
				}
				failure.retryAt = parseRetryAfterResetTime(result.headers, time.Now())
				logger.L().Info("openai_cookie_ws HTTP probe rejected", zap.Int64("account_id", account.ID), zap.Int("slot", slot),
					zap.Int("http", result.status), zap.Int("cookie_count", openAICodexTicketCookieCount(cookies)),
					zap.Bool("completed", observation.completed), zap.Bool("model_match", observation.modelMatch), zap.String("answer_class", observation.answerClass))
				s.deferOpenAICookieWSRecovery(account.ID, slot, reason, failure)
				return nil, nil
			}
			captured := result.capturedAt
			candidate = &openAICookieWSTicket{AccountID: account.ID, Slot: slot, Model: openAICodexTicketDefaultModel,
				Generation: uuid.NewString(), Cookies: cookies, Identity: identity, HTTPVerified: true,
				CapturedAt: captured, RefreshAt: captured.Add(openAICookieWSRefreshAge), ExpiresAt: captured.Add(openAICookieWSLifetime)}
		}
		s.phaseOpenAICookieWSRecovery(account.ID, slot, "refresh", "validating", nil)
		verifyCtx, cancel := context.WithTimeout(harvestCtx, 3*timeout)
		lease, verifyErr := s.validateOpenAICookieWSCandidate(verifyCtx, account, token, candidate)
		cancel()
		if verifyErr != nil {
			if previous != nil && !previous.processVerified && candidate.Generation == previous.Generation {
				// A restored cookie which no longer passes direct WS validation
				// must be re-harvested on the next retry, not retried until 50m.
				invalid := *previous
				invalid.WSVerified = false
				s.openaiCookieWSTickets.Store(key, &invalid)
			}
			s.deferOpenAICookieWSRecovery(account.ID, slot, "ws_probe_failed", cookieWSRecoveryOperationError("ws_validation", verifyErr, 0, nil))
			return nil, nil
		}
		defer lease.Release()
		candidate.WSVerified, candidate.processVerified = true, true
		if !candidate.ready(time.Now()) {
			lease.MarkBroken()
			s.failOpenAICookieWSRecovery(account.ID, slot, "refresh", "waiting", cookieWSRecoveryOperationError("ws_validation", errOpenAIWSCookieExpired, 0, nil), nil)
			return nil, nil
		}
		if s.accountRepo != nil {
			persistCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err = s.accountRepo.UpdateExtra(persistCtx, account.ID, map[string]any{openAICookieWSExtraKeySlot(candidate.Model, slot): candidate})
			cancel()
			if err != nil {
				lease.MarkBroken()
				s.deferOpenAICookieWSRecovery(account.ID, slot, "persist_failed", cookieWSRecoveryFailure("persistence", "cookie_ws_persist_failed", "Verified Cookie could not be saved", 0, nil))
				return nil, nil
			}
		}
		s.openaiCookieWSTickets.Store(key, candidate)
		s.openaiCookieWSRetry.Delete(key)
		s.succeedOpenAICookieWSRecovery(account.ID, slot, "refresh")
		s.getOpenAIWSConnPool().RotateCookieSlot(account.ID, slot, candidate.Generation)
		lease.MarkBroken() // Candidate probe never consumes a business slot.
		logger.L().Info("openai_cookie_ws ready", zap.Int64("account_id", account.ID), zap.String("model", candidate.Model),
			zap.Int("slot", slot), zap.String("generation", candidate.Generation), zap.Time("refresh_at", candidate.RefreshAt), zap.Time("expires_at", candidate.ExpiresAt))
		return nil, nil
	})
}

func openAICookieWSStatus(account *Account, model string, now time.Time) OpenAICodexTicketStatus {
	// Without this process's gateway there is no evidence of a ready socket.
	return (*OpenAIGatewayService)(nil).openAICookieWSRuntimeStatus(account, model, now)
}
