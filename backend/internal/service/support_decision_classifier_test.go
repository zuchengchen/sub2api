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
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	reader.Install(BuildSupportDecisionTable(time.Now()))
	svc := &GatewayService{cfg: testConfig(), supportDecisionReader: reader}
	ctx := WithPublicModelSupportMiss404(context.Background())

	require.False(t, svc.isPureModelSupportMiss(ctx, nil, "claude-opus-4-6", PlatformAnthropic, nil, false, nil, nil))
	require.ErrorIs(t, svc.emptyPoolSelectionError(ctx, nil, "claude-opus-4-6", PlatformAnthropic, nil, false, nil, nil, ErrNoAvailableAccounts), ErrNoAvailableAccounts)
	require.NotErrorIs(t, svc.emptyPoolSelectionError(ctx, nil, "claude-opus-4-6", PlatformAnthropic, nil, false, nil, nil, ErrNoAvailableAccounts), ErrModelNotSupportedByAccounts)
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
