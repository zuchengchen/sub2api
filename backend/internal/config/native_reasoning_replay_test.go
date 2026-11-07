package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNativeReasoningReplayConfigBounds(t *testing.T) {
	require.NoError(t, (GatewayNativeReasoningReplayConfig{}).Validate())
	require.NoError(t, (GatewayNativeReasoningReplayConfig{TTLSeconds: 3600, IOTimeoutMilliseconds: 100, MaxEntryBytes: 256 << 10, MaxEntriesPerUser: 64, MaxEntriesGlobal: 1024, MaxLookups: 32, MaxHistoryBytes: 4 << 20, MaxOutputItems: 128}).Validate())
	for _, c := range []GatewayNativeReasoningReplayConfig{{TTLSeconds: -1}, {TTLSeconds: 3601}, {IOTimeoutMilliseconds: 101}, {MaxEntryBytes: 257 << 10}, {MaxEntriesPerUser: 65}, {MaxEntriesGlobal: 1025}, {MaxLookups: 33}, {MaxHistoryBytes: 5 << 20}, {MaxOutputItems: 129}} {
		require.Error(t, c.Validate())
	}
}
