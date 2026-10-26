package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// Tibo routing for OpenAI accounts that can answer the Astra probe (ChatGPT
// OAuth / setup-token, and API-key including compatible gateways). Each
// account keeps a confirmed HTTP Tibo verdict that only flips when a majority
// of a small vote window agrees; unknown samples never vote. Requests stay on
// HTTP. Background probes run on the harvester tick for enrolled accounts.
const (
	openAITiboHTTPOKReason     = "tibo_http_ok"
	openAITiboRouteOrderReason = "tibo_route_order"
	openAITiboProbeTimeout     = 25 * time.Second
	// A cold account (no sample yet) waits at most this long for its first probe.
	openAITiboProbeWait = 8 * time.Second
	// Admin sparkline: last N Tibo probe outcomes, oldest first.
	openAITiboRecentSampleLimit = 10
	// Gin context key for a leftover routing record from a previous attempt.
	// Routing details stay internal; they are never sent to API clients.
	openAIBPSBypassReasonKey = "openai_bps_bypass_reason"
)

type openAITiboVerdict string

const (
	openAITiboUnknown  openAITiboVerdict = "unknown"
	openAITiboHealthy  openAITiboVerdict = "healthy"
	openAITiboDegraded openAITiboVerdict = "degraded"
)

func (v openAITiboVerdict) definitive() bool {
	return v == openAITiboHealthy || v == openAITiboDegraded
}

type openAITiboRoute string

const (
	openAITiboRouteHTTP     openAITiboRoute = "http"
	openAITiboRouteCookieWS openAITiboRoute = "cookie_ws" // retained for persisted pins / old admin views
)

// openAITiboProbeSample is one Tibo probe observation for a route.
type openAITiboProbeSample struct {
	verdict     openAITiboVerdict // healthy, degraded or unknown
	status      int               // upstream HTTP status; 0 when no response
	answerClass string            // classifyTiboAnswer class, "" when none
	retryAt     *time.Time        // Retry-After / rate-limit reset on a non-200 response
}

// openAITiboSettings is the resolved gateway.openai_tibo_route configuration.
type openAITiboSettings struct {
	interval         time.Duration
	spread           time.Duration
	confirmSamples   int
	confirmSpacing   time.Duration
	minDegradedDwell time.Duration
	maxStale         time.Duration
	maxProbesPerHour int
	probeConcurrency int
	preferHealthy    bool
	alertRatio       float64
	alertMinRequests int
}

// openAITiboRouteConfig applies built-in defaults to zero values, so tests and
// partial configs keep the documented behavior. Spread and the alert ratio
// honor an explicit zero; config.go registers their production defaults.
func (s *OpenAIGatewayService) openAITiboRouteConfig() openAITiboSettings {
	var raw config.OpenAITiboRouteConfig
	if s != nil && s.cfg != nil {
		raw = s.cfg.Gateway.OpenAITiboRoute
	}
	pick := func(value, fallback time.Duration) time.Duration {
		if value > 0 {
			return value
		}
		return fallback
	}
	cfg := openAITiboSettings{
		interval:         pick(raw.Interval, 10*time.Minute),
		spread:           raw.Spread,
		confirmSamples:   raw.ConfirmSamples,
		confirmSpacing:   pick(raw.ConfirmSpacing, 20*time.Second),
		minDegradedDwell: raw.MinDegradedDwell,
		maxStale:         pick(raw.MaxStale, 45*time.Minute),
		maxProbesPerHour: raw.MaxProbesPerHour,
		probeConcurrency: raw.ProbeConcurrency,
		preferHealthy:    raw.SchedulerPreferHealthyRoute,
		alertRatio:       raw.DegradedAlertRatio,
		alertMinRequests: raw.DegradedAlertMinRequests,
	}
	if cfg.spread < 0 {
		cfg.spread = 0
	}
	if cfg.spread > cfg.interval {
		cfg.spread = cfg.interval
	}
	if cfg.confirmSamples <= 0 {
		cfg.confirmSamples = 3
	}
	switch {
	case cfg.minDegradedDwell == 0:
		cfg.minDegradedDwell = 10 * time.Minute
	case cfg.minDegradedDwell < 0:
		cfg.minDegradedDwell = 0
	}
	if cfg.maxProbesPerHour <= 0 {
		cfg.maxProbesPerHour = 20
	}
	if cfg.probeConcurrency <= 0 {
		cfg.probeConcurrency = 4
	}
	if cfg.alertMinRequests <= 0 {
		cfg.alertMinRequests = 20
	}
	return cfg
}

// openAITiboRouteState is the vote state of one (account, route). All fields
// are guarded by the owning openAITiboAccountState mutex.
type openAITiboRouteState struct {
	// verdict is the last confirmed (definitive) verdict; "" until the first
	// definitive sample. checkedAt is the last definitive sample agreeing with
	// it (or the hard evidence that set it).
	verdict   openAITiboVerdict
	checkedAt time.Time
	flippedAt time.Time
	// pending holds definitive samples of a vote window opened by a sample
	// that disagreed with verdict.
	pending       []openAITiboVerdict
	pendingSince  time.Time
	dwellDeferred bool
	unknownStreak int
	// clientErrorStreak counts consecutive probe rejections (4xx other than
	// 408/429): a free plan without astra, a relay refusing the probe. They
	// back off instead of repeating every cadence.
	clientErrorStreak int
	// nextProbeAt is the probe's own schedule (cadence, confirmation,
	// Retry-After, budget, client-error backoff). Account availability is
	// tracked separately in holdUntil/parked, re-synced from the database
	// on every enrollment, so a lifted rate limit can never leave a probe
	// stranded days in the future.
	nextProbeAt time.Time
	// holdUntil defers probes while the account is rate-limited, overloaded
	// or temporarily unschedulable; parked stops them while it is paused,
	// disabled or expired (no known end time).
	holdUntil    time.Time
	parked       bool
	lastSampleAt time.Time
	lastSample   openAITiboProbeSample
	recent       []openAITiboRecentSample // last openAITiboRecentSampleLimit probes, oldest first
	probes       []time.Time              // probe starts within the last hour
	flips        []time.Time              // confirmed flips within the last hour
	inflight     bool
	done         chan struct{}
	persistedAt  time.Time
	// Calibration counters (process lifetime): vote windows opened by a
	// disagreeing sample and how they ended.
	voteWindows   int
	voteConfirmed int
	voteRejected  int
}

type openAITiboAccountState struct {
	mu         sync.Mutex
	accountID  int64
	lastUsedAt time.Time
	http       openAITiboRouteState
}

func (st *openAITiboAccountState) route(_ openAITiboRoute) *openAITiboRouteState {
	return &st.http
}

// openAITiboTransition reports what one sample did to the confirmed verdict.
type openAITiboTransition struct {
	flipped bool
	from    openAITiboVerdict
	to      openAITiboVerdict
	votes   []openAITiboVerdict
}

type openAITiboRecentSample struct {
	Verdict openAITiboVerdict
	At      time.Time
}

func (r *openAITiboRouteState) rememberSample(sample openAITiboProbeSample, now time.Time) {
	if !sample.verdict.definitive() {
		return
	}
	r.recent = append(r.recent, openAITiboRecentSample{Verdict: sample.verdict, At: now})
	if extra := len(r.recent) - openAITiboRecentSampleLimit; extra > 0 {
		r.recent = append([]openAITiboRecentSample(nil), r.recent[extra:]...)
	}
}

// observe applies one sample using the vote rules. It never touches probe
// scheduling; see scheduleNext.
func (r *openAITiboRouteState) observe(sample openAITiboProbeSample, now time.Time, cfg openAITiboSettings) openAITiboTransition {
	r.lastSampleAt = now
	r.lastSample = sample
	r.rememberSample(sample, now)
	if !sample.verdict.definitive() {
		// Unknown samples never vote; the confirmed verdict ages toward max_stale.
		r.unknownStreak++
		return openAITiboTransition{}
	}
	r.unknownStreak = 0
	if len(r.pending) > 0 && now.Sub(r.pendingSince) > cfg.maxStale {
		// Expired without a decision: counted as neither confirmed nor rejected.
		r.pending, r.dwellDeferred = nil, false
	}
	if r.verdict == "" {
		// The first definitive sample establishes the verdict.
		r.verdict, r.checkedAt, r.flippedAt = sample.verdict, now, now
		return openAITiboTransition{flipped: true, from: "", to: sample.verdict, votes: []openAITiboVerdict{sample.verdict}}
	}
	if sample.verdict == r.verdict {
		r.checkedAt = now
		if len(r.pending) == 0 {
			return openAITiboTransition{}
		}
	}
	// A disagreeing sample opens a vote window; later definitive samples
	// (either way) join it until the challenger wins or cannot win.
	if len(r.pending) == 0 {
		r.pendingSince = now
		r.voteWindows++
	}
	r.pending = append(r.pending, sample.verdict)
	if window := cfg.confirmSamples; len(r.pending) > window {
		r.pending = append([]openAITiboVerdict(nil), r.pending[len(r.pending)-window:]...)
	}
	challenger := openAITiboHealthy
	if r.verdict == openAITiboHealthy {
		challenger = openAITiboDegraded
	}
	need := cfg.confirmSamples/2 + 1
	votes := 0
	for _, vote := range r.pending {
		if vote == challenger {
			votes++
		}
	}
	if sample.verdict == challenger && votes >= need {
		if challenger == openAITiboHealthy && cfg.minDegradedDwell > 0 && now.Sub(r.flippedAt) < cfg.minDegradedDwell {
			// Confirmed recovery waits for the dwell; the next agreeing
			// sample after it completes the flip.
			r.dwellDeferred = true
			return openAITiboTransition{}
		}
		transition := openAITiboTransition{flipped: true, from: r.verdict, to: challenger, votes: append([]openAITiboVerdict(nil), r.pending...)}
		r.verdict, r.checkedAt, r.flippedAt = challenger, now, now
		r.pending, r.dwellDeferred = nil, false
		r.voteConfirmed++
		return transition
	}
	if votes < need {
		r.dwellDeferred = false
	}
	if votes+(cfg.confirmSamples-len(r.pending)) < need {
		// The challenger can no longer reach a majority of this window.
		r.pending, r.dwellDeferred = nil, false
		r.voteRejected++
	}
	return openAITiboTransition{}
}

// forceDegraded applies hard evidence (a business response served by another
// model): degraded now, no vote. It returns true when the verdict changed.
func (r *openAITiboRouteState) forceDegraded(now time.Time) bool {
	changed := r.verdict != openAITiboDegraded
	if changed {
		r.flippedAt = now
	}
	// Hard evidence ends an open window without a vote (neither confirmed nor
	// rejected in the calibration counters).
	r.verdict, r.checkedAt = openAITiboDegraded, now
	r.pending, r.dwellDeferred = nil, false
	r.lastSampleAt = now
	r.lastSample = openAITiboProbeSample{verdict: openAITiboDegraded}
	r.rememberSample(r.lastSample, now)
	return changed
}

// effective is the verdict requests use: unknown when there is none yet or
// no definitive sample has agreed within max_stale.
func (r *openAITiboRouteState) effective(now time.Time, cfg openAITiboSettings) openAITiboVerdict {
	if !r.verdict.definitive() || now.Sub(r.checkedAt) > cfg.maxStale {
		return openAITiboUnknown
	}
	return r.verdict
}

// cold means this route has never produced a sample in this process.
func (r *openAITiboRouteState) cold() bool {
	return r.verdict == "" && r.lastSampleAt.IsZero()
}

func (r *openAITiboRouteState) due(now time.Time) bool {
	return !r.inflight && !r.parked && !now.Before(r.nextProbeAt) && !now.Before(r.holdUntil)
}

// nextDue is when the route can next probe (nextProbeAt or the account hold,
// whichever is later); zero while parked.
func (r *openAITiboRouteState) nextDue() time.Time {
	if r.parked {
		return time.Time{}
	}
	if r.holdUntil.After(r.nextProbeAt) {
		return r.holdUntil
	}
	return r.nextProbeAt
}

// openAITiboHold is the account-side availability of a route.
type openAITiboHold struct {
	until  time.Time
	parked bool
}

func (r *openAITiboRouteState) applyHold(hold openAITiboHold) {
	r.holdUntil, r.parked = hold.until, hold.parked
}

// openAITiboAccountHold derives the hold from an authoritative account row:
// parked when it is paused, disabled, expired or no longer a Tibo account;
// held until the latest active rate-limit/overload/unschedulable window;
// zero when it can be probed now.
func openAITiboAccountHold(account *Account, now time.Time) openAITiboHold {
	if account == nil || !isOpenAITiboAccount(account) || account.Status != StatusActive || !account.Schedulable ||
		(account.AutoPauseOnExpired && account.ExpiresAt != nil && !now.Before(*account.ExpiresAt)) {
		return openAITiboHold{parked: true}
	}
	var until time.Time
	for _, at := range []*time.Time{account.RateLimitResetAt, account.OverloadUntil, account.TempUnschedulableUntil, account.modelRateLimitResetAt(openAICodexTicketDefaultModel)} {
		if at != nil && at.After(now) && at.After(until) {
			until = *at
		}
	}
	return openAITiboHold{until: until}
}

const (
	// A probe Retry-After beyond this is treated as a quota reset that the
	// account state (rate_limit_reset_at) tracks; the probe retries sooner.
	openAITiboProbeRetryAfterCap = time.Hour
	// Consecutive client rejections double the cadence up to this cap.
	openAITiboClientErrorBackoffCap = 2 * time.Hour
)

// openAITiboClientError reports a probe rejection that repeating soon cannot
// fix (400/401/403/404...). 408 and 429 are transient and excluded.
func openAITiboClientError(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}

// scheduleNext sets nextProbeAt after a sample.
func (r *openAITiboRouteState) scheduleNext(_ openAITiboRoute, now time.Time, cfg openAITiboSettings) {
	if r.lastSample.verdict == openAITiboUnknown && openAITiboClientError(r.lastSample.status) {
		r.clientErrorStreak++
	} else {
		r.clientErrorStreak = 0
	}
	if r.dwellDeferred && r.lastSample.verdict.definitive() {
		at := r.flippedAt.Add(cfg.minDegradedDwell)
		if !at.After(now) {
			// The dwell is over but this sample did not complete the flip:
			// the next confirmation keeps the normal spacing.
			at = now.Add(cfg.confirmSpacing)
		}
		r.nextProbeAt = at
		return
	}
	delay := openAITiboCadenceDelay(cfg)
	if len(r.pending) > 0 {
		delay = cfg.confirmSpacing
	}
	if r.clientErrorStreak > 1 {
		// 1st rejection: normal cadence; then 2x, 4x, ... up to the cap.
		backoff := cfg.interval
		for i := 1; i < r.clientErrorStreak && backoff < openAITiboClientErrorBackoffCap; i++ {
			backoff *= 2
		}
		if backoff > openAITiboClientErrorBackoffCap {
			backoff = openAITiboClientErrorBackoffCap
		}
		if backoff > delay {
			delay = backoff
		}
	}
	r.nextProbeAt = now.Add(delay)
	if at := r.lastSample.retryAt; at != nil && at.After(r.nextProbeAt) {
		retry := *at
		if limit := now.Add(openAITiboProbeRetryAfterCap); retry.After(limit) {
			retry = limit
		}
		if retry.After(r.nextProbeAt) {
			r.nextProbeAt = retry
		}
	}
}

// openAITiboCadenceDelay returns Interval ± Spread, uniform and inclusive.
func openAITiboCadenceDelay(cfg openAITiboSettings) time.Duration {
	base := cfg.interval
	if base <= 0 {
		base = 10 * time.Minute
	}
	spread := cfg.spread
	if spread < 0 {
		spread = 0
	}
	if spread > base {
		spread = base
	}
	if spread == 0 {
		return base
	}
	spanSec := int64((2 * spread) / time.Second)
	return base - spread + time.Duration(rand.Int64N(spanSec+1))*time.Second
}

func openAITiboStartupDelay(cfg openAITiboSettings) time.Duration {
	base := cfg.interval
	if base <= 0 {
		base = 10 * time.Minute
	}
	secs := int64(base / time.Second)
	return time.Duration(rand.Int64N(secs+1)) * time.Second
}

// takeBudget records a probe start when this route is under
// max_probes_per_hour; otherwise it defers nextProbeAt to the window's end.
func (r *openAITiboRouteState) takeBudget(now time.Time, cfg openAITiboSettings) bool {
	r.probes = pruneOpenAITiboWindow(r.probes, now)
	if len(r.probes) >= cfg.maxProbesPerHour {
		if next := r.probes[0].Add(time.Hour); next.After(r.nextProbeAt) {
			r.nextProbeAt = next
		}
		return false
	}
	r.probes = append(r.probes, now)
	return true
}

// countOpenAITiboWindow counts entries within the last hour without touching
// the slice (pruneOpenAITiboWindow compacts in place and must be assigned).
func countOpenAITiboWindow(times []time.Time, now time.Time) int {
	count := 0
	for _, t := range times {
		if now.Sub(t) < time.Hour {
			count++
		}
	}
	return count
}

// pruneOpenAITiboWindow drops entries older than an hour, compacting the
// backing array in place: always assign the result back.
func pruneOpenAITiboWindow(times []time.Time, now time.Time) []time.Time {
	cut := 0
	for cut < len(times) && now.Sub(times[cut]) >= time.Hour {
		cut++
	}
	if cut == 0 {
		return times
	}
	return append(times[:0], times[cut:]...)
}

// DisableOpenAITiboRouteForTest keeps unit fixtures on the pre-Tibo path.
func (s *OpenAIGatewayService) DisableOpenAITiboRouteForTest() {
	if s != nil {
		s.tiboRouteDisabled = true
	}
}

func (s *OpenAIGatewayService) openAITiboRouteApplies(account *Account) bool {
	if s == nil || s.tiboRouteDisabled || account == nil {
		return false
	}
	if s.cfg != nil && s.cfg.Gateway.OpenAITiboRoute.Enabled != nil && !*s.cfg.Gateway.OpenAITiboRoute.Enabled {
		return false
	}
	return isOpenAITiboAccount(account)
}

func (s *OpenAIGatewayService) loadOpenAITiboAccountState(accountID int64) *openAITiboAccountState {
	raw, ok := s.openaiTiboRoutes.Load(accountID)
	if !ok {
		return nil
	}
	state, _ := raw.(*openAITiboAccountState)
	return state
}

func (s *OpenAIGatewayService) openAITiboAccountState(accountID int64) *openAITiboAccountState {
	if state := s.loadOpenAITiboAccountState(accountID); state != nil {
		return state
	}
	raw, _ := s.openaiTiboRoutes.LoadOrStore(accountID, &openAITiboAccountState{accountID: accountID})
	state, _ := raw.(*openAITiboAccountState)
	return state
}

// openAITiboEffective reads the effective verdict of one route without side
// effects (scheduler and status views).
func (s *OpenAIGatewayService) openAITiboEffective(accountID int64, route openAITiboRoute, now time.Time) openAITiboVerdict {
	state := s.loadOpenAITiboAccountState(accountID)
	if state == nil {
		return openAITiboUnknown
	}
	cfg := s.openAITiboRouteConfig()
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.route(route).effective(now, cfg)
}

// openAITiboRequestVerdicts is the request-path read: it marks the account
// active, seeds persisted verdicts, waits (bounded) only for an account whose
// HTTP route has never been sampled, and queues due probes without blocking.
func (s *OpenAIGatewayService) openAITiboRequestVerdicts(ctx context.Context, account *Account) openAITiboVerdict {
	cfg := s.openAITiboRouteConfig()
	state := s.openAITiboAccountState(account.ID)
	now := time.Now()
	state.mu.Lock()
	state.lastUsedAt = now
	s.seedOpenAITiboStateLocked(state, account.Extra, now, cfg)
	var wait <-chan struct{}
	if skip := openAITiboAccountSkipReason(account, now); skip == "" {
		if state.http.cold() {
			if state.http.inflight {
				wait = state.http.done
			} else {
				wait = s.startOpenAITiboProbeLocked(state, openAITiboRouteHTTP, account, true, cfg, now)
			}
		} else if state.http.due(now) {
			s.startOpenAITiboProbeLocked(state, openAITiboRouteHTTP, account, false, cfg, now)
		}
	}
	// A skipped request snapshot (possibly a stale scheduler copy) only skips
	// the probe; holds come from the database via enrollment and the
	// background probe reload, never from this copy.
	state.mu.Unlock()
	if wait != nil {
		timer := time.NewTimer(openAITiboProbeWait)
		select {
		case <-wait:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	now = time.Now()
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.http.effective(now, cfg)
}

// probeOpenAITiboHTTP sends the Tibo probe once over the account's own proxy
// and token (not the Cookie harvest proxy). It never changes account state.
func (s *OpenAIGatewayService) probeOpenAITiboHTTP(ctx context.Context, account *Account) openAITiboProbeSample {
	unknown := openAITiboProbeSample{verdict: openAITiboUnknown}
	if s == nil || s.httpUpstream == nil || account == nil {
		return unknown
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil || token == "" {
		return unknown
	}
	body, err := json.Marshal(openAITiboProbePayload())
	if err != nil {
		return unknown
	}
	targetURL := chatgptCodexURL
	if account.IsOpenAIApiKey() {
		targetURL = buildOpenAIResponsesURL(account.GetOpenAIBaseURL())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return unknown
	}
	if !account.IsOpenAIApiKey() {
		req.Host = "chatgpt.com"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	applyOpenAITiboProbeIdentity(req.Header)
	if !account.IsOpenAIApiKey() {
		if err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, req.Header, account); err != nil {
			return unknown
		}
	}
	resp, err := s.httpUpstream.Do(req, openAITiboAccountProxyURL(account), account.ID, account.Concurrency)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil || resp == nil || resp.Body == nil {
		if resp != nil {
			unknown.status = resp.StatusCode
		}
		return unknown
	}
	unknown.status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		unknown.retryAt = parseRetryAfterResetTime(resp.Header, time.Now())
		return unknown
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, openAITiboProbeBodyLimit+1))
	if err != nil {
		return unknown
	}
	return openAITiboSampleFromObservation(resp.StatusCode, observeOpenAITiboHTTPProbe(data))
}

// openAITiboSampleFromObservation classifies a completed probe stream: True
// from astra is healthy; any other successful completion (False, "不知道",
// refusal or another model) is degraded; everything else is unknown.
func openAITiboSampleFromObservation(status int, observation *openAITiboHTTPObservation) openAITiboProbeSample {
	sample := openAITiboProbeSample{verdict: openAITiboUnknown, status: status, answerClass: observation.answerClass}
	switch {
	case observation.completed && observation.modelMatch && observation.trueAnswer && !observation.failed:
		sample.verdict = openAITiboHealthy
	case observation.completed && observation.successfulCompletion:
		sample.verdict = openAITiboDegraded
	}
	return sample
}
