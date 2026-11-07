package codexwire

import (
	"os"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

const NativeTLSProfileEnvironment = "SUB2API_CODEX_NATIVE_TLS_PROFILE"

// NativeTLSProfileName is a process-owned choice shared by Responses and auth
// HTTP. Empty retains the existing packaged OpenSSL profile. The GNU profile
// describes the pinned source build and must be explicitly selected; neither
// client headers nor a host-library guess can change this choice.
// Config.Validate rejects unknown values before the application starts.
func NativeTLSProfileName() string {
	if strings.TrimSpace(os.Getenv(NativeTLSProfileEnvironment)) == "openssl35-gnu" {
		return "openssl35-gnu"
	}
	return "openssl36"
}

func NativeHTTPProfile() *tlsfingerprint.Profile {
	return tlsfingerprint.BuiltinProfile(NativeTLSProfileName())
}
