package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeReasoningReplayNativeTLSRejectsUnknownProfile(t *testing.T) {
	t.Setenv("SUB2API_CODEX_NATIVE_TLS_PROFILE", "unverified-profile")
	err := (&Config{}).Validate()
	require.ErrorContains(t, err, "SUB2API_CODEX_NATIVE_TLS_PROFILE")
}
