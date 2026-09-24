package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type cookieProxySessionRequest struct {
	proxy   string
	profile HTTPUpstreamProfile
	close   bool
}

type cookieProxySessionHTTP struct {
	mu             sync.Mutex
	answers        []string
	transportError bool
	requests       []cookieProxySessionRequest
}

func (u *cookieProxySessionHTTP) Do(req *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.requests = append(u.requests, cookieProxySessionRequest{proxy: proxy, profile: HTTPUpstreamProfileFromContext(req.Context()), close: req.Close})
	answer := "True"
	if len(u.answers) > 0 {
		answer, u.answers = u.answers[0], u.answers[1:]
	}
	transportError := u.transportError
	u.mu.Unlock()
	if transportError {
		return nil, errors.New("proxyconnect: failed through " + proxy)
	}
	return cookieWSHTTPResponse(answer), nil
}

func (u *cookieProxySessionHTTP) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func (u *cookieProxySessionHTTP) snapshot() []cookieProxySessionRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]cookieProxySessionRequest(nil), u.requests...)
}

type cookieProxySessionDialer struct {
	mu      sync.Mutex
	proxies []string
	inner   cookieReplenishmentDialer
}

func (d *cookieProxySessionDialer) Dial(ctx context.Context, wsURL string, headers http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	d.mu.Lock()
	d.proxies = append(d.proxies, proxy)
	d.mu.Unlock()
	return d.inner.Dial(ctx, wsURL, headers, proxy)
}

func (d *cookieProxySessionDialer) assertDirect(t *testing.T, count int) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	require.Len(t, d.proxies, count)
	for _, proxy := range d.proxies {
		require.Empty(t, proxy, "Cookie candidate and business sockets must stay direct")
	}
}

func cookieProxySessionFixture(t *testing.T, proxyTemplate string, answers ...string) (*OpenAIGatewayService, *Account, *cookieProxySessionHTTP, *cookieProxySessionDialer) {
	t.Helper()
	s, account, _, _ := cookieReplenishmentFixture(t, false)
	u := &cookieProxySessionHTTP{answers: answers}
	d := &cookieProxySessionDialer{inner: cookieReplenishmentDialer{answer: "True"}}
	s.httpUpstream = u
	s.cfg.Gateway.OpenAICodexTicket.HarvestProxyURL = proxyTemplate
	s.getOpenAIWSConnPool().setClientDialerForTest(d)
	return s, account, u, d
}

func cookieProxySessionFromRequest(t *testing.T, request cookieProxySessionRequest) string {
	t.Helper()
	require.Equal(t, HTTPUpstreamProfileOpenAIHarvest, request.profile)
	require.True(t, request.close, "each attempt requires a new HTTP connection")
	parsed, err := url.Parse(request.proxy)
	require.NoError(t, err)
	require.NotNil(t, parsed.User)
	require.Equal(t, "socks5h", parsed.Scheme)
	require.Equal(t, "dynamic.example:1080", parsed.Host)
	username := parsed.User.Username()
	require.True(t, strings.HasPrefix(username, "customer-session-"))
	session := strings.TrimPrefix(username, "customer-session-")
	require.Regexp(t, `^[0-9a-f]{8}$`, session)
	password, ok := parsed.User.Password()
	require.True(t, ok)
	require.Equal(t, "proxy-test-secret", password)
	return session
}

func TestOpenAICookieWSProxySessionChangesOnlyOnActualHarvestRetry(t *testing.T) {
	for _, placeholder := range []string{"{session}", "{SESSION}", "%7Bsession%7D", "%7bSESSION%7d"} {
		t.Run(placeholder, func(t *testing.T) {
			template := "socks5h://customer-session-" + placeholder + ":proxy-test-secret@dynamic.example:1080"
			s, account, upstream, dialer := cookieProxySessionFixture(t, template, "False", "False", "True")
			key := openAICookieWSKeySlot(account.ID, openAICodexTicketDefaultModel, 0)
			for attempt := 0; attempt < 3; attempt++ {
				s.refreshOpenAICookieWSSlot(context.Background(), account, 0)
				require.Len(t, upstream.snapshot(), attempt+1)
				if attempt < 2 {
					require.False(t, s.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, 0).ready(time.Now()))
					s.refreshOpenAICookieWSSlot(context.Background(), account, 0)
					require.Len(t, upstream.snapshot(), attempt+1, "backoff must not consume another proxy session")
					raw, ok := s.openaiCookieWSRetry.Load(key)
					require.True(t, ok)
					retry := *raw.(*openAICookieWSRetryState)
					retry.nextAttemptAt = time.Now().Add(-time.Second)
					s.openaiCookieWSRetry.Store(key, &retry)
				}
			}
			seen := make(map[string]bool)
			for _, request := range upstream.snapshot() {
				session := cookieProxySessionFromRequest(t, request)
				require.False(t, seen[session], "each retry must receive a newly resolved proxy session")
				seen[session] = true
			}
			require.True(t, s.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, 0).ready(time.Now()))
			s.refreshOpenAICookieWSSlot(context.Background(), account, 0)
			require.Len(t, upstream.snapshot(), 3, "a healthy Cookie is not re-harvested to rotate the session")
			require.Equal(t, template, s.openAICodexTicketHarvestProxyURLContext(context.Background()), "configuration keeps the template for the next actual request")
			dialer.assertDirect(t, 1)
			require.Len(t, dialer.inner.snapshot()[0].writes, 2)
		})
	}
}

func TestOpenAICookieWSProxySessionHealthyPoolDoesNotHarvestAgain(t *testing.T) {
	template := "socks5h://customer-session-{session}:proxy-test-secret@dynamic.example:1080"
	s, account, upstream, dialer := cookieProxySessionFixture(t, template)
	s.refreshOpenAICookieWSTickets(context.Background())
	pool := s.getOpenAIWSConnPool()
	require.Equal(t, [3]int{1, 1, 1}, pool.CookieVerifiedCounts(account.ID))
	requests := upstream.snapshot()
	require.Len(t, requests, 3)
	seen := make(map[string]bool)
	for _, request := range requests {
		session := cookieProxySessionFromRequest(t, request)
		require.False(t, seen[session], "independent Cookie groups receive independent proxy sessions")
		seen[session] = true
	}
	dialer.assertDirect(t, 6)
	s.refreshOpenAICookieWSTickets(context.Background())
	require.Len(t, upstream.snapshot(), 3)
	dialer.assertDirect(t, 6)
	before := cookieReplenishmentSockets(t, pool, account.ID)
	pool.evictConn(account.ID, before[1].id)
	s.refreshOpenAICookieWSTickets(context.Background())
	require.Equal(t, [3]int{1, 1, 1}, pool.CookieVerifiedCounts(account.ID))
	require.Len(t, upstream.snapshot(), 3, "replacement business WS reuses its valid Cookie")
	dialer.assertDirect(t, 7)
	require.Equal(t, template, s.openAICodexTicketHarvestProxyURLContext(context.Background()))
}

func TestOpenAICookieWSProxySessionResolvedCredentialsStayOutOfRecoveryDiagnostics(t *testing.T) {
	template := "socks5h://customer-session-{SESSION}:proxy-test-secret@dynamic.example:1080"
	s, account, upstream, dialer := cookieProxySessionFixture(t, template)
	upstream.transportError = true
	s.refreshOpenAICookieWSSlot(context.Background(), account, 0)
	requests := upstream.snapshot()
	require.Len(t, requests, 1)
	session := cookieProxySessionFromRequest(t, requests[0])
	diagnostic := s.openAICookieWSRecoverySnapshot(account.ID, 0, "refresh")
	require.NotNil(t, diagnostic)
	require.NotNil(t, diagnostic.LastError)
	require.Equal(t, "cookie_ws_proxy_connection", diagnostic.LastError.Code)
	encoded, err := json.Marshal(diagnostic)
	require.NoError(t, err)
	for _, secret := range []string{requests[0].proxy, template, "customer-session", "proxy-test-secret", "dynamic.example", session} {
		require.NotContains(t, string(encoded), secret)
	}
	dialer.assertDirect(t, 0)
}
