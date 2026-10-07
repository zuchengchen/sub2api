package service

import (
	"context"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// openAITiboRun is the route plan of one Forward attempt on a Tibo-routed
// account. HTTP is the only remaining hop.
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
// request. HTTP is the only remaining hop.
func (s *OpenAIGatewayService) openAITiboRouteTiers(_ *Account, _ string, _ bool, httpVerdict openAITiboVerdict) map[openAITiboRoute]openAITiboVerdict {
	return map[openAITiboRoute]openAITiboVerdict{openAITiboRouteHTTP: httpVerdict}
}

// openAITiboOrderRoutes always ends on HTTP.
func openAITiboOrderRoutes(_ map[openAITiboRoute]openAITiboVerdict, _ openAITiboRoute) []openAITiboRoute {
	return []openAITiboRoute{openAITiboRouteHTTP}
}

// newOpenAITiboRun builds the route plan for one Forward attempt.
func (s *OpenAIGatewayService) newOpenAITiboRun(ctx context.Context, c *gin.Context, account *Account, body []byte, scope string) *openAITiboRun {
	cfg := s.openAITiboRouteConfig()
	requestModel := gjson.GetBytes(body, "model").String()
	httpVerdict := s.openAITiboRequestVerdicts(ctx, account)
	run := &openAITiboRun{account: account, scope: scope, cfg: cfg, probePayload: openAICookieWSIsProbePayloadRaw(body)}
	run.tiers = s.openAITiboRouteTiers(account, requestModel, isExplicitOpenAICompactContext(c), httpVerdict)
	run.pinned = s.loadOpenAITiboPin(account.ID, scope, time.Now())
	run.routes = openAITiboOrderRoutes(run.tiers, run.pinned)
	return run
}

// httpReason is the transport reason when the plan serves HTTP.
func (r *openAITiboRun) httpReason() string {
	if r.tier(openAITiboRouteHTTP) == openAITiboHealthy {
		return openAITiboHTTPOKReason
	}
	return openAITiboRouteOrderReason
}

// finishOpenAITiboRun stamps the degraded flag, pins the session to the
// route that served, and runs the passive model check for HTTP.
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
	if !run.probePayload {
		s.observeOpenAITiboResponseModel(run.account, run.served, result.UpstreamModel, result.UpstreamResponseModel)
	}
}

// setOpenAIBPSBypassReason records or clears a leftover routing record.
// Routing details are never sent to API clients.
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
// route, 1 has no verdict or an unknown one (including accounts Tibo does
// not apply to), 2 has only degraded routes. It never starts probes.
func (s *OpenAIGatewayService) openAITiboSchedulerTier(account *Account, requestModel string, now time.Time) int {
	if account == nil || !s.openAITiboRouteApplies(account) {
		return 1
	}
	cfg := s.openAITiboRouteConfig()
	httpVerdict := openAITiboUnknown
	if state := s.loadOpenAITiboAccountState(account.ID); state != nil {
		state.mu.Lock()
		httpVerdict = state.http.effective(now, cfg)
		state.mu.Unlock()
	}
	if httpVerdict == openAITiboHealthy {
		return 0
	}
	rank := 2
	for _, verdict := range s.openAITiboRouteTiers(account, requestModel, false, httpVerdict) {
		if r := openAITiboTierRank(verdict); r < rank {
			rank = r
		}
	}
	return rank
}
