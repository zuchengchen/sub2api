package handler

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTiboHealthyGroupModelWebSocket(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		group := wsAllowlistGroup(true, "gpt-6-luna")
		group.Name = service.TiboHealthyGroupName
		t.Run(mode+"/first", func(t *testing.T) {
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload: `{"type":"response.create","model":"openai/GPT5.6LUNA-max","stream":false}`,
				group:        group, ingressMode: mode, firstFrameCloseExpected: true,
			})
		})
		t.Run(mode+"/later", func(t *testing.T) {
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload:  `{"type":"response.create","model":"gpt-5.4","stream":false}`,
				secondPayload: `{"type":"response.create","model":"gpt-5.4","Model":"gpt6luna-high","stream":false}`,
				group:         group, ingressMode: mode, secondTurnCloseExpected: true,
			})
		})
	}
}

func TestTiboHealthyGroupModelCatalogueAndDetails(t *testing.T) {
	group := &service.Group{ID: 991, Name: service.TiboHealthyGroupName, Platform: service.PlatformOpenAI}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
		group.ID: {{ID: 992, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive,
			Schedulable: true, Credentials: map[string]any{}}},
	}})
	list := requestModelForTest(h, group, "", "")
	require.Equal(t, http.StatusOK, list.Code)
	require.NotContains(t, list.Body.String(), "luna")
	require.Contains(t, list.Body.String(), "gpt-6-astra")
	require.Equal(t, http.StatusNotFound, requestModelForTest(h, group, "gpt-6-luna", "").Code)
	require.Equal(t, http.StatusOK, requestModelForTest(h, group, "gpt-6-astra", "").Code)
	group.Name = service.TiboWelfareGroupName
	require.Equal(t, http.StatusOK, requestModelForTest(h, group, "gpt-6-luna", "").Code)
}

func TestTiboHealthyGroupModelWebSocketSessionRotation(t *testing.T) {
	group := wsAllowlistGroup(true, "gpt-6-luna")
	group.Name = service.TiboHealthyGroupName
	runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:  `{"type":"response.create","model":"gpt-5.4","stream":false}`,
		midPayload:    `{"type":"session.update","session":{"model":"gpt-6-luna"}}`,
		secondPayload: `{"type":"response.create","session":{"model":"gpt-5.4"},"stream":false}`,
		group:         group, ingressMode: service.OpenAIWSIngressModePassthrough, secondTurnCloseExpected: true,
	})
}
