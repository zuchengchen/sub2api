package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveAndSetOpenAIChatGPTAccountHeaders(t *testing.T) {
	ctx := context.Background()

	t.Run("normal_account_passthrough", func(t *testing.T) {
		ownCreds := map[string]any{"chatgpt_account_id": "org-own"}
		normal := &Account{
			ID:          300,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Credentials: ownCreds,
		}
		headers := make(http.Header)
		err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, nil, headers, normal)
		require.NoError(t, err)
		require.Equal(t, "org-own", headers.Get("chatgpt-account-id"),
			"普通账号应透传自身的 chatgpt-account-id")
	})
}
