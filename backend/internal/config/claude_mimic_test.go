package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func validClaudeMimic() ClaudeMimicConfig {
	return ClaudeMimicConfig{
		UserAgentVersion:        "2.1.283",
		StainlessOS:             "Linux",
		StainlessArch:           "x64",
		StainlessLang:           "js",
		StainlessRuntime:        "node",
		StainlessRuntimeVersion: "v26.3.0",
		StainlessPackageVersion: "0.128.0",
	}
}

func TestValidateClaudeMimicEmptyIsAllowed(t *testing.T) {
	require.NoError(t, validateClaudeMimicConfig(ClaudeMimicConfig{}))
}

func TestValidateClaudeMimicCompleteProfileIsAllowed(t *testing.T) {
	require.NoError(t, validateClaudeMimicConfig(validClaudeMimic()))
}

func TestValidateClaudeMimicRejectsPartialProfile(t *testing.T) {
	m := validClaudeMimic()
	m.StainlessArch = ""
	err := validateClaudeMimicConfig(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stainless_arch")
}

func TestValidateClaudeMimicRejectsNonJSStainlessLang(t *testing.T) {
	m := validClaudeMimic()
	m.StainlessLang = "python"
	err := validateClaudeMimicConfig(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stainless_lang")
}

func TestValidateClaudeMimicRejectsUnsupportedUAVersion(t *testing.T) {
	for _, v := range []string{"2.0.0", "not-a-version", "2.1.300-local", ""} {
		m := validClaudeMimic()
		m.UserAgentVersion = v
		require.Error(t, validateClaudeMimicConfig(m), "version %q must be rejected", v)
	}
}

func TestValidateClaudeMimicRejectsWhitespace(t *testing.T) {
	m := validClaudeMimic()
	m.StainlessOS = " Linux"
	require.Error(t, validateClaudeMimicConfig(m))
	m = validClaudeMimic()
	m.StainlessRuntimeVersion = "v26\n3.0"
	require.Error(t, validateClaudeMimicConfig(m))
}
