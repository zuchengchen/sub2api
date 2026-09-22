package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	openAICodexTicketExtraKeyPrefix  = "codex_turn_ticket:"
	openAICodexAstraMinVersion       = "0.153.4"
	openAICodexTicketStatePrefix     = "gAAAAA"
	openAICodexTicketDefaultModel    = "gpt-6-astra"
	openAICodexTicketDefaultSolModel = "gpt-5.6-sol"
)

type openAICodexTicketHarvestContextKey struct{}

func withOpenAICodexTicketHarvest(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAICodexTicketHarvestContextKey{}, true)
}

func isOpenAICodexTicketHarvest(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(openAICodexTicketHarvestContextKey{}).(bool)
	return value
}

// ErrOpenAICodexTicketUnavailable 表示该号该模型没有可用的 292 门票。
// 仅当 fail_closed 为 true 时才会拒绝业务请求；默认没票也继续调用。
var ErrOpenAICodexTicketUnavailable = errors.New("codex turn-state ticket unavailable")

type openAICodexTicket struct {
	AccountID  int64     `json:"account_id"`
	Model      string    `json:"model"`
	State      string    `json:"state"`
	Length     int       `json:"length"`
	Cookies    string    `json:"cookies,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Attempts   int       `json:"attempts"`
}

func openAICodexTicketKey(accountID int64, model string) string {
	return fmt.Sprintf("%d\x00%s", accountID, strings.TrimSpace(model))
}

func openAICodexTicketExtraKey(model string) string {
	return openAICodexTicketExtraKeyPrefix + strings.TrimSpace(model)
}

func normalizeOpenAICodexTicketModel(model string) string {
	return strings.TrimSpace(model)
}

func extractOpenAICodexTicketModel(body []byte) string {
	return normalizeOpenAICodexTicketModel(gjson.GetBytes(body, "model").String())
}

func (s *OpenAIGatewayService) openAICodexTicketConfig() config.OpenAICodexTicketConfig {
	cfg := config.OpenAICodexTicketConfig{}
	if s != nil && s.cfg != nil {
		cfg = s.cfg.Gateway.OpenAICodexTicket
	}
	if cfg.TargetLength <= 0 {
		cfg.TargetLength = 292
	}
	if cfg.TTLSeconds <= 0 {
		cfg.TTLSeconds = 180
	}
	if cfg.RefreshBeforeSeconds < 0 {
		cfg.RefreshBeforeSeconds = 0
	}
	if cfg.HarvestProbeIntervalSeconds <= 0 {
		cfg.HarvestProbeIntervalSeconds = 6
	}
	if cfg.HarvestAttemptTimeoutSeconds <= 0 {
		cfg.HarvestAttemptTimeoutSeconds = 25
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	return cfg
}

func (s *OpenAIGatewayService) openAICodexTicketGatedModel(model string) bool {
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketEnabled() {
		return false
	}
	for _, item := range s.openAICodexTicketConfig().Models {
		if normalizeOpenAICodexTicketModel(item) == model {
			return true
		}
	}
	return false
}

// OpenAICodexTicketStatus 是给管理端看的门票摘要，不含 state blob。
type OpenAICodexTicketStatus struct {
	Model            string     `json:"model"`
	Length           int        `json:"length,omitempty"`
	Ready            bool       `json:"ready"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	Blocked          bool       `json:"blocked"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

func OpenAICodexTicketStatuses(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) []OpenAICodexTicketStatus {
	if !cfg.Enabled || !isOpenAICodexTicketAccount(account) {
		return nil
	}
	models, targetLen := cfg.Models, cfg.TargetLength
	if len(models) == 0 {
		models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	if targetLen <= 0 {
		targetLen = 292
	}
	out := make([]OpenAICodexTicketStatus, 0, len(models))
	for _, model := range models {
		model = normalizeOpenAICodexTicketModel(model)
		if model == "" {
			continue
		}
		status := OpenAICodexTicketStatus{Model: model}
		ticket := parseOpenAICodexTicketFromAny(0, model, nil)
		if account != nil && account.Extra != nil {
			ticket = parseOpenAICodexTicketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)])
		}
		if cfg.TTLSeconds > 0 {
			ticket.clampExpiry(time.Duration(cfg.TTLSeconds) * time.Second)
		}
		if ticket.valid(now, targetLen) {
			status.Ready = true
			status.Length = ticket.Length
			remaining := int64(ticket.ExpiresAt.Sub(now) / time.Second)
			if remaining < 0 {
				remaining = 0
			}
			status.RemainingSeconds = remaining
			exp := ticket.ExpiresAt
			status.ExpiresAt = &exp
		}
		status.Blocked = cfg.FailClosed && !status.Ready
		out = append(out, status)
	}
	return out
}

func (s *OpenAIGatewayService) openAICodexTicketEnabled() bool {
	return s.openAICodexTicketEnabledContext(context.Background())
}

func (s *OpenAIGatewayService) openAICodexTicketEnabledContext(ctx context.Context) bool {
	if s == nil {
		return false
	}
	fallback := s.cfg != nil && s.cfg.Gateway.OpenAICodexTicket.Enabled
	if s.settingService != nil {
		return s.settingService.GetOpenAICodexTicketEnabled(ctx, fallback)
	}
	return fallback
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestProxyURL() string {
	return s.openAICodexTicketHarvestProxyURLContext(context.Background())
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestProxyURLContext(ctx context.Context) string {
	if s.settingService != nil {
		if proxy := s.settingService.GetOpenAICodexTicketHarvestProxyURL(ctx); proxy != "" {
			return proxy
		}
	}
	return strings.TrimSpace(s.openAICodexTicketConfig().HarvestProxyURL)
}

func (t *openAICodexTicket) valid(now time.Time, targetLen int) bool {
	if t == nil {
		return false
	}
	state := strings.TrimSpace(t.State)
	if len(state) != targetLen || t.Length != targetLen || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return false
	}
	if t.ExpiresAt.IsZero() || !now.Before(t.ExpiresAt) {
		return false
	}
	// 没有打票时的 Cookie，292 头单独出站仍会被上游改到 Luna。
	if strings.TrimSpace(t.Cookies) == "" {
		return false
	}
	return true
}

func (t *openAICodexTicket) needsRefresh(now time.Time, refreshBefore time.Duration) bool {
	if t == nil || t.ExpiresAt.IsZero() {
		return true
	}
	return !t.ExpiresAt.After(now.Add(refreshBefore))
}

// clampExpiry 把历史门票的过期时间收到当前 TTL 内。缩短 ttl 后，库里仍写着 1 小时过期的票会按 captured_at+ttl 重新到期。
func openAICodexTicketCookieHeader(h http.Header) string {
	if h == nil {
		return ""
	}
	seen := map[string]struct{}{}
	pairs := make([]string, 0, len(h.Values("Set-Cookie")))
	for _, raw := range h.Values("Set-Cookie") {
		part := strings.TrimSpace(raw)
		if i := strings.Index(part, ";"); i >= 0 {
			part = strings.TrimSpace(part[:i])
		}
		name, value, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		pairs = append(pairs, name+"="+value)
	}
	return strings.Join(pairs, "; ")
}

func openAICodexTicketCookieCount(cookieHeader string) int {
	cookieHeader = strings.TrimSpace(cookieHeader)
	if cookieHeader == "" {
		return 0
	}
	return len(strings.Split(cookieHeader, ";"))
}

func applyOpenAICodexTicketCookies(h http.Header, ticketCookies string) {
	ticketCookies = strings.TrimSpace(ticketCookies)
	if h == nil || ticketCookies == "" {
		return
	}
	merged := map[string]string{}
	order := make([]string, 0, 8)
	add := func(raw string) {
		for _, part := range strings.Split(raw, ";") {
			part = strings.TrimSpace(part)
			name, value, ok := strings.Cut(part, "=")
			name = strings.TrimSpace(name)
			if !ok || name == "" {
				continue
			}
			if _, seen := merged[name]; !seen {
				order = append(order, name)
			}
			merged[name] = value
		}
	}
	add(h.Get("Cookie"))
	add(ticketCookies)
	var b strings.Builder
	for i, name := range order {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(merged[name])
	}
	h.Set("Cookie", b.String())
}

func (t *openAICodexTicket) clampExpiry(ttl time.Duration) {
	if t == nil || ttl <= 0 || t.CapturedAt.IsZero() {
		return
	}
	capAt := t.CapturedAt.Add(ttl)
	if t.ExpiresAt.IsZero() || capAt.Before(t.ExpiresAt) {
		t.ExpiresAt = capAt
	}
}

func (s *OpenAIGatewayService) lookupOpenAICodexTicket(account *Account, model string) *openAICodexTicket {
	if s == nil || account == nil || account.ID <= 0 {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" {
		return nil
	}
	key := openAICodexTicketKey(account.ID, model)
	targetLen := 292
	if s != nil {
		targetLen = s.openAICodexTicketConfig().TargetLength
	}
	now := time.Now()
	ttl := time.Duration(s.openAICodexTicketConfig().TTLSeconds) * time.Second
	var mem *openAICodexTicket
	if raw, ok := s.openaiCodexTickets.Load(key); ok {
		mem, _ = raw.(*openAICodexTicket)
		mem.clampExpiry(ttl)
	}
	var extra *openAICodexTicket
	if account.Extra != nil {
		extra = parseOpenAICodexTicketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)])
		extra.clampExpiry(ttl)
	}
	if extra.valid(now, targetLen) && (mem == nil || extra.CapturedAt.After(mem.CapturedAt)) {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem.valid(now, targetLen) {
		return mem
	}
	if extra != nil {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem != nil {
		s.openaiCodexTickets.Delete(key)
	}
	return nil
}

func parseOpenAICodexTicketFromAny(accountID int64, model string, raw any) *openAICodexTicket {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var ticket openAICodexTicket
	if err := json.Unmarshal(b, &ticket); err != nil {
		return nil
	}
	ticket.AccountID = accountID
	if strings.TrimSpace(model) != "" {
		ticket.Model = model
	}
	ticket.State = strings.TrimSpace(ticket.State)
	if ticket.Length == 0 {
		ticket.Length = len(ticket.State)
	}
	if ticket.State == "" {
		return nil
	}
	return &ticket
}

func (s *OpenAIGatewayService) storeOpenAICodexTicket(ctx context.Context, account *Account, ticket *openAICodexTicket) {
	if s == nil || account == nil || ticket == nil || account.ID <= 0 {
		return
	}
	model := normalizeOpenAICodexTicketModel(ticket.Model)
	ticket.Model = model
	ticket.AccountID = account.ID
	s.openaiCodexTickets.Store(openAICodexTicketKey(account.ID, model), ticket)
	if s.accountRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		openAICodexTicketExtraKey(model): ticket,
	}); err != nil {
		logger.L().Warn("openai_codex_ticket persist failed",
			zap.Int64("account_id", account.ID),
			zap.String("model", model),
			zap.Error(err),
		)
	}
}

// applyOpenAICodexTicket 在出站请求上覆盖 x-codex-turn-state。
// 请求路径只注入已捕获的有效门票，不现场打票；无票则返回
// ErrOpenAICodexTicketUnavailable。打票由后台 harvester 完成。
func (s *OpenAIGatewayService) applyOpenAICodexTicket(ctx context.Context, account *Account, model string, h http.Header) error {
	if s == nil || h == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabledContext(ctx) {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	ticket := s.lookupOpenAICodexTicket(account, model)
	if ticket.valid(time.Now(), cfg.TargetLength) {
		h.Set(openAICodexTurnStateHeader, ticket.State)
		applyOpenAICodexTicketCookies(h, ticket.Cookies)
		return nil
	}
	if !cfg.FailClosed {
		return nil
	}
	return ErrOpenAICodexTicketUnavailable
}

// openAICodexTicketOutboundModel 预测本请求真正出站的模型名，也就是
// applyOpenAICodexTicket 注入时读到的 body.model。
//
// 调度门控与注入必须按同一个模型名判定门票。普通请求下二者同源：Forward 的
// upstreamModel 与本函数都走 resolveOpenAIAccountUpstreamModelForRequest，且
// Forward 会把 body.model 改写成该值后才注入。但 /responses/compact 例外——
// Forward 会把出站模型进一步改写为 compact 映射或 gateway.openai_compact_model
// （默认非空），此时若门控仍按客户端原始模型判定，就会把「实际出站是非门控
// 模型、根本不需要票」的 compact 请求整片误拦成不可调度。
func (s *OpenAIGatewayService) openAICodexTicketOutboundModel(account *Account, requestedModel string, requireCompact bool) string {
	model := strings.TrimSpace(requestedModel)
	if account == nil || model == "" {
		return model
	}
	if !account.IsOpenAI() {
		return canonicalOpenAIAccountSchedulingModel(account, model)
	}
	_, upstreamModel := resolveOpenAIForwardMappedModels(account, model, requireCompact)
	if requireCompact {
		// 与 Forward 同序：compact 兜底模型优先于普通/compact 映射结果。
		if compactModel := strings.TrimSpace(s.resolveOpenAICompactFallbackModel(account, model)); compactModel != "" {
			upstreamModel = compactModel
		}
	}
	if upstreamModel = strings.TrimSpace(upstreamModel); upstreamModel != "" {
		return upstreamModel
	}
	return model
}

// outboundModel 必须是真正会发给上游的模型名（openAICodexTicketOutboundModel），
// 不是客户端原始模型：注入侧读的是出站 body.model，两侧口径必须一致。
func (s *OpenAIGatewayService) openAICodexTicketBlocksAccount(account *Account, outboundModel string) bool {
	if s == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabled() {
		return false
	}
	cfg := s.openAICodexTicketConfig()
	if !cfg.FailClosed {
		return false
	}
	model := normalizeOpenAICodexTicketModel(outboundModel)
	if !s.openAICodexTicketGatedModel(model) {
		return false
	}
	ticket := s.lookupOpenAICodexTicket(account, model)
	return !ticket.valid(time.Now(), cfg.TargetLength)
}

const openAICodexTicketHarvestErrorBodyLimit = 8 << 10

type openAICodexTicketProbeResult struct {
	state   string
	status  int
	headers http.Header
	body    []byte
}

func (s *OpenAIGatewayService) fireOpenAICodexTicketProbe(ctx context.Context, account *Account, token, model, proxyURL string, attemptTimeout time.Duration) (state string, status int, err error) {
	result, err := s.doOpenAICodexTicketProbe(ctx, account, token, model, proxyURL, attemptTimeout)
	if err != nil {
		return "", 0, err
	}
	return result.state, result.status, nil
}

func (s *OpenAIGatewayService) doOpenAICodexTicketProbe(ctx context.Context, account *Account, token, model, proxyURL string, attemptTimeout time.Duration) (*openAICodexTicketProbeResult, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	body := []byte(`{"model":` + jsonString(model) + `,"store":false,"stream":true,"instructions":"Reply with exactly: pong","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAIHarvest))
	req.Close = true
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("session_id", uuid.NewString())
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(attemptCtx, s.accountRepo, req.Header, account); err != nil {
		return nil, err
	}
	applyOpenAICodexTicketHarvestIdentity(req.Header, model)

	// Synthetic probes must use the dedicated no-reuse transport even when the
	// production account is bound to a plugin. This also avoids reading pluginManager
	// while handlers are still wiring it during gateway construction.
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("nil upstream response")
	}
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	result := &openAICodexTicketProbeResult{
		state:   extractOpenAICodexTurnState(resp.Header),
		status:  resp.StatusCode,
		headers: resp.Header.Clone(),
	}
	// 200 只取 turn-state 头，不要把 SSE 正文读下来。401/429 需要一小段 body
	// 判断 usage_limit_reached / 鉴权失败。
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests) && resp.Body != nil {
		result.body, _ = io.ReadAll(io.LimitReader(resp.Body, openAICodexTicketHarvestErrorBodyLimit))
	}
	return result, nil
}

func jsonString(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(b)
}

func applyOpenAICodexTicketHarvestIdentity(h http.Header, model string) {
	ensureCodexIdentityHeaders(h)
	enforceCodexIdentityHeaders(h)
	if !needsOpenAICodexAstraVersion(model) {
		return
	}
	version := strings.TrimSpace(h.Get("version"))
	if version != "" && CompareVersions(version, openAICodexAstraMinVersion) >= 0 {
		return
	}
	use := strings.TrimSpace(CodexCanonicalClientVersion())
	if use == "" || CompareVersions(use, openAICodexAstraMinVersion) < 0 {
		use = openAICodexAstraMinVersion
	}
	h.Set("version", use)
	h.Set("user-agent", buildCodexCLIUserAgent(use))
	h.Set("originator", openai.CodexDefaultOriginator)
}

func needsOpenAICodexAstraVersion(model string) bool {
	m := strings.ToLower(normalizeOpenAICodexTicketModel(model))
	return strings.Contains(m, "gpt-6") || strings.Contains(m, "astra")
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
	logger.L().Info("openai_codex_ticket harvester started",
		zap.Int("ttl_seconds", s.openAICodexTicketConfig().TTLSeconds),
		zap.Int("target_length", s.openAICodexTicketConfig().TargetLength),
		zap.Strings("models", s.openAICodexTicketConfig().Models),
	)
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
			s.refreshOpenAICodexTickets(ctx)
			timer.Reset(time.Duration(s.openAICodexTicketConfig().HarvestProbeIntervalSeconds) * time.Second)
		}
	}
}

// refreshOpenAICodexTickets probes each account/model with a missing or soon-to-expire
// ticket once. The loop waits for all probes, then waits the configured interval
// before starting the next cycle.
func (s *OpenAIGatewayService) refreshOpenAICodexTickets(ctx context.Context) {
	if s == nil || s.accountRepo == nil || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		logger.L().Warn("openai_codex_ticket list accounts failed", zap.Error(err))
		return
	}
	cfg := s.openAICodexTicketConfig()
	now := time.Now()
	refreshBefore := time.Duration(cfg.RefreshBeforeSeconds) * time.Second
	var wg sync.WaitGroup
	probed := 0
	skipped := map[string]int{}
	for i := range accounts {
		account := accounts[i]
		if reason := openAICodexTicketHarvestSkipReason(&account, now); reason != "" {
			skipped[reason]++
			continue
		}
		for _, model := range cfg.Models {
			model := normalizeOpenAICodexTicketModel(model)
			if model == "" {
				continue
			}
			// 已有一张有效且未临近过期的 292 票 → 本周期不打。
			if t := s.lookupOpenAICodexTicket(&account, model); t.valid(now, cfg.TargetLength) && !t.needsRefresh(now, refreshBefore) {
				continue
			}
			acc := account
			// Token/header helpers may update account metadata; each model owns its maps.
			acc.Extra = maps.Clone(account.Extra)
			acc.Credentials = maps.Clone(account.Credentials)
			probed++
			wg.Add(1)
			go func(acc Account, model string) {
				defer wg.Done()
				s.probeOnceOpenAICodexTicket(ctx, &acc, model)
			}(acc, model)
		}
	}
	wg.Wait()
	if probed > 0 || len(skipped) > 0 {
		logger.L().Info("openai_codex_ticket probe cycle",
			zap.Int("probed", probed),
			zap.Any("skipped", skipped),
		)
	}
}

// probeOnceOpenAICodexTicket 走打票代理打一发。命中合格 292（HTTP 200、长度==target、
// gAAAAA 前缀）就落库；312 当 miss。同一 key 并发去重，避免上一发还没回来又叠一发。
func (s *OpenAIGatewayService) probeOnceOpenAICodexTicket(ctx context.Context, account *Account, model string) {
	if s == nil || !isOpenAICodexTicketAccount(account) || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	if reason := openAICodexTicketHarvestSkipReason(account, time.Now()); reason != "" {
		logger.L().Info("openai_codex_ticket probe skip",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.String("reason", reason))
		return
	}
	cfg := s.openAICodexTicketConfig()
	proxyURL := s.openAICodexTicketHarvestProxyURLContext(ctx)
	if proxyURL == "" || s.httpUpstream == nil || ctx.Err() != nil {
		return
	}
	key := openAICodexTicketKey(account.ID, model)
	_, _, _ = s.openaiCodexTicketFlight.Do(key, func() (any, error) {
		harvestCtx := withOpenAICodexTicketHarvest(ctx)
		token, _, err := s.GetAccessToken(harvestCtx, account)
		if err != nil || strings.TrimSpace(token) == "" {
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "token"), zap.Error(err))
			return nil, nil
		}
		result, perr := s.doOpenAICodexTicketProbe(harvestCtx, account, token, model, proxyURL, time.Duration(cfg.HarvestAttemptTimeoutSeconds)*time.Second)
		if perr != nil {
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "error"), zap.Error(perr))
			return nil, nil
		}
		cookies := openAICodexTicketCookieHeader(result.headers)
		if result.status != http.StatusOK || result.state == "" || len(result.state) != cfg.TargetLength || !strings.HasPrefix(result.state, openAICodexTicketStatePrefix) || cookies == "" {
			s.applyOpenAICodexTicketHarvestProbeOutcome(harvestCtx, account, model, result)
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.Int("http", result.status), zap.Int("len", len(result.state)),
				zap.Int("cookie_count", openAICodexTicketCookieCount(cookies)))
			return nil, nil
		}
		now := time.Now()
		ticket := &openAICodexTicket{
			AccountID:  account.ID,
			Model:      model,
			State:      result.state,
			Length:     len(result.state),
			Cookies:    cookies,
			CapturedAt: now,
			ExpiresAt:  now.Add(time.Duration(cfg.TTLSeconds) * time.Second),
			Attempts:   1,
		}
		s.storeOpenAICodexTicket(ctx, account, ticket)
		logger.L().Info("openai_codex_ticket harvested",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.Int("length", ticket.Length), zap.Int("cookie_count", openAICodexTicketCookieCount(cookies)),
			zap.String("mode", "continuous"))
		return nil, nil
	})
}

// openAICodexTicketHarvestSkipReason 只排除「打了也没用」的号：额度 100%、掉认证、
// 过期、管理员手动停调。429 冷却 / 过载 / near-limit 可能是降智，继续打票。
func openAICodexTicketHarvestSkipReason(account *Account, now time.Time) string {
	if account == nil || !isOpenAICodexTicketAccount(account) {
		return "not_ticket_account"
	}
	if account.Status != StatusActive {
		return "not_active"
	}
	if !account.Schedulable {
		return "manual_unschedulable"
	}
	if account.AutoPauseOnExpired && account.ExpiresAt != nil && !now.Before(*account.ExpiresAt) {
		return "expired"
	}
	if openAICodexTicketHarvestAuthBlocked(account, now) {
		return "auth"
	}
	if window := openAICodexTicketHarvestQuotaExhaustedWindow(account, now); window != "" {
		return "quota_" + window
	}
	return ""
}

func openAICodexTicketHarvestAuthBlocked(account *Account, now time.Time) bool {
	if account == nil {
		return false
	}
	if account.Status == StatusError {
		return true
	}
	if account.TempUnschedulableUntil == nil || !now.Before(*account.TempUnschedulableUntil) {
		return false
	}
	return openAICodexTicketHarvestAuthReason(account.TempUnschedulableReason)
}

func openAICodexTicketHarvestAuthReason(reason string) bool {
	r := strings.ToLower(strings.TrimSpace(reason))
	if r == "" {
		return false
	}
	for _, token := range []string{
		"401",
		"oauth 401",
		"authentication failed",
		"unauthorized",
		"token revoked",
		"token_invalidated",
		"refresh_token",
		"token refresh",
	} {
		if strings.Contains(r, token) {
			return true
		}
	}
	return false
}

func openAICodexTicketHarvestQuotaExhaustedWindow(account *Account, now time.Time) string {
	if account == nil {
		return ""
	}
	for _, window := range []string{"5h", "7d"} {
		util, ok := resolveOpenAIQuotaUtilization(account.Extra, window, now)
		if ok && util >= 1 {
			return window
		}
	}
	return ""
}

func (s *OpenAIGatewayService) applyOpenAICodexTicketHarvestProbeOutcome(ctx context.Context, account *Account, model string, result *openAICodexTicketProbeResult) {
	if s == nil || account == nil || result == nil {
		return
	}
	switch result.status {
	case http.StatusUnauthorized:
		// Ticket harvesting is auxiliary. A probe 401 must not pause or disable
		// an account used by normal traffic; the next request owns auth policy.
	case http.StatusTooManyRequests:
		// 满额只写 usage 快照，不走 handle429，以免把降智 429 写成 RateLimitResetAt。
		s.persistOpenAICodexTicketHarvestQuota(ctx, account, result.headers, result.body)
	}
}

func (s *OpenAIGatewayService) persistOpenAICodexTicketHarvestQuota(ctx context.Context, account *Account, headers http.Header, body []byte) {
	if s == nil || account == nil || account.IsShadow() || s.accountRepo == nil {
		return
	}
	updates := openAICodexTicketHarvestQuotaUpdatesFromProbe(headers, body, time.Now())
	if len(updates) == 0 {
		return
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any, len(updates))
	}
	maps.Copy(account.Extra, updates)
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		logger.L().Warn("openai_codex_ticket harvest quota persist failed",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
	}
}

func openAICodexTicketHarvestQuotaUpdatesFromProbe(headers http.Header, body []byte, now time.Time) map[string]any {
	updates := map[string]any{}
	if snapshot := ParseCodexRateLimitHeaders(headers); snapshot != nil {
		maps.Copy(updates, buildCodexUsageExtraUpdates(snapshot, now))
	}
	if !openAICodexTicketHarvestBodyUsageLimitReached(body) {
		return updates
	}
	if openAICodexTicketHarvestUpdatesQuotaExhausted(updates) {
		return updates
	}
	window := "7d"
	if resetAt := parseOpenAIRateLimitResetTime(body); resetAt != nil {
		until := time.Unix(*resetAt, 0).Sub(now)
		if until > 0 && until <= 6*time.Hour {
			window = "5h"
		}
		updates["codex_"+window+"_reset_at"] = time.Unix(*resetAt, 0).UTC().Format(time.RFC3339)
	}
	updates["codex_"+window+"_used_percent"] = 100.0
	if _, ok := updates["codex_usage_updated_at"]; !ok {
		updates["codex_usage_updated_at"] = now.UTC().Format(time.RFC3339)
	}
	return updates
}

func openAICodexTicketHarvestBodyUsageLimitReached(body []byte) bool {
	errType := gjson.GetBytes(body, "error.type").String()
	return errType == "usage_limit_reached"
}

func openAICodexTicketHarvestUpdatesQuotaExhausted(updates map[string]any) bool {
	for _, window := range []string{"5h", "7d"} {
		if value, ok := resolveAccountExtraNumber(updates, "codex_"+window+"_used_percent"); ok && value >= 100 {
			return true
		}
	}
	return false
}

// IsOpenAICodexTicketExtraKey identifies server-managed ticket material.
func IsOpenAICodexTicketExtraKey(key string) bool {
	return strings.HasPrefix(key, openAICodexTicketExtraKeyPrefix)
}

// MergeOpenAICodexTicketExtra preserves only persisted tickets, never summaries or
// blobs supplied by an account edit. The repository repeats this under the row
// lock so a concurrent harvest cannot be overwritten by a stale admin snapshot.
func MergeOpenAICodexTicketExtra(extra, current map[string]any) map[string]any {
	result := maps.Clone(extra)
	for key := range result {
		if IsOpenAICodexTicketExtraKey(key) {
			delete(result, key)
		}
	}
	for key, value := range current {
		if IsOpenAICodexTicketExtraKey(key) {
			if result == nil {
				result = make(map[string]any)
			}
			result[key] = value
		}
	}
	return result
}

// ValidateOpenAICodexTicketHarvestProxyURL validates only syntax, without making
// a network request or including credentials in validation errors.
func ValidateOpenAICodexTicketHarvestProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("harvest proxy must be an HTTP(S) or SOCKS5(h) URL with a host and no path, query or fragment")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("harvest proxy scheme must be http, https, socks5 or socks5h")
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("harvest proxy port must be between 1 and 65535")
		}
	}
	return nil
}

// MaskProxyURL never returns a stored proxy password, even for invalid legacy data.
func MaskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || ValidateOpenAICodexTicketHarvestProxyURL(raw) != nil {
		return ""
	}
	parsed, _ := url.Parse(raw)
	if parsed.User != nil {
		if _, ok := parsed.User.Password(); ok {
			parsed.User = url.UserPassword(parsed.User.Username(), "***")
		}
	}
	return parsed.String()
}

// IsMaskedProxyURL recognizes the exact password placeholder emitted by the API.
func IsMaskedProxyURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return false
	}
	password, ok := parsed.User.Password()
	return ok && password == "***"
}

// Credential shadows do not own tickets. Keep their existing forwarding policy
// instead of imposing a gate for a key the harvester never populates.
func isOpenAICodexTicketAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike() && !account.IsShadow()
}

// IsOpenAICodexTicketPrivateExtraKey also covers the retired account-level proxy
// override, whose credentials may remain in older account records.
func IsOpenAICodexTicketPrivateExtraKey(key string) bool {
	return IsOpenAICodexTicketExtraKey(key) || key == "codex_harvest_proxy_url"
}

// RedactOpenAICodexTicketExtra strips ephemeral ticket material from exports
// without changing the source account or unrelated backup fields.
func RedactOpenAICodexTicketExtra(extra map[string]any) map[string]any {
	redacted := maps.Clone(extra)
	for key := range redacted {
		if IsOpenAICodexTicketPrivateExtraKey(key) {
			delete(redacted, key)
		}
	}
	return redacted
}
