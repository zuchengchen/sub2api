package service

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Opt-in only. The fixture is a private JSON file outside the repository. This
// test uses real upstream credentials but never updates a production database.
type cookieWSLiveFixture struct {
	ID               int64  `json:"id"`
	Email            string `json:"email"`
	AccessToken      string `json:"access_token"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	HarvestProxyURL  string `json:"harvest_proxy_url"`
}

type cookieWSLiveHTTP struct{ HTTPUpstream }

type cookieWSLiveCaptureDialer struct {
	openAIWSClientDialer
	dir     string
	seq     atomic.Int64
	mu      sync.Mutex
	slots   map[string]int
	opened  [2]int
	active  [2]int
	peak    [2]int
	barrier chan struct{}
	ready   int
}
type cookieWSLiveCaptureConn struct {
	openAIWSClientConn
	file      string
	owner     *cookieWSLiveCaptureDialer
	slot      int
	once      sync.Once
	closeOnce sync.Once
}

func (c *cookieWSLiveCaptureConn) RequiresReaderLoop() bool            { return true }
func (c *cookieWSLiveCaptureConn) SupportsIdlePingWithoutReader() bool { return true }
func (c *cookieWSLiveCaptureConn) WriteJSON(ctx context.Context, value any) error {
	if c.owner != nil {
		c.once.Do(func() {
			c.owner.mu.Lock()
			c.owner.ready++
			if c.owner.ready == 20 {
				close(c.owner.barrier)
			}
			c.owner.mu.Unlock()
		})
		select {
		case <-c.owner.barrier:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	encoded, _ := json.Marshal(value)
	_ = os.WriteFile(c.file, encoded, 0600)
	return c.openAIWSClientConn.WriteJSON(ctx, value)
}
func (d *cookieWSLiveCaptureDialer) Dial(ctx context.Context, u string, h http.Header, p string) (openAIWSClientConn, int, http.Header, error) {
	n := d.seq.Add(1)
	encoded, _ := json.Marshal(h)
	_ = os.WriteFile(filepath.Join(d.dir, fmt.Sprintf("dial-%02d.private.json", n)), encoded, 0600)
	c, status, response, err := d.openAIWSClientDialer.Dial(ctx, u, h, p)
	if err != nil {
		return nil, status, response, err
	}
	wrapped := &cookieWSLiveCaptureConn{openAIWSClientConn: c, file: filepath.Join(d.dir, fmt.Sprintf("payload-%02d.private.json", n))}
	d.mu.Lock()
	if slot, ok := d.slots[h.Get("Cookie")]; ok {
		wrapped.owner, wrapped.slot = d, slot
		d.opened[slot]++
		d.active[slot]++
		d.peak[slot] = max(d.peak[slot], d.active[slot])
	}
	d.mu.Unlock()
	return wrapped, status, response, nil
}

func (c *cookieWSLiveCaptureConn) Close() error {
	c.closeOnce.Do(func() {
		if c.owner != nil {
			c.owner.mu.Lock()
			c.owner.active[c.slot]--
			c.owner.mu.Unlock()
		}
	})
	return c.openAIWSClientConn.Close()
}

func (*cookieWSLiveHTTP) Do(req *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
	if HTTPUpstreamProfileFromContext(req.Context()) != HTTPUpstreamProfileOpenAIHarvest || !req.Close {
		return nil, errors.New("live test refuses unvalidated HTTP business fallback")
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return nil, errors.New("invalid private harvest proxy")
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		DisableKeepAlives: true, ForceAttemptHTTP2: false,
		TLSNextProto:        make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if err := proxyutil.ConfigureTransportProxy(transport, u); err != nil {
		return nil, errors.New("proxy transport configuration failed")
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("private HTTP harvest transport failed")
	}
	return response, nil
}

func TestOpenAICookieWSLiveForwardTwenty(t *testing.T) {
	file := os.Getenv("SUB2API_COOKIE_WS_LIVE_FIXTURE")
	if file == "" {
		t.Skip("set SUB2API_COOKIE_WS_LIVE_FIXTURE to opt into billed upstream verification")
	}
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var fixture cookieWSLiveFixture
	require.NoError(t, json.Unmarshal(data, &fixture))
	require.Equal(t, "brendon_quifgy@mail.com", fixture.Email)
	require.NotEmpty(t, fixture.AccessToken)
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Gateway.OpenAICodexTicket = config.OpenAICodexTicketConfig{Enabled: true, Mode: "cookie_ws", CookieWSAccountIDs: []int64{fixture.ID}, HarvestProxyURL: fixture.HarvestProxyURL, HarvestAttemptTimeoutSeconds: 30}
	cfg.Gateway.OpenAIWS = config.GatewayOpenAIWSConfig{
		Enabled: true, OAuthEnabled: true, ResponsesWebsocketsV2: true,
		MaxConnsPerAccount: 40, MinIdlePerAccount: 0, MaxIdlePerAccount: 32,
		DialTimeoutSeconds: 15, ReadTimeoutSeconds: 90, WriteTimeoutSeconds: 15,
		QueueLimitPerConn: 4, StoreDisabledConnMode: "off", PoolTargetUtilization: 1,
	}
	account := ticketTestAccount(fixture.ID)
	account.Name = fixture.Email
	account.Concurrency = 24
	account.Credentials = map[string]any{"access_token": fixture.AccessToken, "chatgpt_account_id": fixture.ChatGPTAccountID}
	if restore := os.Getenv("SUB2API_COOKIE_WS_LIVE_TICKETS"); restore != "" {
		encoded, err := os.ReadFile(restore)
		require.NoError(t, err)
		var restored [2]*openAICookieWSTicket
		require.NoError(t, json.Unmarshal(encoded, &restored))
		if account.Extra == nil {
			account.Extra = make(map[string]any)
		}
		for slot, saved := range restored {
			require.NotNil(t, saved)
			require.True(t, saved.valid(time.Now()))
			require.Equal(t, account.ID, saved.AccountID)
			require.Equal(t, slot, saved.Slot)
			account.Extra[openAICookieWSExtraKeySlot(saved.Model, slot)] = saved
		}
	}
	repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
	svc := &OpenAIGatewayService{cfg: cfg, accountRepo: repo, httpUpstream: &cookieWSLiveHTTP{}, openaiWSStateStore: NewOpenAIWSStateStore(nil), toolCorrector: NewCodexToolCorrector()}
	dialer := &cookieWSLiveCaptureDialer{openAIWSClientDialer: newDefaultOpenAIWSClientDialer(), dir: filepath.Dir(file), barrier: make(chan struct{})}
	svc.getOpenAIWSConnPool().setClientDialerForTest(dialer)
	defer func() {
		if svc.openaiWSPool != nil {
			svc.openaiWSPool.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	var tickets [2]*openAICookieWSTicket
	for slot := 0; slot < 2; slot++ {
		for attempt := 1; attempt <= 12; attempt++ {
			svc.refreshOpenAICookieWSSlot(ctx, account, slot)
			tickets[slot] = svc.lookupOpenAICookieWSTicketSlot(account, openAICodexTicketDefaultModel, slot)
			if tickets[slot].ready(time.Now()) {
				t.Logf("slot %d HTTP and direct WS qualification passed at attempt %d", slot, attempt)
				break
			}
			if attempt < 12 {
				wait := time.Second
				if raw, ok := svc.openaiCookieWSRetry.Load(openAICookieWSKeySlot(account.ID, openAICodexTicketDefaultModel, slot)); ok {
					state := raw.(*openAICookieWSRetryState)
					t.Logf("slot %d attempt %d not qualified, reason=%s", slot, attempt, state.reason)
					if remaining := time.Until(state.nextAttemptAt); remaining > wait {
						wait = remaining
					}
				}
				select {
				case <-ctx.Done():
					t.Fatal("live acquisition deadline exceeded")
				case <-time.After(wait):
				}
			}
		}
		require.True(t, tickets[slot].ready(time.Now()), "each Cookie group must qualify before concurrent forwarding; slot=%d", slot)
	}
	require.NotEqual(t, tickets[0].Cookies, tickets[1].Cookies)
	require.NotEqual(t, tickets[0].Identity.InstallationID, tickets[1].Identity.InstallationID)
	dialer.mu.Lock()
	dialer.slots = map[string]int{tickets[0].Cookies: 0, tickets[1].Cookies: 1}
	dialer.mu.Unlock()
	ticket := tickets[0]
	privateTicket, _ := json.Marshal(tickets)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(file), "ticket.private.json"), privateTicket, 0600))
	type outcome struct {
		Round      int    `json:"round"`
		Request    int    `json:"request"`
		Passed     bool   `json:"passed"`
		Output     string `json:"output"`
		Status     int    `json:"status"`
		ElapsedMS  int64  `json:"elapsed_ms"`
		WS         bool   `json:"ws"`
		Error      bool   `json:"error"`
		ResponseID string `json:"response_id"`
	}
	all := make([]outcome, 0, 40)
	for round := 1; round <= 2; round++ {
		start := make(chan struct{})
		out := make(chan outcome, 20)
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
				c.Request.Header.Set("User-Agent", ticket.Identity.UserAgent)
				c.Request.Header.Set("originator", ticket.Identity.Originator)
				c.Request.Header.Set("session_id", fmt.Sprintf("cookie-live-session-%d", i))
				c.Set("api_key", &APIKey{ID: 900000 + int64(i), UserID: 900000 + int64(i)})
				body, _ := json.Marshal(openAICookieWSProbePayload(false))
				<-start
				begin := time.Now()
				result, forwardErr := svc.Forward(ctx, c, account, body)
				var observation openAICookieWSObservation
				forEachOpenAISSEFrame(recorder.Body.String(), func(event string, frame []byte) { observation.event(frame, event) })
				answer := strings.TrimSpace(observation.delta.String())
				r := outcome{Round: round, Request: i + 1, Passed: observation.completed && observation.trueAnswer && !observation.failed, Output: answer, Status: recorder.Code, ElapsedMS: time.Since(begin).Milliseconds(), Error: forwardErr != nil}
				if result != nil {
					r.WS = result.OpenAIWSMode
					r.ResponseID = result.ResponseID
					if r.ResponseID == "" {
						r.ResponseID = result.RequestID
					}
				}
				out <- r
			}(i)
		}
		close(start)
		wg.Wait()
		close(out)
		passed := 0
		for r := range out {
			all = append(all, r)
			if r.Passed && r.WS && !r.Error {
				passed++
			}
		}
		t.Logf("production Forward round %d: %d/20 True via WS", round, passed)
	}
	dialer.mu.Lock()
	opened, peak := dialer.opened, dialer.peak
	dialer.mu.Unlock()
	result := map[string]any{"account": fixture.Email, "model": openAICodexTicketDefaultModel, "reasoning_effort": "low", "cookie_groups": 2, "opened_ws_per_group": opened, "peak_ws_per_group": peak, "captured_at": ticket.CapturedAt, "refresh_at": ticket.RefreshAt, "expires_at": ticket.ExpiresAt, "active_ping": false, "results": all}
	encoded, err := json.MarshalIndent(result, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(file), "go-live-results.json"), encoded, 0600))
	require.Len(t, all, 40)
	require.Equal(t, [2]int{10, 10}, peak, "each independently acquired Cookie group must serve ten concurrent WS")
	for _, r := range all {
		require.True(t, r.Passed && r.WS && !r.Error, "round=%d request=%d output=%s status=%d", r.Round, r.Request, r.Output, r.Status)
	}
}
