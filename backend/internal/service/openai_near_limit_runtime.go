package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	openAINearLimitThresholdPercent          = 95.0
	defaultOpenAINearLimitMaxConcurrency     = 20
	defaultOpenAINearLimit429ProbeBackoffSec = 2
	defaultOpenAINearLimitTTFTThresholdMS    = 15000
	defaultOpenAINearLimitTTFTBackoffSec     = 30
	defaultOpenAIStreamFirstTokenTimeoutSec  = 60
	openAINearLimitProbeLeaseGrace           = 30 * time.Second
)

type openAINearLimitRuntimeState struct {
	backoffUntil    time.Time
	probeLeaseUntil time.Time
	reason          string
}

type openAINearLimitCandidate struct {
	account      *Account
	usedPercent  float64
	resetAt      time.Time
	window       string
}

type skipOpenAINearLimitKey struct{}

// WithSkipOpenAINearLimitAccounts skips chaining additional near-limit accounts
// for this request. Production request paths do not use it.
func WithSkipOpenAINearLimitAccounts(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, skipOpenAINearLimitKey{}, true)
}

func skipOpenAINearLimitAccounts(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(skipOpenAINearLimitKey{}).(bool)
	return v
}

var (
	openAINearLimitMu    sync.Mutex
	openAINearLimitState = map[int64]*openAINearLimitRuntimeState{}
)

func openAINearLimitMaxConcurrency(cfg *config.Config) int {
	if cfg != nil && cfg.Gateway.OpenAINearLimitMaxConcurrency > 0 {
		return cfg.Gateway.OpenAINearLimitMaxConcurrency
	}
	return defaultOpenAINearLimitMaxConcurrency
}

func openAINearLimit429ProbeBackoff(cfg *config.Config) time.Duration {
	seconds := defaultOpenAINearLimit429ProbeBackoffSec
	if cfg != nil && cfg.Gateway.OpenAINearLimit429ProbeBackoffSeconds > 0 {
		seconds = cfg.Gateway.OpenAINearLimit429ProbeBackoffSeconds
	}
	return time.Duration(seconds) * time.Second
}

func openAINearLimitTTFTThreshold(cfg *config.Config) time.Duration {
	ms := defaultOpenAINearLimitTTFTThresholdMS
	if cfg != nil && cfg.Gateway.OpenAINearLimitTTFTThresholdMS > 0 {
		ms = cfg.Gateway.OpenAINearLimitTTFTThresholdMS
	}
	return time.Duration(ms) * time.Millisecond
}

func openAINearLimitTTFTBackoff(cfg *config.Config) time.Duration {
	seconds := defaultOpenAINearLimitTTFTBackoffSec
	if cfg != nil && cfg.Gateway.OpenAINearLimitTTFTBackoffSeconds > 0 {
		seconds = cfg.Gateway.OpenAINearLimitTTFTBackoffSeconds
	}
	return time.Duration(seconds) * time.Second
}

func streamFirstTokenTimeout(cfg *config.Config) time.Duration {
	if cfg == nil {
		return time.Duration(defaultOpenAIStreamFirstTokenTimeoutSec) * time.Second
	}
	if cfg.Gateway.StreamFirstTokenTimeout <= 0 {
		return 0
	}
	return time.Duration(cfg.Gateway.StreamFirstTokenTimeout) * time.Second
}

func openAINearLimitProbeLease(cfg *config.Config) time.Duration {
	timeout := streamFirstTokenTimeout(cfg)
	if timeout <= 0 {
		timeout = time.Duration(defaultOpenAIStreamFirstTokenTimeoutSec) * time.Second
	}
	return timeout + openAINearLimitProbeLeaseGrace
}

func effectiveOpenAIAccountConcurrency(account *Account, cfg *config.Config) int {
	if account == nil {
		return openAINearLimitMaxConcurrency(cfg)
	}
	capLimit := openAINearLimitMaxConcurrency(cfg)
	effective := account.Concurrency
	if effective <= 0 || effective > capLimit {
		return capLimit
	}
	return effective
}

func effectiveOpenAIAccountLoadFactor(account *Account, cfg *config.Config) int {
	if account == nil {
		return openAINearLimitMaxConcurrency(cfg)
	}
	capLimit := openAINearLimitMaxConcurrency(cfg)
	effective := account.EffectiveLoadFactor()
	if effective <= 0 || effective > capLimit {
		return capLimit
	}
	return effective
}

func classifyOpenAINearLimitCandidate(account *Account, now time.Time) (openAINearLimitCandidate, bool) {
	if account == nil || !account.IsOpenAI() {
		return openAINearLimitCandidate{}, false
	}
	if !openAINearLimitSnapshotFresh(account.Extra, now) {
		return openAINearLimitCandidate{}, false
	}

	best, ok := openAINearLimitWindowHit(account.Extra, "5h", now)
	if seven, sevenOK := openAINearLimitWindowHit(account.Extra, "7d", now); sevenOK {
		if !ok || openAINearLimitHigherPriority(seven, best) {
			best = seven
			ok = true
		}
	}
	if !ok {
		return openAINearLimitCandidate{}, false
	}
	best.account = account
	return best, true
}

func openAINearLimitSnapshotFresh(extra map[string]any, now time.Time) bool {
	if len(extra) == 0 {
		return false
	}
	raw, ok := extra["codex_usage_updated_at"]
	if !ok || raw == nil {
		return false
	}
	updatedAt, err := parseTime(fmt.Sprint(raw))
	if err != nil {
		return false
	}
	if updatedAt.IsZero() {
		return false
	}
	return now.Sub(updatedAt) <= openAICodexAutoPauseStaleAfter
}

func openAINearLimitWindowHit(extra map[string]any, window string, now time.Time) (openAINearLimitCandidate, bool) {
	used := readOpenAIQuotaUsedPercent(extra, window)
	if used < openAINearLimitThresholdPercent {
		return openAINearLimitCandidate{}, false
	}
	if openAIQuotaWindowReset(extra, window, now) {
		return openAINearLimitCandidate{}, false
	}
	resetAt := openAINearLimitResetAt(extra, window, now)
	return openAINearLimitCandidate{usedPercent: used, resetAt: resetAt, window: window}, true
}

func openAINearLimitResetAt(extra map[string]any, window string, now time.Time) time.Time {
	if raw, ok := extra["codex_"+window+"_reset_at"]; ok && raw != nil {
		if resetAt, err := parseTime(fmt.Sprint(raw)); err == nil {
			return resetAt
		}
	}
	resetAfter := parseExtraInt(extra["codex_"+window+"_reset_after_seconds"])
	if resetAfter > 0 {
		base := now
		if updatedRaw, ok := extra["codex_usage_updated_at"]; ok {
			if updatedAt, err := parseTime(fmt.Sprint(updatedRaw)); err == nil {
				base = updatedAt
			}
		}
		return base.Add(time.Duration(resetAfter) * time.Second)
	}
	return time.Time{}
}

func openAINearLimitHigherPriority(a, b openAINearLimitCandidate) bool {
	if a.usedPercent != b.usedPercent {
		return a.usedPercent > b.usedPercent
	}
	if !a.resetAt.Equal(b.resetAt) {
		if a.resetAt.IsZero() {
			return false
		}
		if b.resetAt.IsZero() {
			return true
		}
		return a.resetAt.Before(b.resetAt)
	}
	aPri, bPri := 0, 0
	if a.account != nil {
		aPri = a.account.Priority
	}
	if b.account != nil {
		bPri = b.account.Priority
	}
	if aPri != bPri {
		return aPri < bPri
	}
	var aID, bID int64
	if a.account != nil {
		aID = a.account.ID
	}
	if b.account != nil {
		bID = b.account.ID
	}
	return aID < bID
}

func sortOpenAINearLimitCandidates(cands []openAINearLimitCandidate) {
	for i := 1; i < len(cands); i++ {
		j := i
		for j > 0 && openAINearLimitHigherPriority(cands[j], cands[j-1]) {
			cands[j], cands[j-1] = cands[j-1], cands[j]
			j--
		}
	}
}

// MarkOpenAINearLimit429 records a 2s in-process probe backoff when the account
// is currently a 95% near-limit candidate. It is not a database cooldown.
func MarkOpenAINearLimit429(account *Account) {
	if account == nil {
		return
	}
	now := time.Now()
	if _, ok := classifyOpenAINearLimitCandidate(account, now); !ok {
		return
	}
	openAINearLimitMu.Lock()
	defer openAINearLimitMu.Unlock()
	st := openAINearLimitState[account.ID]
	if st == nil {
		st = &openAINearLimitRuntimeState{}
		openAINearLimitState[account.ID] = st
	}
	st.backoffUntil = now.Add(openAINearLimit429ProbeBackoff(nil))
	st.reason = "429"
}

func markOpenAINearLimit429WithConfig(account *Account, cfg *config.Config) {
	if account == nil {
		return
	}
	now := time.Now()
	if _, ok := classifyOpenAINearLimitCandidate(account, now); !ok {
		return
	}
	openAINearLimitMu.Lock()
	defer openAINearLimitMu.Unlock()
	st := openAINearLimitState[account.ID]
	if st == nil {
		st = &openAINearLimitRuntimeState{}
		openAINearLimitState[account.ID] = st
	}
	st.backoffUntil = now.Add(openAINearLimit429ProbeBackoff(cfg))
	st.reason = "429"
}

func MarkOpenAINearLimitFirstTokenTimeout(account *Account) {
	if account == nil {
		return
	}
	now := time.Now()
	if _, ok := classifyOpenAINearLimitCandidate(account, now); !ok {
		return
	}
	openAINearLimitMu.Lock()
	defer openAINearLimitMu.Unlock()
	st := openAINearLimitState[account.ID]
	if st == nil {
		st = &openAINearLimitRuntimeState{}
		openAINearLimitState[account.ID] = st
	}
	st.backoffUntil = now.Add(openAINearLimitTTFTBackoff(nil))
	st.reason = "first_token_timeout"
}

func updateOpenAINearLimitTTFTState(account *Account, firstToken time.Duration, cfg *config.Config) {
	if account == nil {
		return
	}
	now := time.Now()
	if _, ok := classifyOpenAINearLimitCandidate(account, now); !ok {
		return
	}
	threshold := openAINearLimitTTFTThreshold(cfg)
	openAINearLimitMu.Lock()
	defer openAINearLimitMu.Unlock()
	if firstToken <= threshold {
		delete(openAINearLimitState, account.ID)
		return
	}
	st := openAINearLimitState[account.ID]
	if st == nil {
		st = &openAINearLimitRuntimeState{}
		openAINearLimitState[account.ID] = st
	}
	st.backoffUntil = now.Add(openAINearLimitTTFTBackoff(cfg))
	st.reason = "ttft"
}

func canClaimOpenAINearLimitPreference(accountID int64, now time.Time, checkBackoff bool) bool {
	openAINearLimitMu.Lock()
	defer openAINearLimitMu.Unlock()
	st := openAINearLimitState[accountID]
	if st == nil {
		return true
	}
	if !st.probeLeaseUntil.IsZero() && now.Before(st.probeLeaseUntil) {
		return false
	}
	if checkBackoff && !st.backoffUntil.IsZero() && now.Before(st.backoffUntil) {
		return false
	}
	return true
}

func claimOpenAINearLimitPreference(accountID int64, lease time.Duration) bool {
	now := time.Now()
	openAINearLimitMu.Lock()
	defer openAINearLimitMu.Unlock()
	st := openAINearLimitState[accountID]
	if st == nil {
		st = &openAINearLimitRuntimeState{}
		openAINearLimitState[accountID] = st
	}
	if !st.probeLeaseUntil.IsZero() && now.Before(st.probeLeaseUntil) {
		return false
	}
	st.probeLeaseUntil = now.Add(lease)
	return true
}

func releaseOpenAINearLimitPreference(accountID int64) {
	openAINearLimitMu.Lock()
	defer openAINearLimitMu.Unlock()
	st := openAINearLimitState[accountID]
	if st == nil {
		return
	}
	st.probeLeaseUntil = time.Time{}
}

func logOpenAINearLimitPrioritized(cand openAINearLimitCandidate) {
	if cand.account == nil {
		return
	}
	slog.Info("openai_near_limit_prioritized",
		"account_id", cand.account.ID,
		"window", cand.window,
		"used_percent", cand.usedPercent,
		"reset_at", cand.resetAt,
		"result", "slot_acquired",
	)
}


