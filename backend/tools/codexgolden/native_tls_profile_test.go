//go:build linux

package main

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	"github.com/stretchr/testify/require"
)

func TestNativeTLSProfileBoundToCandidateConfigFingerprint(t *testing.T) {
	cfg := gatewayScenarioConfig()
	t.Setenv(codexwire.NativeTLSProfileEnvironment, "openssl36")
	old := gatewayConfigFingerprint(cfg)
	t.Setenv(codexwire.NativeTLSProfileEnvironment, "openssl35-gnu")
	require.NotEqual(t, old, gatewayConfigFingerprint(cfg))
	t.Setenv(codexwire.NativeTLSProfileEnvironment, "")
	require.Equal(t, old, gatewayConfigFingerprint(cfg))
}
