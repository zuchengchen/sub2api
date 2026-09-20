package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeIntelligentTestHTMLWrapsSVGAndStripsScript(t *testing.T) {
	t.Parallel()
	html, err := SanitizeIntelligentTestHTML(`<svg viewBox="0 0 10 10"><animate attributeName="x" values="0;1"/><script>alert(1)</script></svg>`)
	require.NoError(t, err)
	require.Contains(t, html, "<html")
	require.Contains(t, html, "<svg")
	require.Contains(t, html, "<animate")
	require.NotContains(t, html, "<script")
	require.NotContains(t, html, "alert(1)")
}

func TestSanitizeIntelligentTestHTMLKeepsDocumentAndDropsHandlers(t *testing.T) {
	t.Parallel()
	html, err := SanitizeIntelligentTestHTML(`<!DOCTYPE html><html><body onload="alert(1)"><svg viewBox="0 0 1 1"></svg><iframe src="https://evil.example"></iframe></body></html>`)
	require.NoError(t, err)
	require.Contains(t, html, "<svg")
	require.NotContains(t, html, "onload")
	require.NotContains(t, html, "iframe")
	require.NotContains(t, html, "evil.example")
}

func TestPelicanTestGroupNameUsesGPTPro(t *testing.T) {
	t.Parallel()
	require.Empty(t, pelicanTestGroupName(nil))
	require.Equal(t, "GPT-PRO", pelicanTestGroupName(&Account{Groups: []*Group{{Name: "other"}, {Name: "GPT-PRO"}}}))
	require.Empty(t, pelicanTestGroupName(&Account{Groups: []*Group{{Name: "default"}}}))
}
