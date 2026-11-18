package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Token acquisition may observe a rotation after selection. Resolve the actual
// version before sending so a late 401 cannot invalidate a newer credential.
func (p *OpenAITokenProvider) accountForSentOpenAIAccess(ctx context.Context, selected *Account, authorization string) *Account {
	if p == nil || p.refreshAPI == nil || p.accountRepo == nil || selected == nil {
		return selected
	}
	if _, rejected := p.refreshAPI.openAIFailures.Load(selected.ID); !rejected {
		return selected
	}
	token, bearer := strings.CutPrefix(authorization, "Bearer ")
	if !bearer || token == "" || token == selected.GetOpenAIAccessToken() || !canRecoverOpenAIOAuth(selected) {
		return selected
	}
	latest, err := p.accountRepo.GetByID(ctx, selected.ID)
	if err != nil || latest == nil || latest.ID != selected.ID || !latest.IsActive() || !canRecoverOpenAIOAuth(latest) ||
		!sameOpenAIOAuthOwner(selected, latest) || token != latest.GetOpenAIAccessToken() {
		return nil
	}
	return latest
}

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
	if err != nil || latest == nil || !sameOpenAIOAuthOwner(account, latest) || !latest.IsActive() || !canRecoverOpenAIOAuth(latest) || latest.GetOpenAIAccessToken() == "" || p.refreshAPI.openAIRefreshFailure(latest) != nil {
		return headers, nil, false
	}
	if p.tokenCache != nil {
		_ = p.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(latest))
	}
	// Reload replaces this account's auth state across its token/device
	// snapshots, without touching connections owned by any other account.
	p.openAIOAuthService.retireAccountAuthClient(withCodexSourceBuildAuthIdentity(ctx, latest, headers), latest)
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
	ctx = withCodexSourceBuildAuthIdentity(ctx, owner, headers)
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
func (s *OpenAIGatewayService) refreshOpenAIWSAuthHeaders(ctx context.Context, account *Account, headers http.Header) (http.Header, *Account, error) {
	if s.isAgentIdentityAccount(ctx, account) {
		updated, err := s.refreshOpenAIAgentIdentityHeaders(ctx, account, headers)
		return updated, nil, err
	}
	if s.openAITokenProvider == nil {
		return headers, nil, nil
	}
	owner, err := s.openAIOAuthCredentialOwner(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	if !canRecoverOpenAIOAuth(owner) {
		return headers, nil, nil
	}
	if s.openAITokenProvider.accountRepo != nil {
		latest, readErr := s.openAITokenProvider.accountRepo.GetByID(ctx, owner.ID)
		if readErr != nil || latest == nil || !sameOpenAIOAuthOwner(owner, latest) || !latest.IsActive() || !canRecoverOpenAIOAuth(latest) {
			return nil, nil, errors.New("OpenAI OAuth credential state is unavailable")
		}
		owner = latest
	}
	ctx = withCodexSourceBuildAuthIdentity(ctx, owner, headers)
	token, err := s.openAITokenProvider.GetAccessToken(ctx, owner)
	if err != nil {
		return nil, nil, err
	}
	updated := headers.Clone()
	updated.Set("Authorization", "Bearer "+token)
	return updated, s.openAITokenProvider.accountForSentOpenAIAccess(ctx, owner, updated.Get("Authorization")), nil
}

func (s *OpenAIGatewayService) acquireOpenAIWSWithAuthRecovery(ctx context.Context, pool *openAIWSConnPool, request *openAIWSAcquireRequest) (*openAIWSConnLease, error) {
	initial := *request
	if p := s.openAITokenProvider; p != nil {
		initial.onUnauthorized = p.refreshAPI.rejectVerifiedOpenAIAccess
		if initial.HeadersFactory == nil {
			selected := request.Account
			initial.HeadersFactory = func(ctx context.Context, h http.Header) (http.Header, *Account, error) {
				return h, p.accountForSentOpenAIAccess(ctx, selected, h.Get("Authorization")), nil
			}
		}
	}
	lease, err := pool.Acquire(ctx, initial)
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
	retry := cloneOpenAIWSAcquireRequest(initial)
	retry.Headers = headers
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
