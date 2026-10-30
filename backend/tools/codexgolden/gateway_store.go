//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Only persistence is replaced. An unexpected repository method deliberately
// panics through the embedded nil interface and fails the recorder process.
// Authentication, refresh coordination, request preparation and transport are
// the production implementations.
type gatewayAccounts struct {
	service.AccountRepository
	mu               sync.Mutex
	account          *service.Account
	credentialWrites int
}

func cloneGatewayAccount(account *service.Account) *service.Account {
	copy := *account
	copy.Credentials = maps.Clone(account.Credentials)
	copy.Extra = maps.Clone(account.Extra)
	return &copy
}

func (s *gatewayAccounts) GetByID(_ context.Context, id int64) (*service.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.account.ID {
		return nil, fmt.Errorf("unknown synthetic account %d", id)
	}
	return cloneGatewayAccount(s.account), nil
}
func (s *gatewayAccounts) Update(_ context.Context, account *service.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if account.ID != s.account.ID {
		return errors.New("unexpected account update")
	}
	s.account = cloneGatewayAccount(account)
	s.credentialWrites++
	return nil
}
func (s *gatewayAccounts) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.account.ID {
		return errors.New("unexpected credential update")
	}
	s.account.Credentials = maps.Clone(credentials)
	s.credentialWrites++
	return nil
}
func (s *gatewayAccounts) UpdateExtra(_ context.Context, id int64, values map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.account.ID {
		return errors.New("unexpected extra update")
	}
	for k, v := range values {
		s.account.Extra[k] = v
	}
	return nil
}
func (s *gatewayAccounts) ListByPlatform(context.Context, string) ([]service.Account, error) {
	return nil, nil
}
func (s *gatewayAccounts) SetError(_ context.Context, id int64, message string) error {
	return fmt.Errorf("synthetic account %d unexpectedly rejected: %s", id, message)
}

type gatewayProxies struct {
	service.ProxyRepository
	proxy *service.Proxy
}

func (s *gatewayProxies) GetByID(_ context.Context, id int64) (*service.Proxy, error) {
	if id != s.proxy.ID {
		return nil, errors.New("unknown synthetic proxy")
	}
	p := *s.proxy
	return &p, nil
}

type gatewayCachedToken struct {
	value   string
	expires time.Time
}
type gatewayTokenCache struct {
	mu     sync.Mutex
	tokens map[string]gatewayCachedToken
	locks  map[string]time.Time
}

func (s *gatewayTokenCache) GetAccessToken(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.tokens[key]
	if time.Now().Before(entry.expires) {
		return entry.value, nil
	}
	return "", nil
}
func (s *gatewayTokenCache) SetAccessToken(_ context.Context, key, token string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = make(map[string]gatewayCachedToken)
	}
	s.tokens[key] = gatewayCachedToken{token, time.Now().Add(ttl)}
	return nil
}
func (s *gatewayTokenCache) DeleteAccessToken(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, key)
	return nil
}
func (s *gatewayTokenCache) AcquireRefreshLock(_ context.Context, key string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.locks[key]) {
		return false, nil
	}
	if s.locks == nil {
		s.locks = make(map[string]time.Time)
	}
	s.locks[key] = time.Now().Add(ttl)
	return true, nil
}
func (s *gatewayTokenCache) ReleaseRefreshLock(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.locks, key)
	return nil
}
