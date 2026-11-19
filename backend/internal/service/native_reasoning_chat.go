package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var errNativeReasoningCredentialChanged = errors.New("native reasoning replay credential changed before send")
var errNativeReasoningProjectionUnavailable = errors.New("native reasoning replay sent input unavailable")

type nativeReasoningAuthGuardKey struct{}
type nativeReasoningAuthGuard struct{ principal string }

// This guard runs at the transport boundary, including OAuth recovery clones.
// A credential-owner change must never send previously restored encrypted state.
func checkNativeReasoningRequestCredential(req *http.Request) error {
	if req == nil {
		return nil
	}
	guard, _ := req.Context().Value(nativeReasoningAuthGuardKey{}).(*nativeReasoningAuthGuard)
	if guard == nil {
		return nil
	}
	if guard.principal == "" || nativeReasoningTokenPrincipal(req.Header.Get("Authorization")) != guard.principal {
		return errNativeReasoningCredentialChanged
	}
	return nil
}

// Tokens come exclusively from server-selected credentials. Only stable subject
// claims participate when available; opaque credentials get an irreversible
// fingerprint, so rotation safely loses cache reuse instead of crossing owners.
func nativeReasoningTokenPrincipal(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 7 && strings.EqualFold(raw[:7], "Bearer ") {
		raw = strings.TrimSpace(raw[7:])
	}
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ".")
	if len(parts) == 3 && len(parts[1]) <= 128<<10 {
		if payload, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			canonical, err := nativeReplayCanonicalObject(payload)
			var claims map[string]json.RawMessage
			if err == nil && json.Unmarshal(canonical, &claims) == nil {
				var subject string
				if json.Unmarshal(claims["sub"], &subject) == nil && strings.TrimSpace(subject) != "" {
					identity := map[string]json.RawMessage{}
					for _, name := range []string{"sub", "iss", "aud", "client_id", "azp"} {
						if value, ok := claims[name]; ok {
							identity[name] = value
						}
					}
					if auth := claims["https://api.openai.com/auth"]; len(auth) > 0 {
						var values map[string]json.RawMessage
						if json.Unmarshal(auth, &values) == nil {
							for _, name := range []string{"chatgpt_account_id", "chatgpt_user_id", "user_id", "organization_id"} {
								if value, ok := values[name]; ok {
									identity["auth."+name] = value
								}
							}
						}
					}
					encoded, _ := json.Marshal(identity)
					return nativeReplayDigest(append([]byte("native-replay-principal/v1\x00"), encoded...))
				}
			}
		}
	}
	return nativeReplayDigest([]byte("native-replay-opaque-credential/v1\x00" + raw))
}

func nativeReasoningCredentialIdentity(c *gin.Context, account *Account, token string) string {
	source := codexAccountIdentitySource(c, account)
	if source == nil || source.ID <= 0 || token == "" {
		return ""
	}
	value, _ := json.Marshal(struct {
		Revision             string
		AccountID            int64
		Namespace, Principal string
	}{
		Revision: "chat-native-projection/v1", AccountID: source.ID,
		Namespace: codexAccountIdentityNamespace(source), Principal: nativeReasoningTokenPrincipal(token),
	})
	return nativeReplayDigest(value)
}

// One request-local policy decision is shared by native replay and the target
// model's identity bridge. Resolving it does not require a header session: that
// bridge also accepts existing, explicit body session evidence.
type nativeReasoningChatAuthorization struct {
	mode             string
	model            string
	modelRestricted  bool
	userID, apiKeyID int64
}

// The original 6.1 integration also uses thread identity under its legacy user
// grant. Other models opt in to it through an explicit model grant.
func usesThreadScopedNativeChat(model string, modelRestricted bool) bool {
	return model == nativeReasoningReplayAllUsersModel || (modelRestricted && isCanonicalGPTChatModel(model))
}

func (a nativeReasoningChatAuthorization) targetReplayEnabled() bool {
	return a.mode == NativeReasoningReplayModeReplay && a.userID > 0 && a.apiKeyID > 0 && usesThreadScopedNativeChat(a.model, a.modelRestricted)
}

func (s *OpenAIGatewayService) resolveNativeReasoningChatAuthorization(c *gin.Context, actualModel string) nativeReasoningChatAuthorization {
	off := nativeReasoningChatAuthorization{mode: NativeReasoningReplayModeOff}
	if s == nil || s.nativeReplayRuntime == nil || c == nil || c.Request == nil {
		return off
	}
	value, ok := c.Get("api_key")
	if !ok {
		return off
	}
	key, ok := value.(*APIKey)
	if !ok || key == nil || key.ID <= 0 || key.UserID <= 0 {
		return off
	}
	mode, restricted := s.nativeReplayRuntime.modeForUserModel(c.Request.Context(), key.UserID, actualModel)
	return nativeReasoningChatAuthorization{mode: mode, model: actualModel, modelRestricted: restricted, userID: key.UserID, apiKeyID: key.ID}
}

type nativeReasoningChat struct {
	authorizedModel    string
	runtime            *nativeReasoningRuntime
	mode               string
	original           []byte
	messages           []json.RawMessage
	request            NativeReasoningReplayRequest
	references         []NativeReasoningReplayReference
	binding            nativeReasoningReplayBinding
	toolReverse        map[string]string
	principal          string
	eligible           bool
	replayDisabled     bool
	fallbackTried      bool
	publication        *nativeReasoningPublicationLease
	publicationContext context.Context
	publicationCancel  context.CancelFunc
}

func (s *OpenAIGatewayService) beginNativeReasoningChat(c *gin.Context, account *Account, body []byte, authorization nativeReasoningChatAuthorization) *nativeReasoningChat {
	if s == nil || s.nativeReplayRuntime == nil || c == nil || c.Request == nil || !isCodexOAuthAccount(account) || account.IsOpenAIAgentIdentity() || account.IsOpenAIPersonalAccessToken() {
		return nil
	}
	if s.pluginManager != nil && s.pluginManager.ShouldRouteOpenAIOAuth(account) {
		return nil
	}
	value, ok := c.Get("api_key")
	if !ok {
		return nil
	}
	key, ok := value.(*APIKey)
	if !ok || key == nil || key.ID <= 0 || key.UserID <= 0 {
		return nil
	}
	if authorization.userID != key.UserID || authorization.apiKeyID != key.ID || (authorization.mode != NativeReasoningReplayModeObserve && authorization.mode != NativeReasoningReplayModeReplay) {
		return nil
	}
	mode := authorization.mode
	authorizedModel := ""
	threadScoped := usesThreadScopedNativeChat(authorization.model, authorization.modelRestricted)
	if authorization.modelRestricted {
		if !threadScoped {
			return nil
		}
		authorizedModel = authorization.model
	}
	session := ExtractClientSessionID(c)
	if threadScoped {
		if authorization.model != nativeReasoningReplayAllUsersModel && !gjson.GetBytes(body, "messages").IsArray() {
			return nil
		}
		clientSession, thread, valid := gpt61SolChatExplicitIdentity(c, body)
		if !valid {
			return nil
		}
		// Preserve the namespaces used by existing grants. The new family
		// namespace has a NUL that cannot occur in a raw legacy session header.
		namespace := "gpt-chat/v1:\x00"
		switch authorization.model {
		case nativeReasoningReplayAllUsersModel:
			namespace = "gpt61-chat/v1:"
		case "gpt-6.2-astra", "gpt-6.2-sol":
			namespace = "gpt62-chat/v1:\x00"
		}
		// Frame both values so overlapping session/thread names cannot alias.
		framed, _ := json.Marshal([2]string{clientSession, thread})
		session = namespace + nativeReplayDigest(framed)
	}
	if session == "" || len(body) > s.nativeReplayRuntime.limits.MaxHistoryBytes {
		return nil
	}
	canonical, err := nativeReplayCanonicalObject(body)
	if err != nil {
		return nil
	}
	var request struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(canonical, &request) != nil || len(request.Messages) == 0 {
		return nil
	}
	return &nativeReasoningChat{runtime: s.nativeReplayRuntime, mode: mode, authorizedModel: authorizedModel, original: bytes.Clone(body), messages: request.Messages,
		request: NativeReasoningReplayRequest{Scope: NativeReasoningReplayScope{UserID: key.UserID, APIKeyID: key.ID, SessionID: session}, Messages: request.Messages}}
}

func (n *nativeReasoningChat) prepare(ctx context.Context, c *gin.Context, account *Account, token string, body []byte) []byte {
	if n == nil {
		return body
	}
	n.resetAttempt()
	n.request.Scope.Model = gjson.GetBytes(body, "model").String()
	n.request.Scope.CredentialIdentity = nativeReasoningCredentialIdentity(c, account, token)
	if n.request.Scope.Model == "" || n.request.Scope.CredentialIdentity == "" || (n.authorizedModel != "" && n.request.Scope.Model != n.authorizedModel) {
		n.releasePublication()
		return body
	}
	n.toolReverse = cloneNativeReasoningToolReverse(codexToolNameReverseFromContext(c))
	parameters, err := nativeReasoningProjectionParameters(n.original, body, n.toolReverse)
	if err != nil {
		n.releasePublication()
		return body
	}
	n.request.Parameters = parameters
	n.eligible = true
	prepared, admitted := n.preparePublication(ctx)
	if !admitted {
		// Saturation affects only this optimization on this request; do not
		// evict another producer or wait on an unrelated user's work.
		return body
	}
	if n.mode != NativeReasoningReplayModeReplay || n.replayDisabled {
		return body
	}
	replacements, err := n.restoreAfterPublication(ctx, prepared)
	if err != nil || len(replacements) == 0 {
		return body
	}
	replacements = n.runtime.allowedReplacements(replacements, time.Now())
	if len(replacements) == 0 {
		return body
	}
	next, refs, err := applyNativeReasoningProjection(body, replacements, n.toolReverse, n.runtime.limits)
	if err != nil || len(refs) == 0 {
		return body
	}
	n.references = refs
	return next
}

func (n *nativeReasoningChat) resetAttempt() {
	n.references = nil
	n.binding = nativeReasoningReplayBinding{}
	n.principal = ""
	n.eligible = false
}

func cloneNativeReasoningToolReverse(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// Request construction may promote instructions and project the wire profile.
// Restore only after it has finished, so both matching and SourceInput describe
// the actual body. The transport may compress it, but must not normalize input
// again after this step.
func (n *nativeReasoningChat) prepareRequest(ctx context.Context, c *gin.Context, account *Account, token string, req *http.Request) (*http.Request, error) {
	if n == nil {
		return req, nil
	}
	n.resetAttempt()
	if req == nil || req.GetBody == nil {
		n.releasePublication()
		return req, nil
	}
	reader, err := req.GetBody()
	if err != nil {
		n.releasePublication()
		return req, nil
	}
	maxBody := n.runtime.limits.MaxHistoryBytes + (1 << 20)
	raw, err := io.ReadAll(io.LimitReader(reader, int64(maxBody)+1))
	_ = reader.Close()
	if err != nil || len(raw) > maxBody || !gjson.ValidBytes(raw) {
		n.releasePublication()
		return req, nil
	}
	wire := n.prepare(ctx, c, account, token, raw)
	if !bytes.Equal(wire, raw) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		req.Body = io.NopCloser(bytes.NewReader(wire))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(wire)), nil }
		req.ContentLength = int64(len(wire))
		if req.Header.Get("Content-Length") != "" {
			req.Header.Set("Content-Length", strconv.Itoa(len(wire)))
		}
		setOpenAICodexRoutingHintFromBody(req.Header, account, wire)
	}
	return n.bind(req)
}

// bind records the actual uncompressed body after all production construction
// steps. OAuth retry clones keep this same body; principal changes are guarded.
func (n *nativeReasoningChat) bind(req *http.Request) (*http.Request, error) {
	if n == nil || !n.eligible {
		return req, nil
	}
	if req == nil || req.GetBody == nil {
		return n.unverifiableRequest(req)
	}
	reader, err := req.GetBody()
	if err != nil {
		return n.unverifiableRequest(req)
	}
	raw, err := io.ReadAll(io.LimitReader(reader, int64(n.runtime.limits.MaxHistoryBytes+(1<<20))+1))
	_ = reader.Close()
	if err != nil || len(raw) > n.runtime.limits.MaxHistoryBytes+(1<<20) || !gjson.ValidBytes(raw) {
		return n.unverifiableRequest(req)
	}
	input := gjson.GetBytes(raw, "input")
	if !input.IsArray() || len(input.Raw) > n.runtime.limits.MaxHistoryBytes {
		return n.unverifiableRequest(req)
	}
	digest, err := NativeReasoningReplayInputDigest([]byte(input.Raw))
	if err != nil {
		return n.unverifiableRequest(req)
	}
	n.principal = nativeReasoningTokenPrincipal(req.Header.Get("Authorization"))
	if len(n.references) > 0 {
		if n.principal == "" {
			return req, errNativeReasoningCredentialChanged
		}
		req = req.WithContext(context.WithValue(req.Context(), nativeReasoningAuthGuardKey{}, &nativeReasoningAuthGuard{principal: n.principal}))
	}
	n.binding.sourceInputDigest = digest
	return req, nil
}

func (n *nativeReasoningChat) unverifiableRequest(req *http.Request) (*http.Request, error) {
	n.eligible = false
	n.binding = nativeReasoningReplayBinding{}
	n.releasePublication()
	if len(n.references) > 0 {
		return req, errNativeReasoningProjectionUnavailable
	}
	return req, nil
}

func (n *nativeReasoningChat) preSendFallback(err error) bool {
	if n == nil || n.fallbackTried || len(n.references) == 0 || (!errors.Is(err, errNativeReasoningCredentialChanged) && !errors.Is(err, errNativeReasoningProjectionUnavailable)) {
		return false
	}
	n.fallbackTried = true
	n.replayDisabled = true
	n.resetAttempt()
	return true
}

func (n *nativeReasoningChat) invalidate(ctx context.Context) {
	if n == nil || len(n.references) == 0 {
		return
	}
	n.runtime.rejectReferences(n.references, time.Now())
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), n.runtime.limits.IOTimeout)
	defer cancel()
	_ = n.runtime.engine.Invalidate(bounded, n.request.Scope, n.references)
}

// A complete, explicit validation rejection invalidates the actual restored
// records even when the client has received a queue heartbeat or cancellation
// prevents a retry. The additional request has stricter conditions: no usage,
// partial output, committed response or cancellation, and at most one retry.
func (n *nativeReasoningChat) rejectedFallback(ctx context.Context, c *gin.Context, status int, body []byte, complete bool) bool {
	if n == nil || len(n.references) == 0 || status != http.StatusBadRequest || !complete {
		return false
	}
	canonical, err := nativeReplayCanonicalObject(body)
	if err != nil {
		return false
	}
	parsed := gjson.ParseBytes(canonical)
	if parsed.Get("error.code").String() != "invalid_encrypted_content" {
		return false
	}
	n.invalidate(ctx)
	if n.fallbackTried || ctx.Err() != nil || c == nil || c.Request == nil || c.Request.Context().Err() != nil || c.Writer.Written() || IsResponseCommitted(c) {
		return false
	}
	for _, path := range []string{"usage", "response.usage", "error.usage", "data.usage", "output", "response.output", "choices", "tool_calls", "delta"} {
		if parsed.Get(path).Exists() {
			return false
		}
	}
	n.fallbackTried = true
	n.replayDisabled = true
	n.resetAttempt()
	return true
}

func (n *nativeReasoningChat) canHandoffCompletedStream() bool {
	if n == nil || !n.eligible || n.mode != NativeReasoningReplayModeReplay {
		return false
	}
	model := n.request.Scope.Model
	if n.authorizedModel != "" && n.authorizedModel != model {
		return false
	}
	return usesThreadScopedNativeChat(model, n.authorizedModel != "")
}

// handoffCompletedStream runs only after the capture has accepted a complete
// finish+[DONE] write, while both request contexts are still live. Arm the same
// bounded publication before Flush can expose DONE to a client that closes at
// that protocol boundary. This does not commit: finish still verifies handler
// success, final captures, policy and the actual upstream principal.
func (n *nativeReasoningChat) handoffCompletedStream(ctx context.Context, c *gin.Context, response *http.Response, upstream *NativeReasoningResponseCapture) {
	if !n.canHandoffCompletedStream() || n.publication == nil || n.publicationContext != nil || upstream == nil || ctx.Err() != nil || c == nil || c.Request == nil || c.Request.Context().Err() != nil || c.Writer.Status() != http.StatusOK || GetOpsCyberPolicy(c) != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return
	}
	if upstream.InvalidEncryptedContent() || (response.Request != nil && nativeReasoningTokenPrincipal(response.Request.Header.Get("Authorization")) != n.principal) {
		return
	}
	if _, complete := upstream.CompletedResponse(); !complete {
		return
	}
	bounded, cancel, transferred := n.publication.handoff(ctx, c.Request.Context(), n.runtime.limits.IOTimeout)
	if transferred {
		n.publicationContext, n.publicationCancel = bounded, cancel
	}
}

func (n *nativeReasoningChat) finish(ctx context.Context, c *gin.Context, response *http.Response, upstream *NativeReasoningResponseCapture, delivery *NativeReasoningChatCapture, forwardErr error) {
	defer n.releasePublication()
	if n == nil || !n.eligible || upstream == nil {
		return
	}
	if upstream.InvalidEncryptedContent() {
		n.invalidate(ctx)
		return
	}
	commitCtx := ctx
	if n.publicationContext != nil {
		commitCtx = n.publicationContext
	}
	if forwardErr != nil || delivery == nil || commitCtx.Err() != nil || c == nil || c.Request == nil || (n.publicationContext == nil && c.Request.Context().Err() != nil) || c.Writer.Status() != http.StatusOK || GetOpsCyberPolicy(c) != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return
	}
	if response.Request != nil && nativeReasoningTokenPrincipal(response.Request.Header.Get("Authorization")) != n.principal {
		return
	}
	native, complete := upstream.CompletedResponse()
	if !complete {
		return
	}
	assistant, delivered := delivery.DeliveredAssistant()
	if !delivered {
		return
	}
	if n.publicationContext == nil && n.publication != nil {
		var cancel context.CancelFunc
		var transferred bool
		commitCtx, cancel, transferred = n.publication.handoff(ctx, c.Request.Context(), n.runtime.limits.IOTimeout)
		if !transferred {
			return
		}
		defer cancel()
	}
	_ = n.runtime.engine.commit(commitCtx, n.request, NativeReasoningReplayCompletion{Response: native, DeliveredAssistant: assistant, DeliveredComplete: true, ToolNameReverse: n.toolReverse}, &n.binding)
}
