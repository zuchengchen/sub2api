//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type recordingSupportDecisionReader struct {
	result  SupportDecisionResult
	queries []SupportDecisionQuery
}

func (r *recordingSupportDecisionReader) Lookup(query SupportDecisionQuery) SupportDecisionResult {
	r.queries = append(r.queries, query)
	return r.result
}

func TestSupportDecisionUnknownDoesNotBecomePureMiss404(t *testing.T) {
	for _, state := range []struct {
		name    string
		builtAt time.Time
	}{
		{name: "without_table"},
		{name: "fresh_table", builtAt: time.Now()},
		{name: "stale_table", builtAt: time.Now().Add(-time.Minute)},
	} {
		t.Run(state.name, func(t *testing.T) {
			reader := ProvideSupportDecisionAtomicReader()
			if !state.builtAt.IsZero() {
				reader.Install(BuildSupportDecisionTable(state.builtAt))
			}
			svc := &GatewayService{cfg: testConfig()}
			openai := &OpenAIGatewayService{cfg: testConfig()}
			ProvideSupportDecisionReader(reader, svc, openai)
			require.Same(t, reader, svc.supportDecisionReader)
			require.Same(t, reader, openai.supportDecisionReader)
			ctx := WithPublicModelSupportMiss404(context.Background())

			for _, tc := range []struct {
				name     string
				platform string
				excluded map[int64]struct{}
			}{
				{name: "anthropic_empty_pool", platform: PlatformAnthropic},
				{name: "anthropic_excluded_accounts", platform: PlatformAnthropic, excluded: map[int64]struct{}{1: {}}},
				{name: "composite_empty_pool", platform: PlatformComposite},
			} {
				t.Run(tc.name, func(t *testing.T) {
					err := svc.emptyPoolSelectionError(ctx, nil, "missing-model", tc.platform, tc.excluded, false, nil, nil, ErrNoAvailableAccounts)
					require.ErrorIs(t, err, ErrNoAvailableAccounts)
					require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
				})
			}

			for _, tc := range []struct {
				name           string
				excluded       map[int64]struct{}
				requireCompact bool
				compactBlocked bool
			}{
				{name: "openai_empty_pool"},
				{name: "openai_excluded_accounts", excluded: map[int64]struct{}{1: {}}},
				{name: "openai_compact_empty_pool", requireCompact: true},
				{name: "openai_compact_blocked", requireCompact: true, compactBlocked: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					err := openai.emptyPoolOpenAISelectionError(ctx, nil, nil, "gpt-5.4", tc.excluded, tc.requireCompact, "", "", "", nil, tc.compactBlocked, "")
					if tc.compactBlocked {
						require.ErrorIs(t, err, ErrNoAvailableCompactAccounts)
					} else {
						require.ErrorIs(t, err, ErrNoAvailableAccounts)
					}
					require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
				})
			}
		})
	}
}

func TestSupportDecisionInjectedPureMissReturnsModelNotSupported(t *testing.T) {
	reader := &recordingSupportDecisionReader{result: SupportDecisionPureMiss}
	svc := &GatewayService{cfg: testConfig(), supportDecisionReader: reader}
	ctx := WithPublicModelSupportMiss404(context.Background())

	require.True(t, svc.isPureModelSupportMiss(ctx, nil, "missing-model", PlatformAnthropic, nil, true, nil, nil))
	err := svc.emptyPoolSelectionError(ctx, nil, "missing-model", PlatformAnthropic, nil, true, nil, nil, ErrNoAvailableAccounts)
	require.ErrorIs(t, err, ErrModelNotSupportedByAccounts)
	require.Equal(t, "missing-model", reader.queries[0].RequestedModel)
	require.Equal(t, PlatformAnthropic, reader.queries[0].Scope.Platform)
}

func TestSupportDecisionUnmodeledConstraintsAreUnknownNotPureMiss(t *testing.T) {
	table := BuildSupportDecisionTable(time.Now())
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	reader.Install(table)
	openai := &OpenAIGatewayService{cfg: testConfig(), supportDecisionReader: reader}
	ctx := WithPublicModelSupportMiss404(context.Background())

	require.Equal(t, SupportDecisionUnknown, table.Lookup(SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI},
		RequestedModel: "gpt-5.4",
		RequireCompact: true,
	}))
	require.Equal(t, SupportDecisionUnknown, table.Lookup(SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformGrok},
		RequestedModel: "grok-4.6",
	}))
	require.Equal(t, SupportDecisionUnknown, table.Lookup(SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI},
		RequestedModel: "gpt-5.4",
		Transport:      OpenAIUpstreamTransportAny,
	}))
	require.False(t, isPureOpenAIModelSupportMiss(ctx, openai, nil, nil, "gpt-5.4", nil, true, "", "", "", nil))
	require.ErrorIs(t, openai.emptyPoolOpenAISelectionError(ctx, nil, nil, "gpt-5.4", nil, true, "", "", "", nil, true, ""), ErrNoAvailableCompactAccounts)
	require.ErrorIs(t, openai.emptyPoolOpenAISelectionError(ctx, nil, nil, "gpt-5.4", nil, false, "", "", "", nil, false, "grok_free_quota_soft_gate"), ErrNoAvailableAccounts)
	require.NotErrorIs(t, openai.emptyPoolOpenAISelectionError(ctx, nil, nil, "gpt-5.4", nil, false, "", "", "", nil, false, "grok_free_quota_soft_gate"), ErrModelNotSupportedByAccounts)
}

func TestSupportDecisionSimpleModeIncludeGrouped(t *testing.T) {
	reader := &recordingSupportDecisionReader{result: SupportDecisionUnknown}
	svc := &GatewayService{cfg: &config.Config{RunMode: config.RunModeSimple}, supportDecisionReader: reader}
	ctx := WithPublicModelSupportMiss404(context.Background())
	require.False(t, svc.isPureModelSupportMiss(ctx, nil, "claude-sonnet-4-6", PlatformAnthropic, nil, false, nil, nil))
	require.True(t, reader.queries[0].Scope.IncludeGrouped)
}
