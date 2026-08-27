package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// cyberSimFragment mirrors one extracted conversation item used by the
// cyber interception replay harness (see tools docs in AGENTS.md notes).
type cyberSimFragment struct {
	Role string `json:"role"`
	Kind string `json:"kind"`
	Path string `json:"path"`
	Text string `json:"text"`
}

// TestCyberInterceptionReplay replays archived conversations that previously
// reached upstream OpenAI and were rejected with error code `cyber_policy`.
// It verifies, using the production config and the production reviewer call
// path, that:
//  1. the second-layer candidate prefilter now routes fragments of each
//     conversation to remote review (the pre-change keyword set missed them),
//  2. at least one enabled remote reviewer returns a blocking verdict
//     (restricted/violation) at or above the configured threshold.
//
// The test is env-gated because it loads real credentials and calls external
// reviewer APIs. Run with:
//
//	SUB2API_CYBER_SIM=1 \
//	CYBER_SIM_CONFIG=/tmp/opencode/cm_config_new.json \
//	CYBER_SIM_KEYRING=/etc/sub2api/secrets/content-moderation-keyring.json \
//	CYBER_SIM_FRAGMENTS=/tmp/opencode/simfrags \
//	go test ./internal/service/ -run TestCyberInterceptionReplay -v
func TestCyberInterceptionReplay(t *testing.T) {
	if os.Getenv("SUB2API_CYBER_SIM") != "1" {
		t.Skip("cyber interception replay only runs when SUB2API_CYBER_SIM=1")
	}
	configPath := os.Getenv("CYBER_SIM_CONFIG")
	keyringPath := os.Getenv("CYBER_SIM_KEYRING")
	fragmentsDir := os.Getenv("CYBER_SIM_FRAGMENTS")
	if configPath == "" || keyringPath == "" || fragmentsDir == "" {
		t.Fatalf("CYBER_SIM_CONFIG, CYBER_SIM_KEYRING and CYBER_SIM_FRAGMENTS are required")
	}
	rawConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg, err := parseContentModerationConfig(string(rawConfig))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	svc := &ContentModerationService{}
	svc.ConfigureContentModerationCredentialKeyRing(keyringPath)
	if err := svc.hydrateContentModerationDeepSeekSecrets(cfg); err != nil {
		t.Fatalf("hydrate channel secrets: %v", err)
	}
	newKeywords, err := effectiveContentModerationSecondLayerKeywords(cfg)
	if err != nil {
		t.Fatalf("effective keywords: %v", err)
	}
	newMatcher := newContentModerationPrefilterMatcher(newKeywords)

	// Rebuild the pre-change candidate set by dropping the keywords that were
	// added for cyber interception, so the replay can show the before/after
	// routing difference on the same fragments.
	cyberKeywords := map[string]bool{
		"frida": true, "valorant": true, "idsibutton9": true,
		"邮箱---密码": true, "windowproxy": true, "poc-windowproxy": true,
		"realm replacement": true,
	}
	oldCfg := cloneContentModerationConfig(cfg)
	oldCustom := make([]string, 0, len(oldCfg.CandidateKeywords))
	for _, keyword := range oldCfg.CandidateKeywords {
		if !cyberKeywords[strings.ToLower(strings.TrimSpace(keyword))] {
			oldCustom = append(oldCustom, keyword)
		}
	}
	oldCfg.CandidateKeywords = oldCustom
	oldKeywords, err := effectiveContentModerationSecondLayerKeywords(oldCfg)
	if err != nil {
		t.Fatalf("effective old keywords: %v", err)
	}
	oldMatcher := newContentModerationPrefilterMatcher(oldKeywords)

	// Layer-1 hard keyword matcher, mirroring the production runtime snapshot.
	layer1Keywords, err := effectiveContentModerationKeywords(cfg)
	if err != nil {
		t.Fatalf("effective layer1 keywords: %v", err)
	}
	_, unconditionalMatcher, _ := newContentModerationRuntimeKeywordMatchers(layer1Keywords)

	entries, err := os.ReadDir(fragmentsDir)
	if err != nil {
		t.Fatalf("read fragments dir: %v", err)
	}
	ctx := context.Background()
	threshold := cfg.DeepSeekThreshold
	if threshold <= 0 {
		threshold = 0.8
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		logID := strings.TrimSuffix(entry.Name(), ".json")
		t.Run("log_"+logID, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(fragmentsDir, entry.Name()))
			if err != nil {
				t.Fatalf("read fragments: %v", err)
			}
			var items []cyberSimFragment
			if err := json.Unmarshal(raw, &items); err != nil {
				t.Fatalf("parse fragments: %v", err)
			}
			if len(items) == 0 {
				t.Fatalf("no fragments for %s", logID)
			}

			routedNew := 0
			routedOld := 0
			type hit struct {
				frag       ContentModerationFragment
				indicators []string
			}
			var hits []hit
			hardBlocked := false
			var hardKeyword string
			for _, item := range items {
				fragment, ok := newContentModerationFragment(item.Role, item.Kind, item.Path, item.Text)
				if !ok {
					continue
				}
				matches := newMatcher.MatchAllExcluding(fragment.Text, cfg.KeywordAllowlist)
				oldMatches := oldMatcher.MatchAllExcluding(fragment.Text, cfg.KeywordAllowlist)
				if len(oldMatches) > 0 {
					routedOld++
				}
				if hardKeyword == "" {
					if kw, ok := unconditionalMatcher.Match(fragment.Text); ok {
						hardBlocked = true
						hardKeyword = kw
					}
				}
				if len(matches) > 0 {
					routedNew++
					indicators := make([]string, 0, len(matches))
					seen := make(map[string]bool, len(matches))
					for _, m := range matches {
						if !seen[m.Keyword] {
							seen[m.Keyword] = true
							indicators = append(indicators, m.Keyword)
						}
					}
					sort.Strings(indicators)
					hits = append(hits, hit{frag: fragment, indicators: indicators})
				}
			}
			if routedNew == 0 && !hardBlocked {
				t.Errorf("prefilter routed %d/%d fragments under NEW keywords (old set: %d); conversation would still bypass review", routedNew, len(items), routedOld)
				return
			}
			if hardBlocked {
				t.Logf("layer-1 hard keyword hit: %q -> immediate pre_block without review", hardKeyword)
			}
			t.Logf("prefilter routing: old keywords %d/%d -> new keywords %d/%d fragments selected for remote review", routedOld, len(items), routedNew, len(items))

			blocked := false
			// Production reviews every routed fragment; here we bound the replay
			// to the most intent-bearing candidates: user goal messages first,
			// then assistant narration, then tool traffic by indicator count.
			priority := func(h hit) int {
				switch h.frag.Role {
				case "user":
					return 0
				case "assistant":
					return 1
				default:
					return 2
				}
			}
			sort.SliceStable(hits, func(i, j int) bool {
				if priority(hits[i]) != priority(hits[j]) {
					return priority(hits[i]) < priority(hits[j])
				}
				return len(hits[i].indicators) > len(hits[j].indicators)
			})
			if len(hits) > 8 {
				hits = hits[:8]
			}
			for _, h := range hits {
				if blocked {
					break
				}
				input := contentModerationSecondLayerInput{
					Fragment:          h.frag,
					Evidence:          moderationEvidence{Text: h.frag.Text, Mode: "whole_fragment"},
					MatchedIndicators: h.indicators,
				}
				result, _, err := svc.scanContentModerationRemotePool(ctx, cfg, input)
				if err != nil {
					t.Logf("fragment[role=%s indicators=%v]: pool unavailable: %v", h.frag.Role, h.indicators, err)
					continue
				}
				disposition := result.normalizedDisposition()
				verdictBlocked := result.Blocked && (disposition == ContentModerationReviewDispositionRestricted || disposition == ContentModerationReviewDispositionViolation)
				t.Logf("fragment[role=%s indicators=%v] disposition=%s category=%s confidence=%.2f blocked=%v",
					h.frag.Role, h.indicators, result.Disposition, result.Category, result.Confidence, verdictBlocked)
				if verdictBlocked {
					blocked = true
					break
				}
			}
			if !blocked && !hardBlocked {
				t.Errorf("no reviewer produced a blocking verdict >= %.2f for log %s and no layer-1 hard hit", threshold, logID)
			} else {
				t.Logf("PASS: log %s would be intercepted in pre_block mode", logID)
			}
		})
	}
}
