package service

import (
	"bytes"
	"context"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// openAITiboRun is the route plan of one Forward attempt on a Tibo-routed
// account: routes ordered by tier (healthy > unknown > degraded) and, within a
// tier, HTTP -> BPS -> Cookie WS. HTTP is terminal: its own failures never
// fall through to another route, so the plan ends at HTTP.
type openAITiboRun struct {
	account *Account
	scope   string
	// probePayload marks a business request that is the exact Tibo probe;
	// its model swaps are the probe's own verdict, not passive evidence.
	probePayload bool
	routes       []openAITiboRoute
	tiers        map[openAITiboRoute]openAITiboVerdict
	pinned       openAITiboRoute
	served       openAITiboRoute
	bpsBody      []byte
	cfg          openAITiboSettings
}

func (r *openAITiboRun) first() openAITiboRoute {
	if r == nil || len(r.routes) == 0 {
		return openAITiboRouteHTTP
	}
	return r.routes[0]
}

func (r *openAITiboRun) tier(route openAITiboRoute) openAITiboVerdict {
	if r == nil {
		return openAITiboUnknown
	}
	if verdict, ok := r.tiers[route]; ok {
		return verdict
	}
	return openAITiboUnknown
}

// advance drops the first route after a failure that did not execute there.
func (r *openAITiboRun) advance() {
	if r != nil && len(r.routes) > 1 {
		r.routes = r.routes[1:]
	}
}

// commit records the route about to serve. Degraded HTTP (every route
// degraded or unusable) reaches usage logs via finishOpenAITiboRun, never the
// client.
func (r *openAITiboRun) commit(_ *gin.Context, route openAITiboRoute) {
	if r == nil {
		return
	}
	r.served = route
}

func openAITiboTierRank(verdict openAITiboVerdict) int {
	switch verdict {
	case openAITiboHealthy:
		return 0
	case openAITiboDegraded:
		return 2
	default:
		return 1
	}
}

// openAITiboRouteTiers returns the tier of every route available for this
// request. HTTP is always available. BPS is skipped for models it does not
// serve and for requests carrying tools it cannot run (body == nil skips that
// check for the scheduler). Cookie WS needs an astra upstream model and a
// verified ready slot. off/shadow BPS keeps its existing position (healthy).
func (s *OpenAIGatewayService) openAITiboRouteTiers(account *Account, requestModel string, body []byte, compact bool, httpVerdict, bpsVerdict openAITiboVerdict, cfg openAITiboSettings) map[openAITiboRoute]openAITiboVerdict {
	tiers := map[openAITiboRoute]openAITiboVerdict{openAITiboRouteHTTP: httpVerdict}
	if account.IsExcelBPSEnabledForModel(requestModel) && (body == nil || basispoints.NativeFallbackReason(body) == "") {
		if cfg.bpsProbeMode == config.OpenAITiboBPSProbeEnforce {
			tiers[openAITiboRouteBPS] = bpsVerdict
		} else {
			tiers[openAITiboRouteBPS] = openAITiboHealthy
		}
	}
	upstreamModel := resolveOpenAIAccountUpstreamModelForRequest(account, requestModel, false)
	if !compact && s.openAICookieWSEnabledForModel(account, upstreamModel) && s.openAICookieWSHasReadyTicket(account, upstreamModel) {
		tiers[openAITiboRouteCookieWS] = openAITiboHealthy
	}
	return tiers
}

// openAITiboOrderRoutes orders available routes; a pinned route that is still
// healthy goes first, and routes after HTTP are dropped.
func openAITiboOrderRoutes(tiers map[openAITiboRoute]openAITiboVerdict, pinned openAITiboRoute) []openAITiboRoute {
	order := []openAITiboRoute{openAITiboRouteHTTP, openAITiboRouteBPS, openAITiboRouteCookieWS}
	routes := make([]openAITiboRoute, 0, len(order))
	for rank := 0; rank <= 2; rank++ {
		for _, route := range order {
			if verdict, ok := tiers[route]; ok && openAITiboTierRank(verdict) == rank {
				routes = append(routes, route)
			}
		}
	}
	if pinned != "" && tiers[pinned] == openAITiboHealthy {
		reordered := []openAITiboRoute{pinned}
		for _, route := range routes {
			if route != pinned {
				reordered = append(reordered, route)
			}
		}
		routes = reordered
	}
	for i, route := range routes {
		if route == openAITiboRouteHTTP {
			return routes[:i+1]
		}
	}
	return append(routes, openAITiboRouteHTTP)
}

// newOpenAITiboRun builds the route plan for one Forward attempt. body is the
// request after group/model policy and before transport-specific rewrites; it
// is kept for a later BPS attempt after a Cookie WS failure.
func (s *OpenAIGatewayService) newOpenAITiboRun(ctx context.Context, c *gin.Context, account *Account, body []byte, scope string) *openAITiboRun {
	cfg := s.openAITiboRouteConfig()
	requestModel := gjson.GetBytes(body, "model").String()
	bpsEnabled := account.IsExcelBPSEnabledForModel(openAICodexTicketDefaultModel)
	httpVerdict, bpsVerdict := s.openAITiboRequestVerdicts(ctx, account, bpsEnabled)
	run := &openAITiboRun{account: account, scope: scope, cfg: cfg, probePayload: openAICookieWSIsProbePayloadRaw(body)}
	// Native remote compaction v2 (bare /responses + compaction_trigger) is a
	// compaction turn exactly like the legacy /compact path: Cookie WS has no
	// verified compaction contract, so neither form may be planned onto it.
	run.tiers = s.openAITiboRouteTiers(account, requestModel, body, isExplicitOpenAICompactContext(c), httpVerdict, bpsVerdict, cfg)
	run.pinned = s.loadOpenAITiboPin(account.ID, scope, time.Now())
	run.routes = openAITiboOrderRoutes(run.tiers, run.pinned)
	if _, ok := run.tiers[openAITiboRouteBPS]; ok && run.first() != openAITiboRouteBPS {
		run.bpsBody = bytes.Clone(body)
	}
	return run
}

// bpsBypassReason is the internal BPS bypass reason (openAIBPSBypassReasonKey)
// when BPS is enabled for the model but another route goes first.
func (r *openAITiboRun) bpsBypassReason(body []byte) string {
	if r.first() == openAITiboRouteHTTP && r.tier(openAITiboRouteHTTP) == openAITiboHealthy {
		return openAITiboHTTPOKReason
	}
	if reason := basispoints.NativeFallbackReason(body); reason != "" {
		return reason
	}
	return openAITiboRouteOrderReason
}

// httpReason is the transport reason when the plan serves HTTP although the
// resolver found a ready Cookie WS.
func (r *openAITiboRun) httpReason() string {
	if r.tier(openAITiboRouteHTTP) == openAITiboHealthy {
		return openAITiboHTTPOKReason
	}
	return openAITiboRouteOrderReason
}

// finishOpenAITiboRun stamps the degraded flag, pins the session to the
// route that served, and runs the passive model check for HTTP and BPS
// (Cookie WS checks inside the forwarder, where the socket is known).
func (s *OpenAIGatewayService) finishOpenAITiboRun(run *openAITiboRun, result *OpenAIForwardResult, err error) {
	if run == nil || run.served == "" || result == nil {
		return
	}
	degraded := run.served == openAITiboRouteHTTP && run.tier(openAITiboRouteHTTP) == openAITiboDegraded
	result.RouteDegraded = &degraded
	if err != nil || result.ClientDisconnect {
		return
	}
	s.storeOpenAITiboPin(run.account.ID, run.scope, run.served, time.Now())
	s.openaiTiboStats.noteServed(degraded, run.cfg)
	if run.served != openAITiboRouteCookieWS && !run.probePayload {
		s.observeOpenAITiboResponseModel(run.account, run.served, result.UpstreamModel, result.UpstreamResponseModel)
	}
}

// setOpenAIBPSBypassReason records internally why BPS did not serve this
// attempt; "" clears it. Routing details are never sent to API clients.
func setOpenAIBPSBypassReason(c *gin.Context, reason string) {
	if c == nil {
		return
	}
	c.Set(openAIBPSBypassReasonKey, reason)
}

// resetOpenAIRouteRecord clears the routing record a failed attempt on a
// previous account may have left, since the handler reuses one context and
// response writer across failover attempts. The legacy routing headers are
// deleted defensively; this service no longer sets them.
func resetOpenAIRouteRecord(c *gin.Context) {
	if c == nil {
		return
	}
	setOpenAIBPSBypassReason(c, "")
	if c.Writer == nil {
		return
	}
	h := c.Writer.Header()
	for _, name := range [...]string{"X-Codex2API-Upstream", "X-Codex2API-Basispoints-Bypass", "X-Codex2API-Route-Quality"} {
		h.Del(name)
	}
}

type openAITiboPin struct {
	route     openAITiboRoute
	expiresAt time.Time
}

func openAITiboPinKey(accountID int64, scope string) string {
	return strconv.FormatInt(accountID, 10) + "\x00" + scope
}

// loadOpenAITiboPin returns the route this session last used on the account.
// Sessions without an execution scope are never pinned.
func (s *OpenAIGatewayService) loadOpenAITiboPin(accountID int64, scope string, now time.Time) openAITiboRoute {
	if scope == "" {
		return ""
	}
	raw, ok := s.openaiTiboPins.Load(openAITiboPinKey(accountID, scope))
	if !ok {
		return ""
	}
	pin, ok := raw.(openAITiboPin)
	if !ok || !now.Before(pin.expiresAt) {
		return ""
	}
	return pin.route
}

func (s *OpenAIGatewayService) storeOpenAITiboPin(accountID int64, scope string, route openAITiboRoute, now time.Time) {
	if scope == "" || route == "" {
		return
	}
	s.openaiTiboPins.Store(openAITiboPinKey(accountID, scope), openAITiboPin{route: route, expiresAt: now.Add(s.openAIWSSessionStickyTTL())})
}

func (s *OpenAIGatewayService) pruneOpenAITiboPins(now time.Time) {
	last := s.openaiTiboPinPrunedAt.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < openAITiboPinPruneEvery {
		return
	}
	s.openaiTiboPinPrunedAt.Store(now.UnixNano())
	s.openaiTiboPins.Range(func(key, value any) bool {
		if pin, ok := value.(openAITiboPin); !ok || !now.Before(pin.expiresAt) {
			s.openaiTiboPins.Delete(key)
		}
		return true
	})
}

// openAITiboSchedulerTier ranks an account for scheduling: 0 has a healthy
// route, 1 has no verdict or an unknown one (including non Cookie WS
// accounts), 2 has only degraded routes. It never starts probes.
func (s *OpenAIGatewayService) openAITiboSchedulerTier(account *Account, requestModel string, now time.Time) int {
	if account == nil || !s.openAITiboRouteApplies(account) {
		return 1
	}
	cfg := s.openAITiboRouteConfig()
	httpVerdict, bpsVerdict := openAITiboUnknown, openAITiboUnknown
	if state := s.loadOpenAITiboAccountState(account.ID); state != nil {
		state.mu.Lock()
		httpVerdict, bpsVerdict = state.http.effective(now, cfg), state.bps.effective(now, cfg)
		state.mu.Unlock()
	}
	if httpVerdict == openAITiboHealthy {
		return 0
	}
	rank := 2
	for _, verdict := range s.openAITiboRouteTiers(account, requestModel, nil, false, httpVerdict, bpsVerdict, cfg) {
		if r := openAITiboTierRank(verdict); r < rank {
			rank = r
		}
	}
	return rank
}
