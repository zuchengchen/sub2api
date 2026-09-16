package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

func trustedSchedulingGroupFromContext(ctx context.Context, groupID *int64) *Group {
	if groupID == nil {
		return nil
	}
	group, ok := ctx.Value(ctxkey.Group).(*Group)
	if !ok || !IsGroupContextValid(group) || group.ID != *groupID {
		return nil
	}
	return group
}

func (s *GatewayService) isPureModelSupportMiss(
	ctx context.Context,
	_ []Account,
	requestedModel string,
	platform string,
	excludedIDs map[int64]struct{},
	allowMixedScheduling bool,
	schedGroup *Group,
	groupID *int64,
) bool {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" || !publicModelSupportMiss404Enabled(ctx) || len(excludedIDs) > 0 ||
		s == nil || s.supportDecisionReader == nil || s.cfg == nil || platform == "" {
		return false
	}
	if groupID != nil && (!IsGroupContextValid(schedGroup) || schedGroup.ID != *groupID) {
		return false
	}

	scope := SupportDecisionScope{
		Platform:             platform,
		IncludeGrouped:       groupID == nil && s.cfg.RunMode == config.RunModeSimple,
		AllowMixedScheduling: allowMixedScheduling,
	}
	if groupID != nil {
		scope.GroupID = *groupID
	}
	thinkingEnabled, _ := ThinkingEnabledFromContext(ctx)
	return s.supportDecisionReader.Lookup(SupportDecisionQuery{
		Scope:           scope,
		RequestedModel:  requestedModel,
		RequiresPrivacy: schedGroup != nil && schedGroup.RequirePrivacySet,
		ThinkingEnabled: thinkingEnabled,
	}) == SupportDecisionPureMiss
}

func (s *GatewayService) emptyPoolSelectionError(
	ctx context.Context,
	accounts []Account,
	requestedModel string,
	platform string,
	excludedIDs map[int64]struct{},
	allowMixedScheduling bool,
	schedGroup *Group,
	groupID *int64,
	fallback error,
) error {
	if s.isPureModelSupportMiss(ctx, accounts, requestedModel, platform, excludedIDs, allowMixedScheduling, schedGroup, groupID) {
		return newModelNotSupportedByAccountsError(requestedModel)
	}
	return fallback
}

func isPureOpenAIModelSupportMiss(
	ctx context.Context,
	svc *OpenAIGatewayService,
	groupID *int64,
	_ []Account,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requireCompact bool,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requiredTransport OpenAIUpstreamTransport,
	schedGroup *Group,
) bool {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" || !publicModelSupportMiss404Enabled(ctx) || len(excludedIDs) > 0 ||
		svc == nil || svc.supportDecisionReader == nil || svc.cfg == nil {
		return false
	}
	if groupID != nil && (!IsGroupContextValid(schedGroup) || schedGroup.ID != *groupID || schedGroup.Platform != PlatformOpenAI) {
		return false
	}

	scope := SupportDecisionScope{
		Platform:       PlatformOpenAI,
		IncludeGrouped: groupID == nil && svc.cfg.RunMode == config.RunModeSimple,
	}
	if groupID != nil {
		scope.GroupID = *groupID
	}
	return svc.supportDecisionReader.Lookup(SupportDecisionQuery{
		Scope:              scope,
		RequestedModel:     requestedModel,
		RequiresPrivacy:    schedGroup != nil && schedGroup.RequirePrivacySet,
		EndpointCapability: requiredCapability,
		ImageCapability:    requiredImageCapability,
		RequireCompact:     requireCompact,
		Transport:          requiredTransport,
	}) == SupportDecisionPureMiss
}

func (s *OpenAIGatewayService) emptyPoolOpenAISelectionError(
	ctx context.Context,
	groupID *int64,
	accounts []Account,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requireCompact bool,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requiredTransport OpenAIUpstreamTransport,
	schedGroup *Group,
	compactBlocked bool,
	details string,
) error {
	if compactBlocked {
		return ErrNoAvailableCompactAccounts
	}
	if isPureOpenAIModelSupportMiss(ctx, s, groupID, accounts, requestedModel, excludedIDs, requireCompact, requiredCapability, requiredImageCapability, requiredTransport, schedGroup) {
		return newModelNotSupportedByAccountsError(requestedModel)
	}
	return noAvailableOpenAISelectionError(requestedModel, false, details)
}
