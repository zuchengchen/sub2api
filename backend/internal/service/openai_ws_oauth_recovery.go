package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Codex's first 401 recovery step is a guarded reload of durable credentials.
// Both Changed and Unchanged permit one replay; a missing/mismatched account
// identity stops recovery. Only a subsequent 401 may rotate the refresh token.
func (s *OpenAIGatewayService) reloadOpenAIOAuthHeaders(ctx context.Context, account *Account, headers http.Header) (http.Header, *Account, bool) {
	p := s.openAITokenProvider
	if p == nil || p.accountRepo == nil || !canRecoverOpenAIOAuth(account) || ctx.Err() != nil {
		return headers, nil, false
	}
	expected := account.GetChatGPTAccountID()
	if expected == "" {
		return headers, nil, false
	}
	latest, err := p.accountRepo.GetByID(ctx, account.ID)
	if err != nil || latest == nil || latest.ID != account.ID || !latest.IsActive() || !canRecoverOpenAIOAuth(latest) || latest.GetChatGPTAccountID() != expected || latest.GetOpenAIAccessToken() == "" || p.refreshAPI.openAIRefreshFailure(latest) != nil {
		return headers, nil, false
	}
	if p.tokenCache != nil {
		_ = p.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(latest))
	}
	// Reload replaces this account's auth state across its token/device
	// snapshots, without touching connections owned by any other account.
	p.openAIOAuthService.retireAccountAuthClient(ctx, latest)
	updated := headers.Clone()
	updated.Set("Authorization", "Bearer "+latest.GetOpenAIAccessToken())
	return updated, latest, true
}

func (s *OpenAIGatewayService) recoverOpenAIOAuthHeaders(ctx context.Context, account *Account, headers http.Header) (http.Header, *Account, bool) {
	if s.openAITokenProvider == nil {
		return headers, nil, false
	}
	owner, err := s.openAIOAuthCredentialOwner(ctx, account)
	if err != nil || !canRecoverOpenAIOAuth(owner) {
		return headers, nil, false
	}
	authorization := headers.Get("Authorization")
	rejected := strings.TrimPrefix(authorization, "Bearer ")
	if rejected == "" || rejected == authorization {
		return headers, nil, false
	}
	token, err := s.openAITokenProvider.RefreshAfterUnauthorized(ctx, owner, rejected)
	if err != nil {
		return headers, owner, false
	}
	updated := headers.Clone()
	updated.Set("Authorization", "Bearer "+token)
	return updated, owner, true
}

// refreshOpenAIWSAuthHeaders also runs for delayed prewarming. Never retain an
// old OAuth bearer in a future dial after another request rotates credentials.
func (s *OpenAIGatewayService) refreshOpenAIWSAuthHeaders(ctx context.Context, account *Account, headers http.Header) (http.Header, error) {
	if s.isAgentIdentityAccount(ctx, account) {
		return s.refreshOpenAIAgentIdentityHeaders(ctx, account, headers)
	}
	if s.openAITokenProvider == nil {
		return headers, nil
	}
	owner, err := s.openAIOAuthCredentialOwner(ctx, account)
	if err != nil {
		return nil, err
	}
	if !canRecoverOpenAIOAuth(owner) {
		return headers, nil
	}
	if s.openAITokenProvider.accountRepo != nil {
		latest, readErr := s.openAITokenProvider.accountRepo.GetByID(ctx, owner.ID)
		if readErr != nil || latest == nil || latest.ID != owner.ID || !latest.IsActive() || !canRecoverOpenAIOAuth(latest) || latest.GetCredential("chatgpt_account_id") != owner.GetCredential("chatgpt_account_id") {
			return nil, errors.New("OpenAI OAuth credential state is unavailable")
		}
		owner = latest
	}
	token, err := s.openAITokenProvider.GetAccessToken(ctx, owner)
	if err != nil {
		return nil, err
	}
	updated := headers.Clone()
	updated.Set("Authorization", "Bearer "+token)
	return updated, nil
}

func (s *OpenAIGatewayService) acquireOpenAIWSWithAuthRecovery(ctx context.Context, pool *openAIWSConnPool, request *openAIWSAcquireRequest) (*openAIWSConnLease, error) {
	lease, err := pool.Acquire(ctx, *request)
	var dialErr *openAIWSDialError
	if err == nil || !errors.As(err, &dialErr) || dialErr.StatusCode != http.StatusUnauthorized || ctx.Err() != nil {
		return lease, err
	}
	headers, owner, recovered := s.reloadOpenAIOAuthHeaders(ctx, request.Account, request.Headers)
	if !recovered {
		return nil, err
	}
	request.Headers = headers
	// Do not reuse another connection authenticated with the rejected token.
	// Active leases belong to other turns and must not be closed by this retry.
	retry := cloneOpenAIWSAcquireRequest(*request)
	retry.ForceNewConn = true
	lease, err = pool.Acquire(ctx, retry)
	if err == nil || !errors.As(err, &dialErr) || dialErr.StatusCode != http.StatusUnauthorized || ctx.Err() != nil {
		return lease, err
	}
	headers, owner, recovered = s.recoverOpenAIOAuthHeaders(ctx, owner, headers)
	if !recovered {
		return nil, err
	}
	request.Headers = headers
	retry.Headers = headers
	lease, err = pool.Acquire(ctx, retry)
	if err != nil && errors.As(err, &dialErr) && dialErr.StatusCode == http.StatusUnauthorized {
		s.openAITokenProvider.RecordUnauthorizedAfterRefresh(ctx, owner, strings.TrimPrefix(headers.Get("Authorization"), "Bearer "))
	}
	return lease, err
}
