package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

func (s *OpenAIGatewayService) openAIOAuthCredentialOwner(ctx context.Context, account *Account) (*Account, error) {
	return account, nil
}

type openAIRefreshRejection struct {
	credentials         [sha256.Size]byte
	err                 *openai.RefreshTokenError
	verifiedAccessUntil time.Time
	autoVerified        bool
	probe               *openAIAccessProbeSchedule
}

func openAIRefreshFingerprint(account *Account) [sha256.Size]byte {
	// Do not retain raw tokens in failure state. Include auth identity as well as
	// both tokens so reauthorization, even for the same account, restores access.
	value, _ := json.Marshal([]string{account.Platform, account.Type, account.GetCredential("access_token"), account.GetCredential("refresh_token"), account.GetCredential("client_id"), account.GetCredential("chatgpt_account_id"), account.GetChatGPTUserID(), account.GetCredential(openAIAuthModeCredentialKey), account.GetCredential(openAIAuthModeLegacyCredentialKey)})
	return sha256.Sum256(value)
}

func (api *OAuthRefreshAPI) openAIRejectionForAccount(account *Account) *openAIRefreshRejection {
	if api == nil || account == nil || !account.IsOpenAIOAuth() {
		return nil
	}
	value, ok := api.openAIFailures.Load(account.ID)
	if !ok {
		return nil
	}
	failure, ok := value.(*openAIRefreshRejection)
	if ok && failure.credentials == openAIRefreshFingerprint(account) {
		return failure
	}
	return nil
}

func (api *OAuthRefreshAPI) openAIRefreshFailure(account *Account) *openai.RefreshTokenError {
	if failure := api.openAIRejectionForAccount(account); failure != nil {
		return failure.err
	}
	return nil
}

func (api *OAuthRefreshAPI) recordOpenAIRefreshFailure(account *Account, err *openai.RefreshTokenError) {
	if api == nil || account == nil || !account.IsOpenAIOAuth() || err == nil || !err.Permanent {
		return
	}
	rejection := &openAIRefreshRejection{credentials: openAIRefreshFingerprint(account), err: err}
	if previous := api.openAIRejectionForAccount(account); previous != nil {
		// A duplicate refresh denial supersedes a probe in flight but gets
		// neither a fresh retry budget nor a way around a newer inference 401.
		rejection.probe = previous.probe
		rejection.verifiedAccessUntil = previous.verifiedAccessUntil
		rejection.autoVerified = previous.autoVerified
	} else if isOpenAIRefreshProbeAccount(account, rejection) {
		rejection.probe = &openAIAccessProbeSchedule{next: time.Now()}
	}
	api.openAIFailures.Store(account.ID, rejection)
	if worker := api.accessVerifier.Load(); rejection.probe != nil && worker != nil {
		select {
		case worker.wake <- struct{}{}:
		default:
		}
	}
}

func (r *openAIRefreshRejection) accessVerifiedAt(account *Account, now time.Time) bool {
	if r == nil || !now.Before(r.verifiedAccessUntil) {
		return false
	}
	// An opaque token's stored expiry may have been shortened since the test.
	expiry := openAIAccessTokenExpiresAt(account)
	return expiry != nil && now.Before(*expiry)
}

func sentOpenAIAccessTokenMatches(account *Account, authorization string) bool {
	token := account.GetOpenAIAccessToken()
	return token != "" && authorization == "Bearer "+token
}

func sameOpenAIAccessProbeSnapshot(sent, latest *Account) bool {
	return latest != nil && latest.ID == sent.ID && sameOpenAIOAuthOwner(sent, latest) &&
		valueOrZero(sent.ProxyID) == valueOrZero(latest.ProxyID) && reflect.DeepEqual(sent.Credentials, latest.Credentials)
}

// Keep the refresh rejection even when a completed inference proves this access
// token still works. The proof belongs to the failure observed before sending
// the test, so a newer 401 or a credential replacement wins over a late success.
func (api *OAuthRefreshAPI) verifyOpenAIAccessAfterTest(ctx context.Context, account *Account, before *openAIRefreshRejection, sentAuthorization string, afterVerify func()) bool {
	return api.verifyOpenAIAccess(ctx, account, before, sentAuthorization, false, afterVerify) == openAIAccessProbeVerified
}

func (api *OAuthRefreshAPI) verifyOpenAIAccess(ctx context.Context, account *Account, before *openAIRefreshRejection, sentAuthorization string, automatic bool, afterVerify func()) openAIAccessProbeOutcome {
	if api == nil || api.accountRepo == nil || before == nil || !canRecoverOpenAIOAuth(account) ||
		!sentOpenAIAccessTokenMatches(account, sentAuthorization) ||
		before.credentials != openAIRefreshFingerprint(account) {
		return openAIAccessProbeSkipped
	}
	// Order the successful probe against the background worker that may be
	// publishing an account error for this same refresh rejection.
	mu := api.getLocalLock(OpenAITokenCacheKey(account))
	if err := mu.Lock(ctx); err != nil {
		return openAIAccessProbeRetry
	}
	defer mu.Unlock()
	latest, err := api.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		return openAIAccessProbeRetry
	}
	if latest == nil || latest.ID != account.ID || (!latest.IsActive() && latest.Status != StatusError) ||
		api.openAIRejectionForAccount(latest) != before {
		return openAIAccessProbeSkipped
	}
	expiry := openAIAccessTokenExpiresAt(account)
	currentExpiry := openAIAccessTokenExpiresAt(latest)
	if expiry == nil || currentExpiry == nil {
		return openAIAccessProbeSkipped
	}
	if currentExpiry.Before(*expiry) {
		expiry = currentExpiry
	}
	if ctx.Err() != nil || !time.Now().Before(*expiry) {
		return openAIAccessProbeSkipped
	}
	if automatic {
		if !isOpenAIRefreshProbeAccount(latest, before) || !sameOpenAIAccessProbeSnapshot(account, latest) {
			return openAIAccessProbeSkipped
		}
		if latest.Status == StatusError {
			repo, ok := api.accountRepo.(openAIAccessProbeRecoveryRepository)
			if !ok {
				return openAIAccessProbeSkipped
			}
			// The database also checks the exact document, proxy, scheduling
			// switch and error. Preserve concurrent changes from other processes.
			cleared, err := repo.ClearOpenAIOAuthRefreshErrorIfCredentialsUnchanged(ctx, account.ID, account.Credentials, account.ProxyID, latest.ErrorMessage)
			if err != nil {
				return openAIAccessProbeRetry
			}
			if !cleared {
				return openAIAccessProbeSkipped
			}
		}
	}
	verified := *before
	verified.verifiedAccessUntil = *expiry
	verified.autoVerified = automatic
	verified.probe = nil
	if !api.openAIFailures.CompareAndSwap(account.ID, before, &verified) {
		return openAIAccessProbeSkipped
	}
	if afterVerify != nil {
		afterVerify()
	}
	return openAIAccessProbeVerified
}

// A Responses 401 invalidates both active proof and an older probe in flight.
// Replace the immutable snapshot even if no proof has completed yet.
func (api *OAuthRefreshAPI) rejectVerifiedOpenAIAccess(account *Account, sentAuthorization string) {
	api.rejectOpenAIAccess(context.Background(), account, sentAuthorization, false)
}

func (api *OAuthRefreshAPI) rejectOpenAIAccess(ctx context.Context, account *Account, sentAuthorization string, fromProbe bool) openAIAccessProbeOutcome {
	if api.openAIRejectionForAccount(account) == nil || !sentOpenAIAccessTokenMatches(account, sentAuthorization) {
		return openAIAccessProbeSkipped
	}
	// Serialize a newer inference 401 with the narrow database recovery at
	// the end of a probe. No account lock is held while sending the probe.
	mu := api.getLocalLock(OpenAITokenCacheKey(account))
	if err := mu.Lock(ctx); err != nil {
		return openAIAccessProbeRetry
	}
	defer mu.Unlock()
	if fromProbe {
		latest, err := api.accountRepo.GetByID(ctx, account.ID)
		if err != nil {
			return openAIAccessProbeRetry
		}
		if !sameOpenAIAccessProbeSnapshot(account, latest) {
			return openAIAccessProbeSkipped
		}
	}
	for {
		before := api.openAIRejectionForAccount(account)
		if before == nil {
			return openAIAccessProbeSkipped
		}
		rejected := &openAIRefreshRejection{credentials: before.credentials, err: before.err}
		if api.openAIFailures.CompareAndSwap(account.ID, before, rejected) {
			return openAIAccessProbeUnauthorized
		}
	}
}

func canRecoverOpenAIOAuth(account *Account) bool {
	return account != nil && account.IsOpenAIOAuth() && !account.IsOpenAIPersonalAccessToken() &&
		!account.IsOpenAIAgentIdentity() && !strings.HasPrefix(strings.TrimSpace(account.GetOpenAIAccessToken()), "at-") &&
		strings.TrimSpace(account.GetOpenAIRefreshToken()) != ""
}

type openAIUnauthorizedRefreshExecutor struct {
	OAuthRefreshExecutor
	rejectedToken string
	accountID     string
	owner         openAIOAuthOwner
}

func (e *openAIUnauthorizedRefreshExecutor) AuthCredentialsPersisted(ctx context.Context, previous *Account) {
	if publisher, ok := e.OAuthRefreshExecutor.(interface {
		AuthCredentialsPersisted(context.Context, *Account)
	}); ok {
		publisher.AuthCredentialsPersisted(ctx, previous)
	}
}

func (e *openAIUnauthorizedRefreshExecutor) CanRefresh(account *Account) bool {
	return canRecoverOpenAIOAuth(account) && e.OAuthRefreshExecutor.CanRefresh(account) && account.GetCredential("chatgpt_account_id") == e.accountID && openAIOAuthOwnerForAccount(account) == e.owner
}

func (e *openAIUnauthorizedRefreshExecutor) NeedsRefresh(account *Account, _ time.Duration) bool {
	// A concurrent refresh already repaired this same account. Reuse the durable
	// access token instead of consuming its newly rotated refresh token again.
	return account.GetOpenAIAccessToken() == e.rejectedToken
}

// RefreshAfterUnauthorized performs at most one token-endpoint call, independent
// of expiry. It shares the proactive/background lock and rereads credentials
// before acting. After the guarded reload replay, callers may replay once more
// with this refresh result; a third 401 ends the recovery chain.
func (p *OpenAITokenProvider) RefreshAfterUnauthorized(ctx context.Context, account *Account, rejectedAccessToken string) (string, error) {
	if p == nil || p.refreshAPI == nil || p.executor == nil || !canRecoverOpenAIOAuth(account) || strings.TrimSpace(rejectedAccessToken) == "" {
		return "", errors.New("OpenAI OAuth unauthorized recovery is unavailable")
	}
	executor := &openAIUnauthorizedRefreshExecutor{OAuthRefreshExecutor: p.executor, rejectedToken: rejectedAccessToken, accountID: account.GetCredential("chatgpt_account_id"), owner: openAIOAuthOwnerForAccount(account)}
	ctx = openai.WithCodexRefreshReason(ctx, "unauthorized_recovery")
	result, err := p.refreshAPI.RefreshIfNeeded(withOAuthRefreshRequestPath(ctx), account, executor, 0)
	if err != nil {
		var rejection *openai.RefreshTokenError
		if errors.As(err, &rejection) {
			return "", rejection
		}
		return "", errors.New("OpenAI OAuth unauthorized recovery temporarily failed")
	}
	if result == nil || result.LockHeld || result.Account == nil {
		return "", errors.New("OpenAI OAuth refresh is already in progress")
	}
	if !sameOpenAIOAuthOwner(account, result.Account) {
		return "", errors.New("OpenAI OAuth credential owner changed")
	}
	token := result.Account.GetOpenAIAccessToken()
	if strings.TrimSpace(token) == "" {
		return "", errors.New("OpenAI OAuth refresh returned no access token")
	}
	if p.tokenCache != nil {
		// Delete the previous access token; the ordinary provider path will cache
		// the durable replacement with its expiry-derived TTL.
		_ = p.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(result.Account))
	}
	return token, nil
}

// RecordUnauthorizedAfterRefresh suppresses further refresh attempts only for
// the exact credential version used by the rejected replay. Never clear stored
// tokens or disable a concurrently reauthorized account.
func (p *OpenAITokenProvider) RecordUnauthorizedAfterRefresh(ctx context.Context, account *Account, rejectedAccessToken string) {
	if p == nil || p.refreshAPI == nil || p.accountRepo == nil || !canRecoverOpenAIOAuth(account) {
		return
	}
	mu := p.refreshAPI.getLocalLock(OpenAITokenCacheKey(account))
	if err := mu.Lock(ctx); err != nil {
		return
	}
	defer mu.Unlock()
	latest, err := p.accountRepo.GetByID(ctx, account.ID)
	if err != nil || latest == nil || !sameOpenAIOAuthOwner(account, latest) || !canRecoverOpenAIOAuth(latest) || latest.GetOpenAIAccessToken() != rejectedAccessToken {
		return
	}
	// This bearer already failed an inference replay after refresh. It is
	// stronger evidence than a refresh-token refusal and must not queue a probe.
	p.refreshAPI.openAIFailures.Store(latest.ID, &openAIRefreshRejection{
		credentials: openAIRefreshFingerprint(latest),
		err:         &openai.RefreshTokenError{StatusCode: http.StatusUnauthorized, Permanent: true},
	})
	if p.tokenCache != nil {
		_ = p.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(latest))
	}
}
