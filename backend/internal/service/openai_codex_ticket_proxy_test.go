package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexTicketProxySessionTemplateResolvesOnlyCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, raw, username, password string
		hasPassword                   bool
	}{
		{"raw lowercase", "socks5h://user-sid-{session}:secret@proxy.example:1080", "user-sid-1234abcd", "secret", true},
		{"raw uppercase", "socks5://user-sid-{SESSION}:secret@proxy.example:1080", "user-sid-1234abcd", "secret", true},
		{"encoded lowercase", "http://user-sid-%7bsession%7d:secret@proxy.example:8080", "user-sid-1234abcd", "secret", true},
		{"encoded uppercase", "https://user-sid-%7BSESSION%7D:secret@proxy.example:443", "user-sid-1234abcd", "secret", true},
		{"multiple in username and password", "socks5h://user-{session}-{SESSION}:secret-{session}-%7BSESSION%7D@proxy.example:1080", "user-1234abcd-1234abcd", "secret-1234abcd-1234abcd", true},
		{"password only", "socks5h://user:secret-{session}@proxy.example:1080", "user", "secret-1234abcd", true},
		{"username only", "socks5h://user-{session}@proxy.example:1080", "user-1234abcd", "", false},
		{"escaped delimiters preserved", "socks5h://user%40%3A%2F-{session}:secret%40%3A%2F@proxy.example:1080", "user@:/-1234abcd", "secret@:/", true},
		{"IPv6", "https://user-{session}:secret@[::1]:443/", "user-1234abcd", "secret", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, ValidateOpenAICodexTicketHarvestProxyURL(tc.raw))
			resolved, err := resolveOpenAICodexTicketHarvestProxyURLWithRandom(tc.raw, bytes.NewReader([]byte{0x12, 0x34, 0xab, 0xcd}))
			require.NoError(t, err)
			parsed, err := url.Parse(resolved)
			require.NoError(t, err)
			require.Equal(t, tc.username, parsed.User.Username())
			password, hasPassword := parsed.User.Password()
			require.Equal(t, tc.hasPassword, hasPassword)
			require.Equal(t, tc.password, password)
			require.NotContains(t, resolved, "{session}")
			require.NotContains(t, resolved, "%7B")
			original, err := parseOpenAICodexTicketHarvestProxyURL(tc.raw)
			require.NoError(t, err)
			require.Equal(t, original.Scheme, parsed.Scheme)
			require.Equal(t, original.Host, parsed.Host)
			require.Equal(t, original.Path, parsed.Path)
		})
	}
}

func TestCodexTicketProxySessionPlainURLsRemainUnchangedAndNeedNoEntropy(t *testing.T) {
	for _, raw := range []string{
		"", "http://proxy.example:8080", "https://user:secret@[::1]:443/",
		"socks5://user%2f%40:secret%23%3a@proxy.example:1080",
		"socks5h://user-sid-12345678:secret@proxy.example:1080",
	} {
		resolved, err := resolveOpenAICodexTicketHarvestProxyURLWithRandom("  "+raw+"  ", nil)
		require.NoError(t, err)
		require.Equal(t, raw, resolved)
	}
}

func TestCodexTicketProxySessionRejectsTemplatesOutsideCredentialsSafely(t *testing.T) {
	for _, raw := range []string{
		"socks5h://user:secret@{session}.example:1080",
		"socks5h://user:secret@%7Bsession%7D.example:1080",
		"socks5h://user:secret@[fe80::1%25%7Bsession%7D]:1080",
		"socks5h://user:secret@proxy.example:{session}",
		"socks5h://user:secret@proxy.example:1080/{session}",
		"socks5h://user:secret@proxy.example:1080/%7BSESSION%7D",
		"socks5h://user:secret@proxy.example:1080?session={session}",
		"socks5h://user:secret@proxy.example:1080#{SESSION}",
		"ftp://user-{session}:secret@proxy.example:1080",
		"http://user-{session}:secret%zz@proxy.example:8080",
	} {
		err := ValidateOpenAICodexTicketHarvestProxyURL(raw)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
		require.NotContains(t, err.Error(), "proxy.example")
		require.Empty(t, MaskProxyURL(raw))
		resolved, err := resolveOpenAICodexTicketHarvestProxyURL(raw)
		require.Error(t, err)
		require.Empty(t, resolved)
		require.NotContains(t, err.Error(), "secret")
		require.False(t, IsMaskedProxyURL(strings.ReplaceAll(raw, "secret", "***")))
	}
}

func TestCodexTicketProxySessionMaskRoundTripKeepsTemplate(t *testing.T) {
	for _, template := range []string{"{session}", "{SESSION}", "%7bsession%7d", "%7BSESSION%7D"} {
		raw := "socks5h://user-sid-" + template + ":secret-" + template + "@proxy.example:1080"
		masked := MaskProxyURL(raw)
		require.NotEmpty(t, masked)
		require.NotContains(t, masked, "secret")
		require.True(t, IsMaskedProxyURL(masked))
		require.True(t, IsMaskedProxyURL("socks5h://user-sid-"+template+":***@proxy.example:1080"))
		require.NoError(t, ValidateOpenAICodexTicketHarvestProxyURL(masked))
		original, err := parseOpenAICodexTicketHarvestProxyURL(raw)
		require.NoError(t, err)
		parsed, err := url.Parse(masked)
		require.NoError(t, err)
		require.Equal(t, original.User.Username(), parsed.User.Username())
		password, ok := parsed.User.Password()
		require.True(t, ok)
		require.Equal(t, "***", password)
	}
}

type codexTicketProxyEntropyFailure struct{}

func (codexTicketProxyEntropyFailure) Read([]byte) (int, error) {
	return 0, errors.New(cookieTestSecret)
}

func TestCodexTicketProxySessionEntropyFailureDoesNotExposeSecrets(t *testing.T) {
	for _, reader := range []io.Reader{codexTicketProxyEntropyFailure{}, bytes.NewReader([]byte{1, 2, 3})} {
		resolved, err := resolveOpenAICodexTicketHarvestProxyURLWithRandom("socks5h://user-{session}:secret@proxy.example:1080", reader)
		require.ErrorIs(t, err, errOpenAICodexTicketProxySessionUnavailable)
		require.Empty(t, resolved)
		assertCookieRecoverySafe(t, err)
		require.Nil(t, errors.Unwrap(err))
	}
}

func TestCodexTicketProxySessionEachLegacyProbeUsesNewSession(t *testing.T) {
	u := &httpUpstreamRecorder{responses: []*http.Response{cookieWSHTTPResponse("True"), cookieWSHTTPResponse("True")}}
	s, _ := cookieWSTestService(t, u)
	template := "socks5h://user-sid-{session}:secret@proxy.example:1080"
	var previous string
	for i := 0; i < 2; i++ {
		_, err := s.doOpenAICodexTicketProbe(context.Background(), ticketTestAccount(41), "test-token", openAICodexTicketDefaultModel, template, time.Second)
		require.NoError(t, err)
		parsed, err := url.Parse(u.lastProxyURL)
		require.NoError(t, err)
		session := strings.TrimPrefix(parsed.User.Username(), "user-sid-")
		require.Regexp(t, "^[0-9a-f]{8}$", session)
		require.NotEqual(t, previous, session)
		previous = session
	}
	require.Len(t, u.requests, 2)
}

func TestCookieWSProxySessionInvalidConfigurationIsSafeAndDoesNotSend(t *testing.T) {
	u := &httpUpstreamRecorder{}
	s, d := cookieWSTestService(t, u)
	_, err := s.doOpenAICookieWSHTTPProbe(context.Background(), ticketTestAccount(41), "unused", "socks5h://user:secret@{session}.example:1080", newOpenAICookieWSIdentity())
	var failure *openAICookieWSRecoveryFailure
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "configuration", failure.detail.Stage)
	require.Equal(t, "cookie_ws_harvest_proxy_invalid", failure.detail.Code)
	require.NotContains(t, err.Error(), "secret")
	require.Empty(t, u.requests)
	require.Zero(t, d.dials)
}
