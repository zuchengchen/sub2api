package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadOpenAIRequestCompressionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, yamlValue, envValue string
		want                      bool
	}{
		{name: "default off"},
		{name: "YAML opt in", yamlValue: "true", want: true},
		{name: "environment opt in", envValue: "true", want: true},
		{name: "environment disables YAML opt in", yamlValue: "true", envValue: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("GATEWAY_OPENAI_REQUEST_COMPRESSION_ENABLED", tc.envValue)
			t.Setenv("GATEWAY_OPENAI_WS_EXACT_FIDELITY_ENABLED", "true")
			// Isolate config discovery from any developer or production config.
			configFile := filepath.Join(t.TempDir(), "config.yaml")
			contents := "gateway: {}\n"
			if tc.yamlValue != "" {
				contents = "gateway:\n  openai_request_compression_enabled: " + tc.yamlValue + "\n"
			}
			require.NoError(t, os.WriteFile(configFile, []byte(contents), 0o600))
			t.Setenv("CONFIG_FILE", configFile)

			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Gateway.OpenAIRequestCompressionEnabled)
			require.True(t, cfg.Gateway.OpenAIWS.ExactFidelityEnabled, "compression policy must not change the TLS/WS fidelity flag")
		})
	}
}
