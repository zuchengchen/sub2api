package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRequestIntegrityExplicitModeIgnoresRetiredProtection(t *testing.T) {
	for _, marker := range []any{nil, false, true, "invalid", map[string]any{"enabled": true, "mode": "legacy"}, map[string]any{"enabled": true, "mode": "mode1", "policy_version": 3}} {
		a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"anti_degradation": true, "anti_degrade": marker}}
		require.Equal(t, "off", a.RequestIntegrityMode())
		for _, mode := range []string{"off", "observe", "enforce"} {
			a.Extra[requestIntegrityModeKey] = mode
			require.Equal(t, mode, a.RequestIntegrityMode())
		}
	}
}
