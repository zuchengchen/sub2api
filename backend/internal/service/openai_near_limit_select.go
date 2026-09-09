package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

func (s *OpenAIGatewayService) trySelectOpenAINearLimitTarget(
	ctx context.Context,
	groupID *int64,
	platform string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requireCompact bool,
	requiredCapability OpenAIEndpointCapability,
) *AccountSelectionResult {
	if skipOpenAINearLimitAccounts(ctx) {
		return nil
	}
	platform = NormalizeOpenAICompatiblePlatform(platform)
	if platform != PlatformOpenAI {
		return nil
	}
	accounts, err := s.listSchedulableAccounts(ctx, groupID, platform)
	if err != nil || len(accounts) == 0 {
		return nil
	}
	now := time.Now()
	var cands []openAINearLimitCandidate
	for i := range accounts {
		acc := &accounts[i]
		if excludedIDs != nil {
			if _, excluded := excludedIDs[acc.ID]; excluded {
				continue
			}
		}
		fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, acc, platform, requestedModel, requireCompact, requiredCapability)
		if fresh == nil {
			continue
		}
		cand, ok := classifyOpenAINearLimitCandidate(fresh, now)
		if !ok {
			continue
		}
		if !canClaimOpenAINearLimitPreference(fresh.ID, now, true) {
			continue
		}
		cands = append(cands, cand)
	}
	sortOpenAINearLimitCandidates(cands)
	lease := openAINearLimitProbeLease(s.cfg)
	for _, cand := range cands {
		acc := cand.account
		effective := effectiveOpenAIAccountConcurrency(acc, s.cfg)
		result, err := s.tryAcquireAccountSlot(ctx, acc.ID, effective)
		if err != nil || result == nil || !result.Acquired {
			continue
		}
		if !claimOpenAINearLimitPreference(acc.ID, lease) {
			if result.ReleaseFunc != nil {
				result.ReleaseFunc()
			}
			continue
		}
		release := result.ReleaseFunc
		wrapped := func() {
			releaseOpenAINearLimitPreference(acc.ID)
			if release != nil {
				release()
			}
		}
		if sessionHash != "" {
			_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, acc.ID, openaiStickySessionTTL)
		}
		logOpenAINearLimitPrioritized(cand)
		selection, err := s.newAcquiredSelectionResult(ctx, acc, wrapped)
		if err != nil {
			wrapped()
			continue
		}
		return selection
	}
	return nil
}

func (s *OpenAIGatewayService) trySelectOpenAINearLimitAccount(
	ctx context.Context,
	groupID *int64,
	platform string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requireCompact bool,
	requiredCapability OpenAIEndpointCapability,
) *Account {
	if skipOpenAINearLimitAccounts(ctx) {
		return nil
	}
	platform = NormalizeOpenAICompatiblePlatform(platform)
	if platform != PlatformOpenAI {
		return nil
	}
	accounts, err := s.listSchedulableAccounts(ctx, groupID, platform)
	if err != nil {
		return nil
	}
	now := time.Now()
	var cands []openAINearLimitCandidate
	for i := range accounts {
		acc := &accounts[i]
		if excludedIDs != nil {
			if _, excluded := excludedIDs[acc.ID]; excluded {
				continue
			}
		}
		fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, acc, platform, requestedModel, requireCompact, requiredCapability)
		if fresh == nil {
			continue
		}
		cand, ok := classifyOpenAINearLimitCandidate(fresh, now)
		if !ok {
			continue
		}
		if !canClaimOpenAINearLimitPreference(fresh.ID, now, true) {
			continue
		}
		cands = append(cands, cand)
	}
	sortOpenAINearLimitCandidates(cands)
	if len(cands) == 0 {
		return nil
	}
	if sessionHash != "" {
		_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, cands[0].account.ID, openaiStickySessionTTL)
	}
	logOpenAINearLimitPrioritized(cands[0])
	return cands[0].account
}

// trySelectOpenAINearLimitTarget is the GatewayService near-limit path.
// It uses the database Concurrency value and does not check probe backoff,
// matching the documented two-path difference.
func (s *GatewayService) trySelectOpenAINearLimitTarget(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	platform string,
) *Account {
	if skipOpenAINearLimitAccounts(ctx) {
		return nil
	}
	if platform != PlatformOpenAI {
		return nil
	}
	forcePlatform, hasForcePlatform := ctx.Value(ctxkey.ForcePlatform).(string)
	if hasForcePlatform && forcePlatform == "" {
		hasForcePlatform = false
	}
	accounts, err := s.listSchedulableAccounts(ctx, groupID, platform, hasForcePlatform)
	if err != nil {
		return nil
	}
	now := time.Now()
	var cands []openAINearLimitCandidate
	for i := range accounts {
		acc := &accounts[i]
		if excludedIDs != nil {
			if _, excluded := excludedIDs[acc.ID]; excluded {
				continue
			}
		}
		if !s.isAccountSchedulableForSelection(acc) {
			continue
		}
		if requestedModel != "" && !s.isModelSupportedByAccountWithContext(ctx, acc, requestedModel) {
			continue
		}
		if !s.isAccountSchedulableForQuota(acc) || !s.isAccountSchedulableForWindowCost(ctx, acc, true) || !s.isAccountSchedulableForRPM(ctx, acc, true) {
			continue
		}
		cand, ok := classifyOpenAINearLimitCandidate(acc, now)
		if !ok {
			continue
		}
		cands = append(cands, cand)
	}
	sortOpenAINearLimitCandidates(cands)
	for _, cand := range cands {
		acc := cand.account
		result, err := s.tryAcquireAccountSlot(ctx, acc.ID, acc.Concurrency)
		if err != nil || result == nil || !result.Acquired {
			continue
		}
		// This path does not claim probe backoff/lease. Release the slot immediately
		// because SelectAccountForModelWithExclusions returns *Account, not a selection
		// with a release func. Callers that need a slot acquire again.
		if result.ReleaseFunc != nil {
			result.ReleaseFunc()
		}
		if sessionHash != "" && s.cache != nil {
			_ = s.cache.SetSessionAccountID(ctx, derefGroupID(groupID), sessionHash, acc.ID, openaiStickySessionTTL)
		}
		logOpenAINearLimitPrioritized(cand)
		return acc
	}
	return nil
}
