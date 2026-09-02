//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAITokenCacheKey(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected string
	}{
		{
			name: "basic_account",
			account: &Account{
				ID: 300,
			},
			expected: "openai:account:300",
		},
		{
			name: "account_with_credentials",
			account: &Account{
				ID: 301,
				Credentials: map[string]any{
					"access_token": "test-token",
				},
			},
			expected: "openai:account:301",
		},
		{
			name: "account_id_zero",
			account: &Account{
				ID: 0,
			},
			expected: "openai:account:0",
		},
		{
			name: "large_account_id",
			account: &Account{
				ID: 9999999999,
			},
			expected: "openai:account:9999999999",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := OpenAITokenCacheKey(tt.account)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestGrokTokenCacheKey(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected string
	}{
		{
			name: "basic_account",
			account: &Account{
				ID: 350,
			},
			expected: "grok:account:350",
		},
		{
			name: "account_with_email_uses_account_id",
			account: &Account{
				ID: 351,
				Credentials: map[string]any{
					"email": "same-user@example.com",
				},
			},
			expected: "grok:account:351",
		},
		{
			name: "account_id_zero",
			account: &Account{
				ID: 0,
			},
			expected: "grok:account:0",
		},
		{
			name:     "nil_account",
			account:  nil,
			expected: "grok:account:0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GrokTokenCacheKey(tt.account)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestGrokTokenCacheKeySeparatesAccountsWithSameEmail(t *testing.T) {
	first := &Account{
		ID: 351,
		Credentials: map[string]any{
			"email": "same-user@example.com",
		},
	}
	second := &Account{
		ID: 352,
		Credentials: map[string]any{
			"email": "same-user@example.com",
		},
	}

	require.NotEqual(t, GrokTokenCacheKey(first), GrokTokenCacheKey(second))
}

func TestClaudeTokenCacheKey(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected string
	}{
		{
			name: "basic_account",
			account: &Account{
				ID: 400,
			},
			expected: "claude:account:400",
		},
		{
			name: "account_with_credentials",
			account: &Account{
				ID: 401,
				Credentials: map[string]any{
					"access_token": "claude-token",
				},
			},
			expected: "claude:account:401",
		},
		{
			name: "account_id_zero",
			account: &Account{
				ID: 0,
			},
			expected: "claude:account:0",
		},
		{
			name: "large_account_id",
			account: &Account{
				ID: 9999999999,
			},
			expected: "claude:account:9999999999",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ClaudeTokenCacheKey(tt.account)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestCacheKeyUniqueness(t *testing.T) {
	// 确保不同平台的缓存键不会冲突
	account := &Account{ID: 123}

	openaiKey := OpenAITokenCacheKey(account)
	claudeKey := ClaudeTokenCacheKey(account)

	require.NotEqual(t, openaiKey, claudeKey, "OpenAI and Claude cache keys should be different")
	require.Contains(t, openaiKey, "openai:")
	require.Contains(t, claudeKey, "claude:")
}
