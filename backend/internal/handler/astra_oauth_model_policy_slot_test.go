//go:build unit

package handler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestAstraOAuthModelPolicyRechecksAfterConcurrencyWait(t *testing.T) {
	gin.SetMode(gin.TestMode)
	green := &service.Account{ID: 8, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Extra: astraHandlerExtra("healthy")}
	red := *green
	red.Extra = astraHandlerExtra("degraded")
	snapshot := &fakeSchedulerCache{accounts: []*service.Account{green}}
	gw := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, &config.Config{}, service.NewSchedulerSnapshotService(snapshot, nil, nil, nil, nil), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	attempts := 0
	slots := &concurrencyCacheMock{acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) {
		attempts++
		if attempts < 3 {
			return false, nil
		}
		snapshot.accounts = []*service.Account{&red}
		return true, nil
	}}
	h := &OpenAIGatewayHandler{gatewayService: gw, concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(slots), SSEPingFormatClaude, 0)}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	selection := &service.AccountSelectionResult{Account: green, WaitPlan: &service.AccountWaitPlan{AccountID: green.ID, MaxConcurrency: 1, Timeout: time.Second, MaxWaiting: 2}}
	streamStarted := false
	release, result := h.acquireResponsesAccountSlot(c, nil, "", selection, false, &streamStarted, zap.NewNop())
	require.Equal(t, openAISlotAcquireOK, result)
	require.NotNil(t, release)
	_, err := gw.Forward(context.Background(), c, selection.Account, []byte(`{"model":"gpt-6-astra","input":"hello"}`))
	var failover *service.UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	release()
	require.EqualValues(t, 1, slots.releaseAccountCalled)
	require.GreaterOrEqual(t, attempts, 3)
	require.Equal(t, "healthy", green.OpenAIOAuthModelPolicy().Verdict)
}
