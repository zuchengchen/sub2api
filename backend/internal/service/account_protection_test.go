package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountProtectionExtraMergePreserves429NearLimitKeys(t *testing.T) {
	original := map[string]any{
		"auto_pause_5h_disabled": true,
		"auto_pause_7d_disabled": false,
		"auto_pause_5h_threshold": 0.95,
		"auto_pause_7d_threshold": 0.95,
		"unrelated_ops_key":       "keep-me",
	}
	merged, applied := ApplyOpenAILegacyProtectionBackfill(PlatformOpenAI, nil, original)
	require.True(t, applied)
	require.Equal(t, true, merged["auto_pause_5h_disabled"])
	require.Equal(t, false, merged["auto_pause_7d_disabled"])
	require.Equal(t, 0.95, merged["auto_pause_5h_threshold"])
	require.Equal(t, 0.95, merged["auto_pause_7d_threshold"])
	require.Equal(t, "keep-me", merged["unrelated_ops_key"])
	require.Equal(t, true, merged[AntiDegradationExtraKey])
	require.Equal(t, "legacy", merged[ProtectionScopeExtraKey])
	require.Equal(t, string(codexFingerprintSession), merged[codexFingerprintModeExtraKey])
	require.Equal(t, true, merged[tlsFingerprintEnabledKey])
	require.Equal(t, "nodejs24", merged[tlsFingerprintBuiltinKey])
	require.NotContains(t, merged, tlsFingerprintProfileIDKey)
	marker, ok := merged[AntiDegradeMarkerExtraKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, marker["enabled"])
	require.Equal(t, string(AntiDegradeModeLegacy), marker["mode"])
}

func TestAccountProtectionBackfillSkipsShadowAndNonOpenAI(t *testing.T) {
	parent := int64(9)
	extra := map[string]any{"auto_pause_5h_threshold": 0.8}
	_, applied := ApplyOpenAILegacyProtectionBackfill(PlatformAnthropic, nil, extra)
	require.False(t, applied)
	_, applied = ApplyOpenAILegacyProtectionBackfill(PlatformOpenAI, &parent, extra)
	require.False(t, applied)
	_, applied = ApplyOpenAILegacyProtectionBackfill(PlatformOpenAI, nil, map[string]any{accountProxyModeExtraKey: "random"})
	require.False(t, applied)
}

func TestAccountProtectionBackfillIdempotentWhenAlreadyProtected(t *testing.T) {
	first, applied := ApplyOpenAILegacyProtectionBackfill(PlatformOpenAI, nil, map[string]any{
		"auto_pause_5h_threshold": 0.95,
	})
	require.True(t, applied)
	second, applied := ApplyOpenAILegacyProtectionBackfill(PlatformOpenAI, nil, first)
	require.False(t, applied)
	require.Equal(t, first[AntiDegradationExtraKey], second[AntiDegradationExtraKey])
	require.Equal(t, first["auto_pause_5h_threshold"], second["auto_pause_5h_threshold"])
}

func TestAccountProtectionPreserveDoesNotDrop429KeysOnOrdinarySave(t *testing.T) {
	current := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			AntiDegradationExtraKey:       true,
			ProtectionScopeExtraKey:       "legacy",
			AntiDegradeMarkerExtraKey:     map[string]any{"enabled": true, "mode": "legacy"},
			codexFingerprintModeExtraKey:  string(codexFingerprintSession),
			tlsFingerprintEnabledKey:      true,
			tlsFingerprintBuiltinKey:      "nodejs24",
			"auto_pause_5h_threshold":     0.95,
			"auto_pause_7d_disabled":      true,
		},
	}
	incoming := map[string]any{
		"auto_pause_5h_threshold": 0.95,
		"auto_pause_7d_disabled":  true,
		"notes_extra":             "form",
	}
	merged, err := mergeAccountProtectionForSave(context.Background(), current, incoming)
	require.NoError(t, err)
	require.Equal(t, true, merged[AntiDegradationExtraKey])
	require.Equal(t, "legacy", merged[ProtectionScopeExtraKey])
	require.Equal(t, 0.95, merged["auto_pause_5h_threshold"])
	require.Equal(t, true, merged["auto_pause_7d_disabled"])
}

func TestProtectionStrategyRegistryContainsLegacyAndMode1(t *testing.T) {
	require.Equal(t, []string{"legacy", "mode1"}, RegisteredProtectionModes())
	profiles := ListAntiDegradeStrategyProfiles()
	require.Len(t, profiles, 2)
}

func TestAccountProtectionDefaultOffForNewAccounts(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{}}
	require.False(t, account.AntiDegradationEnabled())
	require.Equal(t, "disabled", account.ProtectionMode())
}
