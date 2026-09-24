package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func cookieAdapterDelta(text string) []byte {
	payload, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": text})
	return payload
}

func TestCookieWSIntelligentCompletionRepairsPartialAndChangedDeltas(t *testing.T) {
	for _, kind := range []string{"response.completed", "response.done"} {
		for _, delta := range []string{"", "<!DOCTYPE html><html>", pelicanCaptureTestHTML, "outdated text"} {
			t.Run(kind+"_"+delta, func(t *testing.T) {
				events := [][]byte{}
				if delta != "" {
					events = append(events, cookieAdapterDelta(delta))
				}
				terminal, _ := json.Marshal(intelligentCaptureCompletion(kind, pelicanCaptureTestHTML))
				events = append(events, terminal)
				conn := &openAIWSCaptureConn{events: events}
				gateway, account, _, _ := newCookieForwardFixture(t, conn)
				svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
				capture := &intelligentCapture{}
				writer := &intelligentSSEWriter{header: http.Header{}}
				c, _ := gin.CreateTestContext(writer)
				ctx := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{prompt: "Draw HTML", testType: "pelican", capture: capture})
				c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
				err := svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", "hi")
				require.NoError(t, err)
				record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
				require.NoError(t, finalizeIntelligentTestRun(record, capture, writer, "", "", "", err))
				require.Equal(t, pelicanCaptureTestHTML, record.Result)
				require.Equal(t, pelicanCaptureTestHTML, writer.outputText())
				require.Equal(t, 1, strings.Count(writer.body.String(), `"type":"content"`), "one final authoritative output without duplicates")
				require.True(t, writer.sawComplete)
			})
		}
	}
}

func TestCookieWSNormalCompletionPreservesStreamingAndOnlyAppendsMissingSuffix(t *testing.T) {
	for _, tc := range []struct {
		name, delta string
		success     bool
	}{
		{"missing_suffix", "<!DOCTYPE html><html>", true},
		{"complete", pelicanCaptureTestHTML, true},
		{"changed", "outdated text", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &openAIWSCaptureConn{events: [][]byte{cookieAdapterDelta(tc.delta), cookieWSCompletion("gpt-6-astra", pelicanCaptureTestHTML)}}
			gateway, account, _, _ := newCookieForwardFixture(t, conn)
			svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
			writer := &intelligentSSEWriter{header: http.Header{}}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
			err := svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", "Draw HTML")
			if tc.success {
				require.NoError(t, err)
				require.Equal(t, pelicanCaptureTestHTML, writer.outputText())
			} else {
				require.Error(t, err)
				require.Equal(t, tc.delta, writer.outputText())
				require.True(t, conn.closed)
			}
			require.Equal(t, tc.success, writer.sawComplete)
		})
	}
}

type cookieAdapterDeadlineConn struct {
	*openAIWSCaptureConn
	deadline time.Time
}

func (c *cookieAdapterDeadlineConn) WriteJSON(ctx context.Context, value any) error {
	c.deadline, _ = ctx.Deadline()
	return c.openAIWSCaptureConn.WriteJSON(ctx, value)
}

type cookieAdapterDeadlineDialer struct{ conn openAIWSClientConn }

func (d *cookieAdapterDeadlineDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	return d.conn, http.StatusSwitchingProtocols, http.Header{}, nil
}

func TestCookieWSIntelligentDeadlinePreservesConfiguredBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		intelligent  bool
		parent, want time.Duration
	}{{"intelligent_300", true, 300 * time.Second, 300 * time.Second}, {"intelligent_60", true, 60 * time.Second, 60 * time.Second}, {"normal_300", false, 300 * time.Second, 90 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			base := &openAIWSCaptureConn{events: [][]byte{cookieWSCompletion("gpt-6-astra", "hello")}}
			gateway, account, _, _ := newCookieForwardFixture(t, base)
			conn := &cookieAdapterDeadlineConn{openAIWSCaptureConn: base}
			gateway.getOpenAIWSConnPool().setClientDialerForTest(&cookieAdapterDeadlineDialer{conn: conn})
			svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
			ctx, cancel := context.WithTimeout(context.Background(), tc.parent)
			defer cancel()
			if tc.intelligent {
				ctx = context.WithValue(ctx, intelligentRunKey{}, &intelligentRunContext{prompt: "Draw HTML", testType: "pelican", capture: &intelligentCapture{}})
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
			require.NoError(t, svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", "hi"))
			require.WithinDuration(t, time.Now().Add(tc.want), conn.deadline, time.Second)
		})
	}
}

func TestCookieWSIntelligentInterruptedRunPreservesPartialDiagnosticText(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{cookieAdapterDelta("<html><svg>")}}
	gateway, account, _, _ := newCookieForwardFixture(t, conn)
	svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
	capture := &intelligentCapture{}
	writer := &intelligentSSEWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(writer)
	ctx := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{prompt: "Draw HTML", testType: "pelican", capture: capture})
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
	err := svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", "hi")
	require.Error(t, err)
	require.False(t, writer.sawComplete)
	require.True(t, conn.closed)
	record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
	require.Error(t, finalizeIntelligentTestRun(record, capture, writer, "", "", "", err))
	require.Equal(t, "<html><svg>", record.Result)
	require.Equal(t, "network_error", record.Status)
}

func TestCookieWSIntelligentRejectsUnsuccessfulTerminalAndCancellation(t *testing.T) {
	for _, kind := range []string{"response.failed", "response.incomplete", "response.cancelled", "response.canceled", "response.done"} {
		t.Run(kind, func(t *testing.T) {
			event := intelligentCaptureCompletion(kind, pelicanCaptureTestHTML)
			event["response"].(map[string]any)["status"] = "incomplete"
			terminal, _ := json.Marshal(event)
			conn := &openAIWSCaptureConn{events: [][]byte{cookieAdapterDelta("<html><svg>"), terminal}}
			gateway, account, _, _ := newCookieForwardFixture(t, conn)
			svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
			capture := &intelligentCapture{}
			writer := &intelligentSSEWriter{header: http.Header{}}
			c, _ := gin.CreateTestContext(writer)
			ctx := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{prompt: "Draw HTML", testType: "pelican", capture: capture})
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
			err := svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", "hi")
			require.Error(t, err)
			require.False(t, writer.sawComplete)
			require.False(t, capture.upstreamComplete())
			require.True(t, conn.closed)
			require.Equal(t, pelicanCaptureTestHTML, capture.outputText(), "failed terminal text is retained only for diagnosis")
		})
	}
	t.Run("parent_cancel", func(t *testing.T) {
		conn := &openAIWSCaptureConn{events: [][]byte{cookieAdapterDelta("<html><svg>"), cookieWSCompletion("gpt-6-astra", pelicanCaptureTestHTML)}, readDelays: []time.Duration{0, time.Second}}
		gateway, account, _, _ := newCookieForwardFixture(t, conn)
		svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
		capture := &intelligentCapture{}
		writer := &intelligentSSEWriter{header: http.Header{}}
		c, _ := gin.CreateTestContext(writer)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		ctx = context.WithValue(ctx, intelligentRunKey{}, &intelligentRunContext{prompt: "Draw HTML", testType: "pelican", capture: capture})
		c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
		err := svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", "hi")
		require.Error(t, err)
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		require.False(t, writer.sawComplete)
		require.True(t, conn.closed)
		require.Equal(t, "<html><svg>", capture.outputText())
	})
}

func TestCookieWSNormalDoneFalseProbeStillClosesConnection(t *testing.T) {
	event := intelligentCaptureCompletion("response.done", "False")
	terminal, _ := json.Marshal(event)
	conn := &openAIWSCaptureConn{events: [][]byte{terminal}}
	gateway, account, _, _ := newCookieForwardFixture(t, conn)
	svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
	writer := &intelligentSSEWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	require.NoError(t, svc.testOpenAICookieWSAccountConnection(c, account, "gpt-6-astra", openAICookieWSProbePrompt))
	require.True(t, conn.closed)
	require.True(t, writer.sawComplete)
	require.Equal(t, "False", writer.outputText())
	require.Len(t, conn.writes, 1)
}
