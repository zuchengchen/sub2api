//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func oauthWSReadDiagnosticContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return c
}

func oauthWSReadDiagnosticAccount() *Account {
	proxyID := int64(41)
	return &Account{ID: 7, Name: "synthetic account", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Name: "synthetic proxy", Username: "do-not-persist-user", Password: "do-not-persist-password"}}
}

func oauthWSReadDiagnosticEvent() *OpsUpstreamErrorEvent {
	return &OpsUpstreamErrorEvent{AtUnixMs: 12, AccountID: 7, Kind: openAIWSReadDiagnosticKind,
		Stage: openAIWSReadDiagnosticStage, Reason: openAIWSReadDiagnosticReason,
		ReplayBoundary: &OpsReplayBoundary{UpstreamProgressObserved: true, UsageSource: OpsReplayUsageAbsent}}
}

func oauthWSReadDiagnosticEvents(t *testing.T, entry *OpsInsertErrorLogInput) []*OpsUpstreamErrorEvent {
	t.Helper()
	require.NotNil(t, entry.UpstreamErrorsJSON)
	events, err := ParseOpsUpstreamErrors(*entry.UpstreamErrorsJSON)
	require.NoError(t, err)
	return events
}

func TestOAuthReliabilityWSReadDiagnosticUsageObservation(t *testing.T) {
	actual := OpenAIUsage{InputTokens: 11, OutputTokens: 3, CacheReadInputTokens: 2}
	for _, tc := range []struct {
		name, wire string
		initial    OpenAIUsage
		observed   bool
		want       OpenAIUsage
	}{
		{name: "absent", wire: `{"type":"response.in_progress","response":{}}`},
		{name: "null", wire: `{"type":"response.in_progress","usage":null}`},
		{name: "array", wire: `{"type":"response.in_progress","usage":[]}`},
		{name: "invalid_json", wire: `{"type":"response.in_progress","usage":{"input_tokens":11}`},
		{name: "zero", wire: `{"type":"response.in_progress","usage":{"input_tokens":0,"output_tokens":0}}`, observed: true},
		{name: "empty_object_existing_parser_semantics", wire: `{"type":"response.in_progress","usage":{}}`, observed: true},
		{name: "actual", wire: `{"type":"response.in_progress","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}`, observed: true, want: actual},
		{name: "nested_actual", wire: `{"type":"response.failed","response":{"usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`, observed: true, want: actual},
		{name: "terminal_zero_preserves_actual", wire: `{"type":"response.failed","usage":{"input_tokens":0,"output_tokens":0}}`, initial: actual, observed: true, want: actual},
		{name: "absent_does_not_infer_from_accumulator", wire: `{"type":"response.failed"}`, initial: actual, want: actual},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, legacy := tc.initial, tc.initial
			require.Equal(t, tc.observed, parseOpenAIWSResponseUsageWithObservation([]byte(tc.wire), &got))
			parseOpenAIWSResponseUsageFromCompletedEvent([]byte(tc.wire), &legacy)
			require.Equal(t, tc.want, got)
			require.Equal(t, legacy, got, "observation cannot change the pre-existing accumulator")
		})
	}
	t.Run("delta_not_parsed_by_reader", func(t *testing.T) {
		require.False(t, openAIWSMessageShouldParseUsage("response.output_text.delta", []byte(`{"type":"response.output_text.delta","usage":{"input_tokens":11}}`)))
		require.False(t, parseOpenAIWSResponseUsageWithObservation([]byte(`{"usage":{}}`), nil))
	})
}

func TestOAuthReliabilityWSReadDiagnosticCopyAndOnce(t *testing.T) {
	c := oauthWSReadDiagnosticContext()
	account := oauthWSReadDiagnosticAccount()
	boundary := OpsReplayBoundary{UpstreamProgressObserved: true, ClientOutputStarted: true, UsageSource: OpsReplayUsageUpstream}
	setOpsUpstreamError(c, 502, "original scalar", "original detail")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: 99, UpstreamStatusCode: 429, Kind: "failover", Message: "prior attempt"})
	before, err := json.Marshal(c.Keys)
	require.NoError(t, err)
	recordOpenAIWSReadDiagnostic(c, account, boundary)
	raw, exists := c.Get(openAIWSReadDiagnosticKey)
	require.True(t, exists)
	stored, ok := raw.(*OpsUpstreamErrorEvent)
	require.True(t, ok)
	boundary.UsageSource = OpsReplayUsageEstimated
	account.Proxy.ID = 88
	account.Proxy.Name = "changed after capture"
	recordOpenAIWSReadDiagnostic(c, &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, boundary)
	require.Equal(t, int64(7), stored.AccountID, "one terminal reader owns this request-local value")
	require.Equal(t, int64(41), *stored.ProxyID)
	require.Equal(t, "synthetic proxy", stored.ProxyName)
	require.Equal(t, OpsReplayUsageUpstream, stored.ReplayBoundary.UsageSource)
	keysWithoutSidecar := make(map[string]any, len(c.Keys))
	for key, value := range c.Keys {
		if key != openAIWSReadDiagnosticKey {
			keysWithoutSidecar[key] = value
		}
	}
	after, err := json.Marshal(keysWithoutSidecar)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after), "producer cannot change scalar, raw events or policy flags")
	AttachOpenAIWSReadDiagnostic(c, nil)
	entry := &OpsInsertErrorLogInput{ErrorMessage: "original row"}
	AttachOpenAIWSReadDiagnostic(c, entry)
	require.NotNil(t, entry.upstreamReadDiagnostic)
	require.Nil(t, entry.UpstreamErrors, "no semantic outcome consumer can see the diagnostic")
	stored.ReplayBoundary.UsageSource = OpsReplayUsageEstimated
	*stored.ProxyID = 93
	require.Equal(t, OpsReplayUsageUpstream, entry.upstreamReadDiagnostic.ReplayBoundary.UsageSource)
	require.Equal(t, int64(41), *entry.upstreamReadDiagnostic.ProxyID)
	second := &OpsInsertErrorLogInput{}
	AttachOpenAIWSReadDiagnostic(c, second)
	require.Nil(t, second.upstreamReadDiagnostic)
	recordOpenAIWSReadDiagnostic(c, account, boundary)
	require.Nil(t, c.Keys[openAIWSReadDiagnosticKey], "consumed tombstone forbids a second producer")
	require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
	events := oauthWSReadDiagnosticEvents(t, entry)
	require.Len(t, events, 1)
	require.Equal(t, "original row", entry.ErrorMessage)
	require.Zero(t, events[0].UpstreamStatusCode)
	require.Empty(t, events[0].UpstreamRequestID)
	require.Empty(t, events[0].UpstreamURL)
	require.Empty(t, events[0].Detail)
	require.Empty(t, events[0].UpstreamResponseBody)
	require.NotContains(t, *entry.UpstreamErrorsJSON, "do-not-persist")
	require.Nil(t, entry.upstreamReadDiagnostic)
	for _, field := range []string{"terminal_seen", "retry_requested"} {
		require.Equal(t, gjson.False, gjson.Get(*entry.UpstreamErrorsJSON, "0.replay_boundary."+field).Type)
	}
}

func TestOAuthReliabilityWSReadDiagnosticScopeOwnership(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*gin.Context)
		clear func(*gin.Context)
	}{
		{"unknown_transport", func(c *gin.Context) { c.Set(openAIClientTransportContextKey, nil) }, func(c *gin.Context) { SetOpenAIClientTransport(c, OpenAIClientTransportHTTP) }},
		{"native_ws", func(c *gin.Context) { SetOpenAIClientTransport(c, OpenAIClientTransportWS) }, func(c *gin.Context) { SetOpenAIClientTransport(c, OpenAIClientTransportHTTP) }},
		{"request_scoped", func(c *gin.Context) { MarkOpsRequestScopedError(c, "invalid_request_error") }, func(c *gin.Context) { c.Set(OpsRequestScopedErrorKey, false) }},
		{"turn_one", func(c *gin.Context) { c.Set(OpsStreamTurnKey, 1) }, func(c *gin.Context) { c.Set(OpsStreamTurnKey, 0) }},
		{"unknown_turn", func(c *gin.Context) { c.Set(OpsStreamTurnKey, "1") }, func(c *gin.Context) { c.Set(OpsStreamTurnKey, 0) }},
		{"local_model", func(c *gin.Context) {
			MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalModelConfiguration)
		}, func(c *gin.Context) { c.Set(OpsClientBusinessLimitedReasonKey, "") }},
		{"cyber", func(c *gin.Context) { MarkOpsCyberPolicy(c, CyberPolicyMark{}) }, ClearOpsCyberPolicy},
		{"cancelled", func(c *gin.Context) {
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := oauthWSReadDiagnosticContext()
			boundary := OpsReplayBoundary{UsageSource: OpsReplayUsageAbsent}
			recordOpenAIWSReadDiagnostic(c, oauthWSReadDiagnosticAccount(), boundary)
			raw, _ := c.Get(openAIWSReadDiagnosticKey)
			require.NotNil(t, raw)
			tc.alter(c)
			other := &OpsInsertErrorLogInput{}
			AttachOpenAIWSReadDiagnostic(c, other)
			require.Nil(t, other.upstreamReadDiagnostic)
			require.Equal(t, raw, c.Keys[openAIWSReadDiagnosticKey], "excluded row must not steal the HTTP owner's snapshot")
			if tc.clear != nil {
				tc.clear(c)
				owner := &OpsInsertErrorLogInput{}
				AttachOpenAIWSReadDiagnostic(c, owner)
				require.NotNil(t, owner.upstreamReadDiagnostic)
			}
			fresh := oauthWSReadDiagnosticContext()
			tc.alter(fresh)
			recordOpenAIWSReadDiagnostic(fresh, oauthWSReadDiagnosticAccount(), boundary)
			_, exists := fresh.Get(openAIWSReadDiagnosticKey)
			require.False(t, exists, "excluded producer cannot create a snapshot")
		})
	}
	for _, tc := range []struct {
		name, platform, accountType string
		want                        bool
	}{
		{"oauth_http", PlatformOpenAI, AccountTypeOAuth, true},
		{"setup_token_http", PlatformOpenAI, AccountTypeSetupToken, true},
		{"api_key", PlatformOpenAI, AccountTypeAPIKey, false},
		{"other_platform", PlatformAnthropic, AccountTypeOAuth, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := oauthWSReadDiagnosticContext()
			account := &Account{ID: 7, Platform: tc.platform, Type: tc.accountType}
			recordOpenAIWSReadDiagnostic(c, account, OpsReplayBoundary{UsageSource: OpsReplayUsageAbsent})
			entry := &OpsInsertErrorLogInput{}
			AttachOpenAIWSReadDiagnostic(c, entry)
			require.Equal(t, tc.want, entry.upstreamReadDiagnostic != nil)
			if tc.want {
				require.Nil(t, entry.upstreamReadDiagnostic.ProxyID)
				require.Equal(t, opsProxyNameUnknown, entry.upstreamReadDiagnostic.ProxyName, "environment routing is not proof of a direct WS route")
			}
		})
	}
}

func TestOAuthReliabilityWSReadDiagnosticSerializationBoundaries(t *testing.T) {
	t.Run("existing_json_keeps_bytes_and_dropped_marker", func(t *testing.T) {
		for _, original := range []string{`[{"message":"prior","dropped_earlier_attempts":73,"opaque_integer":9007199254740993}]`, `malformed legacy JSON`} {
			entry := &OpsInsertErrorLogInput{UpstreamErrorsJSON: &original, upstreamReadDiagnostic: oauthWSReadDiagnosticEvent()}
			require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
			require.Equal(t, original, *entry.UpstreamErrorsJSON)
			require.Nil(t, entry.upstreamReadDiagnostic)
			require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
			require.Equal(t, original, *entry.UpstreamErrorsJSON)
			c := oauthWSReadDiagnosticContext()
			recordOpenAIWSReadDiagnostic(c, oauthWSReadDiagnosticAccount(), OpsReplayBoundary{UsageSource: OpsReplayUsageAbsent})
			AttachOpenAIWSReadDiagnostic(c, entry)
			second := &OpsInsertErrorLogInput{}
			AttachOpenAIWSReadDiagnostic(c, second)
			require.Nil(t, second.upstreamReadDiagnostic, "discarded late diagnostic cannot migrate to a second row")
			require.Equal(t, original, *entry.UpstreamErrorsJSON)
		}
	})
	t.Run("raw_plus_json_preserves_old_precedence", func(t *testing.T) {
		original := `[{"message":"old JSON","dropped_earlier_attempts":91}]`
		raw := []*OpsUpstreamErrorEvent{{Message: "authoritative raw", DroppedEarlierAttempts: 99, UpstreamStatusCode: 502}}
		without := &OpsInsertErrorLogInput{UpstreamErrorsJSON: &original, UpstreamErrors: raw}
		with := &OpsInsertErrorLogInput{UpstreamErrorsJSON: &original, UpstreamErrors: raw, upstreamReadDiagnostic: oauthWSReadDiagnosticEvent()}
		require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(without))
		require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(with))
		require.Equal(t, without, with, "diagnostic does not alter legacy raw-over-JSON precedence")
	})
	t.Run("invalid_source_drops_only_diagnostic", func(t *testing.T) {
		entry := &OpsInsertErrorLogInput{ErrorMessage: "original row", UpstreamErrors: []*OpsUpstreamErrorEvent{{Message: "prior outcome", UpstreamStatusCode: 429}}, upstreamReadDiagnostic: oauthWSReadDiagnosticEvent()}
		entry.upstreamReadDiagnostic.ReplayBoundary.UsageSource = "private-value-do-not-persist"
		require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
		events := oauthWSReadDiagnosticEvents(t, entry)
		require.Len(t, events, 1)
		require.Equal(t, 429, events[0].UpstreamStatusCode)
		require.Equal(t, "original row", entry.ErrorMessage)
		require.NotContains(t, *entry.UpstreamErrorsJSON, "private-value")
	})
	t.Run("original_sixteen_body_window", func(t *testing.T) {
		entry := &OpsInsertErrorLogInput{upstreamReadDiagnostic: oauthWSReadDiagnosticEvent()}
		for i := 0; i < 17; i++ {
			entry.UpstreamErrors = append(entry.UpstreamErrors, &OpsUpstreamErrorEvent{AccountID: int64(i + 1), UpstreamStatusCode: 429, Message: "old outcome", Detail: `{"safe":"detail"}`, UpstreamResponseBody: `{"safe":"body"}`})
		}
		require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
		events := oauthWSReadDiagnosticEvents(t, entry)
		require.Len(t, events, 18)
		require.Empty(t, events[0].Detail)
		require.Empty(t, events[0].UpstreamResponseBody)
		for _, event := range events[1:17] {
			require.NotEmpty(t, event.Detail)
			require.NotEmpty(t, event.UpstreamResponseBody, "diagnostic cannot evict the oldest of the original sixteen bodies")
		}
		require.Equal(t, openAIWSReadDiagnosticKind, events[17].Kind)
	})
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing_bounds_large_%t", large), func(t *testing.T) {
			entry := &OpsInsertErrorLogInput{upstreamReadDiagnostic: oauthWSReadDiagnosticEvent()}
			const originalCount = 300
			for i := 0; i < originalCount; i++ {
				event := &OpsUpstreamErrorEvent{AccountID: int64(i + 1), UpstreamStatusCode: 429, Message: "old outcome"}
				if large {
					event.UpstreamResponseBody = `{"safe":"` + strings.Repeat("x", 40_000) + `"}`
					event.Detail = event.UpstreamResponseBody
				}
				entry.UpstreamErrors = append(entry.UpstreamErrors, event)
			}
			require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
			events := oauthWSReadDiagnosticEvents(t, entry)
			require.LessOrEqual(t, len(events), opsUpstreamErrorsMaxEvents)
			require.LessOrEqual(t, len(*entry.UpstreamErrorsJSON), opsUpstreamErrorsQueueMaxBytes)
			require.Equal(t, originalCount+1-len(events), events[0].DroppedEarlierAttempts)
			require.Equal(t, openAIWSReadDiagnosticKind, events[len(events)-1].Kind)
			before := *entry.UpstreamErrorsJSON
			require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
			require.Equal(t, before, *entry.UpstreamErrorsJSON, "worker repeat must preserve existing drop count")
		})
	}
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("record_account_auth_projection_queued_%t", queued), func(t *testing.T) {
			var captured []*OpsInsertErrorLogInput
			repo := &opsRepoMock{InsertErrorLogFn: func(_ context.Context, input *OpsInsertErrorLogInput) (int64, error) {
				captured = append(captured, input)
				return int64(len(captured)), nil
			}}
			svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil)
			for _, withDiagnostic := range []bool{false, true} {
				status := 502
				entry := &OpsInsertErrorLogInput{CreatedAt: time.Unix(1, 0), ErrorPhase: "upstream", ErrorType: "upstream_error", UpstreamStatusCode: &status,
					UpstreamErrors: []*OpsUpstreamErrorEvent{{Stage: string(GatewayFailureStageAccountAuth), Message: "credential acquisition failed"}}}
				if withDiagnostic {
					entry.upstreamReadDiagnostic = oauthWSReadDiagnosticEvent()
				}
				if queued {
					// The HTTP row has already completed outcome projection before
					// enqueue; the worker must preserve that projection verbatim.
					status = 0
					message := "credential acquisition failed"
					entry.ErrorPhase, entry.ErrorSource, entry.ErrorOwner = string(GatewayFailureStageAccountAuth), "gateway", "provider"
					entry.UpstreamErrorMessage = &message
					require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
				}
				require.NoError(t, svc.RecordError(context.Background(), entry))
			}
			require.Len(t, captured, 2)
			require.Equal(t, string(GatewayFailureStageAccountAuth), captured[1].ErrorPhase)
			require.Equal(t, "gateway", captured[1].ErrorSource)
			require.Equal(t, "provider", captured[1].ErrorOwner)
			require.NotNil(t, captured[1].UpstreamStatusCode)
			require.Zero(t, *captured[1].UpstreamStatusCode)
			require.Equal(t, "credential acquisition failed", *captured[1].UpstreamErrorMessage)
			events := oauthWSReadDiagnosticEvents(t, captured[1])
			require.Len(t, events, 2)
			require.Equal(t, string(GatewayFailureStageAccountAuth), events[0].Stage)
			require.Equal(t, openAIWSReadDiagnosticKind, events[1].Kind)
			without, with := *captured[0], *captured[1]
			without.UpstreamErrorsJSON, with.UpstreamErrorsJSON = nil, nil
			require.Equal(t, without, with, "all row fields outside the diagnostic JSON must match the old path")
		})
	}
}

func TestOAuthReliabilityConcurrentWSReadDiagnosticIsolation(t *testing.T) {
	const count = 32
	account := oauthWSReadDiagnosticAccount() // Same account/proxy shared read-only by all requests.
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]*OpsInsertErrorLogInput, count)
	errors := make([]error, count)
	want := make([]OpsReplayBoundary, count)
	for i := range count {
		want[i] = OpsReplayBoundary{UpstreamProgressObserved: i&1 != 0, ClientOutputStarted: i&2 != 0, TerminalSeen: i&4 != 0, UsageSource: OpsReplayUsageAbsent}
		if i&8 != 0 {
			want[i].UsageSource = OpsReplayUsageUpstream
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := oauthWSReadDiagnosticContext()
			<-start
			recordOpenAIWSReadDiagnostic(c, account, want[i])
			entry := &OpsInsertErrorLogInput{RequestID: fmt.Sprintf("synthetic-%02d", i), CreatedAt: time.Unix(int64(i), 0)}
			AttachOpenAIWSReadDiagnostic(c, entry)
			second := &OpsInsertErrorLogInput{}
			AttachOpenAIWSReadDiagnostic(c, second)
			if second.upstreamReadDiagnostic != nil {
				errors[i] = fmt.Errorf("snapshot stolen by a second row")
				return
			}
			errors[i] = SanitizeOpsUpstreamErrorsForQueue(entry)
			results[i] = entry
		}(i)
	}
	close(start)
	wg.Wait()
	for i, entry := range results {
		require.NoError(t, errors[i])
		require.NotNil(t, entry)
		events := oauthWSReadDiagnosticEvents(t, entry)
		require.Len(t, events, 1)
		require.Equal(t, want[i], *events[0].ReplayBoundary, "request %d on the same upstream credential", i)
		require.Equal(t, account.ID, events[0].AccountID)
		require.Equal(t, int64(41), *events[0].ProxyID)
		require.Equal(t, fmt.Sprintf("synthetic-%02d", i), entry.RequestID)
		require.NotContains(t, *entry.UpstreamErrorsJSON, "synthetic-", "request IDs do not enter fixed diagnostic metadata")
	}
}
