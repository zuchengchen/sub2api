//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func oauthReplayOpsEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	value, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := value.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	return events
}

func TestOAuthReliabilityReplayBoundaryOptionalJSONAndFixedVocabulary(t *testing.T) {
	for _, source := range []OpsReplayUsageSource{OpsReplayUsageAbsent, OpsReplayUsageUpstream, OpsReplayUsageEstimated, "", "unknown", "private-token=do-not-store"} {
		t.Run(string(source), func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			boundary := &OpsReplayBoundary{UsageSource: source}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{Message: "failure", ReplayBoundary: boundary})
			entry := &OpsInsertErrorLogInput{UpstreamErrors: oauthReplayOpsEvents(t, c)}
			require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
			require.NotNil(t, entry.UpstreamErrorsJSON)
			encoded := *entry.UpstreamErrorsJSON
			value := gjson.Get(encoded, "0.replay_boundary")
			valid := source == OpsReplayUsageAbsent || source == OpsReplayUsageUpstream || source == OpsReplayUsageEstimated
			require.Equal(t, valid, value.Exists())
			if valid {
				require.Len(t, value.Map(), 5)
				for _, key := range []string{"upstream_progress_observed", "client_output_started", "terminal_seen", "retry_requested"} {
					require.Equal(t, gjson.False, value.Get(key).Type, key)
				}
				require.Equal(t, string(source), value.Get("usage_source").String())
			} else if source != "" {
				require.NotContains(t, encoded, `"usage_source":"`+string(source)+`"`)
			}
			parsed, err := ParseOpsUpstreamErrors(encoded)
			require.NoError(t, err)
			require.Len(t, parsed, 1)
			require.Equal(t, valid, parsed[0].ReplayBoundary != nil)
		})
	}
	legacy, err := ParseOpsUpstreamErrors(`[{"message":"legacy failure","kind":"stream_read_error"}]`)
	require.NoError(t, err)
	require.Nil(t, legacy[0].ReplayBoundary, "old events have no evidence; false must never be synthesized")
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "replay_boundary")

	// The queue also validates direct event callers that bypass append.
	entry := &OpsInsertErrorLogInput{UpstreamErrors: []*OpsUpstreamErrorEvent{{Message: "failure", ReplayBoundary: &OpsReplayBoundary{UsageSource: "arbitrary secret"}}}}
	require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
	require.NotContains(t, *entry.UpstreamErrorsJSON, "replay_boundary")
	require.NotContains(t, *entry.UpstreamErrorsJSON, "arbitrary secret")
}

func TestOAuthReliabilityReplayBoundaryCopiesAcrossAttemptsTurnsAndQueue(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportWS)
	BeginOpsStreamTurn(c, 1)
	first := OpsReplayBoundary{UsageSource: OpsReplayUsageUpstream, UpstreamProgressObserved: true}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: 11, AtUnixMs: 1, UpstreamRequestID: "same", Message: "first failure", ReplayBoundary: &first})
	first.UsageSource = OpsReplayUsageEstimated
	first.TerminalSeen = true
	stored := oauthReplayOpsEvents(t, c)[0]
	require.Equal(t, OpsReplayUsageUpstream, stored.ReplayBoundary.UsageSource)
	require.False(t, stored.ReplayBoundary.TerminalSeen, "append owns a value copy")
	MarkOpsStreamFailure(c, "upstream_error", "upstream_stream_read_error", "turn one", 502)
	stored.ReplayBoundary.UsageSource = OpsReplayUsageEstimated
	stored.ReplayBoundary.ClientOutputStarted = true
	BeginOpsStreamTurn(c, 2)
	second := OpsReplayBoundary{UsageSource: OpsReplayUsageAbsent, RetryRequested: true}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: 22, AtUnixMs: 1, UpstreamRequestID: "same", Message: "second failure", ReplayBoundary: &second})
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: 23, AtUnixMs: 1, UpstreamRequestID: "same", Message: "uninstrumented attempt"})
	MarkOpsStreamFailure(c, "upstream_error", "upstream_stream_read_error", "turn two", 502)
	MarkOpsStreamError(c, "upstream_error", "duplicate terminal", 502)
	turns := GetOpsStreamErrors(c)
	require.Len(t, turns, 2, "diagnostics do not create additional rows or terminals")
	require.Equal(t, 1, turns[0].Turn)
	require.Equal(t, int64(11), turns[0].UpstreamErrors[0].AccountID)
	require.Equal(t, OpsReplayUsageUpstream, turns[0].UpstreamErrors[0].ReplayBoundary.UsageSource)
	require.False(t, turns[0].UpstreamErrors[0].ReplayBoundary.ClientOutputStarted, "WS snapshot owns another value copy")
	require.Equal(t, 2, turns[1].Turn)
	require.Len(t, turns[1].UpstreamErrors, 2)
	require.Equal(t, int64(22), turns[1].UpstreamErrors[0].AccountID)
	require.Equal(t, second, *turns[1].UpstreamErrors[0].ReplayBoundary)
	require.Nil(t, turns[1].UpstreamErrors[1].ReplayBoundary)
	entry := &OpsInsertErrorLogInput{UpstreamErrors: turns[0].UpstreamErrors}
	require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
	queued := *entry.UpstreamErrorsJSON
	turns[0].UpstreamErrors[0].ReplayBoundary.UsageSource = OpsReplayUsageEstimated
	require.Equal(t, queued, *entry.UpstreamErrorsJSON, "queue owns serialized snapshot")
	require.Equal(t, "upstream", gjson.Get(queued, "0.replay_boundary.usage_source").String())
	require.Nil(t, entry.UpstreamErrors, "raw caller pointers are released at queue boundary")
}

func TestOAuthReliabilityReplayBoundaryQueueKeepsMetadataWithinExistingBounds(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprintf("large=%t", large), func(t *testing.T) {
			total := opsUpstreamErrorsMaxEvents + 44
			entry := &OpsInsertErrorLogInput{}
			for i := 1; i <= total; i++ {
				event := &OpsUpstreamErrorEvent{AtUnixMs: int64(i), AccountID: int64(i), UpstreamStatusCode: 502, Kind: "stream_read_error", Message: "failure",
					ReplayBoundary: &OpsReplayBoundary{UsageSource: OpsReplayUsageAbsent, RetryRequested: i%2 == 0}, Detail: `{"safe":"body"}`}
				if large {
					event.Message = strings.Repeat("m", 2048)
					event.UpstreamURL = strings.Repeat("u", 2048)
					event.Detail = `{"safe":"` + strings.Repeat("d", 40_000) + `"}`
					event.UpstreamResponseBody = event.Detail
				}
				entry.UpstreamErrors = append(entry.UpstreamErrors, event)
			}
			require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
			require.NotNil(t, entry.UpstreamErrorsJSON)
			require.LessOrEqual(t, len(*entry.UpstreamErrorsJSON), opsUpstreamErrorsQueueMaxBytes)
			events, err := ParseOpsUpstreamErrors(*entry.UpstreamErrorsJSON)
			require.NoError(t, err)
			require.LessOrEqual(t, len(events), opsUpstreamErrorsMaxEvents)
			require.Equal(t, int64(total), events[len(events)-1].AccountID)
			require.Equal(t, total-len(events), events[0].DroppedEarlierAttempts)
			if !large {
				require.Len(t, events, opsUpstreamErrorsMaxEvents)
			}
			for i, event := range events {
				require.NotNil(t, event.ReplayBoundary)
				require.Equal(t, OpsReplayUsageAbsent, event.ReplayBoundary.UsageSource)
				require.Equal(t, event.AccountID%2 == 0, event.ReplayBoundary.RetryRequested)
				if i < len(events)-opsUpstreamErrorsBodyWindow {
					require.Empty(t, event.Detail, "old large bodies are still removed")
					require.Empty(t, event.UpstreamResponseBody)
				}
				if i > 0 {
					require.Zero(t, event.DroppedEarlierAttempts)
				}
			}
		})
	}
}

type oauthReplayOpsConcurrentUpstream struct {
	HTTPUpstream
	mu    sync.Mutex
	calls map[int64]int
}

func (u *oauthReplayOpsConcurrentUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	_, err := io.Copy(io.Discard, req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.mu.Lock()
	u.calls[accountID]++
	u.mu.Unlock()
	wire := oauthReliabilityChatSSE(oauthReliabilityChatCreated)
	switch accountID % 3 {
	case 1:
		wire += oauthReliabilityChatSSE(oauthReliabilityChatUsage)
	case 2:
		wire += oauthReliabilityChatSSE(oauthReliabilityChatText)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"same-on-all-concurrent-attempts"}}, Body: io.NopCloser(io.MultiReader(strings.NewReader(wire), oauthReliabilityChatErrorReader{err: io.ErrUnexpectedEOF})), Request: req}, nil
}

func (u *oauthReplayOpsConcurrentUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestOAuthReliabilityConcurrentReplayBoundaryForwardOpsOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &oauthReplayOpsConcurrentUpstream{calls: map[int64]int{}}
	svc := newOpenAIRejectedFieldTestService(nil)
	svc.httpUpstream = upstream
	for i := int64(0); i < 12; i++ {
		t.Run(fmt.Sprintf("request_%02d", i), func(t *testing.T) {
			t.Parallel()
			account := newOpenAIOAuthNamespaceTestAccount()
			account.ID = 300 + i
			body := []byte(`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"synthetic concurrent request"}]}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("User-Agent", "synthetic-contract-client/1.0")
			core, logs := observer.New(zap.ErrorLevel)
			c.Request = c.Request.WithContext(logger.IntoContext(context.Background(), zap.New(core)))
			result, err := svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
			require.Error(t, err)
			wantRetry := i%3 == 0
			var failover *UpstreamFailoverError
			require.Equal(t, wantRetry, errors.As(err, &failover))
			if wantRetry {
				require.Nil(t, result)
			} else {
				require.NotNil(t, result)
			}
			entry := &OpsInsertErrorLogInput{UpstreamErrors: oauthReplayOpsEvents(t, c)}
			require.Len(t, entry.UpstreamErrors, 1)
			require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
			require.NotNil(t, entry.UpstreamErrorsJSON)
			raw := *entry.UpstreamErrorsJSON
			require.Equal(t, account.ID, gjson.Get(raw, "0.account_id").Int())
			boundary := gjson.Get(raw, "0.replay_boundary")
			require.True(t, boundary.IsObject(), raw)
			require.Equal(t, wantRetry, boundary.Get("retry_requested").Bool())
			require.Equal(t, !wantRetry, boundary.Get("upstream_progress_observed").Bool())
			require.Equal(t, i%3 == 2, boundary.Get("client_output_started").Bool())
			require.Equal(t, []string{"absent", "upstream", "estimated"}[i%3], boundary.Get("usage_source").String())
			require.Empty(t, logs.FilterMessage("openai chat_completions stream replay boundary").All())
			upstream.mu.Lock()
			calls := upstream.calls[account.ID]
			upstream.mu.Unlock()
			require.Equal(t, 1, calls)
		})
	}
}

func TestOAuthReliabilityReplayBoundaryReaderPendingFramesAndControls(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, tc := range []struct {
			name, wire, source                    string
			progress, terminal, apiKey, completed bool
		}{
			{name: "pending_actual_usage", wire: "data: " + oauthReliabilityChatUsage + "\n", source: "upstream", progress: true},
			{name: "pending_text", wire: "data: " + oauthReliabilityChatText + "\n", source: "absent", progress: true},
			{name: "pending_tool", wire: "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item_tool\",\"call_id\":\"call_tool\",\"name\":\"lookup\",\"arguments\":\"{}\"}}\n", source: "absent", progress: true},
			{name: "pending_terminal", wire: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n", source: "absent", terminal: true},
			{name: "uninstrumented_api_key", wire: oauthReliabilityChatSSE(oauthReliabilityChatUsage), apiKey: true},
			{name: "completed", wire: oauthReliabilityChatSSE(oauthReliabilityContractCompleted), completed: true},
		} {
			t.Run(fmt.Sprintf("async=%t/%s", async, tc.name), func(t *testing.T) {
				h := newOAuthReliabilityChatHarness(async)
				core, logs := observer.New(zap.ErrorLevel)
				h.c.Request = h.c.Request.WithContext(logger.IntoContext(context.Background(), zap.New(core)))
				if tc.apiKey {
					h.account.Type = AccountTypeAPIKey
				}
				result, err := h.run(tc.wire, io.ErrUnexpectedEOF, nil)
				if tc.completed {
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, 7, result.Usage.InputTokens)
					require.Equal(t, 2, result.Usage.OutputTokens)
					value, _ := h.c.Get(OpsUpstreamErrorsKey)
					require.Nil(t, value, "a successful terminal never creates a diagnostic error event")
					return
				}
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover)
				events := oauthReplayOpsEvents(t, h.c)
				require.Len(t, events, 1)
				if tc.apiKey {
					require.Nil(t, events[0].ReplayBoundary)
					return
				}
				boundary := events[0].ReplayBoundary
				require.NotNil(t, boundary)
				require.Equal(t, tc.progress, boundary.UpstreamProgressObserved)
				require.False(t, boundary.ClientOutputStarted, "a pending frame was observed, not published")
				require.Equal(t, tc.terminal, boundary.TerminalSeen)
				require.Equal(t, OpsReplayUsageSource(tc.source), boundary.UsageSource)
				require.False(t, boundary.RetryRequested)
				require.Empty(t, logs.FilterMessage("openai chat_completions stream replay boundary").All())
			})
		}
	}
}
