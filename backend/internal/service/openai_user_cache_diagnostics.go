package service

// Opt-in, bounded diagnostics for one user's Chat Completions -> Responses
// requests. Only hashes, lengths, protocol enums and numeric usage are written.
// The observer never retries requests, changes billing, or persists payloads.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	userCacheDiagnosticContextKey  = "sub2api_user_cache_diagnostic"
	userCacheDiagnosticMaxItems    = 512
	userCacheDiagnosticMaxLine     = 4 << 20
	userCacheDiagnosticMaxFile     = 16 << 20
	userCacheDiagnosticMaxBytes    = 32 << 20
	userCacheDiagnosticMaxEvents   = 8192
	userCacheDiagnosticMaxAttempts = 4
	userCacheDiagnosticMaxRecords  = 1 + 2*userCacheDiagnosticMaxAttempts
)

type userCacheDiagnosticManager struct {
	mu        sync.Mutex
	signature string
	config    *userCacheDiagnosticConfig
}

type userCacheDiagnosticConfig struct {
	userID int64
	path   string
	until  time.Time
	limit  int
	used   int
}

var userCacheDiagnostics userCacheDiagnosticManager
var userCacheDiagnosticFileMu sync.Mutex

// A mandatory absolute expiry and a maximum of 100 requests prevent accidental
// long-running capture. Missing, invalid, or expired configuration fails closed.
func (m *userCacheDiagnosticManager) admit(userID int64, now time.Time) *userCacheDiagnosticConfig {
	rawUser := strings.TrimSpace(os.Getenv("SUB2_USER_CACHE_DIAGNOSTIC_USER_ID"))
	if rawUser == "" || rawUser != strconv.FormatInt(userID, 10) || userID <= 0 {
		return nil
	}
	path := strings.TrimSpace(os.Getenv("SUB2_USER_CACHE_DIAGNOSTIC_PATH"))
	rawUntil := strings.TrimSpace(os.Getenv("SUB2_USER_CACHE_DIAGNOSTIC_UNTIL"))
	rawLimit := strings.TrimSpace(os.Getenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS"))
	signature := strings.Join([]string{rawUser, path, rawUntil, rawLimit}, "\x00")
	m.mu.Lock()
	defer m.mu.Unlock()
	if signature != m.signature {
		m.signature, m.config = signature, nil
		until, err := time.Parse(time.RFC3339, rawUntil)
		if err != nil || !filepath.IsAbs(path) || !now.Before(until) || until.Sub(now) > time.Hour {
			return nil
		}
		limit := 30
		if rawLimit != "" {
			limit, err = strconv.Atoi(rawLimit)
			if err != nil || limit < 1 || limit > 100 {
				return nil
			}
		}
		m.config = &userCacheDiagnosticConfig{userID: userID, path: path, until: until, limit: limit}
	}
	if m.config == nil || !now.Before(m.config.until) || m.config.used >= m.config.limit {
		return nil
	}
	m.config.used++
	return m.config
}

type userCacheDiagnostic struct {
	config             *userCacheDiagnosticConfig
	traceID            string
	apiKeyID           int64
	sessionHash        string
	usageRequestIDHash string
	mu                 sync.Mutex
	attempts           int
	records            int
}

// Each attempt owns an immutable identity. A previous response may finish after
// the next retry starts; its evidence must not acquire that later retry's ID.
type userCacheDiagnosticAttempt struct {
	diagnostic         *userCacheDiagnostic
	accountID          int64
	attempt            int
	usageRequestIDHash string
	expectsSSE         bool
}

func beginUserCacheDiagnostic(c *gin.Context, body []byte) *userCacheDiagnostic {
	if c == nil || c.Request == nil {
		return nil
	}
	if previous, exists := c.Get(userCacheDiagnosticContextKey); exists {
		diagnostic, _ := previous.(*userCacheDiagnostic)
		return diagnostic
	}
	// Also memoize non-admission so retries cannot consume another sample slot.
	c.Set(userCacheDiagnosticContextKey, (*userCacheDiagnostic)(nil))
	v, exists := c.Get("api_key")
	if !exists {
		return nil
	}
	key, ok := v.(*APIKey)
	if !ok || key == nil {
		return nil
	}
	config := userCacheDiagnostics.admit(key.UserID, time.Now())
	if config == nil {
		return nil
	}
	d := &userCacheDiagnostic{config: config, traceID: generateRequestID(), apiKeyID: key.ID}
	for _, name := range []string{"session-id", "session_id", "x-session-id"} {
		if value := strings.TrimSpace(c.GetHeader(name)); value != "" {
			d.sessionHash = diagnosticSHA256([]byte(value))
			break
		}
	}
	ctx := c.Request.Context()
	if id, _ := ctx.Value(ctxkey.ClientRequestID).(string); strings.TrimSpace(id) != "" {
		d.usageRequestIDHash = diagnosticSHA256([]byte("client:" + strings.TrimSpace(id)))
	} else if id, _ := ctx.Value(ctxkey.RequestID).(string); strings.TrimSpace(id) != "" {
		d.usageRequestIDHash = diagnosticSHA256([]byte("local:" + strings.TrimSpace(id)))
	}
	c.Set(userCacheDiagnosticContextKey, d)
	d.write("ingress", 0, 0, d.usageRequestIDHash, map[string]any{"body": summarizeUserCacheBody(body)})
	return d
}

func (d *userCacheDiagnostic) write(stage string, accountID int64, attempt int, usageIDHash string, fields map[string]any) {
	if d == nil || d.config == nil || !time.Now().Before(d.config.until) {
		return
	}
	d.mu.Lock()
	if d.records >= userCacheDiagnosticMaxRecords {
		d.mu.Unlock()
		return
	}
	d.records++
	d.mu.Unlock()
	event := map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano), "stage": stage,
		"trace_id": d.traceID, "user_id": d.config.userID, "api_key_id": d.apiKeyID,
		"account_id": accountID, "attempt": attempt,
		"session_sha256": d.sessionHash, "usage_request_id_sha256": usageIDHash,
	}
	for k, v := range fields {
		event[k] = v
	}
	raw, err := json.Marshal(event)
	if err != nil || len(raw) > 512<<10 {
		return
	}
	userCacheDiagnosticFileMu.Lock()
	defer userCacheDiagnosticFileMu.Unlock()
	// The operator selects a private directory. Refuse existing symlinks,
	// devices and broadly-readable files; do not overwrite or chmod them.
	if !time.Now().Before(d.config.until) {
		return
	}
	before, err := os.Lstat(d.config.path)
	if info := before; err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size()+int64(len(raw)+1) > userCacheDiagnosticMaxFile {
			return
		}
	} else if !os.IsNotExist(err) {
		return
	}
	flags := os.O_APPEND | os.O_WRONLY
	if os.IsNotExist(err) {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(d.config.path, flags, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	// Recheck the opened inode so replacement between Lstat and Open cannot
	// redirect the write. The configured parent directory must remain private.
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 ||
		opened.Size()+int64(len(raw)+1) > userCacheDiagnosticMaxFile ||
		(before != nil && !os.SameFile(before, opened)) || !time.Now().Before(d.config.until) {
		return
	}
	_, _ = file.Write(append(raw, '\n'))
}

type userCacheValueSummary struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func summarizeUserCacheValue(raw []byte) userCacheValueSummary {
	return userCacheValueSummary{Bytes: len(raw), SHA256: diagnosticCanonicalSHA256(raw)}
}

type userCacheItemSummary struct {
	Index     int                    `json:"index"`
	Value     userCacheValueSummary  `json:"value"`
	Type      string                 `json:"type,omitempty"`
	Role      string                 `json:"role,omitempty"`
	IDHash    string                 `json:"id_sha256,omitempty"`
	Encrypted *userCacheValueSummary `json:"encrypted_content,omitempty"`
}

func summarizeUserCacheItem(item gjson.Result, index int) userCacheItemSummary {
	out := userCacheItemSummary{Index: index, Value: summarizeUserCacheValue([]byte(item.Raw))}
	switch value := item.Get("type").String(); value {
	case "message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "web_search_call", "image_generation_call", "compaction", "configuration_update", "additional_tools":
		out.Type = value
	default:
		if value != "" {
			out.Type = "other"
		}
	}
	switch value := item.Get("role").String(); value {
	case "system", "developer", "user", "assistant", "tool", "function":
		out.Role = value
	}
	if value := item.Get("id").String(); value != "" {
		out.IDHash = diagnosticSHA256([]byte(value))
	}
	if value := item.Get("encrypted_content"); value.Exists() {
		summary := summarizeUserCacheValue([]byte(value.Raw))
		out.Encrypted = &summary
	}
	return out
}

func summarizeUserCacheBody(raw []byte) map[string]any {
	out := map[string]any{"bytes": len(raw), "sha256": diagnosticSHA256(raw)}
	if len(raw) > userCacheDiagnosticMaxBytes || !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
		out["summary_unavailable"] = true
		return out
	}
	parsed := gjson.ParseBytes(raw)
	fields := map[string]userCacheValueSummary{}
	for _, name := range []string{"model", "instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "reasoning_effort", "text", "response_format", "service_tier", "prompt_cache_options", "prompt_cache_key", "context_management", "previous_response_id", "include", "store", "stream"} {
		if value := parsed.Get(name); value.Exists() {
			fields[name] = summarizeUserCacheValue([]byte(value.Raw))
		}
	}
	out["fields"] = fields
	shape := "input"
	if parsed.Get("messages").Exists() {
		shape = "messages"
	}
	value := parsed.Get(shape)
	out["shape"], out["input"] = shape, summarizeUserCacheValue([]byte(value.Raw))
	items := value.Array()
	out["item_count"], out["items_truncated"] = len(items), len(items) > userCacheDiagnosticMaxItems
	summaries := make([]userCacheItemSummary, 0, min(len(items), userCacheDiagnosticMaxItems))
	for i, item := range items {
		if i >= userCacheDiagnosticMaxItems {
			break
		}
		summaries = append(summaries, summarizeUserCacheItem(item, i))
	}
	out["items"] = summaries
	return out
}

// upstreamRequest must run immediately before each real HTTP attempt. It reads
// only GetBody, never the live request body, and returns a retry-specific token.
func (d *userCacheDiagnostic) upstreamRequest(account *Account, request *http.Request) *userCacheDiagnosticAttempt {
	if d == nil || d.config == nil || !time.Now().Before(d.config.until) || account == nil || request == nil {
		return nil
	}
	d.mu.Lock()
	d.attempts++
	attempt := d.attempts
	d.mu.Unlock()
	if attempt > userCacheDiagnosticMaxAttempts {
		return nil
	}
	a := &userCacheDiagnosticAttempt{diagnostic: d, accountID: account.ID, attempt: attempt, usageRequestIDHash: d.usageRequestIDHash}
	fields := map[string]any{"body_unavailable": true}
	if request.GetBody != nil {
		if reader, err := request.GetBody(); err == nil && reader != nil {
			raw, err := io.ReadAll(io.LimitReader(reader, userCacheDiagnosticMaxBytes+1))
			_ = reader.Close()
			if err == nil {
				fields = map[string]any{"body": summarizeUserCacheBody(raw)}
				// The normalized outgoing request is authoritative for streaming.
				// Providers can omit or mislabel the response Content-Type.
				a.expectsSSE = gjson.GetBytes(raw, "stream").Type == gjson.True
			}
		}
	}
	headers := map[string]headerDigest{}
	for _, key := range []string{"session-id", "session_id", "thread-id", "conversation_id", "x-codex-turn-state", "x-codex-turn-metadata"} {
		if value := request.Header.Get(key); value != "" {
			headers[key] = headerDigest{Present: true, Length: len(value), Digest: diagnosticHeaderValueDigest(value)}
		}
	}
	fields["headers"] = headers
	d.write("upstream_request", a.accountID, a.attempt, a.usageRequestIDHash, fields)
	return a
}

// upstreamResponse wraps only the consumer's normal reads. Diagnostic failures
// are not returned to the forwarding path; original bytes and errors survive.
func (a *userCacheDiagnosticAttempt) upstreamResponse(response *http.Response, err error) {
	if a == nil || !time.Now().Before(a.diagnostic.config.until) {
		return
	}
	if err != nil || response == nil {
		a.diagnostic.write("upstream_response", a.accountID, a.attempt, a.usageRequestIDHash, map[string]any{"transport_error": true})
		return
	}
	usageID := a.usageRequestIDHash
	if usageID == "" && response.Header.Get("x-request-id") != "" {
		usageID = diagnosticSHA256([]byte(response.Header.Get("x-request-id")))
	}
	if response.Body == nil {
		a.diagnostic.write("upstream_response", a.accountID, a.attempt, usageID, map[string]any{"status": response.StatusCode, "body_unavailable": true})
		return
	}
	streaming := strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") ||
		(a.expectsSSE && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices)
	response.Body = &userCacheResponseObserver{ReadCloser: response.Body, diagnostic: a.diagnostic, accountID: a.accountID, status: response.StatusCode, attempt: a.attempt,
		usageRequestIDHash: usageID, streaming: streaming, items: map[string]userCacheItemSummary{}, itemRanks: map[string]int{}}
}

// A candidate is a fully framed, strictly parsed observation, not a claim
// that the consumer accepted the protocol or that native replay was eligible.
type userCacheTerminalCandidate struct {
	Schema           int                               `json:"schema"`
	Event            int                               `json:"event"`
	Type             string                            `json:"type"`
	StatusClass      string                            `json:"status_class"`
	ResponseIDHash   string                            `json:"response_id_sha256"`
	Usage            map[string]map[string]json.Number `json:"usage"`
	Items            map[string]userCacheItemSummary   `json:"output_items"`
	PrefixError      bool                              `json:"prefix_error_event"`
	PrefixIDConflict bool                              `json:"prefix_response_id_conflict"`
	Invalid          int                               `json:"prefix_invalid_json_events"`
	Truncated        int                               `json:"prefix_truncated_events"`
	Limited          bool                              `json:"prefix_observation_limited"`
}

type userCacheResponseObserver struct {
	terminalCandidate   *userCacheTerminalCandidate
	eventType           string
	linesSeen           int
	prefixError         bool
	responseIDConflict  bool
	activeReads         int
	closeStarted        bool
	readError           bool
	readErrorPhase      string
	readErrorClass      string
	readErrorAfterClose bool
	sawEOF              bool
	postTerminalBytes   int
	postTerminalLimited bool
	io.ReadCloser
	diagnostic         *userCacheDiagnostic
	accountID          int64
	status             int
	attempt            int
	usageRequestIDHash string
	streaming          bool
	line               []byte
	data               []byte
	skipping           bool
	lineOverflow       bool
	truncated          int
	invalid            int
	events             int
	observedBytes      int
	limited            bool
	terminal           string
	usage              map[string]map[string]json.Number
	items              map[string]userCacheItemSummary
	itemRanks          map[string]int
	responseIDHash     string
	finished           bool
	readMu             sync.Mutex
	mu                 sync.Mutex
}

func (o *userCacheResponseObserver) Read(p []byte) (int, error) {
	o.readMu.Lock()
	defer o.readMu.Unlock()
	o.mu.Lock()
	o.activeReads++
	o.mu.Unlock()
	n, err := o.ReadCloser.Read(p)
	o.mu.Lock()
	defer o.mu.Unlock()
	hadCandidate := o.terminalCandidate != nil
	if !o.finished && time.Now().Before(o.diagnostic.config.until) {
		if n >= 0 && n <= len(p) {
			o.consume(p[:n])
		} else {
			o.invalid++
			o.limited = true
		}
	}
	if !o.finished && err != nil {
		if err == io.EOF {
			o.sawEOF = true
		} else if !o.readError {
			o.readError = true
			o.readErrorPhase = "before_terminal_candidate"
			if hadCandidate {
				o.readErrorPhase = "after_terminal_candidate"
			} else if o.terminalCandidate != nil {
				o.readErrorPhase = "same_read_after_terminal_candidate"
			}
			o.readErrorClass = userCacheReadErrorClass(err)
			o.readErrorAfterClose = o.closeStarted
		}
	}
	o.activeReads--
	if err != nil || o.closeStarted && o.activeReads == 0 {
		o.finishLocked(o.sawEOF, o.readError)
	}
	return n, err
}

func (o *userCacheResponseObserver) Close() error {
	o.mu.Lock()
	o.closeStarted = true
	o.mu.Unlock()
	// Preserve the underlying Close order and return. Do not wait for readMu:
	// an arbitrary Reader need not unblock on Close. A still-active normal Read
	// will publish its own result before the single diagnostic record is emitted.
	err := o.ReadCloser.Close()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.activeReads == 0 {
		o.finishLocked(o.sawEOF, o.readError)
	}
	return err
}

func userCacheReadErrorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, net.ErrClosed), errors.Is(err, io.ErrClosedPipe):
		return "closed"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "eof"
	default:
		return "other"
	}
}

func (o *userCacheResponseObserver) observeSuffix(n int) {
	if n > userCacheDiagnosticMaxBytes-o.postTerminalBytes {
		o.postTerminalBytes = userCacheDiagnosticMaxBytes
		o.postTerminalLimited = true
	} else {
		o.postTerminalBytes += n
	}
}

func (o *userCacheResponseObserver) consume(raw []byte) {
	if o.terminalCandidate != nil {
		o.observeSuffix(len(raw))
		return
	}
	if o.limited || len(raw) == 0 {
		return
	}
	if o.observedBytes+len(raw) > userCacheDiagnosticMaxBytes {
		o.limited = true
		o.line, o.data = nil, nil
		return
	}
	o.observedBytes += len(raw)
	if !o.streaming {
		if len(o.data)+len(raw) > userCacheDiagnosticMaxLine {
			o.truncated++
			o.limited = true
			o.data = nil
			return
		}
		o.data = append(o.data, raw...)
		return
	}
	for len(raw) > 0 && !o.limited {
		end := bytes.IndexByte(raw, '\n')
		n := len(raw)
		if end >= 0 {
			n = end + 1
		}
		if o.lineOverflow || len(o.line)+n > userCacheDiagnosticMaxLine {
			if !o.skipping {
				o.truncated++
			}
			o.line, o.data, o.skipping, o.lineOverflow = nil, nil, true, true
		} else {
			o.line = append(o.line, raw[:n]...)
		}
		raw = raw[n:]
		if end >= 0 {
			if !o.lineOverflow {
				o.consumeLine(bytes.TrimSuffix(bytes.TrimSuffix(o.line, []byte{'\n'}), []byte{'\r'}))
			}
			o.line = o.line[:0]
			o.lineOverflow = false
			if o.terminalCandidate != nil {
				o.observeSuffix(len(raw))
				o.line = nil
				o.data = nil
				o.eventType = ""
				return
			}
		}
	}
}

func (o *userCacheResponseObserver) consumeLine(line []byte) {
	if o.linesSeen == 0 {
		line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
	}
	o.linesSeen++
	if len(line) == 0 {
		if !o.skipping && len(o.data) > 0 {
			o.observe(bytes.TrimSuffix(o.data, []byte{'\n'}))
		}
		o.data = o.data[:0]
		o.skipping = false
		o.eventType = ""
		return
	}
	if o.skipping {
		return
	}
	field, value, _ := bytes.Cut(line, []byte{':'})
	if bytes.Equal(field, []byte("event")) {
		o.eventType = string(bytes.TrimPrefix(value, []byte{' '}))
		return
	}
	if !bytes.Equal(field, []byte("data")) {
		return
	}
	value = bytes.TrimPrefix(value, []byte{' '})
	if len(o.data)+len(value)+1 > userCacheDiagnosticMaxLine {
		o.truncated++
		o.skipping, o.data = true, nil
		return
	}
	o.data = append(o.data, value...)
	o.data = append(o.data, '\n')
}

func (o *userCacheResponseObserver) observe(raw []byte) {
	o.events++
	if o.events > userCacheDiagnosticMaxEvents {
		o.limited = true
		return
	}
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("[DONE]")) {
		return
	}
	if !utf8.Valid(raw) {
		o.invalid++
		return
	}
	if _, err := nativeReplayCanonicalObject(raw); err != nil {
		o.invalid++
		return
	}
	parsed := gjson.ParseBytes(raw)
	kind := ""
	if declared := parsed.Get("type"); declared.Exists() {
		if declared.Type != gjson.String || declared.String() == "" {
			o.invalid++
			return
		}
		kind = declared.String()
	}
	if o.streaming {
		if kind == "" {
			kind = o.eventType
		} else if o.eventType != "" && o.eventType != kind {
			o.invalid++
			return
		}
		if kind == "" {
			o.invalid++
			return
		}
	}
	for _, path := range []string{"error", "incomplete_details", "response.error", "response.incomplete_details"} {
		if value := parsed.Get(path); value.Exists() && value.Type != gjson.Null {
			o.prefixError = true
		}
	}
	switch kind {
	case "error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "response.error":
		o.prefixError = true
	}
	rank := 0
	switch kind {
	case "response.completed", "response.done", "response.failed", "response.incomplete":
		o.terminal, rank = kind, 2
	case "response.output_item.done":
		rank = 1
	}
	identityPaths := []string{"response.id", "response_id"}
	if !o.streaming {
		identityPaths = append(identityPaths, "id")
	}
	for _, path := range identityPaths {
		value := parsed.Get(path)
		if !value.Exists() {
			continue
		}
		if value.Type != gjson.String || value.String() == "" {
			o.responseIDConflict = true
			continue
		}
		hash := diagnosticSHA256([]byte(value.String()))
		if o.responseIDHash != "" && o.responseIDHash != hash {
			o.responseIDConflict = true
		}
		o.responseIDHash = hash
	}
	// Standalone JSON Responses bodies have no event type.
	if !o.streaming {
		switch status := parsed.Get("status").String(); status {
		case "completed", "failed", "incomplete":
			o.terminal, rank = "response."+status, 2
		}
	}
	var eventUsage map[string]map[string]json.Number
	for _, path := range []string{"usage", "response.usage"} {
		usage := parsed.Get(path)
		if !usage.IsObject() {
			continue
		}
		if o.usage == nil {
			o.usage = map[string]map[string]json.Number{}
		}
		values := map[string]json.Number{}
		for _, key := range []string{"input_tokens", "prompt_tokens", "output_tokens", "completion_tokens", "total_tokens", "input_tokens_details.cached_tokens", "input_tokens_details.cache_write_tokens", "input_tokens_details.cache_creation_tokens", "input_tokens_details.image_tokens", "output_tokens_details.reasoning_tokens", "output_tokens_details.image_tokens", "prompt_tokens_details.cached_tokens", "completion_tokens_details.reasoning_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "cache_read_tokens", "cache_write_tokens"} {
			if number := usage.Get(key); number.Type == gjson.Number && len(number.Raw) <= 128 {
				values[key] = json.Number(number.Raw)
			}
		}
		o.usage[path] = values
		if eventUsage == nil {
			eventUsage = map[string]map[string]json.Number{}
		}
		eventUsage[path] = values
	}
	if item := parsed.Get("item"); item.IsObject() {
		index := parsed.Get("output_index")
		if index.Type == gjson.Number {
			if i, err := strconv.Atoi(index.Raw); err == nil {
				o.observeItem(item, i, rank)
			}
		}
	}
	for _, path := range []string{"output", "response.output"} {
		parsed.Get(path).ForEach(func(index, item gjson.Result) bool {
			if index.Type != gjson.Number || index.Int() >= 128 {
				return false
			}
			o.observeItem(item, int(index.Int()), rank)
			return true
		})
	}
	if o.streaming && (kind == "response.completed" || kind == "response.done") && parsed.Get("response").IsObject() {
		id := parsed.Get("response.id")
		if id.Type == gjson.String && id.String() != "" {
			status := parsed.Get("response.status")
			statusClass := "other"
			if !status.Exists() || status.Type == gjson.Null {
				statusClass = "missing"
			} else if status.Type == gjson.String && status.String() == "completed" {
				statusClass = "completed"
			}
			o.terminalCandidate = &userCacheTerminalCandidate{Schema: 1, Event: o.events, Type: kind, StatusClass: statusClass, ResponseIDHash: diagnosticSHA256([]byte(id.String())), Usage: eventUsage, Items: o.items, PrefixError: o.prefixError, PrefixIDConflict: o.responseIDConflict, Invalid: o.invalid, Truncated: o.truncated, Limited: o.limited}
		}
	}
}

func (o *userCacheResponseObserver) observeItem(item gjson.Result, index, rank int) {
	if index < 0 || index >= 128 || !item.IsObject() {
		return
	}
	key := strconv.Itoa(index)
	if previous, exists := o.itemRanks[key]; exists && rank < previous {
		return
	}
	o.itemRanks[key] = rank
	o.items[key] = summarizeUserCacheItem(item, index)
}

func (o *userCacheResponseObserver) finishLocked(eof, readError bool) {
	if o.finished {
		return
	}
	o.finished = true
	if !o.limited && time.Now().Before(o.diagnostic.config.until) {
		// A partial SSE event is not an event. EOF may complete a JSON document,
		// but Close alone does not establish a complete upstream body.
		if !o.streaming && eof && len(o.data) > 0 {
			o.observe(o.data)
		}
	}
	partial := len(o.line) > 0 || o.skipping || (o.streaming && len(o.data) > 0) || (!o.streaming && !eof && len(o.data) > 0)
	o.line, o.data = nil, nil
	o.diagnostic.write("upstream_response", o.accountID, o.attempt, o.usageRequestIDHash, map[string]any{
		"status": o.status, "terminal": o.terminal,
		"usage": o.usage, "output_items": o.items, "response_id_sha256": o.responseIDHash,
		"truncated_events": o.truncated, "invalid_json_events": o.invalid, "observed_events": o.events,
		"observation_limited": o.limited, "partial_event": partial, "read_error": readError, "eof": eof,
		"read_error_phase": userCacheDiagnosticDefault(o.readErrorPhase, "none"), "read_error_class": userCacheDiagnosticDefault(o.readErrorClass, "none"),
		"local_close_started": o.closeStarted, "read_error_observed_after_close_started": o.readErrorAfterClose,
		"terminal_candidate": o.terminalCandidate, "post_terminal_bytes": o.postTerminalBytes, "post_terminal_observation_limited": o.postTerminalLimited,
	})
}

func userCacheDiagnosticDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
