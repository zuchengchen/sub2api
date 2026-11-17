//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestOAuthReliabilityLargeRequest401FullWireReplay(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, route := range []string{"responses", "lite", "chat", "messages", "passthrough"} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/compressed=%t", route, compressed), func(t *testing.T) {
				body, text := compressionForwardRequest(t, route, false)
				original := bytes.Clone(body)
				account, repo, executor, provider := newOpenAIRecoveryFixture()
				account.Extra = newCodexSynthTestAccount(account.ID, AccountTypeOAuth).Extra
				if route == "passthrough" {
					account.Extra["openai_passthrough"] = true
				}
				repo.account = snapshotOAuthRefreshAccount(account)
				cfg := codexSynthEnabledConfig()
				cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
				cfg.Gateway.OpenAIWS.ForceHTTP = true
				cfg.Gateway.OpenAIRequestCompressionEnabled = compressed
				var wires [][]byte
				var auth []string
				var rejectedBodies []*openAIRecoveryBody
				success := &compressionForwardUpstream{t: t}
				upstream := &openAIRecoveryUpstream{do: func(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
					wire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					require.NoError(t, req.Body.Close())
					require.EqualValues(t, len(wire), req.ContentLength)
					require.NotNil(t, req.GetBody)
					replay, err := req.GetBody()
					require.NoError(t, err)
					replayed, err := io.ReadAll(replay)
					require.NoError(t, err)
					require.NoError(t, replay.Close())
					require.True(t, bytes.Equal(wire, replayed), "the transport's rewind contract must retain all wire bytes")
					if len(wires) > 0 {
						require.True(t, bytes.Equal(wires[0], wire), "401 recovery must not reconstruct, trim, duplicate or re-encode request content")
					}
					wires = append(wires, wire)
					auth = append(auth, req.Header.Get("Authorization"))
					decoded := wire
					if compressed {
						require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
						decoder, err := zstd.NewReader(nil)
						require.NoError(t, err)
						decoded, err = decoder.DecodeAll(wire, nil)
						decoder.Close()
						require.NoError(t, err)
					} else {
						require.Empty(t, req.Header.Get("Content-Encoding"))
					}
					requireCompressionForwardContent(t, decoded, text)
					if len(wires) <= 2 {
						rejected := &openAIRecoveryBody{Reader: strings.NewReader(`{"error":{"type":"authentication_error","message":"synthetic token rejected"}}`)}
						rejectedBodies = append(rejectedBodies, rejected)
						return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: rejected, Request: req}, nil
					}
					req.Body = io.NopCloser(bytes.NewReader(wire))
					return success.Do(req, proxy, id, concurrency)
				}}
				svc := &OpenAIGatewayService{cfg: cfg, accountRepo: repo, openAITokenProvider: provider, httpUpstream: upstream, tiboRouteDisabled: true}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				path := "/v1/responses"
				if route == "chat" {
					path = "/v1/chat/completions"
				} else if route == "messages" {
					path = "/v1/messages"
				}
				c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				if route == "lite" {
					c.Request.Header.Set(responsesLiteHeader, "true")
				}
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				var result *OpenAIForwardResult
				var err error
				switch route {
				case "chat":
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-6-astra")
				case "messages":
					result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-6-astra")
				default:
					result, err = svc.Forward(context.Background(), c, account, body)
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Len(t, wires, 3, "only initial, guarded reload and one refreshed attempt are allowed")
				require.Equal(t, []string{"Bearer rejected-access", "Bearer rejected-access", "Bearer repaired-access"}, auth)
				require.Equal(t, int32(1), executor.calls.Load())
				for _, rejected := range rejectedBodies {
					require.True(t, rejected.closed)
				}
				require.True(t, bytes.Equal(original, body), "the caller's complete history must remain immutable")
				require.Contains(t, recorder.Body.String(), "OK")
			})
		}
	}
}
