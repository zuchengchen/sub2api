package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCodexCatalogPinnedHash fails when the embedded models.json is refreshed
// without updating CodexCatalogPinnedHash. Regenerate with
// `go run ./tools/codexprompts -models <path> -write`, which prints the hash.
func TestCodexCatalogPinnedHash(t *testing.T) {
	sum := sha256.Sum256(codexCatalogModelsJSON)
	require.Equal(t, CodexCatalogPinnedHash, hex.EncodeToString(sum[:]))
}

func TestCodexCatalogModelResolution(t *testing.T) {
	model, ok := CodexCatalogModelForSlug("gpt-5.6-sol-max")
	require.True(t, ok)
	require.Equal(t, "gpt-5.6-sol", model.Slug)
	require.NotEmpty(t, model.ModelMessages.InstructionsTemplate)

	_, ok = CodexCatalogModelForSlug("definitely-not-a-codex-model")
	require.False(t, ok)
}
