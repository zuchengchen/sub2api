package config

import "fmt"

// GatewayNativeReasoningReplayConfig only controls bounded storage work. The
// authenticated user allowlist and rollout mode are an operator-owned setting.
type GatewayNativeReasoningReplayConfig struct {
	TTLSeconds            int `mapstructure:"ttl_seconds"`
	IOTimeoutMilliseconds int `mapstructure:"io_timeout_ms"`
	MaxEntryBytes         int `mapstructure:"max_entry_bytes"`
	MaxEntriesPerUser     int `mapstructure:"max_entries_per_user"`
	MaxEntriesGlobal      int `mapstructure:"max_entries_global"`
	MaxLookups            int `mapstructure:"max_lookups"`
	MaxHistoryBytes       int `mapstructure:"max_history_bytes"`
	MaxOutputItems        int `mapstructure:"max_output_items"`
}

func (c GatewayNativeReasoningReplayConfig) Validate() error {
	for _, limit := range []struct {
		name           string
		value, maximum int
	}{
		{"ttl_seconds", c.TTLSeconds, 3600},
		{"io_timeout_ms", c.IOTimeoutMilliseconds, 100},
		{"max_entry_bytes", c.MaxEntryBytes, 256 << 10},
		{"max_entries_per_user", c.MaxEntriesPerUser, 64},
		{"max_entries_global", c.MaxEntriesGlobal, 1024},
		{"max_lookups", c.MaxLookups, 32},
		{"max_history_bytes", c.MaxHistoryBytes, 4 << 20},
		{"max_output_items", c.MaxOutputItems, 128},
	} {
		if limit.value < 0 || limit.value > limit.maximum {
			return fmt.Errorf("gateway.native_reasoning_replay.%s must be between 0 and %d", limit.name, limit.maximum)
		}
	}
	return nil
}
