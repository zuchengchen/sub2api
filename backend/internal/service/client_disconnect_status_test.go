//go:build unit

package service

import (
	"context"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 两条传输层失败处理的共同契约：请求 context 已取消（客户端断开）时不记 Ops 上游错误事件；
// 请求 context 仍存活时即便错误是 context.Canceled 也照常记录，不归为客户端断开。
func TestTransportErrorHandlers_ClientDisconnectRecordsNoOpsEvent(t *testing.T) {
	account := &Account{ID: 9, Name: "acc", Platform: PlatformAnthropic}
	clientErr := &url.Error{Op: "Post", URL: "https://upstream.example/v1/messages", Err: context.Canceled}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	handlers := []struct {
		name   string
		handle func(ctx context.Context, c *gin.Context) error
	}{
		{"anthropic", func(ctx context.Context, c *gin.Context) error {
			s := &GatewayService{accountRepo: &transportTempUnschedRepoStub{}}
			return s.handleUpstreamTransportError(ctx, c, account, clientErr, OpsUpstreamErrorEvent{})
		}},
		{"openai", func(ctx context.Context, c *gin.Context) error {
			s := &OpenAIGatewayService{accountRepo: &openaiTransportAccountRepoStub{}}
			return s.handleOpenAIUpstreamTransportError(ctx, c, account, clientErr, false)
		}},
	}

	for _, h := range handlers {
		t.Run(h.name+"/client disconnected", func(t *testing.T) {
			c := newTransportErrorTestGin(t)

			err := h.handle(canceledCtx, c)

			require.ErrorIs(t, err, context.Canceled)
			_, recorded := c.Get(OpsUpstreamErrorsKey)
			require.False(t, recorded, "客户端断开不是上游故障")
			_, messageSet := c.Get(OpsUpstreamErrorMessageKey)
			require.False(t, messageSet)
		})
		t.Run(h.name+"/request context alive", func(t *testing.T) {
			c := newTransportErrorTestGin(t)

			err := h.handle(context.Background(), c)

			require.ErrorIs(t, err, context.Canceled)
			raw, recorded := c.Get(OpsUpstreamErrorsKey)
			require.True(t, recorded)
			require.Len(t, raw.([]*OpsUpstreamErrorEvent), 1)
		})
	}
}
