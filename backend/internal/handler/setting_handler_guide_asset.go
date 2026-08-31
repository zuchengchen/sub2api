package handler

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"

	"github.com/gin-gonic/gin"
)

// GetGuideAsset serves a file uploaded for the usage guide. The guide is a
// public page, so its images must be readable without a session.
//
// Only whitelisted image types are served inline. Everything else is sent as an
// attachment with application/octet-stream and nosniff, which is what prevents
// an uploaded .svg or .html from executing script under our own origin and
// stealing an administrator session.
func (h *SettingHandler) GetGuideAsset(c *gin.Context) {
	if h.guideAssets == nil {
		response.Error(c, http.StatusNotFound, "file was not found")
		return
	}

	path, name, contentType, inline, err := h.guideAssets.Open(c.Request.Context(), c.Param("id"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	disposition := "attachment"
	if inline {
		disposition = "inline"
	}

	// Both a plain filename and the RFC 5987 form are sent so non-ASCII names
	// survive without breaking clients that only read the plain parameter.
	c.Header("Content-Disposition", fmt.Sprintf(
		"%s; filename=%s; filename*=UTF-8''%s",
		disposition,
		asciiFallbackName(name),
		url.PathEscape(name),
	))
	c.Header("Content-Type", contentType)
	c.Header("X-Content-Type-Options", "nosniff")
	// An uploaded file must never be treated as active content for this origin.
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Header("Cache-Control", "public, max-age=86400")
	c.File(path)
}

// asciiFallbackName reduces a display name to a quoted ASCII form for the
// legacy filename parameter.
func asciiFallbackName(name string) string {
	quoted := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r < 32 || r == '"' || r == '\\' || r == 127:
			quoted = append(quoted, '_')
		case r > 127:
			quoted = append(quoted, '_')
		default:
			quoted = append(quoted, r)
		}
	}
	if len(quoted) == 0 {
		return `"file"`
	}
	return `"` + string(quoted) + `"`
}
