package service

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func mint780Pair(exp time.Time, node string) []string {
	claims := `{"exp":` + strconv.FormatInt(exp.Unix(), 10) + `,"host":"chat.gateway.` + node + `.api.openai.com"}`
	return []string{"__cflb=route", "__oailb=e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"}
}

func mint780State(issued time.Time) string {
	raw := make([]byte, 585)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}

func storeTestCodex780Ticket(svc *OpenAIGatewayService, account *Account) *openAICodexTicket {
	now := time.Now()
	pair := mint780Pair(now.Add(time.Hour), "unified-174")
	ticket := &openAICodexTicket{
		AccountID:      account.ID,
		Model:          openAICodexTicketDefaultModel,
		State:          mint780State(now),
		Length:         openAICodex780Length,
		Cookies:        strings.Join(pair, "; "),
		HarvestCookies: pair,
		Gateway:        "unified-174",
		Transport:      "sse",
		IssuedAt:       now,
		CapturedAt:     now,
		ExpiresAt:      now.Add(openAICodex780TTL),
		UserAgent:      openAICodex780UserAgent,
		Originator:     openAICodex780Originator,
	}
	svc.storeOpenAICodex780Ticket(ticket)
	return ticket
}

func TestCodex780RouteValidation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	pair := mint780Pair(now.Add(time.Hour), "unified-174")
	got, _, err := codex780Route(pair, "any", now)
	require.NoError(t, err)
	require.Equal(t, pair, got)
	require.Equal(t, "unified-174", codex780CookieGateway(pair))
	_, _, err = codex780Route(pair[:1], "any", now)
	require.Error(t, err)
	_, _, err = codex780Route(mint780Pair(now.Add(-time.Second), "unified-174"), "any", now)
	require.Error(t, err)
	_, _, err = codex780Route(pair, "unified-88", now)
	require.Error(t, err)
}

func TestCodex780CreatedBoundedAndStrict(t *testing.T) {
	good := `{"type":"response.created","response":{"id":"r1","model":"gpt-6-astra"}}`
	require.NoError(t, readCodex780Created(strings.NewReader("data: "+good+"\n\n"), "gpt-6-astra"))
	require.Error(t, readCodex780Created(strings.NewReader("data: "+good+"\n\n"), "gpt-6-sol"))
	require.Error(t, readCodex780Created(strings.NewReader("data: {\"type\":\"response.failed\"}\n\n"), "gpt-6-astra"))
}

func TestCodex780MintProbe(t *testing.T) {
	account := ticketTestAccount(1)
	account.Credentials["chatgpt_account_id"] = "acct"
	s := &OpenAIGatewayService{cfg: &config.Config{}}
	pair := mint780Pair(time.Now().Add(time.Hour), "unified-174")
	upstream := &codex780Upstream{
		state: mint780State(time.Now()),
		pair:  pair,
		body:  `data: {"type":"response.created","response":{"id":"r","model":"gpt-6-astra"}}` + "\n\n",
	}
	s.httpUpstream = upstream
	out := s.mintCodex780(context.Background(), account, "token", "gpt-6-astra", "socks5h://127.0.0.1:1080")
	require.NoError(t, out.Err)
	require.Equal(t, openAICodex780Length, len(out.State))
	require.Equal(t, pair, out.Cookies)
	require.Equal(t, "unified-174", out.Gateway)
	require.Equal(t, "codex-tui", upstream.last.Header.Get("originator"))
	require.NotContains(t, string(upstream.lastBody), "additional_tools")
}

func TestApplyOpenAICodexTicket_TiboHopInjects780(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292}, nil)
	account := ticketTestAccount(41)
	account.Extra = map[string]any{"openai_excel_bps": true}
	ticket := storeTestCodex780Ticket(svc, account)
	h := http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(withOpenAITiboInjectTicket(context.Background()), account, "gpt-6-astra", h))
	require.Equal(t, ticket.State, h.Get(openAICodexTurnStateHeader))
	require.Equal(t, openAICodex780Length, len(h.Get(openAICodexTurnStateHeader)))
	require.Contains(t, h.Get("Cookie"), "__cflb=")
	require.Contains(t, h.Get("Cookie"), "__oailb=")
}

type codex780Upstream struct {
	last     *http.Request
	lastBody []byte
	state    string
	pair     []string
	body     string
}

func (u *codex780Upstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.last = req
	if req.Body != nil {
		u.lastBody, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, u.state)
	for _, c := range u.pair {
		h.Add("Set-Cookie", c)
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(u.body))}, nil
}

func (u *codex780Upstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}
