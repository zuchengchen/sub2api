package service

import (
	"context"
	"encoding/json"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Background Tibo probing hangs off the harvester tick. Eligible ChatGPT
// OAuth accounts are enrolled and probed when due; each (account, route)
// probes at most once at a time, within max_probes_per_hour, and the process
// runs at most probe_concurrency probes.
const (
	openAITiboVerdictExtraKeyPrefix = "codex_tibo_verdict:"
	// A probe that could not start (semaphore wait timeout, account no longer
	// eligible) retries after this delay without counting as a sample.
	openAITiboProbeAbortRetry = time.Minute
	openAITiboPinPruneEvery   = time.Minute
)

func (s *OpenAIGatewayService) openAITiboProbeSem() chan struct{} {
	s.openaiTiboProbeSemOnce.Do(func() {
		s.openaiTiboProbeSem = make(chan struct{}, s.openAITiboRouteConfig().probeConcurrency)
	})
	return s.openaiTiboProbeSem
}

// startOpenAITiboProbeLocked starts one probe for route unless one is already
// running. It returns the channel closed when the probe finishes, or nil when
// no probe started (budget exhausted, or a non-blocking start found the
// process-wide semaphore full). account may be nil for background probes; the
// probe then reloads the authoritative account. Caller holds state.mu.
func (s *OpenAIGatewayService) startOpenAITiboProbeLocked(state *openAITiboAccountState, route openAITiboRoute, account *Account, blocking bool, cfg openAITiboSettings, now time.Time) <-chan struct{} {
	return s.startOpenAITiboProbeCtxLocked(context.Background(), state, route, account, blocking, cfg, now)
}

func (s *OpenAIGatewayService) startOpenAITiboProbeCtxLocked(parent context.Context, state *openAITiboAccountState, route openAITiboRoute, account *Account, blocking bool, cfg openAITiboSettings, now time.Time) <-chan struct{} {
	r := state.route(route)
	if r.inflight {
		return r.done
	}
	sem := s.openAITiboProbeSem()
	acquired := false
	if !blocking {
		select {
		case sem <- struct{}{}:
			acquired = true
		default:
			// The next tick retries; never block a request or the tick.
			return nil
		}
	}
	if !r.takeBudget(now, cfg) {
		if acquired {
			<-sem
		}
		return nil
	}
	var snapshot *Account
	if account != nil {
		copied := *account
		copied.Extra = maps.Clone(account.Extra)
		copied.Credentials = maps.Clone(account.Credentials)
		snapshot = &copied
	}
	done := make(chan struct{})
	r.inflight, r.done = true, done
	s.openaiTiboProbeWG.Add(1)
	go func() {
		defer s.openaiTiboProbeWG.Done()
		s.runOpenAITiboProbe(parent, state, route, snapshot, acquired)
	}()
	return done
}

// runOpenAITiboProbe runs detached from any request (a cancelled request must
// not discard the verdict); parent is the harvester context for tick probes
// and context.Background otherwise.
func (s *OpenAIGatewayService) runOpenAITiboProbe(parent context.Context, state *openAITiboAccountState, route openAITiboRoute, account *Account, acquired bool) {
	ctx, cancel := context.WithTimeout(parent, openAITiboProbeTimeout)
	defer cancel()
	if parent.Err() != nil {
		if acquired {
			<-s.openAITiboProbeSem()
		}
		s.abortOpenAITiboProbe(state, route)
		return
	}
	sem := s.openAITiboProbeSem()
	if !acquired {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			s.abortOpenAITiboProbe(state, route)
			return
		}
	}
	defer func() { <-sem }()
	if account == nil {
		latest, err := s.latestOpenAITiboAccount(ctx, state.accountID)
		if err != nil || !s.openAITiboRouteApplies(latest) {
			// Paused, rate-limited or disabled accounts are not sampled; their
			// verdict ages toward max_stale.
			s.abortOpenAITiboProbe(state, route)
			return
		}
		account = latest
	}
	sample := s.probeOpenAITiboHTTP(ctx, account)
	if parent.Err() != nil {
		// Harvester shutdown interrupted the probe; this is not a sample.
		s.abortOpenAITiboProbe(state, route)
		return
	}
	s.recordOpenAITiboSample(state, route, account, sample)
}

func (s *OpenAIGatewayService) abortOpenAITiboProbe(state *openAITiboAccountState, route openAITiboRoute) {
	state.mu.Lock()
	r := state.route(route)
	if n := len(r.probes); n > 0 {
		// Only one probe per route runs, so the last start is this one.
		r.probes = r.probes[:n-1]
	}
	done := r.done
	r.inflight, r.done = false, nil
	if retry := time.Now().Add(openAITiboProbeAbortRetry); retry.After(r.nextProbeAt) {
		r.nextProbeAt = retry
	}
	state.mu.Unlock()
	if done != nil {
		close(done)
	}
}

// recordOpenAITiboSample applies one probe sample: vote, schedule, persist a
// changed or aging confirmed verdict, and log flips.
func (s *OpenAIGatewayService) recordOpenAITiboSample(state *openAITiboAccountState, route openAITiboRoute, account *Account, sample openAITiboProbeSample) {
	cfg := s.openAITiboRouteConfig()
	now := time.Now()
	state.mu.Lock()
	r := state.route(route)
	before := r.effective(now, cfg)
	transition := r.observe(sample, now, cfg)
	r.scheduleNext(route, now, cfg)
	done := r.done
	r.inflight, r.done = false, nil
	if transition.flipped && transition.from != "" {
		r.flips = append(pruneOpenAITiboWindow(r.flips, now), now)
	}
	after := r.effective(now, cfg)
	persist := openAITiboShouldPersist(r, transition, now, cfg)
	if persist {
		r.persistedAt = now
	}
	record := openAITiboVerdictRecordFor(r)
	flipsHour, probesHour := countOpenAITiboWindow(r.flips, now), countOpenAITiboWindow(r.probes, now)
	state.mu.Unlock()
	if done != nil {
		close(done)
	}
	s.openaiTiboStats.noteProbe(transition.flipped && transition.from != "")
	fields := []zap.Field{
		zap.Int64("account_id", state.accountID), zap.String("route", string(route)),
		zap.String("sample", string(sample.verdict)), zap.Int("http", sample.status), zap.String("answer_class", sample.answerClass),
		zap.String("effective_before", string(before)), zap.String("effective_after", string(after)),
		zap.Int("flips_hour", flipsHour), zap.Int("probes_hour", probesHour),
	}
	switch {
	case transition.flipped:
		votes := make([]string, 0, len(transition.votes))
		for _, vote := range transition.votes {
			votes = append(votes, string(vote))
		}
		logger.L().Info("openai_tibo verdict flip", append(fields,
			zap.String("from", string(transition.from)), zap.String("to", string(transition.to)), zap.Strings("votes", votes))...)
	case before != after:
		logger.L().Info("openai_tibo effective verdict", fields...)
	}
	if persist {
		s.persistOpenAITiboVerdict(state.accountID, route, record)
	}
}

// openAITiboShouldPersist writes confirmed flips, and refreshes an unchanged
// verdict once it has aged half of max_stale, so a restart keeps it usable.
func openAITiboShouldPersist(r *openAITiboRouteState, transition openAITiboTransition, now time.Time, cfg openAITiboSettings) bool {
	if !r.verdict.definitive() {
		return false
	}
	if transition.flipped {
		return true
	}
	return r.checkedAt.After(r.persistedAt) && now.Sub(r.persistedAt) >= cfg.maxStale/2
}

type openAITiboVerdictRecord struct {
	Verdict   openAITiboVerdict `json:"verdict"`
	Model     string            `json:"model"`
	CheckedAt time.Time         `json:"checked_at"`
	FlippedAt time.Time         `json:"flipped_at"`
}

func openAITiboVerdictRecordFor(r *openAITiboRouteState) openAITiboVerdictRecord {
	return openAITiboVerdictRecord{Verdict: r.verdict, Model: openAICodexTicketDefaultModel, CheckedAt: r.checkedAt, FlippedAt: r.flippedAt}
}

func openAITiboVerdictExtraKey(route openAITiboRoute) string {
	return openAITiboVerdictExtraKeyPrefix + string(route)
}

func parseOpenAITiboVerdictRecord(raw any) (openAITiboVerdictRecord, bool) {
	var record openAITiboVerdictRecord
	if raw == nil {
		return record, false
	}
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &record) != nil || !record.Verdict.definitive() || record.CheckedAt.IsZero() {
		return openAITiboVerdictRecord{}, false
	}
	return record, true
}

func (s *OpenAIGatewayService) persistOpenAITiboVerdict(accountID int64, route openAITiboRoute, record openAITiboVerdictRecord) {
	if s == nil || s.accountRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	value := map[string]any{
		"verdict": string(record.Verdict), "model": record.Model,
		"checked_at": record.CheckedAt.UTC().Format(time.RFC3339Nano), "flipped_at": record.FlippedAt.UTC().Format(time.RFC3339Nano),
	}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{openAITiboVerdictExtraKey(route): value}); err != nil {
		logger.L().Warn("openai_tibo verdict persist failed", zap.Int64("account_id", accountID), zap.String("route", string(route)), zap.Error(err))
	}
}

// seedOpenAITiboStateLocked adopts persisted verdicts for routes this process
// has not sampled yet, subject to max_stale. Caller holds state.mu.
func (s *OpenAIGatewayService) seedOpenAITiboStateLocked(state *openAITiboAccountState, extra map[string]any, now time.Time, cfg openAITiboSettings) {
	if len(extra) == 0 {
		return
	}
	route := openAITiboRouteHTTP
	r := state.route(route)
	if !r.cold() || r.inflight {
		return
	}
	record, ok := parseOpenAITiboVerdictRecord(extra[openAITiboVerdictExtraKey(route)])
	if !ok || record.CheckedAt.After(now) || now.Sub(record.CheckedAt) > cfg.maxStale {
		return
	}
	r.verdict, r.checkedAt, r.flippedAt, r.persistedAt = record.Verdict, record.CheckedAt, record.FlippedAt, record.CheckedAt
	r.nextProbeAt = record.CheckedAt.Add(openAITiboCadenceDelay(cfg))
}

const openAITiboEnrollInterval = time.Minute

// enrollOpenAITiboAccounts loads eligible ChatGPT OAuth accounts into the
// probe map, seeds persisted verdicts, and staggers first nextProbeAt so a
// restart does not fire every account at once. Tests that set
// openaiTiboLoaded skip the listing path and inject state themselves.
func (s *OpenAIGatewayService) enrollOpenAITiboAccounts(ctx context.Context) {
	if s == nil || s.accountRepo == nil || s.openaiTiboLoaded.Load() {
		return
	}
	now := time.Now()
	last := s.openaiTiboEnrolledAt.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < openAITiboEnrollInterval {
		return
	}
	if !s.openaiTiboEnrolledAt.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	defer func() { _ = recover() }()
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		s.openaiTiboEnrolledAt.Store(last)
		return
	}
	cfg := s.openAITiboRouteConfig()
	enrolled := 0
	for i := range accounts {
		account := &accounts[i]
		if !s.openAITiboRouteApplies(account) || openAITiboAccountSkipReason(account, now) != "" {
			continue
		}
		state := s.openAITiboAccountState(account.ID)
		state.mu.Lock()
		firstSeen := state.http.nextProbeAt.IsZero() && state.http.lastSampleAt.IsZero() && state.http.verdict == ""
		s.seedOpenAITiboStateLocked(state, account.Extra, now, cfg)
		if firstSeen && (state.http.nextProbeAt.IsZero() || !state.http.nextProbeAt.After(now)) {
			state.http.nextProbeAt = now.Add(openAITiboStartupDelay(cfg))
		}
		enrolled++
		state.mu.Unlock()
	}
	if enrolled > 0 {
		logger.L().Info("openai_tibo accounts enrolled", zap.Int("accounts", enrolled))
	}
}

// latestOpenAITiboAccount reloads the account for a background probe. It
// applies the same pause/rate-limit skip as Cookie harvest.
func (s *OpenAIGatewayService) latestOpenAITiboAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil || accountID <= 0 {
		return nil, errOpenAITiboAccountUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	account, err := s.accountRepo.GetByID(readCtx, accountID)
	if err != nil || account == nil || account.ID != accountID {
		return nil, errOpenAITiboAccountUnavailable
	}
	current := *account
	current.Extra = maps.Clone(account.Extra)
	current.Credentials = maps.Clone(account.Credentials)
	if !s.openAITiboRouteApplies(&current) || openAITiboAccountSkipReason(&current, time.Now()) != "" {
		return nil, errOpenAITiboAccountUnavailable
	}
	return &current, nil
}

// probeOpenAITiboRoutes is the harvester-tick hook: probe due HTTP routes of
// enrolled Tibo-routed accounts in the background, including idle ones.
func (s *OpenAIGatewayService) probeOpenAITiboRoutes(ctx context.Context) {
	if s == nil || s.tiboRouteDisabled || ctx.Err() != nil || s.httpUpstream == nil {
		return
	}
	s.enrollOpenAITiboAccounts(ctx)
	cfg := s.openAITiboRouteConfig()
	now := time.Now()
	s.pruneOpenAITiboPins(now)
	s.openaiTiboStats.rollover(now)
	s.openaiTiboRoutes.Range(func(_, value any) bool {
		state, ok := value.(*openAITiboAccountState)
		if !ok || state == nil {
			return true
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.http.due(now) {
			s.startOpenAITiboProbeCtxLocked(ctx, state, openAITiboRouteHTTP, nil, false, cfg, now)
		}
		return true
	})
}

// openAITiboStats counts process-wide Tibo routing activity per hour.
type openAITiboStats struct {
	mu          sync.Mutex
	hour        time.Time
	probes      int
	flips       int
	served      int
	degraded    int
	lastAlertAt time.Time
}

func (st *openAITiboStats) rolloverLocked(now time.Time) {
	hour := now.Truncate(time.Hour)
	if st.hour.Equal(hour) {
		return
	}
	if !st.hour.IsZero() && (st.probes > 0 || st.served > 0) {
		logger.L().Info("openai_tibo hourly summary", zap.Time("hour", st.hour), zap.Int("probes", st.probes),
			zap.Int("flips", st.flips), zap.Int("served", st.served), zap.Int("degraded_served", st.degraded))
	}
	st.hour, st.probes, st.flips, st.served, st.degraded = hour, 0, 0, 0, 0
}

func (st *openAITiboStats) rollover(now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rolloverLocked(now)
}

func (st *openAITiboStats) noteProbe(flipped bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rolloverLocked(time.Now())
	st.probes++
	if flipped {
		st.flips++
	}
}

func (st *openAITiboStats) noteFlip() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rolloverLocked(time.Now())
	st.flips++
}

// noteServed counts one Tibo-routed request and warns (at most every ten
// minutes) when the degraded share of this hour exceeds the configured ratio.
func (st *openAITiboStats) noteServed(degraded bool, cfg openAITiboSettings) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	st.rolloverLocked(now)
	st.served++
	if degraded {
		st.degraded++
	}
	if cfg.alertRatio <= 0 || st.served < cfg.alertMinRequests {
		return
	}
	ratio := float64(st.degraded) / float64(st.served)
	if ratio <= cfg.alertRatio || now.Sub(st.lastAlertAt) < 10*time.Minute {
		return
	}
	st.lastAlertAt = now
	logger.L().Warn("openai_tibo degraded ratio high", zap.Float64("ratio", ratio), zap.Float64("threshold", cfg.alertRatio),
		zap.Int("served", st.served), zap.Int("degraded_served", st.degraded), zap.Time("hour", st.hour))
}

// OpenAITiboRouteStatus is the admin view of one route's Tibo state.
type OpenAITiboRouteStatus struct {
	Route        string     `json:"route"`
	Verdict      string     `json:"verdict"`
	Confirmed    string     `json:"confirmed,omitempty"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
	FlippedAt    *time.Time `json:"flipped_at,omitempty"`
	NextProbeAt  *time.Time `json:"next_probe_at,omitempty"`
	PendingVotes []string   `json:"pending_votes,omitempty"`
	LastSample   string     `json:"last_sample,omitempty"`
	LastHTTP     int        `json:"last_http,omitempty"`
	LastAnswer   string     `json:"last_answer,omitempty"`
	ProbesHour   int        `json:"probes_hour"`
	FlipsHour    int        `json:"flips_hour"`
	// Calibration counters since process start. A vote window opens on a
	// sample disagreeing with the confirmed verdict; confirmed means it
	// flipped, rejected means later samples outvoted it (sample noise).
	VoteWindows   int `json:"vote_windows"`
	VoteConfirmed int `json:"vote_confirmed"`
	VoteRejected  int `json:"vote_rejected"`
}

func openAITiboTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// openAITiboRouteStatuses returns HTTP verdict state for the admin view.
// It never starts probes.
func (s *OpenAIGatewayService) openAITiboRouteStatuses(account *Account, now time.Time) []OpenAITiboRouteStatus {
	if s == nil || account == nil || !s.openAITiboRouteApplies(account) {
		return nil
	}
	cfg := s.openAITiboRouteConfig()
	routes := []openAITiboRoute{openAITiboRouteHTTP}
	out := make([]OpenAITiboRouteStatus, 0, len(routes))
	state := s.loadOpenAITiboAccountState(account.ID)
	for _, route := range routes {
		item := OpenAITiboRouteStatus{Route: string(route), Verdict: string(openAITiboUnknown)}
		if state != nil {
			state.mu.Lock()
			r := state.route(route)
			item.Verdict = string(r.effective(now, cfg))
			item.Confirmed = string(r.verdict)
			item.CheckedAt, item.FlippedAt, item.NextProbeAt = openAITiboTimePtr(r.checkedAt), openAITiboTimePtr(r.flippedAt), openAITiboTimePtr(r.nextProbeAt)
			for _, vote := range r.pending {
				item.PendingVotes = append(item.PendingVotes, string(vote))
			}
			if !r.lastSampleAt.IsZero() {
				item.LastSample, item.LastHTTP, item.LastAnswer = string(r.lastSample.verdict), r.lastSample.status, r.lastSample.answerClass
			}
			item.ProbesHour, item.FlipsHour = countOpenAITiboWindow(r.probes, now), countOpenAITiboWindow(r.flips, now)
			item.VoteWindows, item.VoteConfirmed, item.VoteRejected = r.voteWindows, r.voteConfirmed, r.voteRejected
			state.mu.Unlock()
		}
		out = append(out, item)
	}
	return out
}

// observeOpenAITiboResponseModel is the passive model check at the end of a
// business response. An astra request answered by another model is hard
// evidence: the route turns degraded without a vote and a confirmation probe
// is queued. Other models are only logged for now. It returns true for an
// astra mismatch so Cookie WS callers can retire the socket.
func (s *OpenAIGatewayService) observeOpenAITiboResponseModel(account *Account, route openAITiboRoute, sentModel, responseModel string) bool {
	sentModel, responseModel = strings.TrimSpace(sentModel), strings.TrimSpace(responseModel)
	if s == nil || account == nil || sentModel == "" || responseModel == "" || upstreamModelsMatchForAudit(sentModel, responseModel) {
		return false
	}
	astra := sentModel == openAICodexTicketDefaultModel
	s.logOpenAITiboModelMismatch(account.ID, route, sentModel, responseModel, astra)
	if !astra {
		return false
	}
	if route != openAITiboRouteCookieWS && s.openAITiboRouteApplies(account) {
		s.forceOpenAITiboDegraded(account, route)
	}
	return true
}

func (s *OpenAIGatewayService) forceOpenAITiboDegraded(account *Account, route openAITiboRoute) {
	cfg := s.openAITiboRouteConfig()
	state := s.openAITiboAccountState(account.ID)
	now := time.Now()
	state.mu.Lock()
	r := state.route(route)
	before := r.effective(now, cfg)
	changed := r.forceDegraded(now)
	if changed {
		r.flips = append(pruneOpenAITiboWindow(r.flips, now), now)
		r.persistedAt = now
	}
	record := openAITiboVerdictRecordFor(r)
	if route == openAITiboRouteHTTP {
		r.nextProbeAt = now
		s.startOpenAITiboProbeLocked(state, route, account, false, cfg, now)
	}
	state.mu.Unlock()
	if !changed {
		return
	}
	s.openaiTiboStats.noteFlip()
	logger.L().Info("openai_tibo verdict flip", zap.Int64("account_id", account.ID), zap.String("route", string(route)),
		zap.String("from", string(before)), zap.String("to", string(openAITiboDegraded)), zap.String("cause", "response_model_mismatch"))
	s.persistOpenAITiboVerdict(account.ID, route, record)
}

// openAICookieWSTurnModelMismatch is the Cookie WS passive check on a raw
// upstream event (before any client-facing model rewrite): a successful
// completion of an astra turn by another model means the caller retires this
// socket. Only the exact Tibo probe payload is left to the probe observer.
func (s *OpenAIGatewayService) openAICookieWSTurnModelMismatch(account *Account, sentModel string, payload []byte, eventType string) bool {
	if !s.openAITiboRouteApplies(account) {
		return false
	}
	model, success := openAICodexSuccessfulCompletionModel(payload, eventType)
	if !success {
		return false
	}
	return s.observeOpenAITiboResponseModel(account, openAITiboRouteCookieWS, sentModel, model)
}

func (s *OpenAIGatewayService) logOpenAITiboModelMismatch(accountID int64, route openAITiboRoute, sentModel, responseModel string, astra bool) {
	every := 10 * time.Minute
	if astra {
		every = time.Minute
	}
	key := strings.Join([]string{strconv.FormatInt(accountID, 10), string(route), sentModel, responseModel}, "\x00")
	now := time.Now()
	if raw, ok := s.openaiTiboMismatchLog.Load(key); ok {
		if last, ok := raw.(time.Time); ok && now.Sub(last) < every {
			return
		}
	}
	s.openaiTiboMismatchLog.Store(key, now)
	logger.L().Info("openai_tibo response model mismatch", zap.Int64("account_id", accountID), zap.String("route", string(route)),
		zap.String("sent_model", sentModel), zap.String("response_model", responseModel), zap.Bool("hard_evidence", astra))
}
