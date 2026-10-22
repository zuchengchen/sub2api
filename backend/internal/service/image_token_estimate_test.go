package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func noisyPNGBase64ForTest(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(3))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestEstimateInlineImageTokens(t *testing.T) {
	data := noisyPNGBase64ForTest(t, 700, 900)
	require.Greater(t, len(data), 1_000_000)
	require.Equal(t, int64(840), estimateInlineImageTokens(data))
	require.Equal(t, int64(840), estimateInlineImageTokens("data:image/png;base64,"+data))
	require.Equal(t, imageTokenFallback, estimateInlineImageTokens("UklGRiQAAABXRUJQVlA4IA=="))
	require.Equal(t, imageTokenMinimum, imageTokensForDimensions(10, 10))
	require.LessOrEqual(t, imageTokensForDimensions(8000, 8000), int64(imageTokenMaxPixels/imageTokenPixelDivisor+1))
}

func TestCodexSynthesisEstimatedTokensChargesImagesByPixels(t *testing.T) {
	data := noisyPNGBase64ForTest(t, 700, 900)
	responsesItem := map[string]any{
		"type": "message", "role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": "describe"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + data},
		},
	}
	claudeItem := json.RawMessage(`{"type":"message","role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}]}`)
	got := codexSynthesisEstimatedTokens([]any{responsesItem, claudeItem})
	require.Less(t, got, 3000, "naive len/4 would be ~%d", len(data)/2)
	require.Greater(t, got, 1680)

	// Context-window budget no longer rejects a small conversation with screenshots.
	_, _, err := codexSynthesisTrimToBudget([]any{responsesItem, claudeItem}, 200_000, nil)
	require.NoError(t, err)

	text := map[string]any{"type": "message", "role": "user", "content": "data: plain text"}
	raw, _ := json.Marshal(text)
	require.Equal(t, len(raw)/4+1, codexSynthesisEstimatedTokens([]any{text}))
}
