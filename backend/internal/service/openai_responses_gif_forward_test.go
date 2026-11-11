//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Capture the actual outbound request: checking just the compatibility map
// would miss image options discarded by later OAuth or WS transformations.
func TestOpenAIResponsesGIFFrameControlsForward(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		for _, tc := range []struct {
			name      string
			request   string
			model     string
			count     int64
			duration  int64
			hasFrames bool
		}{
			{
				name: "top_level_flare",
				request: `{"model":"gpt-image-2.6-flare","prompt":"draw an animation",` +
					`"output_format":"gif","frame_count":4,"frame_duration_ms":120}`,
				model: "gpt-image-2.6-flare", count: 4, duration: 120, hasFrames: true,
			},
			{
				name: "top_level_sunburst",
				request: `{"model":"gpt-image-2.6-sunburst","prompt":"draw an animation",` +
					`"output_format":"gif","frame_count":16,"frame_duration_ms":125}`,
				model: "gpt-image-2.6-sunburst", count: 16, duration: 125, hasFrames: true,
			},
			{
				name: "existing_tool_wins",
				request: `{"model":"gpt-image-2.6-flare","input":"draw an animation",` +
					`"output_format":"gif","frame_count":4,"frame_duration_ms":120,` +
					`"tools":[{"type":"image_generation","model":"gpt-image-2.6-sunburst","frame_count":9,"frame_duration_ms":200}]}`,
				model: "gpt-image-2.6-sunburst", count: 9, duration: 200, hasFrames: true,
			},
			{
				name: "fill_only_missing_tool_field",
				request: `{"model":"gpt-image-2.6-flare","input":"draw an animation",` +
					`"output_format":"gif","frame_count":4,"frame_duration_ms":120,` +
					`"tools":[{"type":"image_generation","frame_count":8}]}`,
				model: "gpt-image-2.6-flare", count: 8, duration: 120, hasFrames: true,
			},
			{
				name: "omitted_fields_remain_omitted",
				request: `{"model":"gpt-image-2.6-flare","prompt":"draw an animation",` +
					`"output_format":"gif"}`,
				model: "gpt-image-2.6-flare",
			},
			{
				name: "explicit_responses_tool",
				request: `{"model":"gpt-6.1-sol","input":"draw an animation",` +
					`"tools":[{"type":"image_generation","model":"gpt-image-2.6-flare",` +
					`"output_format":"gif","frame_count":6,"frame_duration_ms":250}]}`,
				model: "gpt-image-2.6-flare", count: 6, duration: 250, hasFrames: true,
			},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				t.Setenv("SUB2API_IMAGES_MAIN_MODEL", "gpt-5.6-luna")
				gin.SetMode(gin.TestMode)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")
				cfg := &config.Config{}
				cfg.Gateway.OpenAIWS.Enabled = transport == "ws"
				cfg.Gateway.OpenAIWS.OAuthEnabled = true
				cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
				cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
				cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
				// The upstream denial remains an error after the compatibility fix.
				// A correct forwarded request does not grant GIF preview access.
				failure := imageUserErrorTestEvent(t, "response.failed", "image_generation_user_error", false)
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(fmt.Sprintf("data: %s\n\n", failure))),
				}}
				conn := &openAIWSCaptureConn{events: [][]byte{failure}}
				pool := newOpenAIWSConnPool(cfg)
				pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: conn})
				defer pool.Close()
				svc := &OpenAIGatewayService{
					tiboRouteDisabled: true, cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{},
					openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
					toolCorrector:    NewCodexToolCorrector(), openaiWSPool: pool,
				}
				account := &Account{
					ID: 85, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
					Schedulable: true, Concurrency: 1,
					Credentials: map[string]any{"access_token": "test-token"},
					Extra:       astraHealthyTestExtra(map[string]any{"responses_websockets_v2_enabled": transport == "ws"}),
				}
				_, err := svc.Forward(context.Background(), c, account, []byte(tc.request))
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover)
				require.Equal(t, http.StatusBadRequest, rec.Code)
				requireImageUserErrorResponse(t, rec.Body.Bytes(), "image_generation_user_error")
				outbound := upstream.lastBody
				if transport == "ws" {
					require.Empty(t, upstream.requests)
					require.Len(t, conn.writes, 1)
					outbound, err = json.Marshal(conn.writes[0])
					require.NoError(t, err)
				} else {
					require.Len(t, upstream.requests, 1)
					require.Empty(t, conn.writes)
				}
				tool := gjson.GetBytes(outbound, "tools.0")
				require.Equal(t, tc.model, tool.Get("model").String())
				require.Equal(t, "gif", tool.Get("output_format").String())
				require.Equal(t, tc.hasFrames, tool.Get("frame_count").Exists())
				require.Equal(t, tc.hasFrames, tool.Get("frame_duration_ms").Exists())
				if tc.hasFrames {
					require.Equal(t, tc.count, tool.Get("frame_count").Int())
					require.Equal(t, tc.duration, tool.Get("frame_duration_ms").Int())
				}
				require.False(t, gjson.GetBytes(outbound, "frame_count").Exists())
				require.False(t, gjson.GetBytes(outbound, "frame_duration_ms").Exists())
			})
		}
	}
}
