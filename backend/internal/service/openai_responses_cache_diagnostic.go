package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const responsesCacheDiagnosticEnv = "SUB2_RESPONSES_CACHE_DIAGNOSTIC_"

type responsesCacheDiagnosticSettings struct {
	user, key, models, path, until, limit string
}

func responsesCacheDiagnosticSettingsFromEnv() responsesCacheDiagnosticSettings {
	read := func(name string) string { return strings.TrimSpace(os.Getenv(responsesCacheDiagnosticEnv + name)) }
	return responsesCacheDiagnosticSettings{
		user: read("USER_ID"), key: read("API_KEY_ID"), models: read("MODELS"),
		path: read("PATH"), until: read("UNTIL"), limit: read("MAX_REQUESTS"),
	}
}

type responsesCacheDiagnosticWindow struct {
	*userCacheDiagnosticConfig
	settings responsesCacheDiagnosticSettings
	apiKeyID int64
	models   map[string]string // values are the operator-configured names safe to write
	secret   [32]byte
	id       string
}

var responsesCacheDiagnostics struct {
	sync.Mutex
	settings responsesCacheDiagnosticSettings
	window   *responsesCacheDiagnosticWindow
}

func privateResponsesCacheDiagnosticDirectory(path string) bool {
	info, err := os.Lstat(filepath.Dir(path))
	return err == nil && info.IsDir() && info.Mode().Perm()&0077 == 0
}

func newResponsesCacheDiagnosticWindow(settings responsesCacheDiagnosticSettings, key *APIKey, now time.Time) *responsesCacheDiagnosticWindow {
	limits := newUserCacheDiagnosticConfig(key.UserID, settings.path, settings.until, settings.limit, now)
	if limits == nil || len(settings.models) == 0 || len(settings.models) > 4096 || !privateResponsesCacheDiagnosticDirectory(settings.path) {
		return nil
	}
	models := make(map[string]string)
	for raw := range strings.SplitSeq(settings.models, ",") {
		model := strings.TrimSpace(raw)
		if len(models) >= 16 || models[model] != "" || !isCanonicalGPTChatModel(model) {
			return nil
		}
		models[model] = model
	}
	w := &responsesCacheDiagnosticWindow{userCacheDiagnosticConfig: limits, apiKeyID: key.ID, settings: settings, models: models}
	if _, err := io.ReadFull(rand.Reader, w.secret[:]); err != nil {
		return nil
	}
	w.id = w.mac("window", nil)[:32]
	return w
}

func (w *responsesCacheDiagnosticWindow) mac(purpose string, value []byte) string {
	mac := hmac.New(sha256.New, w.secret[:])
	_, _ = io.WriteString(mac, purpose)
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(value)
	return hex.EncodeToString(mac.Sum(nil))
}

// The same quota and secret apply to HTTP requests and WebSocket turns. An old
// observer never follows a changed configuration or refreshes an expired window.
func (w *responsesCacheDiagnosticWindow) admit(consume bool) (int, bool) {
	responsesCacheDiagnostics.Lock()
	defer responsesCacheDiagnostics.Unlock()
	if !w.activeLocked() || w.used >= w.limit {
		return 0, false
	}
	if consume {
		w.used++
	}
	return w.used, true
}

func (w *responsesCacheDiagnosticWindow) activeLocked() bool {
	return responsesCacheDiagnostics.window == w && time.Now().Before(w.until) && w.settings == responsesCacheDiagnosticSettingsFromEnv()
}

// ResponsesCacheDiagnosticObserver belongs to one authenticated HTTP request
// or WebSocket connection. Its header snapshot contains only window-keyed
// fingerprints of recognized fields from the original client handshake.
type ResponsesCacheDiagnosticObserver struct {
	window       *responsesCacheDiagnosticWindow
	headers      map[string]responsesCacheDiagnosticValue
	wsConnection atomic.Int64
}

func NewResponsesCacheDiagnosticObserver(c *gin.Context, key *APIKey) *ResponsesCacheDiagnosticObserver {
	rawUser := strings.TrimSpace(os.Getenv(responsesCacheDiagnosticEnv + "USER_ID"))
	if rawUser == "" || c == nil || c.Request == nil || key == nil || key.ID <= 0 || key.UserID <= 0 ||
		rawUser != strconv.FormatInt(key.UserID, 10) || strings.TrimSpace(os.Getenv(responsesCacheDiagnosticEnv+"API_KEY_ID")) != strconv.FormatInt(key.ID, 10) {
		return nil
	}
	authenticated := getAPIKeyFromContext(c)
	if authenticated == nil || authenticated.UserID != key.UserID || authenticated.ID != key.ID {
		return nil
	}
	settings := responsesCacheDiagnosticSettingsFromEnv()
	if settings.user != strconv.FormatInt(key.UserID, 10) || settings.key != strconv.FormatInt(key.ID, 10) || isOpenAIResponsesCompactPath(c) {
		return nil
	}
	responsesCacheDiagnostics.Lock()
	if responsesCacheDiagnostics.settings != settings {
		responsesCacheDiagnostics.settings = settings
		responsesCacheDiagnostics.window = newResponsesCacheDiagnosticWindow(settings, key, time.Now())
	}
	w := responsesCacheDiagnostics.window
	active := w != nil && time.Now().Before(w.until) && w.used < w.limit
	responsesCacheDiagnostics.Unlock()
	if !active {
		return nil
	}
	return &ResponsesCacheDiagnosticObserver{window: w, headers: w.headerIdentity(c.Request.Header)}
}

// ResponsesCacheDiagnostic describes a single logical turn, including its
// handler-observed attempts. It retains neither client bytes nor a Gin context.
type ResponsesCacheDiagnostic struct {
	window *responsesCacheDiagnosticWindow
	event  map[string]any

	mu            sync.Mutex
	finished      bool
	attempts      int
	switches      int
	lastAccount   int64
	lastModel     string // configured route name only
	firstAccounts []int64
}

func (o *ResponsesCacheDiagnosticObserver) Begin(body []byte, model, transport string) *ResponsesCacheDiagnostic {
	if o == nil || o.window == nil || (transport != "http" && transport != "ws") {
		return nil
	}
	w := o.window
	selected := w.models[model]
	if selected == "" {
		return nil
	}
	if _, active := w.admit(false); !active {
		return nil
	}
	payload, source, ok := parseResponsesCacheDiagnosticPayload(body, transport)
	if !ok {
		return nil
	}
	sequence, ok := w.admit(true)
	if !ok {
		return nil
	}
	event := map[string]any{
		"window_id": w.id, "sequence": sequence, "started_at": time.Now().UTC(),
		"user_id": w.userID, "api_key_id": w.apiKeyID, "transport": transport,
		"selected_model": selected, "request_model_source": source, "body_bytes": len(body),
	}
	if transport == "ws" {
		o.wsConnection.CompareAndSwap(0, int64(sequence))
		event["ws_connection_sample"] = o.wsConnection.Load()
	}
	if rawModel := payload.body["model"]; rawModel.Type == gjson.String {
		event["request_model_match"] = rawModel.Str == selected
		if rawModel.Str != selected {
			event["request_model_hmac"] = w.mac("requested_model", []byte(rawModel.Str))
		}
	}
	event["identity"] = w.inputIdentity(o.headers, payload)
	event["input"], event["fields"] = w.prefix(payload.body)
	return &ResponsesCacheDiagnostic{window: w, event: event}
}

// RecordAttempt records each attempt actually started by the handler. Internal
// retries below that boundary are not inferred. The retained list is bounded.
func (d *ResponsesCacheDiagnostic) RecordAttempt(accountID int64, actualModel string) {
	if d == nil || accountID <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished {
		return
	}
	if d.attempts > 0 && d.lastAccount != accountID {
		d.switches++
	}
	d.lastAccount = accountID
	d.lastModel = d.window.models[actualModel]
	d.attempts++
	if len(d.firstAccounts) < 8 {
		d.firstAccounts = append(d.firstAccounts, accountID)
	}
}

// Finish publishes one record for the terminal outcome, including metered
// partial failures. Late or duplicate calls cannot attach a different result.
func (d *ResponsesCacheDiagnostic) Finish(accountID int64, result *OpenAIForwardResult, turnErr error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.finished {
		d.mu.Unlock()
		return
	}
	d.finished = true
	event := d.event
	d.event = nil
	if d.attempts > 0 {
		if result == nil && (d.lastModel == "" || d.lastAccount != accountID) {
			// A final failure supplies no forward result to prove a different
			// model. Require the final account's configured route instead.
			d.mu.Unlock()
			return
		}
		if d.lastModel != "" {
			event["final_selected_model"] = d.lastModel
		}
		event["attempts"], event["account_switches"] = d.attempts, d.switches
		event["attempt_accounts"], event["attempt_accounts_truncated"] = d.firstAccounts, d.attempts > len(d.firstAccounts)
	}
	d.mu.Unlock()

	w := d.window
	responsesCacheDiagnostics.Lock()
	active := w.activeLocked()
	responsesCacheDiagnostics.Unlock()
	if !active || !privateResponsesCacheDiagnosticDirectory(w.path) {
		return
	}
	event["finished_at"], event["account_id"] = time.Now().UTC(), accountID
	event["model_evidence"], event["usage_state"] = "selected_only", "unavailable"
	switch {
	case errors.Is(turnErr, context.Canceled):
		event["status"] = "canceled"
	case errors.Is(turnErr, context.DeadlineExceeded):
		event["status"] = "deadline"
	case turnErr != nil:
		event["status"] = "error"
	case result == nil:
		event["status"] = "no_result"
	default:
		event["status"] = "forward_returned"
	}
	if result != nil {
		// ForwardResult leaves UpstreamModel empty for an unchanged request.
		// A non-empty value must match the operator's exact allowed name.
		sentModel := result.UpstreamModel
		if sentModel == "" {
			sentModel = result.Model
		}
		sent := w.models[sentModel]
		if sent == "" || result.ImageEndpoint || result.ImageCount > 0 || result.Usage.ImageOutputTokens > 0 || result.VideoCount > 0 || result.AudioUsage != nil {
			return
		}
		event["model_evidence"], event["sent_model"] = "forward_result", sent
		switch {
		case result.UpstreamResponseModelConflict:
			event["reported_model_status"] = "conflicting"
		case result.UpstreamResponseModel == "":
			event["reported_model_status"] = "unreported"
		case result.UpstreamResponseModel == sent:
			event["reported_model_status"] = "same"
		default:
			event["reported_model_status"] = "different"
		}
		if reported := w.models[result.UpstreamResponseModel]; reported != "" {
			event["reported_model"] = reported
		}
		switch result.UpstreamEndpoint {
		case "/v1/responses", "/v1/chat/completions", "/v1/messages":
			event["upstream_endpoint"] = result.UpstreamEndpoint
		}
		responseID := result.ResponseID
		// Native WS relay results can carry the response.create result ID in
		// RequestID. HTTP request IDs have independent semantics.
		if responseID == "" && event["transport"] == "ws" && result.OpenAIWSMode && len(result.RequestID) <= 4*maxPersistedSessionIDLength &&
			ClassifyOpenAIPreviousResponseIDKind(result.RequestID) == OpenAIPreviousResponseIDKindResponseID {
			responseID = result.RequestID
		}
		if id := w.identityValue("response_id", responseID); id.HMAC != "" {
			event["response_id_hmac"] = id.HMAC
		}
		event["gateway_ws_mode"] = result.OpenAIWSMode
		switch result.UpstreamTerminalEvent {
		case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled",
			"response.canceled", "response.interrupted", "error":
			event["terminal_event"] = result.UpstreamTerminalEvent
		case "":
		default:
			event["terminal_event"] = "other"
		}
		usage := result.Usage
		event["usage_state"] = "forward_counts"
		if usage == (OpenAIUsage{}) {
			event["usage_state"] = "zero_or_unreported"
		}
		event["usage"] = map[string]int{"input_tokens": usage.InputTokens, "cached_tokens": usage.CacheReadInputTokens,
			"cache_creation_tokens": usage.CacheCreationInputTokens, "output_tokens": usage.OutputTokens}
	}
	appendUserCacheDiagnosticEvent(w.path, w.until, event)
}
