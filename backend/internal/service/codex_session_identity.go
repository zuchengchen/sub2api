package service

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const codexSessionIdentityIdleTTL = 24 * time.Hour
const codexSessionIdentityLimit = 8192

type codexSessionIdentitySnapshot struct {
	identity codexOutboundIdentity
	profile  codexWireProfile
	lastUsed time.Time
	cacheKey string
}

var codexSessionIdentities = struct {
	sync.Mutex
	entries map[string]codexSessionIdentitySnapshot
}{entries: make(map[string]codexSessionIdentitySnapshot)}

// Resolve this before rewriting client IDs. The context snapshot is reused by
// later header builders, including WS reconnects with already rewritten bodies.
func (s *OpenAIGatewayService) codexSessionIdentity(c *gin.Context, account *Account, rawBody []byte) codexSessionIdentitySnapshot {
	source := codexAccountIdentitySource(c, account)
	namespace := codexAccountIdentityNamespace(source)
	if namespace == "" && source != nil {
		namespace = fmt.Sprintf("account:%d", source.ID)
	}
	namespace = fmt.Sprintf("%s:key:%d", namespace, getAPIKeyIDFromContext(c))
	contextKey := "codex_session_identity:" + namespace
	if c != nil {
		if cached, ok := c.Get(contextKey); ok {
			if snapshot, ok := cached.(codexSessionIdentitySnapshot); ok {
				if snapshot.cacheKey != "" {
					codexSessionIdentities.Lock()
					if entry, exists := codexSessionIdentities.entries[snapshot.cacheKey]; exists {
						entry.lastUsed = time.Now()
						codexSessionIdentities.entries[snapshot.cacheKey] = entry
					}
					codexSessionIdentities.Unlock()
				}
				return snapshot
			}
		}
	}
	var headers http.Header
	if c != nil && c.Request != nil {
		headers = c.Request.Header
	}
	parsed := openAIRequestPayloadView(rawBody)
	evidence := collectCodexTurnEvidence(parsed.Get("client_metadata"), headers, parsed)
	// A cache key or a prompt is not proof that requests share a session.
	session, thread := strings.TrimSpace(evidence.ClientSessionID), strings.TrimSpace(evidence.ClientThreadID)
	if session == "" {
		session = thread
	}
	key := ""
	anonymous := c != nil && c.GetString("codex_anonymous_http_session") != "" && c.GetString("codex_anonymous_http_session") == session
	if session != "" && !anonymous {
		key = fmt.Sprintf("%x", sha256.Sum256([]byte(namespace+"\x00"+session+"\x00"+thread)))
	}
	now := time.Now()
	codexSessionIdentities.Lock()
	defer codexSessionIdentities.Unlock()
	snapshot, found := codexSessionIdentities.entries[key]
	if !found || now.Sub(snapshot.lastUsed) >= codexSessionIdentityIdleTTL {
		// Resolving settings may hit the database. Do not hold the shared cache
		// lock while it runs; concurrent first requests converge below.
		codexSessionIdentities.Unlock()
		override := ""
		if source != nil {
			override = s.codexIdentityOverrideUA(source)
		}
		identity := resolveCodexOutboundIdentity(override)
		mode := codexWireProfileModeAuto
		if s != nil && s.cfg != nil {
			mode = s.cfg.Gateway.CodexWire.Profile
		}
		profile, _ := resolveCodexWireProfile(identity.version, mode)
		snapshot = codexSessionIdentitySnapshot{identity: identity, profile: profile}
		codexSessionIdentities.Lock()
		if existing, ok := codexSessionIdentities.entries[key]; ok && now.Sub(existing.lastUsed) < codexSessionIdentityIdleTTL {
			snapshot, found = existing, true
		}
	}
	snapshot.lastUsed = now
	snapshot.cacheKey = key
	if key != "" {
		if len(codexSessionIdentities.entries) >= codexSessionIdentityLimit {
			oldestKey := ""
			oldestTime := now
			for candidate, entry := range codexSessionIdentities.entries {
				if now.Sub(entry.lastUsed) >= codexSessionIdentityIdleTTL {
					delete(codexSessionIdentities.entries, candidate)
				} else if entry.lastUsed.Before(oldestTime) {
					oldestKey, oldestTime = candidate, entry.lastUsed
				}
			}
			if len(codexSessionIdentities.entries) >= codexSessionIdentityLimit && !found {
				delete(codexSessionIdentities.entries, oldestKey)
			}
		}
		codexSessionIdentities.entries[key] = snapshot
	}
	if c != nil {
		c.Set(contextKey, snapshot)
	}
	return snapshot
}

func (s *OpenAIGatewayService) codexWireProfileForRequest(c *gin.Context, account *Account, rawBody []byte) (codexWireProfile, bool) {
	if !isCodexOAuthAccount(account) {
		return codexWireProfile{}, false
	}
	return s.codexSessionIdentity(c, account, rawBody).profile, true
}

func (s *OpenAIGatewayService) codexSessionClientVersion(c *gin.Context, account *Account, rawBody []byte) string {
	return s.codexSessionIdentity(c, account, rawBody).identity.version
}

func (s *OpenAIGatewayService) applyCodexSessionIdentity(c *gin.Context, account *Account, rawBody []byte, headers http.Header) {
	if headers == nil || !isCodexOAuthAccount(account) || !codexIdentityEnforcement.Load() {
		return
	}
	identity := s.codexSessionIdentity(c, account, rawBody).identity
	headers.Set("User-Agent", identity.userAgent)
	headers.Set("originator", identity.originator)
	headers.Set("version", identity.version)
}
