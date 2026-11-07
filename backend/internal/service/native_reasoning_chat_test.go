package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func nativeChatJWT(subject string, expiry int64) string {
	raw, _ := json.Marshal(map[string]any{"sub": subject, "iss": "synthetic-issuer", "aud": "synthetic-codex", "exp": expiry})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".synthetic-signature"
}

func TestNativeReasoningReplayTransportPrincipalGuard(t *testing.T) {
	first := nativeChatJWT("first-owner", 1)
	refreshed := nativeChatJWT("first-owner", 2)
	other := nativeChatJWT("other-owner", 2)
	require.Equal(t, nativeReasoningTokenPrincipal(first), nativeReasoningTokenPrincipal(refreshed))
	require.NotEqual(t, nativeReasoningTokenPrincipal(first), nativeReasoningTokenPrincipal(other))
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req = req.WithContext(context.WithValue(req.Context(), nativeReasoningAuthGuardKey{}, &nativeReasoningAuthGuard{principal: nativeReasoningTokenPrincipal(first)}))
	req.Header.Set("Authorization", "Bearer "+refreshed)
	require.NoError(t, checkNativeReasoningRequestCredential(req))
	req.Header.Set("Authorization", "Bearer "+other)
	require.ErrorIs(t, checkNativeReasoningRequestCredential(req), errNativeReasoningCredentialChanged)
	upstream := &httpUpstreamRecorder{}
	s := &OpenAIGatewayService{httpUpstream: upstream}
	_, err := s.doOpenAIUpstreamOnce(req, "", &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth})
	require.ErrorIs(t, err, errNativeReasoningCredentialChanged)
	require.Empty(t, upstream.requests, "changed principal must be stopped before network transmission")
}

func TestNativeReasoningReplayRejectedLocalBarrier(t *testing.T) {
	limits := DefaultNativeReasoningReplayLimits()
	limits.MaxEntriesGlobal = 2
	r := &nativeReasoningRuntime{limits: limits}
	first := NativeReasoningReplayReference{Key: "entry", Digest: "first", ScopeDigest: "scope"}
	next := NativeReasoningReplayReference{Key: "entry", Digest: "next", ScopeDigest: "scope"}
	now := time.Now()
	r.rejectReferences([]NativeReasoningReplayReference{first}, now)
	values := []NativeReasoningReplayReplacement{{Reference: first}, {Reference: next}}
	allowed := r.allowedReplacements(values, now.Add(time.Second))
	require.Len(t, allowed, 1)
	require.Equal(t, next, allowed[0].Reference)
	r.rejectReferences([]NativeReasoningReplayReference{{Key: "second", Digest: "d", ScopeDigest: "scope"}, {Key: "overflow", Digest: "d", ScopeDigest: "scope"}}, now)
	require.LessOrEqual(t, len(r.rejected.entries), 2)
	require.Empty(t, r.allowedReplacements(values, now.Add(time.Second)), "capacity must fall back instead of forgetting active refusals")
	require.Len(t, r.allowedReplacements(values, now.Add(limits.TTL+time.Second)), 2)
}

func TestNativeReasoningReplayCredentialFallbackBounded(t *testing.T) {
	n := &nativeReasoningChat{references: []NativeReasoningReplayReference{{Key: "owned"}}, eligible: true}
	require.False(t, n.preSendFallback(errors.New("unrelated")))
	require.True(t, n.preSendFallback(errNativeReasoningCredentialChanged))
	require.True(t, n.replayDisabled)
	require.False(t, n.eligible)
	require.Empty(t, n.references)
	require.False(t, n.preSendFallback(errNativeReasoningCredentialChanged))
}

func TestNativeReasoningReplayValidationRetryRequiresProof(t *testing.T) {
	for _, tc := range []struct {
		name, body                                  string
		status                                      int
		incomplete, committed, canceled, invalidate bool
	}{
		{name: "quoted_code", body: `{"error":{"code":"other","message":"invalid_encrypted_content"}}`},
		{name: "duplicate_code", body: `{"error":{"code":"other","code":"invalid_encrypted_content"}}`},
		{name: "usage", body: `{"error":{"code":"invalid_encrypted_content"},"usage":null}`, invalidate: true},
		{name: "partial_output", body: `{"error":{"code":"invalid_encrypted_content"},"output":[]}`, invalidate: true},
		{name: "incomplete_body", body: `{"error":{"code":"invalid_encrypted_content"}}`, incomplete: true},
		{name: "different_status", body: `{"error":{"code":"invalid_encrypted_content"}}`, status: 401},
		{name: "committed_headers", body: `{"error":{"code":"invalid_encrypted_content"}}`, committed: true, invalidate: true},
		{name: "client_canceled", body: `{"error":{"code":"invalid_encrypted_content"}}`, canceled: true, invalidate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, _, request, completion := nativeReplayFixture(t)
			require.NoError(t, engine.Commit(context.Background(), request, completion))
			next := nativeReplayContinuation(request, completion)
			replacements, err := engine.Restore(context.Background(), next)
			require.NoError(t, err)
			require.Len(t, replacements, 1)
			runtime := &nativeReasoningRuntime{engine: engine, limits: engine.limits}
			n := &nativeReasoningChat{runtime: runtime, request: next, references: []NativeReasoningReplayReference{replacements[0].Reference}, eligible: true}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
			if tc.committed {
				c.Writer.WriteHeaderNow()
			}
			if tc.canceled {
				cancel()
			}
			status := tc.status
			if status == 0 {
				status = http.StatusBadRequest
			}
			require.False(t, n.rejectedFallback(ctx, c, status, []byte(tc.body), !tc.incomplete), "these cases never authorize an additional upstream request")
			later, err := engine.Restore(context.Background(), next)
			require.NoError(t, err)
			if tc.invalidate {
				require.Empty(t, later, "a verified rejection must poison later restore independently of retry permission")
				require.Empty(t, runtime.allowedReplacements(replacements, time.Now()), "the local rejection barrier must also be installed")
			} else {
				require.Len(t, later, 1, "unverified errors must not poison a valid record")
				require.Len(t, runtime.allowedReplacements(replacements, time.Now()), 1)
			}
		})
	}
}
