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
			scheduler := &openAIQueueSchedulerCache{available: func(id int64) bool { return id == 2 }}
			h, upstream, _ := newOpenAIQueueHandlerWithScheduler(t, 2, &openAIQueueAdmissionCache{}, scheduler, time.Second, testutil.NewRedisGatewayCache(t))
			forward := func(userID int64, question, metadata, thread string) {
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
				endpoint.run(h, c)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}
			forward(100, "Original first turn", `"client_metadata":{"session_id":"thread-a"},`, "")
			scheduler.available = func(int64) bool { return true }
			forward(100, "Summary after compaction", "", "thread-a")
			forward(101, "Summary after compaction", "", "thread-a")
			require.Equal(t, []int64{2, 2, 1}, upstream.accountIDs)
		})
	}
}
