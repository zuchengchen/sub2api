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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	openAICodexTicketExtraKeyPrefix        = "codex_turn_ticket:"
	openAICodexTicketRevokedExtraKeyPrefix = "codex_turn_ticket_revoked:"
	openAICodexTicketWatchKey              = "openai_codex_ticket_watch"
	openAICodexAstraMinVersion             = "0.153.4"
	openAICodexTicketStatePrefix           = "gAAAAA"
	// 312 是上游把请求改去降级模型时回的 state 长度，不是 HTTP 状态码。
	openAICodexTicketDegradedLength = 312
	// 打票探针要读完 SSE 才能核对完成模型，超过这个上限就当没打中。
	openAICodexTicketProbeBodyLimit  = 4 << 20
	openAICodexTicketDefaultModel    = "gpt-6-astra"
	openAICodexTicketDefaultSolModel = "gpt-5.6-sol"
	// 上游把 gpt-5.6-sol 完成成 gpt-6-sol，说明这张 292 票是好的。
	openAICodexTicketUpgradedSolModel = "gpt-6-sol"
	// 新鲜票少于这个数量就继续打。多一张可以，少一张不行。
	openAICodexTicketPoolTarget = 2
	// 最新一张超过这个票龄，即使已经有两张也再补一张。
	openAICodexTicketPoolRefillAge = 90 * time.Second
	openAICodexTicketPoolLimit     = 4
)

// openAICodexSharedTicketState 是全池共用的 Astra 票，以及找下一张时的轮换进度。
type openAICodexSharedTicketState struct {
	mu      sync.Mutex
	tickets []*openAICodexTicket
	tried   map[int64]struct{}
}

// openAICodexTicketHarvestBackoff 是进程内的下次探测时间。发布后不再改写。
type openAICodexTicketHarvestBackoff struct {
	nextProbeAt time.Time
}

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
	SessionID  string    `json:"session_id,omitempty"`
	Version    string    `json:"version,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
	Originator string    `json:"originator,omitempty"`
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

func openAICodexTicketRevokedExtraKey(model string) string {
	return openAICodexTicketRevokedExtraKeyPrefix + strings.TrimSpace(model)
}

type openAICodexTicketInjectionSlotKey struct{}

type openAICodexTicketInjectionSlot struct {
	State        string
	RequestModel string
	TicketModel  string
	AccountID    int64
	SessionID    string
	Version      string
	UserAgent    string
	Originator   string
}

const openAICodexTicketIdentityKey = "openai_codex_ticket_identity"

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
	if s.openAICookieWSModeConfigured() {
		return model == openAICodexTicketDefaultModel
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
	Model             string     `json:"model"`
	Mode              string     `json:"mode,omitempty"`
	CapturedAt        *time.Time `json:"captured_at,omitempty"`
	RefreshAt         *time.Time `json:"refresh_at,omitempty"`
	CookieGroupsReady int        `json:"cookie_groups_ready,omitempty"`
	CookieGroupsTotal int        `json:"cookie_groups_total,omitempty"`
	WSPerGroup        int        `json:"ws_per_group,omitempty"`
	VerifiedWS        int        `json:"verified_ws,omitempty"`
	MinimumWS         int        `json:"minimum_ws,omitempty"`
	Length            int        `json:"length,omitempty"`
	Ready             bool       `json:"ready"`
	RemainingSeconds  int64      `json:"remaining_seconds"`
	Blocked           bool       `json:"blocked"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

func OpenAICodexTicketStatuses(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) []OpenAICodexTicketStatus {
	if !cfg.Enabled || !isOpenAICodexTicketAccount(account) {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Mode), openAICookieWSMode) {
		if openAICookieWSAccountConfigured(account, cfg) {
			return []OpenAICodexTicketStatus{openAICookieWSStatus(account, openAICodexTicketDefaultModel, now)}
		}
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
		if model == openAICodexTicketDefaultModel && openAICookieWSAccountConfigured(account, cfg) {
			out = append(out, openAICookieWSStatus(account, model, now))
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
		if ticket.usable(targetLen) && !openAICodexTicketRevokedInExtra(account, ticket) {
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
		status.Blocked = cfg.FailClosed && !ticket.usable(targetLen)
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

// usable 是可以注入的票：长度、前缀和打票 Cookie 都对。过期仍可用。
func (t *openAICodexTicket) usable(targetLen int) bool {
	if t == nil {
		return false
	}
	state := strings.TrimSpace(t.State)
	if len(state) != targetLen || t.Length != targetLen || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return false
	}
	// 没有打票时的 Cookie，292 头单独出站仍会被上游改到 Luna。
	return strings.TrimSpace(t.Cookies) != ""
}

// hasHarvestIdentity 表示票上记着打票那一跳的会话身份。
// 没有这组身份的旧票仍可注入，但会被新打到的票换掉。
func (t *openAICodexTicket) hasHarvestIdentity() bool {
	return t != nil &&
		strings.TrimSpace(t.SessionID) != "" &&
		strings.TrimSpace(t.Version) != "" &&
		strings.TrimSpace(t.UserAgent) != "" &&
		strings.TrimSpace(t.Originator) != ""
}

func (t *openAICodexTicket) valid(now time.Time, targetLen int) bool {
	if !t.usable(targetLen) {
		return false
	}
	return !t.ExpiresAt.IsZero() && now.Before(t.ExpiresAt)
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

func openAICodexTicketInjected(h http.Header, targetLen int) bool {
	if h == nil || targetLen <= 0 {
		return false
	}
	state := strings.TrimSpace(h.Get(openAICodexTurnStateHeader))
	return len(state) == targetLen && strings.HasPrefix(state, openAICodexTicketStatePrefix) && strings.TrimSpace(h.Get("Cookie")) != ""
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

// lookupBorrowedOpenAICodexTicket finds a 292-plus-cookie ticket on another
// account. Unexpired tickets win; otherwise the newest expired one is used.
func (s *OpenAIGatewayService) lookupBorrowedOpenAICodexTicket(ctx context.Context, exceptAccountID int64, model string) *openAICodexTicket {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	now := time.Now()
	var fresh, stale *openAICodexTicket
	for i := range accounts {
		if accounts[i].ID == exceptAccountID {
			continue
		}
		ticket := s.lookupOpenAICodexTicket(&accounts[i], model)
		if !ticket.usable(cfg.TargetLength) || s.openAICodexTicketStateRevoked(ticket, &accounts[i]) {
			continue
		}
		if ticket.valid(now, cfg.TargetLength) {
			if fresh == nil || ticket.ExpiresAt.After(fresh.ExpiresAt) {
				fresh = ticket
			}
			continue
		}
		if stale == nil || ticket.CapturedAt.After(stale.CapturedAt) {
			stale = ticket
		}
	}
	if fresh != nil {
		return fresh
	}
	return stale
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
	if extra.usable(targetLen) && !s.openAICodexTicketStateRevoked(extra, account) && (mem == nil || !mem.usable(targetLen) || s.openAICodexTicketStateRevoked(mem, account) || !extra.CapturedAt.Before(mem.CapturedAt)) {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem.usable(targetLen) && !s.openAICodexTicketStateRevoked(mem, account) {
		return mem
	}
	if extra != nil && !s.openAICodexTicketStateRevoked(extra, account) {
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

func (s *OpenAIGatewayService) openAICodexTicketHeld(account *Account) bool {
	if s == nil || account == nil {
		return false
	}
	if s.openAICookieWSAccountEnabled(account) {
		return s.lookupOpenAICookieWSTicket(account, openAICodexTicketDefaultModel).ready(time.Now())
	}
	if s.openAICookieWSModeConfigured() {
		return false
	}
	ticket := s.lookupOpenAICodexTicket(account, openAICodexTicketDefaultModel)
	return ticket.hasHarvestIdentity() &&
		ticket.usable(s.openAICodexTicketConfig().TargetLength) &&
		!s.openAICodexTicketStateRevoked(ticket, account) &&
		ticket.AccountID == account.ID
}

func applyOpenAICodexTicketIdentity(h http.Header, ticket *openAICodexTicket) {
	if h == nil || !ticket.hasHarvestIdentity() {
		return
	}
	h.Set("session_id", ticket.SessionID)
	h.Set("version", ticket.Version)
	h.Set("user-agent", ticket.UserAgent)
	h.Set("originator", ticket.Originator)
}

func rememberOpenAICodexTicketIdentity(c *gin.Context, slot *openAICodexTicketInjectionSlot) {
	if c == nil || slot == nil || strings.TrimSpace(slot.SessionID) == "" {
		return
	}
	c.Set(openAICodexTicketIdentityKey, &openAICodexTicket{
		SessionID:  slot.SessionID,
		Version:    slot.Version,
		UserAgent:  slot.UserAgent,
		Originator: slot.Originator,
	})
}

// restoreOpenAICodexTicketIdentity 在身份收口之后把打票会话身份写回去。
func restoreOpenAICodexTicketIdentity(c *gin.Context, h http.Header) {
	if c == nil || h == nil {
		return
	}
	raw, ok := c.Get(openAICodexTicketIdentityKey)
	ticket, _ := raw.(*openAICodexTicket)
	if !ok || !ticket.hasHarvestIdentity() {
		return
	}
	applyOpenAICodexTicketIdentity(h, ticket)
}

func (s *OpenAIGatewayService) storeOpenAICodexTicket(ctx context.Context, account *Account, ticket *openAICodexTicket) {
	if s == nil || account == nil || ticket == nil || account.ID <= 0 {
		return
	}
	if s.openAICodexTicketHeld(account) {
		return
	}
	model := normalizeOpenAICodexTicketModel(ticket.Model)
	ticket.Model = model
	ticket.AccountID = account.ID
	s.openaiCodexTickets.Store(openAICodexTicketKey(account.ID, model), ticket)
	if model == openAICodexTicketDefaultModel {
		s.rememberSharedOpenAICodexTicket(ticket)
	}
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
	if s.openAICookieWSEnabledForModel(account, model) {
		// Cookie mode is WS-only. Only the final WS header builder may inject
		// its private pool metadata; a legacy HTTP path must fail closed.
		return ErrOpenAICodexTicketUnavailable
	}
	if s.openAICookieWSModeConfigured() {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	ticket := s.sharedOpenAICodexTicketForInjection(ctx, account)
	if ticket.usable(cfg.TargetLength) && !s.openAICodexTicketStateRevoked(ticket, account) && ticket.AccountID == account.ID {
		h.Set(openAICodexTurnStateHeader, ticket.State)
		applyOpenAICodexTicketCookies(h, ticket.Cookies)
		applyOpenAICodexTicketIdentity(h, ticket)
		if slot, _ := ctx.Value(openAICodexTicketInjectionSlotKey{}).(*openAICodexTicketInjectionSlot); slot != nil {
			slot.State = ticket.State
			slot.RequestModel = model
			slot.TicketModel = ticket.Model
			slot.AccountID = ticket.AccountID
			slot.SessionID = ticket.SessionID
			slot.Version = ticket.Version
			slot.UserAgent = ticket.UserAgent
			slot.Originator = ticket.Originator
		}
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
	if s.openAICookieWSEnabledForModel(account, outboundModel) {
		return !s.lookupOpenAICookieWSTicket(account, strings.TrimSpace(outboundModel)).ready(time.Now())
	}
	if s.openAICookieWSModeConfigured() {
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
	ticket := s.sharedOpenAICodexTicketForInjection(context.Background(), account)
	return !ticket.usable(cfg.TargetLength) || s.openAICodexTicketStateRevoked(ticket, account) || ticket.AccountID != account.ID
}

const openAICodexTicketHarvestErrorBodyLimit = 8 << 10

type openAICodexTicketProbeResult struct {
	capturedAt time.Time
	state      string
	sessionID  string
	version    string
	userAgent  string
	originator string
	status     int
	headers    http.Header
	body       []byte
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
		state:      extractOpenAICodexTurnState(resp.Header),
		sessionID:  req.Header.Get("session_id"),
		version:    req.Header.Get("version"),
		userAgent:  req.Header.Get("user-agent"),
		originator: req.Header.Get("originator"),
		status:     resp.StatusCode,
		headers:    resp.Header.Clone(),
	}
	// 200 要读 SSE，核对 response.completed 的模型。读超上限时 probeOnce 视为未打中。
	// 401/429 只取一小段 body，判断 usage_limit_reached / 鉴权失败。
	switch {
	case resp.StatusCode == http.StatusOK && resp.Body != nil:
		result.body, _ = io.ReadAll(io.LimitReader(resp.Body, openAICodexTicketProbeBodyLimit+1))
	case (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests) && resp.Body != nil:
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
			s.refreshOpenAICookieWSTickets(ctx)
			s.refreshOpenAICodexTickets(ctx)
			timer.Reset(time.Duration(s.openAICodexTicketConfig().HarvestProbeIntervalSeconds) * time.Second)
		}
	}
}

func openAICodexTicketFreshWindow(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 180 * time.Second
	}
	return ttl
}

func (s *OpenAIGatewayService) currentSharedOpenAICodexTicket() *openAICodexTicket {
	if s == nil {
		return nil
	}
	s.openaiCodexShared.mu.Lock()
	defer s.openaiCodexShared.mu.Unlock()
	return newestOpenAICodexTicket(s.openaiCodexShared.tickets)
}

func newestOpenAICodexTicket(tickets []*openAICodexTicket) *openAICodexTicket {
	var newest *openAICodexTicket
	for _, ticket := range tickets {
		if ticket == nil {
			continue
		}
		if newest == nil || ticket.CapturedAt.After(newest.CapturedAt) {
			newest = ticket
		}
	}
	return newest
}

func (s *OpenAIGatewayService) rememberSharedOpenAICodexTicket(ticket *openAICodexTicket) {
	if s == nil || !ticket.usable(s.openAICodexTicketConfig().TargetLength) || s.openAICodexTicketStateRevoked(ticket, nil) {
		return
	}
	s.openaiCodexShared.mu.Lock()
	defer s.openaiCodexShared.mu.Unlock()
	kept := make([]*openAICodexTicket, 0, len(s.openaiCodexShared.tickets)+1)
	replaced := false
	for _, existing := range s.openaiCodexShared.tickets {
		if existing == nil || existing.State == ticket.State {
			replaced = existing != nil && existing.State == ticket.State
			continue
		}
		kept = append(kept, existing)
	}
	kept = append(kept, ticket)
	slices.SortFunc(kept, func(a, b *openAICodexTicket) int {
		if a.CapturedAt.After(b.CapturedAt) {
			return -1
		}
		if b.CapturedAt.After(a.CapturedAt) {
			return 1
		}
		return 0
	})
	if len(kept) > openAICodexTicketPoolLimit {
		kept = kept[:openAICodexTicketPoolLimit]
	}
	s.openaiCodexShared.tickets = kept
	if !replaced {
		s.openaiCodexShared.tried = nil
	}
}

func (s *OpenAIGatewayService) adoptSharedOpenAICodexTicket(accounts []Account) {
	cfg := s.openAICodexTicketConfig()
	for i := range accounts {
		ticket := s.lookupOpenAICodexTicket(&accounts[i], openAICodexTicketDefaultModel)
		if ticket.usable(cfg.TargetLength) && !s.openAICodexTicketStateRevoked(ticket, &accounts[i]) {
			s.rememberSharedOpenAICodexTicket(ticket)
		}
	}
}

func (s *OpenAIGatewayService) openAICodexTicketPoolNeedsHunt(now time.Time) bool {
	if s == nil {
		return false
	}
	window := openAICodexTicketFreshWindow(time.Duration(s.openAICodexTicketConfig().TTLSeconds) * time.Second)
	s.openaiCodexShared.mu.Lock()
	defer s.openaiCodexShared.mu.Unlock()
	fresh := 0
	var newest time.Time
	for _, ticket := range s.openaiCodexShared.tickets {
		if ticket == nil || !ticket.usable(s.openAICodexTicketConfig().TargetLength) {
			continue
		}
		if now.Sub(ticket.CapturedAt) >= window {
			continue
		}
		fresh++
		if ticket.CapturedAt.After(newest) {
			newest = ticket.CapturedAt
		}
	}
	if fresh < openAICodexTicketPoolTarget {
		return true
	}
	return newest.IsZero() || now.Sub(newest) >= openAICodexTicketPoolRefillAge
}

// sharedOpenAICodexTicketForInjection 只返回这个号自己打到的 Astra 票。
// Astra 和 Sol 请求都带这一张，不借用别的号。
func (s *OpenAIGatewayService) sharedOpenAICodexTicketForInjection(ctx context.Context, account *Account) *openAICodexTicket {
	if s == nil || account == nil {
		return nil
	}
	_ = ctx
	ticket := s.lookupOpenAICodexTicket(account, openAICodexTicketDefaultModel)
	if ticket.usable(s.openAICodexTicketConfig().TargetLength) && !s.openAICodexTicketStateRevoked(ticket, account) && ticket.AccountID == account.ID {
		return ticket
	}
	return nil
}

func (s *OpenAIGatewayService) pickOpenAICodexTicketHarvestAccount(eligible []Account) *Account {
	if s == nil || len(eligible) == 0 {
		return nil
	}
	s.openaiCodexShared.mu.Lock()
	defer s.openaiCodexShared.mu.Unlock()
	if s.openaiCodexShared.tried == nil {
		s.openaiCodexShared.tried = map[int64]struct{}{}
	}
	untried := make([]Account, 0, len(eligible))
	for i := range eligible {
		if _, ok := s.openaiCodexShared.tried[eligible[i].ID]; !ok {
			untried = append(untried, eligible[i])
		}
	}
	if len(untried) == 0 {
		s.openaiCodexShared.tried = map[int64]struct{}{}
		untried = eligible
	}
	chosen := untried[randIntN(len(untried))]
	s.openaiCodexShared.tried[chosen.ID] = struct{}{}
	return &chosen
}

// refreshOpenAICodexTickets 给还没有自己的有效票的号打票。
// 已有打票会话身份、且未被作废的票不会被新票换掉。
func (s *OpenAIGatewayService) refreshOpenAICodexTickets(ctx context.Context) {
	if s == nil || s.accountRepo == nil || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) || s.openAICookieWSModeConfigured() {
		return
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		logger.L().Warn("openai_codex_ticket list accounts failed", zap.Error(err))
		return
	}
	now := time.Now()
	eligible := make([]Account, 0, len(accounts))
	for i := range accounts {
		if s.openAICookieWSAccountEnabled(&accounts[i]) {
			continue
		}
		if openAICodexTicketHarvestSkipReason(&accounts[i], now) != "" {
			continue
		}
		if s.openAICodexTicketHeld(&accounts[i]) {
			continue
		}
		eligible = append(eligible, accounts[i])
	}
	if len(eligible) == 0 {
		return
	}
	account := s.pickOpenAICodexTicketHarvestAccount(eligible)
	if account == nil {
		return
	}
	account.Extra = maps.Clone(account.Extra)
	account.Credentials = maps.Clone(account.Credentials)
	s.probeOnceOpenAICodexTicket(ctx, account, openAICodexTicketDefaultModel)
}

// probeOnceOpenAICodexTicket 走打票代理打一发。命中合格 292（HTTP 200、长度==target、
// gAAAAA 前缀、打票 Cookie，且完整成功响应的模型就是所请求模型）就落库；312 和
// 完成模型不符都当 miss。同一 key 并发去重，避免上一发还没回来又叠一发。
func (s *OpenAIGatewayService) probeOnceOpenAICodexTicket(ctx context.Context, account *Account, model string) {
	if s == nil || !isOpenAICodexTicketAccount(account) || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) || s.openAICookieWSModeConfigured() {
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
			s.noteOpenAICodexTicketHarvestMiss(account.ID, model, time.Now())
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "token"), zap.Error(err))
			return nil, nil
		}
		result, perr := s.doOpenAICodexTicketProbe(harvestCtx, account, token, model, proxyURL, time.Duration(cfg.HarvestAttemptTimeoutSeconds)*time.Second)
		if perr != nil {
			s.noteOpenAICodexTicketHarvestMiss(account.ID, model, time.Now())
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "error"), zap.Error(perr))
			return nil, nil
		}
		cookies := openAICodexTicketCookieHeader(result.headers)
		completionModel, modelOK := openAICodexTicketProbeModelMatches(result.body, model)
		if result.status != http.StatusOK || result.state == "" || len(result.state) != cfg.TargetLength || !strings.HasPrefix(result.state, openAICodexTicketStatePrefix) || cookies == "" || !modelOK {
			s.applyOpenAICodexTicketHarvestProbeOutcome(harvestCtx, account, model, result)
			s.noteOpenAICodexTicketHarvestMiss(account.ID, model, time.Now())
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.Int("http", result.status), zap.Int("len", len(result.state)),
				zap.Int("cookie_count", openAICodexTicketCookieCount(cookies)),
				zap.String("completion_model", completionModel),
				zap.Bool("model_match", modelOK))
			return nil, nil
		}
		if s.openAICodexTicketHeld(account) {
			return nil, nil
		}
		s.clearOpenAICodexTicketHarvestBackoff(account.ID, model)
		now := time.Now()
		ticket := &openAICodexTicket{
			AccountID:  account.ID,
			Model:      model,
			State:      result.state,
			Length:     len(result.state),
			Cookies:    cookies,
			SessionID:  result.sessionID,
			Version:    result.version,
			UserAgent:  result.userAgent,
			Originator: result.originator,
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

func (s *OpenAIGatewayService) openAICodexTicketHarvestRetryWaiting(accountID int64, model string, now time.Time) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	raw, ok := s.openaiCodexTicketHarvestBackoff.Load(openAICodexTicketKey(accountID, model))
	if !ok {
		return false
	}
	state, ok := raw.(*openAICodexTicketHarvestBackoff)
	if !ok || state == nil || state.nextProbeAt.IsZero() {
		return false
	}
	return now.Before(state.nextProbeAt)
}

func (s *OpenAIGatewayService) noteOpenAICodexTicketHarvestMiss(accountID int64, model string, now time.Time) {
	if s == nil || accountID <= 0 {
		return
	}
	next := &openAICodexTicketHarvestBackoff{nextProbeAt: now.Add(time.Minute)}
	s.openaiCodexTicketHarvestBackoff.Store(openAICodexTicketKey(accountID, model), next)
	logger.L().Info("openai_codex_ticket harvest retry scheduled",
		zap.Int64("account_id", accountID),
		zap.String("model", model),
		zap.Time("next_probe_at", next.nextProbeAt),
	)
}

func (s *OpenAIGatewayService) clearOpenAICodexTicketHarvestBackoff(accountID int64, model string) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openaiCodexTicketHarvestBackoff.Delete(openAICodexTicketKey(accountID, model))
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

// openAICodexTicketProbeModelMatches reports the successful completion model.
// A ticket is acceptable only when every successful completion names model.
// A later matching event does not erase an earlier mismatch. Oversized or
// unfinished bodies are not a match.
func openAICodexTicketProbeModelMatches(body []byte, model string) (string, bool) {
	if len(body) == 0 || len(body) > openAICodexTicketProbeBodyLimit {
		return "", false
	}
	seen := ""
	saw, matches := false, true
	inspect := func(payload []byte, eventType string) {
		got, ok := openAICodexSuccessfulCompletionModel(payload, eventType)
		if !ok {
			return
		}
		saw = true
		if seen == "" {
			seen = got
		}
		if got != model {
			matches = false
			seen = got
		}
	}
	forEachOpenAISSEFrame(string(body), func(eventType string, payload []byte) {
		inspect(payload, eventType)
	})
	if !saw {
		inspect(body, "")
	}
	if !saw || !matches {
		return seen, false
	}
	return seen, true
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
	if strings.TrimSpace(gjson.GetBytes(payload, "object").String()) == "response" &&
		strings.TrimSpace(gjson.GetBytes(payload, "status").String()) == "completed" &&
		openAICodexTicketJSONValueEmpty(payload, "error") {
		model := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
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

func openAICodexTicketStateIs312(state string) bool {
	return openAICodexTicketStateValid(state, openAICodexTicketDegradedLength)
}

func openAICodexTicketStateValid(state string, n int) bool {
	if len(state) != n || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return false
	}
	for _, c := range state {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '=') {
			return false
		}
	}
	return true
}

type openAICodexTicketWatch struct {
	state       string
	model       string
	ticketModel string
	accountID   int64
	revoke      func(state, ticketModel string, accountID int64, reason string)
	once        sync.Once
}

func (w *openAICodexTicketWatch) fail(reason string) {
	if w == nil || w.revoke == nil || strings.TrimSpace(w.state) == "" {
		return
	}
	w.once.Do(func() {
		w.revoke(w.state, w.ticketModel, w.accountID, reason)
	})
}

func openAICodexTicketWatchFromGin(c *gin.Context) *openAICodexTicketWatch {
	if c == nil {
		return nil
	}
	raw, ok := c.Get(openAICodexTicketWatchKey)
	if !ok {
		return nil
	}
	watch, _ := raw.(*openAICodexTicketWatch)
	return watch
}

func (o *upstreamResponseModelObserver) adoptOpenAICodexTicketWatch(c *gin.Context) {
	if o == nil {
		return
	}
	o.ticketWatch = openAICodexTicketWatchFromGin(c)
}

func (o *upstreamResponseModelObserver) noteOpenAICodexTicketCompletion(payload []byte, eventType string) {
	if o == nil || o.ticketWatch == nil {
		return
	}
	model, ok := openAICodexSuccessfulCompletionModel(payload, eventType)
	if !ok || openAICodexTicketCompletionKeepsTicket(o.ticketWatch.model, model) {
		return
	}
	o.ticketWatch.fail("model_mismatch")
}

// openAICodexTicketCompletionKeepsTicket 判断完成模型是否还配得上这张票。
// 出站 gpt-5.6-sol、完成成 gpt-6-sol 是票生效的信号，不作废。
func openAICodexTicketCompletionKeepsTicket(requested, actual string) bool {
	requested = strings.TrimSpace(requested)
	actual = strings.TrimSpace(actual)
	if actual == "" {
		return false
	}
	if requested == actual {
		return true
	}
	return requested == openAICodexTicketDefaultSolModel && actual == openAICodexTicketUpgradedSolModel
}

// applyOpenAICodexTicketForRequest 注入门票，并让这条请求的响应观察器盯着这一张 state。
func (s *OpenAIGatewayService) applyOpenAICodexTicketForRequest(ctx context.Context, c *gin.Context, account *Account, model string, h http.Header) error {
	slot := &openAICodexTicketInjectionSlot{}
	err := s.applyOpenAICodexTicket(context.WithValue(ctx, openAICodexTicketInjectionSlotKey{}, slot), account, model, h)
	if err != nil {
		return err
	}
	s.armOpenAICodexTicketWatch(c, slot)
	rememberOpenAICodexTicketIdentity(c, slot)
	return nil
}

func (s *OpenAIGatewayService) armOpenAICodexTicketWatch(c *gin.Context, slot *openAICodexTicketInjectionSlot) {
	if s == nil || c == nil || slot == nil || strings.TrimSpace(slot.State) == "" {
		return
	}
	watch := &openAICodexTicketWatch{
		state:       slot.State,
		model:       strings.TrimSpace(slot.RequestModel),
		ticketModel: strings.TrimSpace(slot.TicketModel),
		accountID:   slot.AccountID,
		revoke:      s.revokeOpenAICodexTicketState,
	}
	c.Set(openAICodexTicketWatchKey, watch)
	if obs := upstreamResponseModelObserverFromContext(c); obs != nil {
		obs.ticketWatch = watch
	}
}

func (s *OpenAIGatewayService) observeOpenAICodexTicketEvent(c *gin.Context, eventType string, payload []byte) {
	watch := openAICodexTicketWatchFromGin(c)
	if watch == nil {
		return
	}
	model, ok := openAICodexSuccessfulCompletionModel(payload, eventType)
	if !ok || openAICodexTicketCompletionKeepsTicket(watch.model, model) {
		return
	}
	watch.fail("model_mismatch")
}

func (s *OpenAIGatewayService) observeOpenAICodexTicketResponseHeader(c *gin.Context, upstream http.Header) {
	if !openAICodexTicketStateIs312(extractOpenAICodexTurnState(upstream)) {
		return
	}
	if watch := openAICodexTicketWatchFromGin(c); watch != nil {
		watch.fail("state_312")
	}
}

func openAICodexTicketRevokedMemKey(accountID int64, state string) string {
	return fmt.Sprintf("%d\x00%s", accountID, strings.TrimSpace(state))
}

func (s *OpenAIGatewayService) openAICodexTicketStateRevoked(ticket *openAICodexTicket, account *Account) bool {
	if s == nil || ticket == nil {
		return false
	}
	state := strings.TrimSpace(ticket.State)
	if state == "" {
		return false
	}
	accountID := ticket.AccountID
	if account != nil && account.ID > 0 {
		accountID = account.ID
	}
	if _, ok := s.openaiCodexTicketRevoked.Load(openAICodexTicketRevokedMemKey(accountID, state)); ok {
		return true
	}
	if account == nil || account.ID != accountID {
		return false
	}
	return openAICodexTicketRevokedInExtra(account, ticket)
}

func openAICodexTicketRevokedInExtra(account *Account, ticket *openAICodexTicket) bool {
	if account == nil || account.Extra == nil || ticket == nil {
		return false
	}
	marker, _ := account.Extra[openAICodexTicketRevokedExtraKey(ticket.Model)].(string)
	return marker != "" && marker == strings.TrimSpace(ticket.State)
}

// revokeOpenAICodexTicketState 只作废这个号自己的这一张票。
// 别的号即使带过同一段 state，也不会被一起拿掉。
func (s *OpenAIGatewayService) revokeOpenAICodexTicketState(state, ticketModel string, accountID int64, reason string) {
	if s == nil || accountID <= 0 {
		return
	}
	state = strings.TrimSpace(state)
	ticketModel = strings.TrimSpace(ticketModel)
	if state == "" || ticketModel == "" {
		return
	}
	if _, loaded := s.openaiCodexTicketRevoked.LoadOrStore(openAICodexTicketRevokedMemKey(accountID, state), time.Now()); loaded {
		return
	}
	if raw, ok := s.openaiCodexTickets.Load(openAICodexTicketKey(accountID, ticketModel)); ok {
		ticket, _ := raw.(*openAICodexTicket)
		if ticket != nil && ticket.State == state {
			s.openaiCodexTickets.Delete(openAICodexTicketKey(accountID, ticketModel))
		}
	}
	s.openaiCodexShared.mu.Lock()
	kept := make([]*openAICodexTicket, 0, len(s.openaiCodexShared.tickets))
	for _, ticket := range s.openaiCodexShared.tickets {
		if ticket == nil || (ticket.AccountID == accountID && ticket.State == state) {
			continue
		}
		kept = append(kept, ticket)
	}
	s.openaiCodexShared.tickets = kept
	s.openaiCodexShared.mu.Unlock()
	s.persistOpenAICodexTicketRevocation(accountID, ticketModel, state)
	logger.L().Info("openai_codex_ticket revoked",
		zap.Int64("account_id", accountID),
		zap.String("model", ticketModel),
		zap.String("reason", reason),
	)
}

func (s *OpenAIGatewayService) persistOpenAICodexTicketRevocation(accountID int64, model, state string) {
	if s == nil || s.accountRepo == nil || accountID <= 0 || strings.TrimSpace(model) == "" || strings.TrimSpace(state) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
		openAICodexTicketRevokedExtraKey(model): state,
	}); err != nil {
		logger.L().Warn("openai_codex_ticket revoke persist failed",
			zap.Int64("account_id", accountID),
			zap.String("model", model),
			zap.Error(err),
		)
	}
}

// IsOpenAICodexTicketExtraKey identifies server-managed ticket material.
func IsOpenAICodexTicketExtraKey(key string) bool {
	return strings.HasPrefix(key, openAICodexTicketExtraKeyPrefix) || strings.HasPrefix(key, openAICodexTicketRevokedExtraKeyPrefix) || strings.HasPrefix(key, openAICookieWSExtraKeyPrefix)
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
