package service

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	grokVidgenHost        = "vidgen.x.ai"
	grokImgenHost         = "imgen.x.ai"
	grokMediaVidgenPrefix = "/v1/media/vidgen"
	grokMediaImgenPrefix  = "/v1/media/imgen"
)

var grokXAIAPIHosts = map[string]struct{}{
	"api.x.ai":     {},
	"auth.x.ai":    {},
	"docs.x.ai":    {},
	"console.x.ai": {},
	"grok.x.ai":    {},
}

// grokMediaPublicOrigin is the scheme+host Grok CLI will GET next.
// Host comes from the inbound request (nginx sets Host $host). X-Forwarded-Host
// is ignored so a client cannot mint a URL on an attacker domain.
func grokMediaPublicOrigin(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	host := strings.TrimSpace(c.Request.Host)
	if host == "" {
		return ""
	}
	scheme := "https"
	if proto := firstForwardedProto(c.GetHeader("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	} else if c.Request.URL != nil {
		switch strings.ToLower(strings.TrimSpace(c.Request.URL.Scheme)) {
		case "http", "https":
			scheme = strings.ToLower(c.Request.URL.Scheme)
		}
	}
	return scheme + "://" + host
}

func firstForwardedProto(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if i := strings.Index(raw, ","); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "http" || raw == "https" {
		return raw
	}
	return ""
}

func originFromAbsoluteURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return ""
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme != "http" && scheme != "https" {
		return ""
	}
	return scheme + "://" + parsed.Host
}

func rewriteGrokMediaCDNURLStrings(body []byte, publicOrigin string) []byte {
	if len(body) == 0 || strings.TrimSpace(publicOrigin) == "" {
		return body
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return body
	}
	if !rewriteGrokMediaCDNURLValue(&value, publicOrigin) {
		return body
	}
	rewritten, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return rewritten
}

func rewriteGrokMediaCDNURLValue(value *any, publicOrigin string) bool {
	if value == nil || strings.TrimSpace(publicOrigin) == "" {
		return false
	}
	switch typed := (*value).(type) {
	case map[string]any:
		changed := false
		for key, child := range typed {
			childValue := child
			if rewriteGrokMediaCDNURLValue(&childValue, publicOrigin) {
				typed[key] = childValue
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for index, child := range typed {
			childValue := child
			if rewriteGrokMediaCDNURLValue(&childValue, publicOrigin) {
				typed[index] = childValue
				changed = true
			}
		}
		return changed
	case string:
		if rewritten, ok := rewriteOfficialXAIMediaURL(typed, publicOrigin); ok {
			*value = rewritten
			return true
		}
	}
	return false
}

func rewriteOfficialXAIMediaURL(rawURL, publicOrigin string) (string, bool) {
	publicOrigin = strings.TrimRight(strings.TrimSpace(publicOrigin), "/")
	if publicOrigin == "" {
		return "", false
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.User != nil {
		return "", false
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", false
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return "", false
	}
	if !isRewritableXAIMediaHost(parsed.Hostname()) {
		return "", false
	}
	prefix := grokMediaProxyPrefix(parsed.Hostname())
	if prefix == "" {
		return "", false
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	object := strings.TrimPrefix(path, "/")
	switch strings.ToLower(parsed.Hostname()) {
	case grokVidgenHost:
		if !IsAllowedGrokVidgenObject(object) {
			return "", false
		}
	case grokImgenHost:
		if !IsAllowedGrokImgenObject(object) {
			return "", false
		}
	}
	out := publicOrigin + prefix + path
	if parsed.RawQuery != "" {
		out += "?" + parsed.RawQuery
	}
	return out, true
}

func grokMediaProxyPrefix(host string) string {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case grokVidgenHost:
		return grokMediaVidgenPrefix
	case grokImgenHost:
		return grokMediaImgenPrefix
	default:
		return ""
	}
}

func isRewritableXAIMediaHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == grokVidgenHost || host == grokImgenHost {
		return true
	}
	if !strings.HasSuffix(host, ".x.ai") {
		return false
	}
	if _, skip := grokXAIAPIHosts[host]; skip {
		return false
	}
	return false
}

func isGrokMediaCDNProxyURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Path == "" {
		return false
	}
	path := parsed.EscapedPath()
	return strings.HasPrefix(path, grokMediaVidgenPrefix+"/") ||
		strings.HasPrefix(path, grokMediaImgenPrefix+"/") ||
		path == grokMediaVidgenPrefix ||
		path == grokMediaImgenPrefix
}

// IsAllowedGrokVidgenObject reports whether a /v1/media/vidgen/* path may be
// fetched from vidgen.x.ai. Only the official bucket is allowed.
func IsAllowedGrokVidgenObject(object string) bool {
	object = strings.TrimSpace(strings.TrimPrefix(object, "/"))
	if object == "" || strings.ContainsAny(object, `\\@`) {
		return false
	}
	decoded, err := url.PathUnescape(object)
	if err != nil {
		return false
	}
	decoded = strings.TrimPrefix(decoded, "/")
	if decoded == "" || strings.Contains(decoded, "..") || strings.Contains(decoded, "\\") {
		return false
	}
	if !strings.HasPrefix(decoded, "xai-vidgen-bucket/") {
		return false
	}
	rest := strings.TrimPrefix(decoded, "xai-vidgen-bucket/")
	if rest == "" || strings.ContainsAny(rest, "/\\") {
		return false
	}
	if len(rest) > 200 {
		return false
	}
	for _, r := range rest {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return strings.HasSuffix(strings.ToLower(rest), ".mp4")
}

// IsAllowedGrokImgenObject allows only relative paths on the hardcoded imgen host.
func IsAllowedGrokImgenObject(object string) bool {
	object = strings.TrimSpace(strings.TrimPrefix(object, "/"))
	if object == "" || strings.ContainsAny(object, `\\@:`) {
		return false
	}
	decoded, err := url.PathUnescape(object)
	if err != nil {
		return false
	}
	decoded = strings.TrimPrefix(decoded, "/")
	if decoded == "" || strings.Contains(decoded, "..") || strings.ContainsAny(decoded, `\\:@`) {
		return false
	}
	if strings.Contains(decoded, "://") {
		return false
	}
	for _, r := range decoded {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' || r == '/' {
			continue
		}
		return false
	}
	return true
}
