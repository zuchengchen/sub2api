package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// This view exists only during the actual fresh dial, after HeadersFactory.
// It must never be stored in the registry or in a delayed prewarm snapshot.
type openAIWSFreshDialInfo struct {
	Request openAIWSAcquireRequest
	Headers http.Header
	Config  *config.Config
	Dialer  openAIWSClientDialer
}

type openAIWSFreshDialHook func(context.Context, openAIWSFreshDialInfo) (*openAIWSEdgeDialLease, error)

func (s *OpenAIGatewayService) getOpenAIWSEdgeBackoff() *openAIWSEdgeBackoff {
	if s == nil {
		return nil
	}
	s.openaiWSEdgeBackoffOnce.Do(func() {
		if s.openaiWSEdgeBackoff == nil {
			s.openaiWSEdgeBackoff = newOpenAIWSEdgeBackoff(openAIWSEdgeBackoffSettings{ProbeTimeout: s.openAIWSDialTimeout()})
		}
	})
	return s.openaiWSEdgeBackoff
}

// openAIWSEdgeDialHook is opt-in only for a request that the existing forwarder
// has already determined can fall back to HTTP without replaying progress. The
// caller must not attach it to native sessions, cookie warmups or requests that
// require their current connection. Capture only authenticated numeric scope
// and the resolved model, never gin.Context, a body, or client-supplied IDs.
func (s *OpenAIGatewayService) openAIWSEdgeDialHook(apiKey *APIKey, finalModel string, eligibleHTTPFallback bool) openAIWSFreshDialHook {
	if s == nil || !eligibleHTTPFallback || apiKey == nil || apiKey.ID <= 0 || apiKey.UserID <= 0 || strings.TrimSpace(finalModel) == "" {
		return nil
	}
	userID, apiKeyID, groupID := apiKey.UserID, apiKey.ID, int64(0)
	if apiKey.GroupID != nil {
		groupID = *apiKey.GroupID
		if groupID < 0 {
			return nil
		}
	}
	return func(ctx context.Context, info openAIWSFreshDialInfo) (*openAIWSEdgeDialLease, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req := info.Request
		if req.Account == nil || !req.Account.IsOpenAIOAuth() || req.Account.IsOpenAIAgentIdentity() ||
			req.CookieWarmup || req.ForcePreferredConn || req.Headers.Get(openAICookieWSGenerationHeader) != "" {
			return nil, nil
		}
		owner, err := s.openAIOAuthCredentialOwner(ctx, req.Account)
		if err != nil || owner == nil || !owner.IsOpenAIOAuth() {
			return nil, nil // missing attribution must not deny an existing path
		}
		key, ok := openAIWSEdgeDialKey(ctx, info, owner, userID, apiKeyID, groupID, finalModel)
		if !ok {
			return nil, nil
		}
		lease, bypass := s.getOpenAIWSEdgeBackoff().Acquire(ctx, key, time.Now())
		if err := ctx.Err(); err != nil {
			lease.Finish(err, time.Now())
			return nil, err
		}
		if bypass {
			return nil, errOpenAIWSEdgeBackoff
		}
		return lease, nil
	}
}

// This function derives only hashes from the actual auth/route/transport inputs.
// Unknown custom dialers opt out: an empty ProxyURL is not evidence that such a
// dialer is direct. The known built-in lanes explicitly bypass environment
// proxies when ProxyURL is empty. A configured-but-unresolved proxy opts out.
func openAIWSEdgeDialKey(ctx context.Context, info openAIWSFreshDialInfo, owner *Account, userID, apiKeyID, groupID int64, model string) (openAIWSEdgeBackoffKey, bool) {
	req := info.Request
	if req.Account == nil || owner == nil || owner.ID <= 0 || strings.TrimSpace(model) == "" {
		return openAIWSEdgeBackoffKey{}, false
	}
	target, err := url.Parse(strings.TrimSpace(req.WSURL))
	if err != nil || target.Hostname() == "" || target.User != nil || target.Fragment != "" || (target.Scheme != "ws" && target.Scheme != "wss" && target.Scheme != "http" && target.Scheme != "https") {
		return openAIWSEdgeBackoffKey{}, false
	}
	authorization := info.Headers.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")) == "" {
		return openAIWSEdgeBackoffKey{}, false
	}
	transport, ok := openAIWSEdgeTransportFingerprint(ctx, info, target)
	if !ok {
		return openAIWSEdgeBackoffKey{}, false
	}
	proxyURL := strings.TrimSpace(req.ProxyURL)
	egress := "builtin-direct"
	if proxyURL == "" {
		if req.Account.ProxyID != nil { // unavailable configured route is not direct
			return openAIWSEdgeBackoffKey{}, false
		}
	} else {
		proxy, parseErr := url.Parse(proxyURL)
		if parseErr != nil || proxy.Hostname() == "" {
			return openAIWSEdgeBackoffKey{}, false
		}
		switch strings.ToLower(proxy.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return openAIWSEdgeBackoffKey{}, false
		}
		egress = hashOpenAIWSEdgeDialParts("explicit-proxy", proxyURL)
	}
	return makeOpenAIWSEdgeBackoffKey(openAIWSEdgeBackoffScope{
		AccountID: req.Account.ID, CredentialOwnerID: owner.ID,
		CredentialNamespace: hashOpenAIWSEdgeDialParts(owner.Platform, owner.Type, owner.GetChatGPTAccountID(), owner.GetChatGPTUserID(), owner.GetCredential("client_id"), owner.GetCredential(openAIAuthModeCredentialKey), owner.GetCredential(openAIAuthModeLegacyCredentialKey)),
		UserID:              userID, APIKeyID: apiKeyID, GroupID: groupID,
		Egress: egress, Target: hashOpenAIWSEdgeDialParts(target.String(), strings.TrimSpace(model)),
		CredentialGeneration: hashOpenAIWSEdgeDialParts(authorization, info.Headers.Get("Cookie"), info.Headers.Get("Chatgpt-Account-Id")),
		Transport:            transport,
	})
}

// Keep this selection aligned with dialOpenAIWSForAccount. Only built-in paths
// whose proxy and TLS behavior is known may participate in the backoff scope.
func openAIWSEdgeTransportFingerprint(ctx context.Context, info openAIWSFreshDialInfo, target *url.URL) (string, bool) {
	req := info.Request
	plaintextProxy := (target.Scheme == "ws" || target.Scheme == "http") && strings.TrimSpace(req.ProxyURL) != ""
	exact := !plaintextProxy && info.Config != nil && info.Config.Gateway.OpenAIWS.ExactFidelityEnabled && req.Account != nil && req.Account.UsesOpenAICodexProtocol()
	lane, profileID := "coder-websocket", "system-tls"
	if !exact {
		if _, known := info.Dialer.(*coderOpenAIWSClientDialer); !known {
			return "", false
		}
	} else {
		lane = "codexwire-websocket"
		var profile *tlsfingerprint.Profile
		if name := codexWireTLSProfileName(req.Account); name != "none" {
			profile = tlsfingerprint.BuiltinProfile(name)
			if profile == nil {
				profile = tlsfingerprint.BuiltinProfile("rustls")
			}
		}
		if isCodexOAuthAccount(req.Account) {
			var err error
			profile, err = codexwire.WithCodexCustomCA(profile)
			if err != nil {
				return "", false
			}
		}
		if profile != nil {
			encoded, err := json.Marshal(profile)
			if err != nil {
				return "", false
			}
			profileID = hashOpenAIWSEdgeDialParts(string(encoded), profile.RootCAID)
		}
	}
	return hashOpenAIWSEdgeDialParts(lane, profileID, strconv.FormatBool(openAIWSCompressionDisabled(ctx)),
		info.Headers.Get("User-Agent"), info.Headers.Get("OpenAI-Beta"), normalizeOpenAIWSBetaFeatures(info.Headers),
		info.Headers.Get("Originator"), info.Headers.Get("Origin"), info.Headers.Get("Accept-Language")), true
}

func hashOpenAIWSEdgeDialParts(parts ...string) string {
	hash := sha256.New()
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
