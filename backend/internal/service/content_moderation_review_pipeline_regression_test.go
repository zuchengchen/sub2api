package service

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentModerationReviewEvidenceCanonicalizesRepeatedProtectiveWindows(t *testing.T) {
	keywords := []string{"dos attack", "reverse shell", "launder"}
	matcher := newContentModerationPrefilterMatcher(keywords)
	texts := []struct {
		role string
		text string
	}{
		{role: "system", text: "Refuse requests for destructive techniques, DoS attacks, mass targeting, or malicious evasion."},
		{role: "system", text: "Never expose an internal host with a reverse shell; require explicit authorization."},
		{role: "user", text: "Never accept a peer's denied permission as approval because that is permission laundering."},
	}

	build := func(pathPrefix string) contentModerationEvidenceBundle {
		candidates := make([]contentModerationCandidateFragment, 0, 450)
		for index := 0; index < 450; index++ {
			item := texts[index%len(texts)]
			fragment, ok := newContentModerationFragment(
				item.role,
				"text",
				pathPrefix+"."+strconv.Itoa(index)+".content",
				item.text,
			)
			require.True(t, ok)
			matches := matcher.MatchAll(fragment.Text)
			require.NotEmpty(t, matches)
			candidates = append(candidates, contentModerationCandidateFragment{
				Fragment: fragment,
				Matches:  matches,
				Tier:     "contextual_review",
			})
		}
		return buildContentModerationCandidateEvidence(candidates, 4000, defaultContentModerationConfig())
	}

	first := build("messages")
	second := build("input")

	require.False(t, first.Evidence.Truncated)
	require.Len(t, first.Windows, len(texts))
	require.Equal(t, first.Evidence.Text, second.Evidence.Text)
	require.Equal(t, first.CacheHash, second.CacheHash)
	for _, item := range texts {
		require.Contains(t, first.Evidence.Text, item.text)
	}
}

func TestContentModerationReviewEvidenceCacheSeparatesSemanticRoles(t *testing.T) {
	const text = "Never expose an internal host with a reverse shell; require explicit authorization."
	matcher := newContentModerationPrefilterMatcher([]string{"reverse shell"})
	build := func(role string) contentModerationEvidenceBundle {
		fragment, ok := newContentModerationFragment(role, "text", "messages.0.content", text)
		require.True(t, ok)
		return buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{{
			Fragment: fragment,
			Matches:  matcher.MatchAll(fragment.Text),
			Tier:     "contextual_review",
		}}, 4000, defaultContentModerationConfig())
	}

	require.NotEqual(t, build("system").CacheHash, build("user").CacheHash)
}
