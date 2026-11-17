//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const oauthReplayOpsCreated = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_diagnostics\",\"status\":\"in_progress\",\"output\":[]}}\n\n"
const oauthReplayOpsUsage = "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":19,\"output_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":4}}}}\n\n"
const oauthReplayOpsZeroUsage = "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n"
const oauthReplayOpsText = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic answer\"}\n\n"

type oauthReplayOpsReadError struct{ err error }

func (r oauthReplayOpsReadError) Read([]byte) (int, error) { return 0, r.err }

type oauthReplayOpsUpstream struct {
	service.HTTPUpstream
	mu          sync.Mutex
	wire        string
	readErr     error
	secondFails bool
	accountIDs  []int64
	onFirst     func()
}

func (u *oauthReplayOpsUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	_, err := io.Copy(io.Discard, req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	first := len(u.accountIDs) == 1
	u.mu.Unlock()
	if first && u.onFirst != nil {
		u.onFirst()
	}
	var reader io.Reader = strings.NewReader(u.wire)
	if accountID == 2 && !u.secondFails {
		reader = strings.NewReader("data: " + oauthReliabilityHandlerModelSuccess + "\n\n")
	} else if u.readErr != nil {
		reader = io.MultiReader(reader, oauthReplayOpsReadError{u.readErr})
	}
	// Deliberately identical upstream IDs on different attempts. Attribution
	// must come from the actual event constructor, never ID/time matching.
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"same-synthetic-request"}}, Body: io.NopCloser(reader), Request: req}, nil
}

func (u *oauthReplayOpsUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

// This public forwarding -> middleware -> queue -> repository contract uses
// only pre-existing APIs and JSON assertions, so the test can also be applied
// alone to the old commit for a meaningful missing-field red baseline.
func TestOAuthReliabilityReplayBoundaryForwardOpsAtErrorLevel(t *testing.T) {
	for _, tc := range []struct {
		name, wire, source                                                   string
		readErr                                                              error
		progress, output, terminal, retry, keepalive, exhausted, secondFails bool
	}{
		{name: "safe_eof", source: "absent", retry: true},
		{name: "heartbeat_metadata_read_error", wire: ": heartbeat\n\n" + oauthReplayOpsCreated, readErr: io.ErrUnexpectedEOF, source: "absent", retry: true},
		{name: "zero_usage_is_observed", wire: oauthReplayOpsZeroUsage, readErr: io.ErrUnexpectedEOF, source: "upstream", retry: true},
		{name: "actual_usage_then_zero", wire: oauthReplayOpsUsage + oauthReplayOpsZeroUsage, readErr: io.ErrUnexpectedEOF, source: "upstream", progress: true},
		{name: "text_estimate", wire: oauthReplayOpsCreated + oauthReplayOpsText, readErr: io.ErrUnexpectedEOF, source: "estimated", progress: true, output: true},
		{name: "keepalive_estimate_without_progress", wire: oauthReplayOpsCreated, readErr: io.ErrUnexpectedEOF, source: "estimated", keepalive: true},
		{name: "pending_terminal_before_read_error", wire: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n", readErr: io.ErrUnexpectedEOF, source: "estimated", terminal: true},
		{name: "candidate_not_executed_when_budget_exhausted", wire: oauthReplayOpsCreated, readErr: io.ErrUnexpectedEOF, source: "absent", retry: true, exhausted: true},
		{name: "next_uninstrumented_account_stays_unknown", wire: oauthReplayOpsCreated, readErr: io.ErrUnexpectedEOF, source: "absent", retry: true, secondFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 4)
			gin.SetMode(gin.TestMode)
			core, logs := observer.New(zap.ErrorLevel)
			ctx := logger.IntoContext(context.Background(), zap.New(core))
			upstream := &oauthReplayOpsUpstream{wire: tc.wire, readErr: tc.readErr, secondFails: tc.secondFails}
			h, usage := oauthReliabilityModelHandler(t, upstream, false, false)
			if tc.exhausted {
				h.maxAccountSwitches = 0
			}
			seed, _ := oauthReliabilityModelRequest(t, "chat", true, ctx)
			repo := &ingressRejectOpsRepo{}
			ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				for key, value := range seed.Keys {
					c.Set(key, value)
				}
				if tc.keepalive {
					upstream.onFirst = func() {
						c.Header("Content-Type", "text/event-stream")
						_, _ = c.Writer.WriteString(": synthetic keepalive\n\n")
						c.Writer.Flush()
					}
				}
				h.ChatCompletions(c)
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, seed.Request)
			require.Empty(t, logs.FilterMessage("openai chat_completions stream replay boundary").All(), "INFO must be disabled for this persistence proof")
			wantCalls := []int64{1}
			if tc.retry && !tc.exhausted {
				wantCalls = append(wantCalls, 2)
			}
			require.Equal(t, wantCalls, upstream.accountIDs, "diagnostics never authorize extra attempts")
			require.Equal(t, int64(1), OpsErrorLogQueueLength(), recorder.Body.String())
			job := <-opsErrorLogQueue
			require.NotNil(t, job.entry.UpstreamErrorsJSON)
			queued := *job.entry.UpstreamErrorsJSON
			require.True(t, gjson.Valid(queued))
			var observed []gjson.Result
			for _, event := range gjson.Parse(queued).Array() {
				if kind := event.Get("kind").String(); kind == "failover" || kind == "stream_read_error" {
					observed = append(observed, event)
				}
			}
			wantEvents := 1
			if tc.secondFails {
				wantEvents++
			}
			require.Len(t, observed, wantEvents, queued)
			first := observed[0]
			require.Equal(t, int64(1), first.Get("account_id").Int())
			boundary := first.Get("replay_boundary")
			require.True(t, boundary.IsObject(), "the exact failed attempt needs a persisted observation: %s", queued)
			require.Len(t, boundary.Map(), 5)
			for key, want := range map[string]bool{"upstream_progress_observed": tc.progress, "client_output_started": tc.output, "terminal_seen": tc.terminal, "retry_requested": tc.retry} {
				value := boundary.Get(key)
				require.True(t, value.Exists(), "false is observed; absence is unknown: %s", key)
				require.Equal(t, want, value.Bool(), key)
			}
			require.Equal(t, tc.source, boundary.Get("usage_source").String())
			if tc.secondFails {
				require.Equal(t, int64(2), observed[1].Get("account_id").Int())
				require.False(t, observed[1].Get("replay_boundary").Exists(), "API-key reader is uninstrumented, not a copy of prior OAuth evidence")
			}
			for _, private := range []string{"synthetic-token", "synthetic-key", "synthetic request", "synthetic answer"} {
				require.NotContains(t, boundary.Raw, private)
			}
			recovered := tc.retry && !tc.exhausted && !tc.secondFails
			if recovered {
				require.Equal(t, http.StatusOK, job.entry.StatusCode)
				require.Contains(t, job.entry.ErrorMessage, "Recovered upstream error")
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Contains(t, recorder.Body.String(), "model available on the next credential")
			} else {
				require.Equal(t, http.StatusBadGateway, job.entry.StatusCode)
				require.NotContains(t, job.entry.ErrorMessage, "Recovered")
			}
			// Exercise the actual worker persistence boundary without a live DB.
			require.NoError(t, ops.RecordErrorBatch(context.Background(), []*service.OpsInsertErrorLogInput{job.entry}))
			require.Len(t, repo.entries, 1)
			require.Equal(t, queued, *repo.entries[0].UpstreamErrorsJSON)
			if recovered || tc.source == "upstream" || tc.source == "estimated" || tc.secondFails {
				require.Len(t, usage.created, 1, "same pre-existing billing branch")
				bill := <-usage.created
				if recovered {
					require.Equal(t, int64(2), bill.AccountID)
					require.Equal(t, 7, bill.InputTokens+bill.CacheReadTokens)
					require.Equal(t, 2, bill.OutputTokens)
				} else if tc.source == "upstream" {
					require.Equal(t, 19, bill.InputTokens+bill.CacheReadTokens)
					require.Equal(t, 4, bill.CacheReadTokens)
					require.Equal(t, 7, bill.OutputTokens)
				} else {
					require.Positive(t, bill.InputTokens)
				}
			} else {
				require.Empty(t, usage.created, "a retry candidate is never newly estimated/billed")
			}
		})
	}
}

func TestOAuthReliabilityReplayBoundaryForwardCancellationDoesNotInventOpsEvent(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := &oauthReplayOpsUpstream{wire: oauthReplayOpsCreated, readErr: context.Canceled, onFirst: cancel}
	h, _ := oauthReliabilityModelHandler(t, upstream, false, false)
	seed, _ := oauthReliabilityModelRequest(t, "chat", true, ctx)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		for key, value := range seed.Keys {
			c.Set(key, value)
		}
		h.ChatCompletions(c)
		value, _ := c.Get(service.OpsUpstreamErrorsKey)
		events, _ := value.([]*service.OpsUpstreamErrorEvent)
		for _, event := range events {
			require.NotEqual(t, "stream_read_error", event.Kind)
			require.NotEqual(t, "failover", event.Kind)
		}
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, seed.Request)
	require.Equal(t, []int64{1}, upstream.accountIDs)
	for len(opsErrorLogQueue) > 0 {
		job := <-opsErrorLogQueue
		if job.entry.UpstreamErrorsJSON != nil {
			require.NotContains(t, *job.entry.UpstreamErrorsJSON, "replay_boundary")
		}
	}
}
