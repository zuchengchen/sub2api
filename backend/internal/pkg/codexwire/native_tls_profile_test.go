package codexwire

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeReasoningReplayNativeTLSProfileSelection(t *testing.T) {
	for _, value := range []string{"", "openssl36", "openssl35-gnu"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(NativeTLSProfileEnvironment, value)
			name := value
			if name == "" {
				name = "openssl36"
			}
			require.Equal(t, name, NativeTLSProfileName())
			profile := NativeHTTPProfile()
			require.NotNil(t, profile)
			require.True(t, profile.H1)
			require.False(t, profile.H2)
			require.Empty(t, profile.ALPNProtocols)
			if value == "openssl35-gnu" {
				require.Equal(t, []uint16{0, 1, 2}, profile.PointFormats)
				require.Contains(t, profile.Extensions, uint16(27))
			} else {
				require.Equal(t, []uint16{0}, profile.PointFormats)
				require.NotContains(t, profile.Extensions, uint16(27))
			}
		})
	}
}
