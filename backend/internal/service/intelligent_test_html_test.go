package service

import (
	"strings"
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
	require.True(t, strings.HasPrefix(html, "<!DOCTYPE html>"))
	require.Contains(t, html, "<svg")
	require.NotContains(t, html, "onload")
	require.NotContains(t, html, "iframe")
	require.NotContains(t, html, "evil.example")
}

func TestPreviewIntelligentTestHTMLRejectsIncompleteRoots(t *testing.T) {
	t.Parallel()
	for name, output := range map[string]string{
		"unclosed document":            `<html><body><svg></svg></body>`,
		"unclosed SVG":                 `<svg><path d="M0 0"/>`,
		"unclosed nested SVG":          `<svg><svg></svg>`,
		"nested SVG in document":       `<html><body><svg><svg></svg></body></html>`,
		"comment is not SVG close":     `<svg><!-- </svg> -->`,
		"script is not SVG close":      `<svg><script>const close = "</svg>";</script>`,
		"script is not document close": `<html><body><svg></svg><script>const close = "</html>";</script>`,
		"unfinished SVG start":         `<svg viewBox="0 0 10 10"`,
		"unfinished document start":    `<html`,
		"fenced incomplete":            "```html\n<html><svg></svg>\n```",
	} {
		t.Run(name, func(t *testing.T) {
			doc, issue := PreviewIntelligentTestHTML(output)
			require.Empty(t, doc)
			require.Equal(t, "incomplete", issue)
			require.Empty(t, ExtractIntelligentTestHTMLDocument(output))
			_, err := SanitizeIntelligentTestHTML(output)
			require.ErrorIs(t, err, errIntelligentHTMLIncomplete)
		})
	}
}

func TestPreviewIntelligentTestHTMLPreservesFragmentLayout(t *testing.T) {
	t.Parallel()
	fragment := `<style>.scene{width:900px}.bird{fill:orange}</style><section class="scene"><h1>Pelican</h1><svg viewBox="0 0 900 500"><g class="bird"><svg x="5"><circle r="3"/></svg></g></svg><p>A complete scene</p></section><style>.scene p{color:white}</style>`
	doc, issue := PreviewIntelligentTestHTML("Here is the animation:\n```html\n" + fragment + "\n```\nEnd of example.")
	require.Empty(t, issue)
	require.Contains(t, doc, fragment)
	require.True(t, strings.HasPrefix(doc, "<!DOCTYPE html>"))
	require.NotContains(t, doc, "```")
	require.NotContains(t, doc, "Here is the animation")
	require.NotContains(t, doc, "End of example")
}

func TestPreviewIntelligentTestHTMLPreservesFragmentText(t *testing.T) {
	t.Parallel()
	for _, output := range []string{
		`<svg></svg>Visible caption`,
		"```html\nScene caption <svg></svg>Visible caption\n```",
	} {
		doc, issue := PreviewIntelligentTestHTML(output)
		require.Empty(t, issue)
		require.Contains(t, doc, "Visible caption")
		if strings.HasPrefix(output, "```") {
			require.Contains(t, doc, "Scene caption")
			require.NotContains(t, doc, "```")
		}
	}
}

func TestPreviewIntelligentTestHTMLPreservesCompleteDocuments(t *testing.T) {
	t.Parallel()
	for name, output := range map[string]string{
		"doctype retained":        `<!doctype HTML><HTML><body><SVG><SVG /></SVG></body></HTML>`,
		"public doctype retained": `<!doctype html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd"><html><body><svg></svg></body></html>`,
		"doctype added":           `<html><head><style>svg{width:800px}</style></head><body><svg></svg></body></html>`,
	} {
		t.Run(name, func(t *testing.T) {
			doc, issue := PreviewIntelligentTestHTML(output)
			require.Empty(t, issue)
			if strings.HasPrefix(output, "<!doctype") {
				require.Equal(t, output, doc)
			} else {
				require.Equal(t, "<!DOCTYPE html>"+output, doc)
			}
		})
	}
}

func TestPreviewIntelligentTestHTMLReportsRemovedScripts(t *testing.T) {
	t.Parallel()
	for name, output := range map[string]string{
		"script":         `<svg><script>alert(1)</script><animate attributeName="x" values="0;1"/></svg>`,
		"event handler":  `<svg onload="alert(1)"><animate attributeName="x" values="0;1"/></svg>`,
		"JavaScript URL": `<svg><a href="javascript:alert(1)"><animate attributeName="x" values="0;1"/></a></svg>`,
	} {
		t.Run(name, func(t *testing.T) {
			doc, issue := PreviewIntelligentTestHTML(output)
			require.Equal(t, "scripts_removed", issue)
			require.Contains(t, doc, "<animate")
			require.NotContains(t, doc, "<script")
			require.NotContains(t, doc, "onload")
			require.NotContains(t, doc, "javascript:")
		})
	}
}

func TestPreviewIntelligentTestHTMLReportsUnavailable(t *testing.T) {
	t.Parallel()
	for _, output := range []string{
		"No image generated.",
		`<html><body>No SVG</body></html>`,
		`<svg><desc>` + strings.Repeat("x", intelligentPelicanHTMLLimit) + `</desc></svg>`,
	} {
		doc, issue := PreviewIntelligentTestHTML(output)
		require.Empty(t, doc)
		require.Equal(t, "unavailable", issue)
	}
}

func TestPelicanTestGroupNameUsesGPTPro(t *testing.T) {
	t.Parallel()
	require.Empty(t, pelicanTestGroupName(nil))
	require.Equal(t, "GPT-PRO", pelicanTestGroupName(&Account{Groups: []*Group{{Name: "other"}, {Name: "GPT-PRO"}}}))
	require.Empty(t, pelicanTestGroupName(&Account{Groups: []*Group{{Name: "default"}}}))
}
