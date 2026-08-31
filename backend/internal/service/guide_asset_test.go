package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func newGuideAssetStore(t *testing.T) *GuideAssetStore {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	settings := NewSettingService(&guideSettingRepo{values: map[string]string{}}, &config.Config{})
	return NewGuideAssetStore(settings)
}

func TestGuideAssetSaveListOpenDelete(t *testing.T) {
	ctx := context.Background()
	store := newGuideAssetStore(t)

	png, err := store.Save(ctx, "screen shot.png", 4, strings.NewReader("data"))
	require.NoError(t, err)
	require.Equal(t, "screen shot.png", png.Name)
	require.True(t, png.Inline, "an image must be servable inline")
	require.EqualValues(t, 4, png.Size)
	require.Regexp(t, `^[0-9a-f]{32}\.png$`, png.ID, "stored id must be generated, not user-controlled")
	require.Equal(t, "/api/v1/guide/assets/"+png.ID, png.URL)

	// A non-image keeps its name but must never be inline.
	bat, err := store.Save(ctx, "tool.bat", 3, strings.NewReader("rem"))
	require.NoError(t, err)
	require.False(t, bat.Inline)

	// SVG is an image to users but an executable document to browsers, so it
	// must be treated as a download.
	svg, err := store.Save(ctx, "logo.svg", 3, strings.NewReader("<s>"))
	require.NoError(t, err)
	require.False(t, svg.Inline, "svg must not render inline; it would allow stored XSS")

	assets, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, assets, 3)

	path, name, contentType, inline, err := store.Open(ctx, png.ID)
	require.NoError(t, err)
	require.Equal(t, "screen shot.png", name)
	require.Equal(t, "image/png", contentType)
	require.True(t, inline)
	body, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "data", string(body))

	_, _, contentType, inline, err = store.Open(ctx, bat.ID)
	require.NoError(t, err)
	require.Equal(t, "application/octet-stream", contentType)
	require.False(t, inline)

	require.NoError(t, store.Delete(ctx, png.ID))
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "delete must remove the file from disk")
	_, _, _, _, err = store.Open(ctx, png.ID)
	require.Equal(t, "GUIDE_ASSET_NOT_FOUND", infraerrors.Reason(err))

	remaining, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 2)
}

func TestGuideAssetRejectsTraversalAndUnindexedFiles(t *testing.T) {
	ctx := context.Background()
	store := newGuideAssetStore(t)

	for _, id := range []string{
		"../../../etc/passwd",
		"..%2f..%2fetc%2fpasswd",
		"sub/dir.png",
		`..\..\windows\win.ini`,
		"",
		"a\x00.png",
	} {
		_, _, _, _, err := store.Open(ctx, id)
		require.Error(t, err, "id %q must be rejected", id)
		require.Contains(t, []string{"GUIDE_ASSET_ID_INVALID", "GUIDE_ASSET_NOT_FOUND"}, infraerrors.Reason(err))
	}

	// A file present on disk but absent from the index must not be served,
	// so a stray file cannot be reached by guessing its name.
	require.NoError(t, os.MkdirAll(store.baseDir, 0o750))
	stray := filepath.Join(store.baseDir, "deadbeefdeadbeefdeadbeefdeadbeef.png")
	require.NoError(t, os.WriteFile(stray, []byte("x"), 0o640))
	_, _, _, _, err := store.Open(ctx, "deadbeefdeadbeefdeadbeefdeadbeef.png")
	require.Equal(t, "GUIDE_ASSET_NOT_FOUND", infraerrors.Reason(err))
}

func TestGuideAssetEnforcesSizeAndQuota(t *testing.T) {
	ctx := context.Background()
	store := newGuideAssetStore(t)

	_, err := store.Save(ctx, "big.bin", GuideAssetMaxBytes+1, strings.NewReader("x"))
	require.Error(t, err)
	require.Equal(t, "GUIDE_ASSET_TOO_LARGE", infraerrors.Reason(err))

	// A lying Content-Length must not let the body exceed the cap: the writer
	// counts real bytes rather than trusting the declared size.
	oversized := strings.Repeat("a", GuideAssetMaxBytes+64)
	_, err = store.Save(ctx, "lying.bin", 8, strings.NewReader(oversized))
	require.Error(t, err)
	require.Equal(t, "GUIDE_ASSET_TOO_LARGE", infraerrors.Reason(err))

	// The failed upload must leave nothing behind.
	assets, err := store.List(ctx)
	require.NoError(t, err)
	require.Empty(t, assets)
	entries, err := os.ReadDir(store.baseDir)
	if err == nil {
		require.Empty(t, entries, "a rejected upload must not leave a partial file")
	}
}

func TestSanitizeGuideAssetName(t *testing.T) {
	require.Equal(t, "report.pdf", sanitizeGuideAssetName(`C:\Users\admin\report.pdf`))
	require.Equal(t, "report.pdf", sanitizeGuideAssetName("/tmp/report.pdf"))
	require.Equal(t, "passwd", sanitizeGuideAssetName("../../../etc/passwd"))
	require.Equal(t, "截图.png", sanitizeGuideAssetName("  截图.png  "))
	require.Equal(t, "", sanitizeGuideAssetName(""))
	require.Equal(t, "", sanitizeGuideAssetName("..."))
	require.Equal(t, "ab.png", sanitizeGuideAssetName("a\x00b.png"))

	long := strings.Repeat("n", 300) + ".png"
	got := sanitizeGuideAssetName(long)
	require.LessOrEqual(t, len(got), GuideAssetMaxNameLength)
	require.True(t, strings.HasSuffix(got, ".png"), "the extension must survive truncation")
}
