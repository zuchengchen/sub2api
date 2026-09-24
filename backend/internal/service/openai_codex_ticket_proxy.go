package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
)

var errOpenAICodexTicketProxySessionUnavailable = errors.New("harvest proxy session could not be generated")

// Escape only the two supported literal placeholders before net/url validates
// the URL. URL-encoded placeholders already parse normally. The parser below
// permits placeholders only in userinfo, never in the destination or routing.
var openAICodexTicketProxyTemplateEscaper = strings.NewReplacer(
	"{session}", "%7Bsession%7D",
	"{SESSION}", "%7BSESSION%7D",
)

func parseOpenAICodexTicketHarvestProxyURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(openAICodexTicketProxyTemplateEscaper.Replace(strings.TrimSpace(raw)))
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || strings.Contains(parsed.Host, "{session}") || strings.Contains(parsed.Host, "{SESSION}") {
		return nil, errors.New("harvest proxy must be an HTTP(S) or SOCKS5(h) URL with a host and no path, query or fragment; session placeholders are allowed only in credentials")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("harvest proxy scheme must be http, https, socks5 or socks5h")
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("harvest proxy port must be between 1 and 65535")
		}
	}
	return parsed, nil
}

// ValidateOpenAICodexTicketHarvestProxyURL validates only syntax, without making
// a network request, resolving a session, or exposing credentials in errors.
func ValidateOpenAICodexTicketHarvestProxyURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	_, err := parseOpenAICodexTicketHarvestProxyURL(raw)
	return err
}

// MaskProxyURL preserves a username template while always hiding the password,
// including passwords which contain templates. Invalid legacy data stays hidden.
func MaskProxyURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	parsed, err := parseOpenAICodexTicketHarvestProxyURL(raw)
	if err != nil {
		return ""
	}
	if parsed.User != nil {
		if _, ok := parsed.User.Password(); ok {
			parsed.User = url.UserPassword(parsed.User.Username(), "***")
		}
	}
	return parsed.String()
}

// IsMaskedProxyURL recognizes the exact password placeholder emitted by the API.
func IsMaskedProxyURL(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	parsed, err := parseOpenAICodexTicketHarvestProxyURL(raw)
	if err != nil || parsed.User == nil {
		return false
	}
	password, ok := parsed.User.Password()
	return ok && password == "***"
}

// Call this immediately before each HTTP probe, not when loading configuration
// or entering a retry loop. Each attempt receives an independent proxy session.
func resolveOpenAICodexTicketHarvestProxyURL(raw string) (string, error) {
	return resolveOpenAICodexTicketHarvestProxyURLWithRandom(raw, rand.Reader)
}

func resolveOpenAICodexTicketHarvestProxyURLWithRandom(raw string, random io.Reader) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw, nil
	}
	parsed, err := parseOpenAICodexTicketHarvestProxyURL(raw)
	if err != nil {
		return "", err
	}
	if parsed.User == nil {
		return raw, nil
	}
	username := parsed.User.Username()
	password, hasPassword := parsed.User.Password()
	hasTemplate := func(value string) bool {
		return strings.Contains(value, "{session}") || strings.Contains(value, "{SESSION}")
	}
	if !hasTemplate(username) && !hasTemplate(password) {
		return raw, nil
	}
	var entropy [4]byte
	if _, err := io.ReadFull(random, entropy[:]); err != nil {
		return "", errOpenAICodexTicketProxySessionUnavailable
	}
	session := hex.EncodeToString(entropy[:])
	replacer := strings.NewReplacer("{session}", session, "{SESSION}", session)
	username = replacer.Replace(username)
	if hasPassword {
		parsed.User = url.UserPassword(username, replacer.Replace(password))
	} else {
		parsed.User = url.User(username)
	}
	return parsed.String(), nil
}
