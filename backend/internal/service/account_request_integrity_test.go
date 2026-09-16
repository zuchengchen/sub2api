package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountProtectionRequestIntegrityDefaults(t *testing.T) {
	off := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{}}
	require.Equal(t, "off", off.RequestIntegrityMode())

	legacy := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		AntiDegradationExtraKey:   true,
		AntiDegradeMarkerExtraKey: map[string]any{"enabled": true, "mode": "legacy"},
	}}
	require.Equal(t, "observe", legacy.RequestIntegrityMode())

	mode1 := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		AntiDegradationExtraKey: true,
		AntiDegradeMarkerExtraKey: map[string]any{
			"enabled": true, "mode": "mode1", "policy_version": mode1PolicyVersion,
		},
	}}
	require.Equal(t, "enforce", mode1.RequestIntegrityMode())
}
