package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Temporary live-review harness: replays the candidate windows of archived
// body 234924 through the REAL remote reviewer pool (DeepSeek official,
// consensus=1) using the deployment key ring.
func TestZZLiveReviewBody234924(t *testing.T) {
	for _, path := range []string{"/tmp/opencode/cm_config_new.json", "/tmp/opencode/keyring.json"} {
		if _, err := os.Stat(path); err != nil {
			t.Skip("live review fixtures not present")
		}
	}
	rawCfg, err := os.ReadFile("/tmp/opencode/cm_config_new.json")
	require.NoError(t, err)
	var cfg ContentModerationConfig
	require.NoError(t, json.Unmarshal(rawCfg, &cfg))
	cfg.Enabled = true
	cfg.AutoBanEnabled = false
	cfg.EmailOnHit = false

	// Cost control: single official channel is enough under consensus=1.
	channels := make([]ContentModerationDeepSeekChannel, 0, 1)
	for _, ch := range cfg.DeepSeekChannels {
		if ch.ID == "deepseek-official" {
			channels = append(channels, ch)
		}
	}
	require.Len(t, channels, 1)

	keyRing := NewContentModerationArchiveKeyRingFile("/tmp/opencode/keyring.json")
	cipher := NewContentModerationCredentialCipher(keyRing)
	key, err := cipher.DecryptDeepSeekAPIKey(channels[0].ID, channels[0].APIKeyEnvelope)
	require.NoError(t, err)
	channels[0].APIKey = key
	cfg.DeepSeekChannels = channels

	body, err := os.ReadFile("/tmp/opencode/sim/body_234924.json")
	require.NoError(t, err)

	keywords, err := effectiveContentModerationKeywords(&cfg)
	require.NoError(t, err)
	candidateKeywords, err := effectiveContentModerationSecondLayerKeywords(&cfg)
	require.NoError(t, err)
	prefilter := newContentModerationPrefilterMatcher(candidateKeywords)
	keywordMatcher := newContentModerationKeywordMatcher(keywords)
	unconditionalMatcher, _, _ := newContentModerationRuntimeKeywordMatchers(keywords)

	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled:          true,
		config:                      &cfg,
		keywordMatcher:              keywordMatcher,
		unconditionalKeywordMatcher: unconditionalMatcher,
		secondLayerPrefilterMatcher: prefilter,
		fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
	}

	allFragments := ExtractContentModerationFragments(ContentModerationProtocolOpenAIResponses, body)
	current, _ := partitionContentModerationReviewFragments(allFragments)
	scopes := buildContentModerationFragmentScopes(
		current, keywordMatcher, prefilter, cfg.KeywordAllowlist,
		contentModerationReviewInputLimit(&cfg),
	)
	checkFrags := buildContentModerationCheckFragments(current, scopes)
	boundary := buildContentModerationCrossMessageBoundaryFragments(current, keywordMatcher, prefilter, cfg.KeywordAllowlist)
	checkFrags = append(boundary, checkFrags...)

	// Mirror the unified loop's candidate collection.
	type candKey struct {
		hash  string
		tier  string
		whole bool
	}
	var candidates []contentModerationCandidateFragment
	seen := map[candKey]bool{}
	add := func(cand contentModerationCandidateFragment) {
		k := candKey{cand.Fragment.Hash, cand.Tier, cand.WholeFragment}
		if seen[k] {
			return
		}
		seen[k] = true
		candidates = append(candidates, cand)
	}
	for _, cf := range checkFrags {
		fragment := cf.Fragment
		keyword, _, reviewMatches := classifyUnifiedHardKeywordMatches(fragment, runtime)
		if keyword != "" {
			continue // would be a local hard block before Layer 2
		}
		if len(reviewMatches) > 0 {
			tier := contentModerationKeywordTierContextualReview
			add(contentModerationCandidateFragment{Fragment: fragment, Matches: reviewMatches, Tier: tier, WholeFragment: true, WholeFragmentTruncated: cf.WholeFragmentTruncated})
			continue
		}
		cand, ok := contentModerationCandidateReviewFragment(fragment)
		if !ok {
			continue
		}
		matches := prefilter.MatchAllExcluding(cand.Text, cfg.KeywordAllowlist)
		if len(matches) == 0 {
			continue
		}
		tier := "candidate"
		if cf.WholeFragmentTruncated {
			tier = contentModerationKeywordTierContextualReview
		}
		add(contentModerationCandidateFragment{
			Fragment: cand, Matches: matches, Tier: tier,
			WholeFragment: cf.WholeFragment || contentModerationPreserveWholeUserFragment(cand),
			WholeFragmentTruncated: cf.WholeFragmentTruncated,
		})
	}
	t.Logf("candidates collected: %d", len(candidates))

	repo := &contentModerationReplayRepo{}
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)

	limit := contentModerationReviewInputLimit(&cfg)
	anyBlock := false
	seenEvidence := map[string]bool{}
	for i, cand := range candidates {
		bundle := buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{cand}, limit, &cfg)
		evHash := sha256.Sum256([]byte(bundle.Evidence.Text))
		evKey := hex.EncodeToString(evHash[:])
		if seenEvidence[evKey] {
			t.Logf("[%2d] duplicate evidence window (%s…), skipped", i+1, evKey[:12])
			continue
		}
		seenEvidence[evKey] = true

		primary := cand.Fragment
		primary.Text = bundle.Evidence.Text
		result, attempted, err := svc.scanUnifiedSecondLayerPrepared(context.Background(), &cfg, contentModerationSecondLayerInput{
			Fragment: primary, Evidence: bundle.Evidence,
			KeywordTier: defaultContentModerationString(cand.Tier, "candidate"),
			KeywordRuleID: contentModerationKeywordRuleID(cand.Matches[0].Keyword),
		})
		kws := make([]string, 0, len(cand.Matches))
		for _, m := range cand.Matches {
			kws = append(kws, m.Keyword)
		}
		if err != nil {
			t.Logf("[%2d] path=%s kw=%v ERR attempted=%v err=%v", i+1, cand.Fragment.Path, kws, attempted, err)
			continue
		}
		provider := ""
		model := ""
		ms := 0
		for _, a := range result.ReviewAttempts {
			provider = a.Provider
			model = a.Model
			ms = a.LatencyMS
			break
		}
		verdict := "ALLOW"
		if result.Blocked {
			verdict = "BLOCK"
			anyBlock = true
		}
		t.Logf("[%2d] path=%s kw=%v => %s cat=%q conf=%.2f label=%q provider=%s model=%s %dms consensus=%q reason=%q",
			i+1, cand.Fragment.Path, kws, verdict, result.Category, result.Confidence, result.Label, provider, model, ms, result.ConsensusStatus, truncateSimString(result.Reason, 160))
	}
	if anyBlock {
		t.Logf("FINAL: at least one window BLOCKED by the live reviewer pool")
	} else {
		t.Logf("FINAL: all reviewed windows ALLOWED by the live reviewer pool")
	}
}

func truncateSimString(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
