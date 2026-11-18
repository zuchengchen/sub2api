package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
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
	api.openAIFailures.Store(account.ID, &openAIRefreshRejection{credentials: openAIRefreshFingerprint(account), err: err})
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

// Keep the refresh rejection even when a completed inference proves this access
// token still works. The proof belongs to the failure observed before sending
// the test, so a newer 401 or a credential replacement wins over a late success.
func (api *OAuthRefreshAPI) verifyOpenAIAccessAfterTest(ctx context.Context, account *Account, before *openAIRefreshRejection, sentAuthorization string, afterVerify func()) bool {
	if api == nil || api.accountRepo == nil || before == nil || !canRecoverOpenAIOAuth(account) ||
		!sentOpenAIAccessTokenMatches(account, sentAuthorization) ||
		before.credentials != openAIRefreshFingerprint(account) {
		return false
	}
	// Order the successful probe against the background worker that may be
	// publishing an account error for this same refresh rejection.
	mu := api.getLocalLock(OpenAITokenCacheKey(account))
	if err := mu.Lock(ctx); err != nil {
		return false
	}
	defer mu.Unlock()
	latest, err := api.accountRepo.GetByID(ctx, account.ID)
	if err != nil || latest == nil || latest.ID != account.ID || (!latest.IsActive() && latest.Status != StatusError) ||
		before.credentials != openAIRefreshFingerprint(latest) {
		return false
	}
	expiry := openAIAccessTokenExpiresAt(account)
	currentExpiry := openAIAccessTokenExpiresAt(latest)
	if expiry == nil || currentExpiry == nil {
		return false
	}
	if currentExpiry.Before(*expiry) {
		expiry = currentExpiry
	}
	if !time.Now().Before(*expiry) {
		return false
	}
	verified := *before
	verified.verifiedAccessUntil = *expiry
	if !api.openAIFailures.CompareAndSwap(account.ID, before, &verified) {
		return false
	}
	if afterVerify != nil {
		afterVerify()
	}
	return true
}

// A Responses 401 invalidates both active proof and an older probe in flight.
// Replace the immutable snapshot even if no proof has completed yet.
func (api *OAuthRefreshAPI) rejectVerifiedOpenAIAccess(account *Account, sentAuthorization string) {
	for {
		before := api.openAIRejectionForAccount(account)
		if before == nil || !sentOpenAIAccessTokenMatches(account, sentAuthorization) {
			return
		}
		rejected := &openAIRefreshRejection{credentials: before.credentials, err: before.err}
		if api.openAIFailures.CompareAndSwap(account.ID, before, rejected) {
			return
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
	p.refreshAPI.recordOpenAIRefreshFailure(latest, &openai.RefreshTokenError{StatusCode: http.StatusUnauthorized, Permanent: true})
	if p.tokenCache != nil {
		_ = p.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(latest))
	}
}
