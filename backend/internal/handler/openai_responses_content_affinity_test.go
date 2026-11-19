//go:build unit

package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIResponsesHistoryKeepsFirstTurnAccount(t *testing.T) {
	bindings := testutil.NewRedisGatewayCache(t)
	scheduler := &openAIQueueSchedulerCache{available: func(int64) bool { return true }}
	h, upstream, _ := newOpenAIQueueHandlerWithScheduler(t, 2, &openAIQueueAdmissionCache{}, scheduler, time.Second, bindings)
	const path = "/v1/responses"
	const start = `{"model":"gpt-5.1","input":[{"role":"developer","content":"Stable guide"},{"role":"user","content":"First question"}`
	c, rec := openAIQueueRequestContext(t, path, start+`]}`)
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	// Reuse an existing binding to the lower-priority account. A changed hash
	// on the second request would send it to the other, available account.
	oldHash := service.DeriveSessionHashFromSeed(`compat_cs_model=gpt-5.1|system="Stable guide"|first_user="First question"`)
	require.NoError(t, h.gatewayService.BindStickySession(c.Request.Context(), key.GroupID, oldHash, 2))
	h.Responses(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	c, rec = openAIQueueRequestContext(t, path, start+`,{"role":"assistant","content":"OK"},{"role":"developer","content":"New context"},{"role":"user","content":"Continue"}]}`)
	h.Responses(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	c, rec = openAIQueueRequestContext(t, path, `{"model":"gpt-5.1","input":[{"role":"developer","content":"Stable guide"},{"role":"user","content":"Another conversation"}]}`)
	h.Responses(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []int64{2, 2, 1}, upstream.accountIDs)
}

func TestOpenAITextAccountAffinitySurvivesReplacedHistory(t *testing.T) {
	for _, endpoint := range []struct {
		path, items string
		run         func(*OpenAIGatewayHandler, *gin.Context)
	}{
		{"/v1/responses", "input", (*OpenAIGatewayHandler).Responses},
		{"/v1/chat/completions", "messages", (*OpenAIGatewayHandler).ChatCompletions},
	} {
		t.Run(endpoint.items, func(t *testing.T) {
			bindings := testutil.NewRedisGatewayCache(t)
			scheduler := &openAIQueueSchedulerCache{available: func(int64) bool { return true }}
			h, upstream, _ := newOpenAIQueueHandlerWithScheduler(t, 2, &openAIQueueAdmissionCache{}, scheduler, time.Second, bindings)
			var originalHash string
			forward := func(userID int64, question, metadata, thread string, expectedAccount int64) {
				t.Helper()
				body := fmt.Sprintf(`{"model":"gpt-5.1",%s%q:[{"role":"user","content":%q}]}`, metadata, endpoint.items, question)
				c, rec := openAIQueueRequestContext(t, endpoint.path, body)
				key, ok := middleware.GetAPIKeyFromContext(c)
				require.True(t, ok)
				key.ID, key.UserID, key.User.ID = userID+1000, userID, userID
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID})
				if thread != "" {
					c.Request.Header.Set("thread-id", thread)
				}
				var hash string
				if endpoint.items == "input" {
					hash = h.gatewayService.GenerateResponsesAccountSessionHash(c, []byte(body), "gpt-5.1")
				} else {
					hash = h.gatewayService.GenerateChatCompletionsSessionHash(c, []byte(body), "gpt-5.1")
				}
				require.NotEmpty(t, hash)
				ctx := c.Request.Context()
				// Keep the load snapshot constant. Only the first user's existing
				// binding can select the lower-priority account.
				if originalHash == "" {
					originalHash = hash
					require.NoError(t, h.gatewayService.BindStickySession(ctx, key.GroupID, hash, expectedAccount))
				}
				bound, err := bindings.GetSessionAccountID(ctx, *key.GroupID, "openai:"+hash)
				if userID == 100 {
					require.Equal(t, originalHash, hash)
					require.NoError(t, err)
					require.Equal(t, expectedAccount, bound)
				} else {
					require.NotEqual(t, originalHash, hash)
					require.ErrorIs(t, err, service.ErrStickySessionNotFound)
				}
				endpoint.run(h, c)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				bound, err = bindings.GetSessionAccountID(ctx, *key.GroupID, "openai:"+hash)
				require.NoError(t, err)
				require.Equal(t, expectedAccount, bound)
			}
			forward(100, "Original first turn", `"client_metadata":{"session_id":"thread-a"},`, "", 2)
			forward(100, "Summary after compaction", "", "thread-a", 2)
			forward(101, "Summary after compaction", "", "thread-a", 1)
			require.Equal(t, []int64{2, 2, 1}, upstream.accountIDs)
		})
	}
}
