package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	openAICodex780Length     = 780
	openAICodex780TTL        = 240 * time.Second
	openAICodex780UserAgent  = "codex-tui/0.154.0 (Ubuntu 24.04; x86_64) OVH (codex-tui; 0.154.0)"
	openAICodex780Originator = "codex-tui"
	openAITibo780CookieKey   = "openai_tibo_780_cookie"
)

var (
	codex780GatewayRE      = regexp.MustCompile(`(?:^|[.])(?:chat\.)?gateway\.(unified-[0-9]{1,5})(?:[.]|$)`)
	codex780TargetRE       = regexp.MustCompile(`^(?:chat\.gateway\.)?(unified-[0-9]{1,5})(?:\.api\.openai\.com)?$`)
	codex780GatewayAliasRE = regexp.MustCompile("^(?:unified[-_.]?)?([0-9]{1,5})$")
)

type codex780MintError struct {
	kind   string
	detail string
}

func (e *codex780MintError) Error() string { return e.detail }

func mint780RouteError(detail string) error {
	return &codex780MintError{kind: "invalid_route", detail: detail}
}

func normalizeCodex780Gateway(target string) string {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" || target == "*" {
		return "any"
	}
	if match := codex780GatewayAliasRE.FindStringSubmatch(target); len(match) == 2 {
		return "unified-" + match[1]
	}
	if match := codex780TargetRE.FindStringSubmatch(target); len(match) == 2 {
		return match[1]
	}
	return target
}

func codex780GatewayAllowed(actual, target string) bool {
	return actual != "" && (target == "any" || actual == target)
}

func codex780CookieGateway(cookies []string) string {
	for _, cookie := range cookies {
		name, value, _ := strings.Cut(cookie, "=")
		if name != "__oailb" {
			continue
		}
		parts := strings.Split(value, ".")
		if len(parts) != 3 {
			return ""
		}
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
		match := codex780GatewayRE.FindSubmatch(raw)
		if len(match) == 2 {
			return string(match[1])
		}
	}
	return ""
}

// Only the two LB cookies are retained. JWT claims are routing hints, not verified identity.
func codex780Route(cookies []string, target string, now time.Time) ([]string, time.Time, error) {
	target = normalizeCodex780Gateway(target)
	bad := mint780RouteError("route_cookie_invalid")
	pair := map[string]string{}
	for _, cookie := range cookies {
		name, value, ok := strings.Cut(cookie, "=")
		if name != "__cflb" && name != "__oailb" {
			continue
		}
		if !ok || value == "" || len(value) > 4096 || strings.ContainsAny(value, ";,\r\n\t ") || pair[name] != "" {
			return nil, time.Time{}, bad
		}
		for _, c := range value {
			if c < 33 || c > 126 {
				return nil, time.Time{}, bad
			}
		}
		pair[name] = value
	}
	if target == "" {
		return nil, time.Time{}, mint780RouteError("route_target_missing")
	}
	if pair["__cflb"] == "" {
		return nil, time.Time{}, mint780RouteError("route_cflb_missing")
	}
	if pair["__oailb"] == "" {
		return nil, time.Time{}, mint780RouteError("route_oailb_missing")
	}
	parts := strings.Split(pair["__oailb"], ".")
	if len(parts) != 3 {
		return nil, time.Time{}, bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, time.Time{}, bad
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 || claims.Exp >= 4102444800 {
		return nil, time.Time{}, mint780RouteError("route_expiry_invalid")
	}
	if claims.Exp <= now.Unix() {
		return nil, time.Time{}, mint780RouteError("route_pair_expired")
	}
	match := codex780GatewayRE.FindSubmatch(raw)
	if len(match) != 2 {
		return nil, time.Time{}, mint780RouteError("route_gateway_unknown")
	}
	if !codex780GatewayAllowed(string(match[1]), target) {
		return nil, time.Time{}, mint780RouteError("route_gateway_mismatch: got=" + string(match[1]) + " want=" + target)
	}
	return []string{"__cflb=" + pair["__cflb"], "__oailb=" + pair["__oailb"]}, time.Unix(claims.Exp, 0), nil
}

type openAICodex780Shape struct {
	Blocks   int
	IssuedAt time.Time
}

func parseOpenAICodex780Shape(value string) (openAICodex780Shape, error) {
	value = strings.TrimSpace(value)
	if len(value) > 2048 || strings.ContainsAny(value, "\r\n\t ") {
		return openAICodex780Shape{}, errors.New("invalid state encoding")
	}
	core := strings.TrimRight(value, "=")
	if len(value)-len(core) > 2 {
		return openAICodex780Shape{}, errors.New("invalid state padding")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(core)
	if err != nil || len(raw) < 73 || raw[0] != 0x80 || (len(raw)-57)%16 != 0 {
		return openAICodex780Shape{}, errors.New("unrecognized state envelope")
	}
	issuedUnix := binary.BigEndian.Uint64(raw[1:9])
	if issuedUnix < 1577836800 || issuedUnix >= 4102444800 {
		return openAICodex780Shape{}, errors.New("state timestamp out of range")
	}
	return openAICodex780Shape{Blocks: (len(raw) - 57) / 16, IssuedAt: time.Unix(int64(issuedUnix), 0)}, nil
}

func splitCodex780SSELine(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b != '\r' && b != '\n' {
			continue
		}
		n := 1
		if b == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			if i+1 < len(data) && data[i+1] == '\n' {
				n = 2
			}
		}
		return i + n, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func readCodex780Created(body io.Reader, model string) error {
	scanner := bufio.NewScanner(io.LimitReader(body, 16*1024))
	scanner.Buffer(make([]byte, 1024), 16*1024)
	scanner.Split(splitCodex780SSELine)
	var data []string
	eventName := ""
	for scanner.Scan() {
		line := strings.TrimPrefix(scanner.Text(), "\ufeff")
		if line == "" {
			raw := []byte(strings.Join(data, "\n"))
			var event struct {
				Type     string `json:"type"`
				Response struct {
					ID    string `json:"id"`
					Model string `json:"model"`
				} `json:"response"`
			}
			if json.Unmarshal(raw, &event) == nil && event.Type == "response.created" {
				if (eventName != "" && eventName != event.Type) || strings.TrimSpace(event.Response.ID) == "" || event.Response.Model != model {
					return &codex780MintError{kind: "model_mismatch", detail: "mint model declaration mismatch"}
				}
				return nil
			}
			data = nil
			eventName = ""
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		if field == "data" {
			data = append(data, value)
		}
		if field == "event" {
			eventName = value
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return &codex780MintError{kind: "response_incomplete_or_error", detail: "mint created event missing or incomplete"}
}

func codex780ResponseRoute(resp *http.Response, seed []string, target string, now time.Time) ([]string, error) {
	var incoming []string
	for _, cookie := range resp.Cookies() {
		if cookie.Name != "__cflb" && cookie.Name != "__oailb" {
			continue
		}
		if cookie.Value == "" || cookie.MaxAge < 0 || (cookie.MaxAge == 0 && !cookie.Expires.IsZero() && !now.Before(cookie.Expires)) {
			return nil, mint780RouteError("route_cookie_deleted")
		}
		incoming = append(incoming, cookie.Name+"="+cookie.Value)
	}
	if len(incoming) == 0 {
		incoming = seed
	}
	clean, _, err := codex780Route(incoming, target, now)
	return clean, err
}

func openAICodex780Key(accountID int64, model string) string {
	return "780\x00" + openAICodexTicketKey(accountID, strings.TrimSpace(model))
}

func (t *openAICodexTicket) usable780(now time.Time) bool {
	if t == nil || t.Length != openAICodex780Length {
		return false
	}
	state := strings.TrimSpace(t.State)
	if len(state) != openAICodex780Length || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return false
	}
	shape, err := parseOpenAICodex780Shape(state)
	if err != nil || shape.IssuedAt.After(now.Add(30*time.Second)) || !now.Before(shape.IssuedAt.Add(openAICodex780TTL)) {
		return false
	}
	cookies := t.HarvestCookies
	if len(cookies) == 0 && strings.TrimSpace(t.Cookies) != "" {
		cookies = strings.Split(t.Cookies, "; ")
	}
	target := t.Gateway
	if target == "" {
		target = "any"
	}
	if _, _, err := codex780Route(cookies, target, now); err != nil {
		if _, _, err = codex780Route(cookies, "any", now); err != nil {
			return false
		}
	}
	return t.ExpiresAt.IsZero() || now.Before(t.ExpiresAt)
}

func (s *OpenAIGatewayService) lookupOpenAICodex780Ticket(account *Account, model string) *openAICodexTicket {
	if s == nil || account == nil {
		return nil
	}
	raw, ok := s.openaiCodexTickets.Load(openAICodex780Key(account.ID, normalizeOpenAICodexTicketModel(model)))
	if !ok {
		return nil
	}
	ticket, _ := raw.(*openAICodexTicket)
	return ticket
}

func (s *OpenAIGatewayService) storeOpenAICodex780Ticket(ticket *openAICodexTicket) {
	if s == nil || ticket == nil || ticket.AccountID <= 0 {
		return
	}
	s.openaiCodexTickets.Store(openAICodex780Key(ticket.AccountID, normalizeOpenAICodexTicketModel(ticket.Model)), ticket)
}

type codex780MintResult struct {
	State    string
	Cookies  []string
	Gateway  string
	IssuedAt time.Time
	Status   int
	Err      error
}

func (s *OpenAIGatewayService) mintCodex780(ctx context.Context, account *Account, token, model, proxy string) codex780MintResult {
	var out codex780MintResult
	if s == nil || account == nil || s.httpUpstream == nil {
		out.Err = errors.New("mint unavailable")
		return out
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" {
		model = openAICodexTicketDefaultModel
	}
	payload := []byte(`{"model":` + jsonString(model) + `,"instructions":"","stream":true,"store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}],"reasoning":{"effort":"low"},"tool_choice":"auto","parallel_tool_calls":false}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, bytes.NewReader(payload))
	if err != nil {
		out.Err = err
		return out
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAIHarvest))
	req.Close = true
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("session-id", uuid.NewString())
	req.Header.Set("User-Agent", openAICodex780UserAgent)
	req.Header.Set("originator", openAICodex780Originator)
	if err = resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, req.Header, account); err != nil {
		out.Err = err
		return out
	}
	var seed []string
	if cached := s.lookupOpenAICodex780Ticket(account, model); cached.usable780(time.Now()) {
		seed = cached.HarvestCookies
	}
	if len(seed) > 0 {
		req.Header.Set("Cookie", strings.Join(seed, "; "))
	}
	proxy, err = resolveOpenAICodexTicketHarvestProxyURL(proxy)
	if err != nil {
		out.Err = err
		return out
	}
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		out.Err = err
		return out
	}
	if resp == nil {
		out.Err = errors.New("mint response missing")
		return out
	}
	out.Status = resp.StatusCode
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if resp.StatusCode != http.StatusOK {
		out.Err = &codex780MintError{kind: "http_status", detail: "mint http status"}
		return out
	}
	out.State = extractOpenAICodexTurnState(resp.Header)
	modelErr := readCodex780Created(resp.Body, model)
	out.Cookies, err = codex780ResponseRoute(resp, seed, "any", time.Now())
	if modelErr != nil {
		out.Err = modelErr
		return out
	}
	if err != nil {
		out.Err = err
		return out
	}
	if len(out.State) != openAICodex780Length || !strings.HasPrefix(out.State, openAICodexTicketStatePrefix) {
		out.Err = &codex780MintError{kind: "state_length", detail: "mint turn-state is not 780"}
		return out
	}
	shape, err := parseOpenAICodex780Shape(out.State)
	if err != nil {
		out.Err = err
		return out
	}
	out.IssuedAt = shape.IssuedAt
	out.Gateway = codex780CookieGateway(out.Cookies)
	return out
}

func (s *OpenAIGatewayService) ensureOpenAICodex780Ticket(ctx context.Context, account *Account, model string) *openAICodexTicket {
	if s == nil || account == nil {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" {
		model = openAICodexTicketDefaultModel
	}
	now := time.Now()
	if cached := s.lookupOpenAICodex780Ticket(account, model); cached.usable780(now) {
		return cached
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !s.openAICodexTicketEnabledContext(ctx) {
		return nil
	}
	proxy := s.openAICodexTicketHarvestProxyURLContext(ctx)
	if strings.TrimSpace(proxy) == "" || s.httpUpstream == nil {
		return nil
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil
	}
	timeout := 25 * time.Second
	if cfg := s.openAICodexTicketConfig(); cfg.HarvestAttemptTimeoutSeconds > 0 {
		timeout = time.Duration(cfg.HarvestAttemptTimeoutSeconds) * time.Second
	}
	mintCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := s.mintCodex780(mintCtx, account, token, model, proxy)
	if out.Err != nil || out.State == "" {
		return nil
	}
	issued := out.IssuedAt
	if issued.IsZero() {
		issued = now
	}
	ticket := &openAICodexTicket{
		AccountID:      account.ID,
		Model:          model,
		State:          out.State,
		Length:         openAICodex780Length,
		Cookies:        strings.Join(out.Cookies, "; "),
		HarvestCookies: out.Cookies,
		Gateway:        out.Gateway,
		Transport:      "sse",
		IssuedAt:       issued,
		CapturedAt:     now,
		ExpiresAt:      issued.Add(openAICodex780TTL),
		UserAgent:      openAICodex780UserAgent,
		Originator:     openAICodex780Originator,
	}
	s.storeOpenAICodex780Ticket(ticket)
	return ticket
}

func injectOpenAICodex780(h http.Header, ticket *openAICodexTicket) {
	if h == nil || !ticket.usable780(time.Now()) {
		return
	}
	h.Set(openAICodexTurnStateHeader, ticket.State)
	h.Set("Cookie", strings.Join(ticket.HarvestCookies, "; "))
	if strings.TrimSpace(h.Get("Cookie")) == "" && strings.TrimSpace(ticket.Cookies) != "" {
		h.Set("Cookie", ticket.Cookies)
	}
}
