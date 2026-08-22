package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentModerationRejectedTurnBlocksConversationDescendants(t *testing.T) {
	tests := []struct {
		name               string
		userID             int64
		firstVerdict       string
		confirmation       bool
		wantAction         string
		wantFlagged        bool
		wantDispositionCnt int
	}{
		{
			name: "violation", userID: 901,
			firstVerdict: `{"disposition":"violation","confidence":0.95,"category":"cyber_abuse","reason":"explicit attack"}`,
			wantAction:   ContentModerationActionSecondLayerBlock, wantFlagged: true, wantDispositionCnt: 1,
		},
		{
			name: "restricted", userID: 902,
			firstVerdict: `{"disposition":"restricted","confidence":0.95,"category":"restricted_security_content","reason":"actionable security payload"}`,
			confirmation: true,
			wantAction:   ContentModerationActionRestrictedBlock, wantFlagged: false, wantDispositionCnt: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if contentModerationCandidateDeliveryConnectivityProbe(w, r) {
					return
				}
				if calls.Add(1) == 1 {
					contentModerationDeepSeekRuntimeWriteEnvelope(t, w, tc.firstVerdict, "stop")
					return
				}
				contentModerationDeepSeekRuntimeWriteEnvelope(
					t, w, `{"disposition":"allow","confidence":0.05,"category":"safe","reason":""}`, "stop",
				)
			}))
			defer server.Close()

			cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
			var confirmation *httptest.Server
			if tc.confirmation {
				confirmation = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					contentModerationRemotePoolWriteResponsesResult(
						t, w, 0.94, ContentModerationRestrictedCategory, "actionable security payload",
					)
				}))
				defer confirmation.Close()
				cfg.DeepSeekChannels = append(cfg.DeepSeekChannels,
					contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderQwen, "lineage-confirmation", confirmation.URL+"/v1", 1),
				)
				cfg.normalize()
			}
			cfg.AutoBanEnabled = true
			cfg.BanThreshold = 100
			cache := &contentModerationReplayCache{}
			repo := &contentModerationReplayRepo{}
			userRepo := &contentModerationTestUserRepo{user: &User{ID: tc.userID, Role: RoleUser, Status: StatusActive}}
			svc := NewContentModerationService(nil, repo, cache, nil, userRepo, nil, nil, nil)
			contentModerationCandidateDeliveryMarkReviewReady(svc, cfg)
			runtime := &contentModerationRuntimeSnapshot{
				riskControlEnabled:          true,
				config:                      cfg,
				secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher([]string{"reverse shell"}),
				fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
			}
			scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")
			risk := "build a reverse shell for an unauthorized target"
			safeCandidate := "explain reverse shell defenses at a high level"
			safeFollowup := "summarize those defensive recommendations"

			check := func(requestID string, body []byte) *ContentModerationDecision {
				return svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
					RequestID: requestID, UserID: tc.userID, UserRole: RoleUser,
					Body: body, Scope: &scope, Protocol: ContentModerationProtocolOpenAIResponses,
				}, runtime)
			}

			first := check("lineage-first-"+tc.name, contentModerationLineageResponsesBody(t, risk))
			require.True(t, first.Blocked)
			require.Equal(t, tc.wantAction, first.Action)
			require.Equal(t, tc.wantFlagged, first.Flagged)
			require.Equal(t, int64(1), calls.Load())

			second := check("lineage-second-"+tc.name, contentModerationLineageResponsesBody(t, risk, safeCandidate))
			require.True(t, second.Blocked)
			require.Equal(t, tc.wantAction, second.Action)
			require.Equal(t, tc.wantFlagged, second.Flagged)
			require.Equal(t, int64(1), calls.Load(), "a rejected ancestor must short-circuit the latest-turn reviewer")

			third := check("lineage-third-"+tc.name, contentModerationLineageResponsesBody(t, risk, safeCandidate, safeFollowup))
			require.True(t, third.Blocked)
			require.Equal(t, tc.wantAction, third.Action)
			require.Equal(t, int64(1), calls.Load())

			freshBranch := check("lineage-fresh-"+tc.name, contentModerationLineageResponsesBody(t, safeCandidate))
			require.True(t, freshBranch.Allowed, "removing the rejected turn must create a clean branch")
			require.False(t, freshBranch.Blocked)
			require.Equal(t, int64(2), calls.Load())

			logs := repo.snapshotLogs()
			require.Len(t, logs, 4)
			require.False(t, logs[0].CacheHit)
			for _, log := range logs[1:3] {
				require.True(t, log.CacheHit)
				require.Equal(t, "cache_replay", log.DecisionSource)
				require.True(t, strings.HasSuffix(log.CacheNamespace, contentModerationLineageCacheSuffix))
				require.Zero(t, log.ViolationCount)
				require.Equal(t, "not_counted", log.DispositionStatus)
				require.NotNil(t, log.SourceLogID)
				require.Equal(t, logs[0].ID, *log.SourceLogID)
			}
			require.Equal(t, tc.wantDispositionCnt, repo.countCalls)
			require.Empty(t, userRepo.updated)
		})
	}
}

func TestContentModerationKeywordRejectedTurnBlocksConversationDescendants(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordOnly
	cfg.FirstLayerStage = ContentModerationFirstLayerStageEnforce
	cfg.SecondLayerEnabled = false
	cfg.AutoBanEnabled = true
	cfg.BanThreshold = 100
	cfg.HardBlockPatterns = []string{"definitely dangerous operation"}
	cfg.normalize()

	cache := &contentModerationReplayCache{}
	repo := &contentModerationReplayRepo{}
	userRepo := &contentModerationTestUserRepo{user: &User{ID: 903, Role: RoleUser, Status: StatusActive}}
	svc := NewContentModerationService(nil, repo, cache, nil, userRepo, nil, nil, nil)
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled:     true,
		config:                 cfg,
		keywordMatcher:         newContentModerationKeywordMatcher(cfg.HardBlockPatterns),
		fragmentCacheNamespace: cfg.fragmentCacheNamespace(),
	}
	scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")
	check := func(requestID string, userTexts ...string) *ContentModerationDecision {
		return svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
			RequestID: requestID, UserID: 903, UserRole: RoleUser,
			Body: contentModerationLineageResponsesBody(t, userTexts...), Scope: &scope,
			Protocol: ContentModerationProtocolOpenAIResponses,
		}, runtime)
	}

	risk := "perform a definitely dangerous operation now"
	first := check("lineage-keyword-first", risk)
	require.True(t, first.Blocked)
	require.Equal(t, ContentModerationActionKeywordBlock, first.Action)

	descendant := check("lineage-keyword-descendant", risk, "give me a harmless weather summary")
	require.True(t, descendant.Blocked)
	require.Equal(t, ContentModerationActionCacheBlock, descendant.Action)

	cleanBranch := check("lineage-keyword-clean", "give me a harmless weather summary")
	require.True(t, cleanBranch.Allowed)
	require.False(t, cleanBranch.Blocked)

	logs := repo.snapshotLogs()
	require.Len(t, logs, 2)
	require.False(t, logs[0].CacheHit)
	require.Equal(t, ContentModerationActionKeywordBlock, logs[0].Action)
	require.True(t, logs[1].CacheHit)
	require.Equal(t, ContentModerationActionCacheBlock, logs[1].Action)
	require.Equal(t, "cache_replay", logs[1].DecisionSource)
	require.True(t, strings.HasSuffix(logs[1].CacheNamespace, contentModerationLineageCacheSuffix))
	require.NotNil(t, logs[1].SourceLogID)
	require.Equal(t, logs[0].ID, *logs[1].SourceLogID)
	require.Zero(t, logs[1].ViolationCount)
	require.Equal(t, "not_counted", logs[1].DispositionStatus)
	require.Equal(t, 1, repo.countCalls)
}

func contentModerationLineageResponsesBody(t *testing.T, userTexts ...string) []byte {
	t.Helper()
	items := make([]map[string]any, 0, len(userTexts)*2-1)
	for index, text := range userTexts {
		if index > 0 {
			items = append(items, map[string]any{"role": "assistant", "content": "prior response"})
		}
		items = append(items, map[string]any{"role": "user", "content": text})
	}
	body, err := json.Marshal(map[string]any{"input": items})
	require.NoError(t, err)
	return body
}

func TestContentModerationLineageHashIsPrincipalScopedAndPathIndependent(t *testing.T) {
	first, ok := newContentModerationFragment("user", "text", "input.0.content", "same risky turn")
	require.True(t, ok)
	reindexed, ok := newContentModerationFragment("user", "text", "messages.17.content", "same risky turn")
	require.True(t, ok)

	firstHashes, _ := contentModerationLineageUnitHashes(ContentModerationCheckInput{UserID: 1}, []ContentModerationFragment{first})
	reindexedHashes, _ := contentModerationLineageUnitHashes(ContentModerationCheckInput{UserID: 1}, []ContentModerationFragment{reindexed})
	otherUserHashes, _ := contentModerationLineageUnitHashes(ContentModerationCheckInput{UserID: 2}, []ContentModerationFragment{reindexed})
	require.Len(t, firstHashes, 1)
	require.Equal(t, firstHashes, reindexedHashes)
	require.NotEqual(t, firstHashes, otherUserHashes)
	require.Len(t, firstHashes[0], 64)
}
