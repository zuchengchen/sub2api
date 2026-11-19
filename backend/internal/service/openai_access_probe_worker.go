package service

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// A single worker shares the process's credential rejection map. A marker
// owns one schedule for its lifetime, regardless of request traffic.
const (
	openAIAccessProbeInterval = time.Second
	openAIAccessProbeTimeout  = 30 * time.Second
)

var openAIAccessProbeBackoff = [...]time.Duration{30 * time.Second, 2 * time.Minute}

type openAIAccessProbeOutcome uint8

const (
	openAIAccessProbeRetry openAIAccessProbeOutcome = iota
	openAIAccessProbeVerified
	openAIAccessProbeUnauthorized
	openAIAccessProbeSkipped
)

type openAIAccessProbeResult struct {
	outcome       openAIAccessProbeOutcome
	authorization string // Actual final request, never retained in the failure map.
}

// Created with a rejection. After publication only the single verification
// worker reads or writes these fields. next is zero after a terminal outcome.
type openAIAccessProbeSchedule struct {
	next    time.Time
	retries int
}

type openAIAccessProber func(context.Context, *Account) openAIAccessProbeResult

type openAIAccessVerifier struct {
	wake   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

type openAIAccessProbeRecoveryRepository interface {
	ClearOpenAIOAuthRefreshErrorIfCredentialsUnchanged(context.Context, int64, map[string]any, *int64, string) (bool, error)
}

// StartOpenAIAccessVerification also picks up rejections published before
// the service graph finished starting. Stop is final for this instance.
func (api *OAuthRefreshAPI) StartOpenAIAccessVerification(tests *AccountTestService) {
	if tests != nil {
		api.startOpenAIAccessVerification(tests.probeRejectedOpenAIAccess)
	}
}

func (api *OAuthRefreshAPI) startOpenAIAccessVerification(probe openAIAccessProber) {
	if api == nil || api.accountRepo == nil || probe == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &openAIAccessVerifier{wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel}
	if !api.accessVerifier.CompareAndSwap(nil, w) {
		cancel()
		return
	}
	go func() {
		defer close(w.done)
		api.restoreOpenAIAccessProbes(ctx)
		api.runOpenAIAccessVerification(ctx, w.wake, probe)
	}()
}

func (api *OAuthRefreshAPI) StopOpenAIAccessVerification() {
	if api != nil {
		if w := api.accessVerifier.Load(); w != nil {
			w.cancel()
			<-w.done
		}
	}
}

func (api *OAuthRefreshAPI) nextOpenAIAccessProbe() (id int64, failure *openAIRefreshRejection) {
	api.openAIFailures.Range(func(key, value any) bool {
		r := value.(*openAIRefreshRejection)
		if r.probe != nil && !r.probe.next.IsZero() && (failure == nil || r.probe.next.Before(failure.probe.next)) {
			id, failure = key.(int64), r
		}
		return true
	})
	return
}

func (api *OAuthRefreshAPI) runOpenAIAccessVerification(ctx context.Context, wake <-chan struct{}, probe openAIAccessProber) {
	var available time.Time
	for ctx.Err() == nil {
		id, failure := api.nextOpenAIAccessProbe()
		var timer <-chan time.Time // nil when the queue is empty
		if failure != nil {
			schedule := failure.probe
			next := schedule.next
			if available.After(next) {
				next = available
			}
			if wait := time.Until(next); wait <= 0 {
				outcome, deferred := api.runOpenAIAccessProbe(ctx, id, failure, probe)
				available = time.Now().Add(openAIAccessProbeInterval)
				// A new denial for this credential invalidates a result in
				// flight but retains its rate limit and remaining retry budget.
				if current, ok := api.openAIFailures.Load(id); ok && current != failure && current.(*openAIRefreshRejection).probe == schedule {
					outcome = openAIAccessProbeRetry
				}
				switch {
				case !deferred.IsZero():
					schedule.next = deferred
				case outcome == openAIAccessProbeRetry && schedule.retries < len(openAIAccessProbeBackoff):
					schedule.next = time.Now().Add(openAIAccessProbeBackoff[schedule.retries])
					schedule.retries++
				default:
					schedule.next = time.Time{}
				}
				continue
			} else {
				timer = time.After(wait)
			}
		}
		select {
		case <-ctx.Done():
		case <-wake:
		case <-timer:
		}
	}
}

func isOpenAIRefreshProbeAccount(account *Account, failure *openAIRefreshRejection) bool {
	if failure == nil || failure.err == nil || !canRecoverOpenAIOAuth(account) || account.ID <= 0 || !account.Schedulable ||
		account.GetOpenAIAccessToken() == "" || failure.credentials != openAIRefreshFingerprint(account) {
		return false
	}
	if !account.IsActive() && !(account.Status == StatusError && account.ErrorMessage == nonRetryableTokenRefreshMessage(failure.err)) {
		return false
	}
	now := time.Now()
	expiry := openAIAccessTokenExpiresAt(account)
	return expiry != nil && now.Before(*expiry) && !(account.AutoPauseOnExpired && account.ExpiresAt != nil && !now.Before(*account.ExpiresAt))
}

// A cooling account retains a single deferred job. Only the exact refresh
// error may be cleared on success; all quota/traffic gates remain independent.
func openAIAccessProbeCooldown(account *Account) time.Time {
	var until time.Time
	for _, t := range []*time.Time{account.RateLimitResetAt, account.TempUnschedulableUntil, account.OverloadUntil} {
		if t != nil && t.After(until) {
			until = *t
		}
	}
	return until
}

func (api *OAuthRefreshAPI) runOpenAIAccessProbe(parent context.Context, id int64, failure *openAIRefreshRejection, probe openAIAccessProber) (openAIAccessProbeOutcome, time.Time) {
	ctx, cancel := context.WithTimeout(parent, openAIAccessProbeTimeout)
	defer cancel()
	account, err := api.accountRepo.GetByID(ctx, id)
	if err != nil && !errors.Is(err, ErrAccountNotFound) {
		return openAIAccessProbeRetry, time.Time{}
	}
	if account == nil || account.ID != id || api.openAIRejectionForAccount(account) != failure || !isOpenAIRefreshProbeAccount(account, failure) {
		return openAIAccessProbeSkipped, time.Time{}
	}
	if until := openAIAccessProbeCooldown(account); until.After(time.Now()) {
		if expiry := openAIAccessTokenExpiresAt(account); until.Before(*expiry) {
			return openAIAccessProbeSkipped, until
		}
		return openAIAccessProbeSkipped, time.Time{}
	}
	result := probe(ctx, account)
	if ctx.Err() != nil {
		return openAIAccessProbeRetry, time.Time{}
	}
	switch result.outcome {
	case openAIAccessProbeVerified:
		result.outcome = api.verifyOpenAIAccess(ctx, account, failure, result.authorization, true, nil)
		if result.outcome == openAIAccessProbeVerified {
			slog.Info("openai_access_probe_verified", "account_id", id)
		}
	case openAIAccessProbeUnauthorized:
		// Authenticate the final bearer again here before revoking a
		// concurrent manual success for this same version.
		result.outcome = api.rejectOpenAIAccess(ctx, account, result.authorization, true)
		if result.outcome == openAIAccessProbeUnauthorized {
			slog.Info("openai_access_probe_unauthorized", "account_id", id)
		}
	}
	return result.outcome, time.Time{}
}
