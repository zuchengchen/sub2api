//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// TestCheckErrorPolicy — 6 table-driven cases for the pure logic function
// ---------------------------------------------------------------------------

func TestCheckErrorPolicy(t *testing.T) {
	tests := []struct {
		name       string
		account    *Account
		statusCode int
		body       []byte
		expected   ErrorPolicyResult
	}{
		{
			name: "no_policy_oauth_returns_none",
			account: &Account{
				ID:       1,
				Type:     AccountTypeOAuth,
				Platform: PlatformGrok,
				// no custom error codes, no temp rules
			},
			statusCode: 500,
			body:       []byte(`"error"`),
			expected:   ErrorPolicyNone,
		},
		{
			name: "custom_error_codes_hit_returns_matched",
			account: &Account{
				ID:       2,
				Type:     AccountTypeAPIKey,
				Platform: PlatformGrok,
				Credentials: map[string]any{
					"custom_error_codes_enabled": true,
					"custom_error_codes":         []any{float64(429), float64(500)},
				},
			},
			statusCode: 500,
			body:       []byte(`"error"`),
			expected:   ErrorPolicyMatched,
		},
		{
			name: "custom_error_codes_miss_returns_skipped",
			account: &Account{
				ID:       3,
				Type:     AccountTypeAPIKey,
				Platform: PlatformGrok,
				Credentials: map[string]any{
					"custom_error_codes_enabled": true,
					"custom_error_codes":         []any{float64(429), float64(500)},
				},
			},
			statusCode: 503,
			body:       []byte(`"error"`),
			expected:   ErrorPolicySkipped,
		},
		{
			name: "custom_error_codes_excluding_529_skip_global_cooldown",
			account: &Account{
				ID:       33,
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"custom_error_codes_enabled": true,
					"custom_error_codes":         []any{float64(429)},
				},
			},
			statusCode: 529,
			body:       []byte(`{"error":{"message":"overloaded"}}`),
			expected:   ErrorPolicySkipped,
		},
		{
			name: "pool_mode_skips_global_529_cooldown",
			account: &Account{
				ID:       34,
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode": true,
				},
			},
			statusCode: 529,
			body:       []byte(`{"error":{"message":"overloaded"}}`),
			expected:   ErrorPolicySkipped,
		},
		{
			name: "ordinary_account_uses_global_529_cooldown",
			account: &Account{
				ID:       35,
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
			},
			statusCode: 529,
			body:       []byte(`{"error":{"message":"overloaded"}}`),
			expected:   ErrorPolicyMatched,
		},
		{
			name: "custom_error_codes_including_529_take_precedence",
			account: &Account{
				ID:       36,
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"custom_error_codes_enabled": true,
					"custom_error_codes":         []any{float64(529)},
				},
			},
			statusCode: 529,
			body:       []byte(`{"error":{"message":"overloaded"}}`),
			expected:   ErrorPolicyMatched,
		},
		{
			name: "temp_unschedulable_hit_returns_temp_unscheduled",
			account: &Account{
				ID:       4,
				Type:     AccountTypeOAuth,
				Platform: PlatformGrok,
				Credentials: map[string]any{
					"temp_unschedulable_enabled": true,
					"temp_unschedulable_rules": []any{
						map[string]any{
							"error_code":       float64(503),
							"keywords":         []any{"overloaded"},
							"duration_minutes": float64(10),
							"description":      "overloaded rule",
						},
					},
				},
			},
			statusCode: 503,
			body:       []byte(`overloaded service`),
			expected:   ErrorPolicyTempUnscheduled,
		},
		{
			name: "temp_unschedulable_401_first_hit_returns_temp_unscheduled",
			account: &Account{
				ID:       14,
				Type:     AccountTypeOAuth,
				Platform: PlatformGrok,
				Credentials: map[string]any{
					"temp_unschedulable_enabled": true,
					"temp_unschedulable_rules": []any{
						map[string]any{
							"error_code":       float64(401),
							"keywords":         []any{"unauthorized"},
							"duration_minutes": float64(10),
						},
					},
				},
			},
			statusCode: 401,
			body:       []byte(`unauthorized`),
			expected:   ErrorPolicyTempUnscheduled,
		},
		{
			name: "temp_unschedulable_body_miss_returns_none",
			account: &Account{
				ID:       5,
				Type:     AccountTypeOAuth,
				Platform: PlatformGrok,
				Credentials: map[string]any{
					"temp_unschedulable_enabled": true,
					"temp_unschedulable_rules": []any{
						map[string]any{
							"error_code":       float64(503),
							"keywords":         []any{"overloaded"},
							"duration_minutes": float64(10),
							"description":      "overloaded rule",
						},
					},
				},
			},
			statusCode: 503,
			body:       []byte(`random msg`),
			expected:   ErrorPolicyNone,
		},
		{
			name: "custom_error_codes_override_temp_unschedulable",
			account: &Account{
				ID:       6,
				Type:     AccountTypeAPIKey,
				Platform: PlatformGrok,
				Credentials: map[string]any{
					"custom_error_codes_enabled": true,
					"custom_error_codes":         []any{float64(503)},
					"temp_unschedulable_enabled": true,
					"temp_unschedulable_rules": []any{
						map[string]any{
							"error_code":       float64(503),
							"keywords":         []any{"overloaded"},
							"duration_minutes": float64(10),
							"description":      "overloaded rule",
						},
					},
				},
			},
			statusCode: 503,
			body:       []byte(`overloaded`),
			expected:   ErrorPolicyMatched, // custom codes take precedence
		},
		{
			name: "pool_mode_custom_error_codes_hit_returns_matched",
			account: &Account{
				ID:       7,
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":                  true,
					"custom_error_codes_enabled": true,
					"custom_error_codes":         []any{float64(401), float64(403)},
				},
			},
			statusCode: 401,
			body:       []byte(`unauthorized`),
			expected:   ErrorPolicyMatched,
		},
		{
			name: "pool_mode_without_custom_error_codes_returns_skipped",
			account: &Account{
				ID:       8,
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode": true,
				},
			},
			statusCode: 401,
			body:       []byte(`unauthorized`),
			expected:   ErrorPolicySkipped,
		},
		{
			name: "pool_mode_temp_unschedulable_hit_returns_temp_unscheduled",
			account: &Account{
				ID:       9,
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":                  true,
					"temp_unschedulable_enabled": true,
					"temp_unschedulable_rules": []any{
						map[string]any{
							"error_code":       float64(http.StatusServiceUnavailable),
							"keywords":         []any{"unavailable"},
							"duration_minutes": float64(30),
						},
					},
				},
			},
			statusCode: http.StatusServiceUnavailable,
			body:       []byte(`Service temporarily unavailable`),
			expected:   ErrorPolicyTempUnscheduled,
		},
		{
			name: "pool_mode_temp_unschedulable_miss_returns_skipped",
			account: &Account{
				ID:       10,
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":                  true,
					"temp_unschedulable_enabled": true,
					"temp_unschedulable_rules": []any{
						map[string]any{
							"error_code":       float64(http.StatusServiceUnavailable),
							"keywords":         []any{"maintenance"},
							"duration_minutes": float64(30),
						},
					},
				},
			},
			statusCode: http.StatusServiceUnavailable,
			body:       []byte(`Service temporarily unavailable`),
			expected:   ErrorPolicySkipped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &errorPolicyRepoStub{}
			svc := NewRateLimitService(repo, nil, &config.Config{}, nil)

			result := svc.CheckErrorPolicy(context.Background(), tt.account, tt.statusCode, tt.body)
			require.Equal(t, tt.expected, result, "unexpected ErrorPolicyResult")
		})
	}
}

func TestHandleUpstreamError_PoolModePolicies(t *testing.T) {
	t.Run("pool_mode_without_custom_error_codes_still_skips", func(t *testing.T) {
		repo := &errorPolicyRepoStub{}
		svc := NewRateLimitService(repo, nil, &config.Config{}, nil)
		account := &Account{
			ID:       30,
			Type:     AccountTypeAPIKey,
			Platform: PlatformOpenAI,
			Credentials: map[string]any{
				"pool_mode": true,
			},
		}

		shouldDisable := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"))

		require.False(t, shouldDisable)
		require.Equal(t, 0, repo.setErrCalls)
		require.Equal(t, 0, repo.tempCalls)
	})

	t.Run("pool_mode_with_custom_error_codes_uses_local_error_policy", func(t *testing.T) {
		repo := &errorPolicyRepoStub{}
		svc := NewRateLimitService(repo, nil, &config.Config{}, nil)
		account := &Account{
			ID:       31,
			Type:     AccountTypeAPIKey,
			Platform: PlatformOpenAI,
			Credentials: map[string]any{
				"pool_mode":                  true,
				"custom_error_codes_enabled": true,
				"custom_error_codes":         []any{float64(401)},
			},
		}

		shouldDisable := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte("unauthorized"))

		require.True(t, shouldDisable)
		require.Equal(t, 1, repo.setErrCalls)
		require.Equal(t, 0, repo.tempCalls)
	})

	t.Run("pool_mode_explicit_temp_rule_stops_scheduling", func(t *testing.T) {
		repo := &errorPolicyRepoStub{}
		svc := NewRateLimitService(repo, nil, &config.Config{}, nil)
		account := &Account{
			ID:       32,
			Type:     AccountTypeAPIKey,
			Platform: PlatformOpenAI,
			Credentials: map[string]any{
				"pool_mode":                  true,
				"temp_unschedulable_enabled": true,
				"temp_unschedulable_rules": []any{
					map[string]any{
						"error_code":       float64(http.StatusServiceUnavailable),
						"keywords":         []any{"unavailable"},
						"duration_minutes": float64(30),
					},
				},
			},
		}

		shouldDisable := svc.HandleUpstreamError(
			context.Background(),
			account,
			http.StatusServiceUnavailable,
			http.Header{},
			[]byte("Service temporarily unavailable"),
		)

		require.True(t, shouldDisable)
		require.Equal(t, 0, repo.setErrCalls)
		require.Equal(t, 1, repo.tempCalls)
	})

	t.Run("pool_mode_temp_rule_miss_still_skips", func(t *testing.T) {
		repo := &errorPolicyRepoStub{}
		svc := NewRateLimitService(repo, nil, &config.Config{}, nil)
		account := &Account{
			ID:       33,
			Type:     AccountTypeAPIKey,
			Platform: PlatformOpenAI,
			Credentials: map[string]any{
				"pool_mode":                  true,
				"temp_unschedulable_enabled": true,
				"temp_unschedulable_rules": []any{
					map[string]any{
						"error_code":       float64(http.StatusServiceUnavailable),
						"keywords":         []any{"maintenance"},
						"duration_minutes": float64(30),
					},
				},
			},
		}

		shouldDisable := svc.HandleUpstreamError(
			context.Background(),
			account,
			http.StatusServiceUnavailable,
			http.Header{},
			[]byte("Service temporarily unavailable"),
		)

		require.False(t, shouldDisable)
		require.Equal(t, 0, repo.setErrCalls)
		require.Equal(t, 0, repo.tempCalls)
	})
}

// ---------------------------------------------------------------------------
// TestApplyErrorPolicy — 4 table-driven cases for the wrapper method
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// errorPolicyRepoStub — minimal AccountRepository stub for error policy tests
// ---------------------------------------------------------------------------

type errorPolicyRepoStub struct {
	mockAccountRepoForTest
	tempCalls           int
	setErrCalls         int
	lastErrorMsg        string
	modelRateLimitCalls []modelNotFoundRateLimitCall
}

func (r *errorPolicyRepoStub) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	r.tempCalls++
	return nil
}

func (r *errorPolicyRepoStub) SetError(ctx context.Context, id int64, errorMsg string) error {
	r.setErrCalls++
	r.lastErrorMsg = errorMsg
	return nil
}

func (r *errorPolicyRepoStub) SetModelRateLimit(_ context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	call := modelNotFoundRateLimitCall{accountID: id, scope: scope, resetAt: resetAt}
	if len(reason) > 0 {
		call.reason = reason[0]
	}
	r.modelRateLimitCalls = append(r.modelRateLimitCalls, call)
	return nil
}
