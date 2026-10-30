//go:build linux

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

type gatewayConnectProxy struct {
	server      *httptest.Server
	mu          sync.Mutex
	connections map[net.Conn]bool
	targets     map[string]int
	denied      []string
}

// Every CONNECT terminates at the local recorder. No hostname from the request
// is ever resolved or dialled. Denials are retained as failed evidence.
func newGatewayConnectProxy(upstream string) *gatewayConnectProxy {
	p := &gatewayConnectProxy{connections: make(map[net.Conn]bool), targets: make(map[string]int)}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || (r.Host != "chatgpt.com:443" && r.Host != "auth.openai.com:443") {
			p.mu.Lock()
			p.denied = append(p.denied, r.Method+" "+r.Host)
			p.mu.Unlock()
			http.Error(w, "target denied by local recorder", http.StatusForbidden)
			return
		}
		remote, err := net.DialTimeout("tcp", upstream, 5*time.Second)
		if err != nil {
			http.Error(w, "local recorder unavailable", http.StatusBadGateway)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = remote.Close()
			return
		}
		p.mu.Lock()
		p.targets[r.Host]++
		p.connections[client] = true
		p.connections[remote] = true
		p.mu.Unlock()
		defer func() {
			_ = client.Close()
			_ = remote.Close()
			p.mu.Lock()
			delete(p.connections, client)
			delete(p.connections, remote)
			p.mu.Unlock()
		}()
		if _, err = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(remote, buffer); _ = remote.Close(); close(done) }()
		_, _ = io.Copy(client, remote)
		_ = client.Close()
		<-done
	}))
	return p
}
func (p *gatewayConnectProxy) close() {
	p.mu.Lock()
	for c := range p.connections {
		_ = c.Close()
	}
	p.mu.Unlock()
	p.server.Close()
}

// routeGatewayRequest sits after the local TLS terminator. Bootstrap requests
// keep the original socket recorder; core requests go to the actual gateway.
// It never constructs an upstream request or a candidate recording itself.
func routeGatewayRequest(ingress string, transport *http.Transport, recordError func(error)) func(net.Conn, *bufio.Reader, string, string, string, []headerKV) (bool, bool) {
	return func(c net.Conn, br *bufio.Reader, method, target, proto string, headers []headerKV) (bool, bool) {
		path := strings.SplitN(target, "?", 2)[0]
		if path != "/backend-api/codex/responses" && path != "/backend-api/codex/models" {
			return false, false
		}
		get := func(name string) string {
			for _, h := range headers {
				if strings.EqualFold(h.Name, name) {
					return h.Value
				}
			}
			return ""
		}
		if strings.EqualFold(get("upgrade"), "websocket") {
			conn, err := net.DialTimeout("tcp", ingress, 5*time.Second)
			if err != nil {
				recordError(err)
				return true, true
			}
			defer conn.Close()
			var head bytes.Buffer
			fmt.Fprintf(&head, "%s %s %s\r\n", method, target, proto)
			for _, h := range headers {
				fmt.Fprintf(&head, "%s: %s\r\n", h.Name, h.Value)
			}
			head.WriteString("\r\n")
			if _, err := conn.Write(head.Bytes()); err != nil {
				recordError(err)
				return true, true
			}
			done := make(chan struct{})
			go func() { _, _ = io.Copy(conn, br); _ = conn.Close(); close(done) }()
			_, _ = io.Copy(c, conn)
			_ = c.Close()
			<-done
			return true, true
		}
		body, _, err := readBody(br, get("content-length"), get("transfer-encoding"))
		if err != nil {
			recordError(err)
			return true, true
		}
		req, err := http.NewRequest(method, "http://"+ingress+target, bytes.NewReader(body))
		if err != nil {
			recordError(err)
			return true, true
		}
		for _, h := range headers {
			req.Header.Add(h.Name, h.Value)
		}
		req.Host = get("host")
		req.Header.Del("Content-Length")
		req.Header.Del("Transfer-Encoding")
		response, err := transport.RoundTrip(req)
		if err != nil {
			recordError(err)
			return true, true
		}
		defer response.Body.Close()
		if err := response.Write(c); err != nil {
			recordError(err)
			return true, true
		}
		return true, false
	}
}
