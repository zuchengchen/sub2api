package service

import (
	"context"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIResponseAuthOwnerNativePassthroughChecksEveryFrame(t *testing.T) {
	for _, mode := range []string{"other_site_user", "other_oauth_user", "same_site_user_other_key", "unknown_native_origin"} {
		t.Run(mode, func(t *testing.T) {
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			upstream := newStagedPassthroughConn()
			upstream.Send(`{"type":"response.completed","response":{"id":"resp_current","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
			svc := newPassthroughLifecycleService(cfg, upstream)
			account := responseAuthOwnerAccount()
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
			if mode != "unknown_native_origin" {
				svc.bindHTTPResponseAccount(context.Background(), responseAuthOwnerContext(101, 201), account, "resp_prior")
			}
			if mode == "other_oauth_user" {
				account = snapshotOAuthRefreshAccount(account)
				account.Credentials["chatgpt_user_id"] = "new-oauth-user"
			}
			userID := int64(101)
			if mode == "other_site_user" {
				userID = 102
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
				c.Set("api_key", &APIKey{ID: 202, UserID: userID})
				return nil
			})
			defer server.Close()
			client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-5.2","input":[{"role":"user","content":"first"}]}`)
			defer client.CloseNow()
			requirePassthroughUpstreamWrite(t, upstream, time.Second)
			_, _, err := client.Read(ctx)
			require.NoError(t, err)
			second := []byte(`{"type":"response.create","model":"gpt-5.2","previous_response_id":"resp_prior","input":[{"role":"user","content":"second"}]}`)
			require.NoError(t, client.Write(ctx, coderws.MessageText, second))
			rejected := mode == "other_site_user" || mode == "other_oauth_user"
			if rejected {
				// Read the actual policy close before cleanup. coder/websocket
				// acknowledges it here, allowing the server close handshake to
				// finish; waiting only on serverErr would deadlock that handshake.
				readCtx, readCancel := context.WithTimeout(ctx, time.Second)
				_, _, closeErr := client.Read(readCtx)
				readCancel()
				require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(closeErr), "foreign continuation must produce a timely policy close")
				require.ErrorContains(t, closeErr, "previous_response_id is not available")
				select {
				case err := <-serverErr:
					require.ErrorIs(t, err, errOpenAIResponseAuthOwnerMismatch)
				case <-ctx.Done():
					t.Fatal("native relay did not reject the foreign continuation")
				}
				select {
				case wire := <-upstream.writes:
					t.Fatalf("foreign continuation reached the upstream: %s", wire)
				default:
				}
			} else {
				wire := requirePassthroughUpstreamWrite(t, upstream, time.Second)
				require.Equal(t, "resp_prior", gjson.GetBytes(wire, "previous_response_id").String())
				upstream.Send(`{"type":"response.completed","response":{"id":"resp_native_second","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
				_, _, err = client.Read(ctx)
				require.NoError(t, err)
				_ = client.CloseNow()
				select {
				case <-serverErr:
				case <-ctx.Done():
					t.Fatal("native relay did not stop")
				}
				stored, found, err := svc.getOpenAIWSStateStore().(openAIResponseAuthOwnerStore).getResponseAuthOwner(context.Background(), 0, "resp_native_second")
				require.NoError(t, err)
				require.True(t, found, "successful delivered native response must establish cross-protocol origin")
				require.Equal(t, int64(101), stored.UserID)
			}
		})
	}
}
