package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type responseAuthOwnerTestCache struct {
	GatewayCache
	mu      sync.Mutex
	values  map[string][]byte
	readErr error
}

func (c *responseAuthOwnerTestCache) SetOpenAIResponseAuthOwner(_ context.Context, key string, payload []byte, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = map[string][]byte{}
	}
	if _, ok := c.values[key]; ok {
		return false, nil
	}
	c.values[key] = append([]byte(nil), payload...)
	return true, nil
}
func (c *responseAuthOwnerTestCache) GetOpenAIResponseAuthOwner(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.values[key]...), c.readErr
}
func responseAuthOwnerAccount() *Account {
	return &Account{ID: 818, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"access_token": "original-token", "refresh_token": "original-refresh", "chatgpt_account_id": "workspace", "chatgpt_user_id": "owner"}, Extra: astraHealthyTestExtra(map[string]any{"openai_passthrough": true, "openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeOff})}
}
func responseAuthOwnerContext(user, key int64) *gin.Context {
	c := newOpenAIWSExecutionScopeTestContext(nil)
	c.Set("api_key", &APIKey{ID: key, UserID: user})
	return c
}

func TestOpenAIResponseAuthOwnerChecksRecordedIdentityAndTenant(t *testing.T) {
	for _, mode := range []string{"same_owner_refresh", "same_user_other_key", "other_user", "other_oauth_user", "other_workspace", "other_row", "new_auth_mode", "oauth_to_apikey", "unknown_credential_revision", "unknown_unchanged"} {
		t.Run(mode, func(t *testing.T) {
			account := responseAuthOwnerAccount()
			origin := responseAuthOwnerContext(101, 201)
			if strings.HasPrefix(mode, "unknown_") {
				delete(account.Credentials, "chatgpt_user_id")
			}
			svc := &OpenAIGatewayService{openaiWSStateStore: NewOpenAIWSStateStore(nil)}
			svc.bindOpenAIResponseAuthOwner(context.Background(), origin, account, "resp_origin")
			current := snapshotOAuthRefreshAccount(account)
			request := responseAuthOwnerContext(101, 201)
			allowed := false
			switch mode {
			case "same_owner_refresh":
				current.Credentials["access_token"] = "rotated-token"
				current.Credentials["refresh_token"] = "rotated-refresh"
				allowed = true
			case "same_user_other_key":
				request = responseAuthOwnerContext(101, 202)
				allowed = true
			case "other_user":
				request = responseAuthOwnerContext(102, 202)
			case "other_oauth_user":
				current.Credentials["chatgpt_user_id"] = "new-user"
			case "other_workspace":
				current.Credentials["chatgpt_account_id"] = "new-workspace"
			case "other_row":
				current.ID++
			case "new_auth_mode":
				current.Credentials[openAIAuthModeCredentialKey] = "agent_identity"
			case "oauth_to_apikey":
				current.Type = AccountTypeAPIKey
			case "unknown_credential_revision":
				current.Credentials["access_token"] = "replacement"
			case "unknown_unchanged":
				allowed = true
			}
			err := svc.validateOpenAIResponseAuthOwner(context.Background(), request, current, "resp_origin")
			if allowed {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errOpenAIResponseAuthOwnerMismatch)
			}
			require.NoError(t, svc.validateOpenAIResponseAuthOwner(context.Background(), request, current, "resp_external"), "unknown native response origins keep existing handling")
		})
	}
}

func TestOpenAIResponseAuthOwnerSharedCacheAndOriginalBinding(t *testing.T) {
	cache := &responseAuthOwnerTestCache{}
	account := responseAuthOwnerAccount()
	c := responseAuthOwnerContext(101, 201)
	writer := &OpenAIGatewayService{openaiWSStateStore: NewOpenAIWSStateStore(cache)}
	writer.bindOpenAIResponseAuthOwner(context.Background(), c, account, "resp_shared")
	changed := snapshotOAuthRefreshAccount(account)
	changed.Credentials["chatgpt_user_id"] = "replacement"
	reader := &OpenAIGatewayService{openaiWSStateStore: NewOpenAIWSStateStore(cache)}
	require.NoError(t, reader.validateOpenAIResponseAuthOwner(context.Background(), c, account, "resp_shared"))
	require.ErrorIs(t, reader.validateOpenAIResponseAuthOwner(context.Background(), c, changed, "resp_shared"), errOpenAIResponseAuthOwnerMismatch)
	// A later observation / sticky refresh cannot redefine the old response owner.
	other := &OpenAIGatewayService{openaiWSStateStore: NewOpenAIWSStateStore(cache)}
	other.bindOpenAIResponseAuthOwner(context.Background(), c, changed, "resp_shared")
	require.ErrorIs(t, other.validateOpenAIResponseAuthOwner(context.Background(), c, changed, "resp_shared"), errOpenAIResponseAuthOwnerMismatch)
	require.NoError(t, other.validateOpenAIResponseAuthOwner(context.Background(), c, account, "resp_shared"))
	crossGroup := responseAuthOwnerContext(101, 201)
	group := int64(7)
	crossGroup.Set("api_key", &APIKey{ID: 201, UserID: 101, GroupID: &group})
	require.NoError(t, other.validateOpenAIResponseAuthOwner(context.Background(), crossGroup, changed, "resp_shared"), "a different group's unknown binding is not an ownership assertion")
}

func TestOpenAIResponseAuthOwnerCacheFailureAndExpiry(t *testing.T) {
	for _, mode := range []string{"read_error", "malformed", "expired"} {
		t.Run(mode, func(t *testing.T) {
			cache := &responseAuthOwnerTestCache{values: map[string][]byte{}}
			key := openAIResponseAuthOwnerCacheKey(0, "resp_shared")
			switch mode {
			case "read_error":
				cache.readErr = errors.New("synthetic cache failure")
			case "malformed":
				cache.values[key] = []byte(`{"owner":`)
			case "expired":
				raw, err := json.Marshal(openAIResponseAuthOwnerBinding{Owner: "expired", ExpiresUnixNano: time.Now().Add(-time.Second).UnixNano()})
				require.NoError(t, err)
				cache.values[key] = raw
			}
			svc := &OpenAIGatewayService{openaiWSStateStore: NewOpenAIWSStateStore(cache)}
			err := svc.validateOpenAIResponseAuthOwner(context.Background(), responseAuthOwnerContext(101, 201), responseAuthOwnerAccount(), "resp_shared")
			if mode == "expired" {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "a cache fault must not masquerade as an unknown native origin")
			}
		})
	}
}

func TestOpenAIResponseAuthOwnerHTTPPositiveAndTenantIsolation(t *testing.T) {
	for _, mode := range []string{"same_owner_refresh", "same_user_other_key", "other_user", "unknown_native_origin"} {
		t.Run(mode, func(t *testing.T) {
			account := responseAuthOwnerAccount()
			origin := responseAuthOwnerContext(101, 201)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_next\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n"))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, tiboRouteDisabled: true}
			if mode != "unknown_native_origin" {
				svc.bindHTTPResponseAccount(context.Background(), origin, account, "resp_origin")
			}
			request := responseAuthOwnerContext(101, 201)
			switch mode {
			case "same_owner_refresh":
				account.Credentials["access_token"] = "rotated-token"
				account.Credentials["refresh_token"] = "rotated-refresh"
			case "same_user_other_key":
				request = responseAuthOwnerContext(101, 202)
			case "other_user":
				request = responseAuthOwnerContext(102, 202)
			}
			body := []byte(`{"model":"gpt-5.2","stream":false,"instructions":"synthetic","previous_response_id":"resp_origin","input":[{"role":"user","content":"next"}]}`)
			_, err := svc.Forward(context.Background(), request, account, body)
			if mode == "other_user" {
				require.ErrorIs(t, err, errOpenAIResponseAuthOwnerMismatch)
				require.Empty(t, upstream.requests)
			} else {
				require.NoError(t, err)
				require.Equal(t, "resp_origin", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
			}
		})
	}
}
