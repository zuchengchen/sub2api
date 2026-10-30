package service

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexRefreshReasonUsesJWTExpiryBeforeLastRefresh(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, tc := range []struct{ name, payload, last, want string }{
		{"near", fmt.Sprintf(`{"exp":%d}`, now.Add(5*time.Minute).Unix()), "", "proactive_expiry"},
		{"expired", `{"exp":0}`, "", "proactive_expiry"},
		{"far_old_refresh", fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix()), now.Add(-9 * 24 * time.Hour).Format(time.RFC3339), "forced"},
		{"missing_old_refresh", `{}`, now.Add(-9 * 24 * time.Hour).Format(time.RFC3339), "scheduled"},
		{"bad_exp_old_refresh", `{"exp":"invalid"}`, now.Add(-9 * 24 * time.Hour).Format(time.RFC3339), "scheduled"},
		{"missing_timestamp", `{}`, "", "forced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "e30." + base64.RawURLEncoding.EncodeToString([]byte(tc.payload)) + ".signature", "last_refresh": tc.last, "expires_at": now.Add(time.Minute).Format(time.RFC3339)}}
			require.Equal(t, tc.want, codexRefreshReasonForAccount(account, now))
		})
	}
}

func codexTestJWTExpiry(exp int64) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp))) + ".signature"
}

func TestOpenAIAutomaticRefreshUsesOfficialWindow(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "synthetic-refresh", "expires_at": time.Now().Add(time.Minute).Format(time.RFC3339)}}
	r := &OpenAITokenRefresher{}
	account.Credentials["access_token"] = codexTestJWTExpiry(time.Now().Add(20 * time.Minute).Unix())
	require.False(t, r.NeedsRefresh(account, time.Hour), "shared 30-minute/hour windows must not refresh GPT early")
	account.Credentials["access_token"] = codexTestJWTExpiry(time.Now().Add(4 * time.Minute).Unix())
	require.True(t, r.NeedsRefresh(account, time.Second), "GPT uses its own five-minute window")
	account.Credentials["access_token"] = "opaque-token"
	require.False(t, r.NeedsRefresh(account, time.Hour), "expires_at alone is not an official auth expiry claim")
	account.Credentials["last_refresh"] = time.Now().Add(-9 * 24 * time.Hour).Format(time.RFC3339)
	require.True(t, r.NeedsRefresh(account, time.Hour))
	account.Credentials["last_refresh"] = time.Now().Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	require.False(t, r.NeedsRefresh(account, time.Hour))
}
