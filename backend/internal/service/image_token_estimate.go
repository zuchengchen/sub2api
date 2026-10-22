package service

import (
	"encoding/base64"
	"image"
	_ "image/gif"  // register GIF header decoder for image.DecodeConfig
	_ "image/jpeg" // register JPEG header decoder for image.DecodeConfig
	_ "image/png"  // register PNG header decoder for image.DecodeConfig
	"math"
	"strings"

	"github.com/tidwall/gjson"
)

// Byte-length token heuristics must not be applied to inline image data: the
// model bills images by pixel area, not by base64 size.
const (
	estimateBytesPerToken        = 4
	imageTokenPixelDivisor       = 750
	imageTokenMaxLongEdge        = 2576
	imageTokenMaxPixels          = 3_750_000
	imageTokenFallback     int64 = 1600
	imageTokenMinimum      int64 = 85
)

// estimateInlineImageTokens approximates one inline image's input tokens from
// base64 data or a data: URL. Only the image header is decoded.
func estimateInlineImageTokens(data string) int64 {
	data = strings.TrimSpace(data)
	if strings.HasPrefix(data, "data:") {
		if idx := strings.IndexByte(data, ','); idx >= 0 {
			data = data[idx+1:]
		}
	}
	if data == "" {
		return 0
	}
	trimmed := strings.TrimRight(data, "=")
	if rem := len(trimmed) % 4; rem != 0 {
		trimmed += strings.Repeat("=", 4-rem)
	}
	cfg, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(trimmed)))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return imageTokenFallback
	}
	return imageTokensForDimensions(cfg.Width, cfg.Height)
}

func imageTokensForDimensions(width, height int) int64 {
	w, h := float64(width), float64(height)
	scale := 1.0
	if long := math.Max(w, h); long > imageTokenMaxLongEdge {
		scale = imageTokenMaxLongEdge / long
	}
	if pixels := w * h * scale * scale; pixels > imageTokenMaxPixels {
		scale *= math.Sqrt(imageTokenMaxPixels / pixels)
	}
	tokens := int64(w * scale * h * scale / imageTokenPixelDivisor)
	if tokens < imageTokenMinimum {
		tokens = imageTokenMinimum
	}
	return tokens
}

// estimateJSONTokensWithImages applies len/4 to a JSON value while charging
// inline images (data:image/... URLs, or base64 "data" next to an image
// media_type/mime_type) by pixel area.
func estimateJSONTokensWithImages(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	imageBytes, imageTokens := scanJSONInlineImages(gjson.ParseBytes(raw))
	textBytes := int64(len(raw)) - imageBytes
	if textBytes < 0 {
		textBytes = 0
	}
	return int(textBytes/estimateBytesPerToken + imageTokens)
}

func scanJSONInlineImages(node gjson.Result) (imageBytes, imageTokens int64) {
	if !node.IsArray() && !node.IsObject() {
		return 0, 0
	}
	mediaType := ""
	if node.IsObject() {
		for _, key := range []string{"media_type", "mime_type", "mimeType"} {
			if v := node.Get(key); v.Type == gjson.String {
				mediaType = strings.ToLower(v.String())
				break
			}
		}
	}
	sourceIsBase64 := node.IsObject() && node.Get("type").String() == "base64"
	node.ForEach(func(key, child gjson.Result) bool {
		if child.Type == gjson.String {
			value := child.String()
			if strings.HasPrefix(value, "data:image/") || (key.String() == "data" && strings.HasPrefix(mediaType, "image/") && value != "") {
				imageBytes += int64(len(child.Raw))
				imageTokens += estimateInlineImageTokens(value)
				return true
			}
			// Other inline base64 payloads (PDF input_file.file_data, data:
			// URLs of any type, Anthropic base64 document sources) are billed
			// by their decoded content, not their base64 length. Charge a
			// capped flat estimate so a large PDF cannot trip the budget.
			if isInlineBase64Blob(key.String(), value, sourceIsBase64) {
				imageBytes += int64(len(child.Raw))
				imageTokens += inlineBlobTokens(len(value))
			}
			return true
		}
		b, t := scanJSONInlineImages(child)
		imageBytes += b
		imageTokens += t
		return true
	})
	return imageBytes, imageTokens
}

const inlineBlobTokenCap int64 = imageTokenFallback

func isInlineBase64Blob(key, value string, sourceIsBase64 bool) bool {
	if len(value) < 256 {
		return false
	}
	if strings.HasPrefix(value, "data:") && strings.Contains(value[:min(len(value), 128)], ";base64,") {
		return true
	}
	switch key {
	case "file_data":
		return true
	case "data":
		return sourceIsBase64
	}
	return false
}

func inlineBlobTokens(size int) int64 {
	tokens := int64(size / estimateBytesPerToken)
	if tokens > inlineBlobTokenCap {
		return inlineBlobTokenCap
	}
	return tokens
}
