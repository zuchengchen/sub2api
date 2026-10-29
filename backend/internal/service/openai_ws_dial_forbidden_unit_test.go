//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Edge-level 403s must not consume the account 403 strike budget; the
// failover error still lets the handler try the next account.
func TestNewOpenAIWSDialForbiddenFailoverErrorEdgeSkipsAccountPenalty(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	rateSvc := &RateLimitService{accountRepo: repo}
	svc := &OpenAIGatewayService{rateLimitService: rateSvc}
	account := &Account{ID: 11, Name: "acc", Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	failoverErr := svc.newOpenAIWSDialForbiddenFailoverError(context.Background(), account, "gpt-5.1", &openAIWSDialError{
		StatusCode:   http.StatusForbidden,
		ResponseBody: []byte(`<html><title>Just a moment...</title></html>`),
		Err:          errors.New("websocket upgrade failed: status 403"),
	})
	require.NotNil(t, failoverErr)
	require.Equal(t, http.StatusForbidden, failoverErr.StatusCode)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.Zero(t, repo.setErrorCalls)
	require.Zero(t, repo.tempCalls)
}

// A structured 403 is account-level evidence and keeps the existing OpenAI
// 403 policy (temporary cooldown) while still switching accounts.
func TestNewOpenAIWSDialForbiddenFailoverErrorStructuredAppliesAccountPolicy(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	rateSvc := &RateLimitService{accountRepo: repo, openAI403CounterCache: &openAI403CounterCacheStub{}}
	svc := &OpenAIGatewayService{rateLimitService: rateSvc}
	account := &Account{ID: 12, Name: "acc", Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	failoverErr := svc.newOpenAIWSDialForbiddenFailoverError(context.Background(), account, "gpt-5.1", &openAIWSDialError{
		StatusCode:   http.StatusForbidden,
		ResponseBody: []byte(`{"error":{"message":"forbidden","type":"permission_error"}}`),
		Err:          errors.New("websocket upgrade failed: status 403"),
	})
	require.NotNil(t, failoverErr)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.Equal(t, 1, repo.tempCalls)
}
