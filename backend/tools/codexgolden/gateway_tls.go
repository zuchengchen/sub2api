//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

// Capture the default TLS lanes through real Forward/WS ingress. The local
// listener records ClientHello then closes, just like recordTLS for the CLI.
// No trust bypass or external connection is needed to observe this boundary.
func recordGatewayNativeTLS() ([]map[string]any, error) {
	hellos := newHelloSet()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				raw, err := readTLSRecord(bufio.NewReader(conn))
				if err != nil {
					return
				}
				hello, err := parseClientHello(raw)
				if err == nil {
					hellos.add(hello)
				}
			}()
		}
	}()
	proxy := newGatewayConnectProxy(ln.Addr().String())
	defer proxy.close()
	proxyURL, _ := url.Parse(proxy.server.URL)
	port, _ := strconv.Atoi(proxyURL.Port())
	p := &service.Proxy{ID: 1, Protocol: "http", Host: "127.0.0.1", Port: port, Status: service.StatusActive}
	account := &service.Account{ID: 900, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Concurrency: 1, ProxyID: &p.ID, Proxy: p,
		Credentials: map[string]any{"access_token": "synthetic-native-tls", "chatgpt_account_id": "synthetic-native-account"},
		Extra:       map[string]any{"openai_passthrough": true, "openai_oauth_responses_websockets_v2_mode": "passthrough", "openai_oauth_responses_websockets_v2_enabled": true}}
	accounts := &gatewayAccounts{account: account}
	for _, websocket := range []bool{false, true} {
		cfg := &config.Config{}
		disabled := false
		cfg.Gateway.OpenAITiboRoute.Enabled = &disabled
		cfg.Gateway.OpenAIWS.Enabled = websocket
		cfg.Gateway.OpenAIWS.OAuthEnabled = websocket
		cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = websocket
		cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = websocket
		cfg.Gateway.OpenAIWS.IngressModeDefault = "passthrough"
		cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
		cfg.Gateway.OpenAIWS.CodexHTTPH2Enabled = false
		cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 2
		svc := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, repository.NewHTTPUpstream(cfg), nil, nil, nil, nil, nil, nil, nil, nil)
		svc.StopOpenAICodexTicketHarvester()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		body := []byte(`{"model":"gpt-5.5","instructions":"Reply briefly.","input":[{"role":"user","content":"ok"}],"stream":true,"store":false}`)
		if !websocket {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body))).WithContext(ctx)
			c.Request.Header.Set("User-Agent", "codex_cli_rs/0.166.0")
			_, forwardErr := svc.Forward(ctx, c, account, body)
			if !hellos.hasLanes("openssl") {
				cancel()
				svc.CloseOpenAIWSPool()
				return nil, fmt.Errorf("native HTTP did not reach TLS capture: %v", forwardErr)
			}
		} else {
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				_, first, err := conn.Read(ctx)
				if err != nil {
					return
				}
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				_ = svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "synthetic-native-tls", first, nil)
			}))
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{HTTPHeader: http.Header{"User-Agent": {"codex_cli_rs/0.166.0"}}})
			if err == nil {
				err = conn.Write(ctx, coderws.MessageText, append([]byte(`{"type":"response.create",`), body[1:]...))
				if err == nil {
					select {
					case <-done:
					case <-ctx.Done():
					}
				}
				_ = conn.CloseNow()
			}
			cancel()
			server.Close()
			if err != nil {
				svc.CloseOpenAIWSPool()
				return nil, fmt.Errorf("native TLS WS ingress: %w", err)
			}
		}
		cancel()
		svc.CloseOpenAIWSPool()
	}
	if !hellos.hasLanes("rustls", "openssl") {
		return nil, fmt.Errorf("gateway native TLS capture missing HTTP or WS lane")
	}
	rows := hellos.export()
	for _, row := range rows {
		row["server_name"] = "localhost"
	}
	return rows, nil
}
