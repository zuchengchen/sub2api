package basispoints

import (
	"fmt"
	"net/url"
	"strings"
)

// HTTPS image references pass through; this bridge never uploads client image data.
func validateImage(part object) error {
	raw, ok := part["image_url"].(string)
	if !ok || raw == "" {
		return fmt.Errorf("basispoints input_image requires an HTTPS image_url; file IDs are unsupported")
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "data:") {
		return fmt.Errorf("basispoints does not accept data:image/base64 image input; provide an HTTPS image URL, or disable Basispoints and start a new conversation to send this image")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || strings.TrimSpace(raw) != raw {
		return fmt.Errorf("basispoints input_image requires an absolute HTTPS image URL without embedded credentials")
	}
	if fileID := text(part["file_id"]); fileID != "" {
		return fmt.Errorf("basispoints input_image does not support file_id; provide only an HTTPS image_url")
	}
	if detail, exists := part["detail"]; exists && detail != nil {
		switch text(detail) {
		case "auto", "low", "high":
		default:
			return fmt.Errorf("basispoints image detail must be auto, low or high")
		}
	}
	return nil
}
