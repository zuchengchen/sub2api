package repository

import (
	"bufio"
	"context"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type auxiliaryRawRequest struct {
	connection int32
	names      []string
	body       string
}

func auxiliaryRawServer(t *testing.T) (string, <-chan auxiliaryRawRequest) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	seen := make(chan auxiliaryRawRequest, 16)
	var serial atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			id := serial.Add(1)
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				for {
					if _, err := reader.ReadString('\n'); err != nil {
						return
					}
					row := auxiliaryRawRequest{connection: id}
					length := 0
					for {
						line, err := reader.ReadString('\n')
						if err != nil {
							return
						}
						if line == "\r\n" {
							break
						}
						name, value, _ := strings.Cut(strings.TrimSpace(line), ":")
						row.names = append(row.names, name)
						if name == "content-length" {
							length, _ = strconv.Atoi(strings.TrimSpace(value))
						}
					}
					body := make([]byte, length)
					if _, err := io.ReadFull(reader, body); err != nil {
						return
					}
					row.body = string(body)
					seen <- row
					_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: 51\r\n\r\n{\"access_token\":\"synthetic\",\"refresh_token\":\"next\"}")
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return "http://" + listener.Addr().String(), seen
}

func TestCodexAuxiliaryH1RawHeadersAndScopedReuse(t *testing.T) {
	t.Setenv(codexDeviceClientDisabledEnv, "")
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	t.Setenv("SSL_CERT_FILE", "")
	origin, seen := auxiliaryRawServer(t)
	client, err := newCodexDeviceReqClient("", time.Second)
	require.NoError(t, err)
	get := func(path, account, token string) auxiliaryRawRequest {
		t.Helper()
		_, err := client.R().SetHeader("User-Agent", "codex-synthetic").SetHeader("Authorization", "Bearer "+token).SetHeader("Chatgpt-Account-Id", account).Get(origin + path)
		require.NoError(t, err)
		return <-seen
	}
	a := get("/backend-api/wham/usage", "acct-a", "synthetic-a")
	b := get("/backend-api/wham/rate-limit-reset-credits", "acct-a", "synthetic-a")
	require.Equal(t, a.connection, b.connection)
	require.Equal(t, []string{"user-agent", "authorization", "chatgpt-account-id", "accept", "host"}, a.names, "GET must have neither Content-Type nor extra browser headers")
	require.Equal(t, a.names, b.names)
	other := get("/backend-api/wham/usage", "acct-b", "synthetic-b")
	require.NotEqual(t, a.connection, other.connection)
	for _, path := range []string{"/backend-api/accounts/check", "/backend-api/wham/settings/user", "/unrelated"} {
		first := get(path, "acct-a", "synthetic-a")
		second := get(path, "acct-a", "synthetic-a")
		require.NotEqual(t, first.connection, second.connection, "web/unknown endpoints retain fresh connections")
	}
}

func TestOAuthCodeExchangeOmitsAuthManagerHeaders(t *testing.T) {
	t.Setenv(codexDeviceClientDisabledEnv, "")
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	t.Setenv("SSL_CERT_FILE", "")
	origin, seen := auxiliaryRawServer(t)
	svc := &openaiOAuthService{tokenURL: origin + "/oauth/token"}
	_, err := svc.ExchangeCode(context.Background(), "synthetic-code", "synthetic-verifier", "", "", "")
	require.NoError(t, err)
	row := <-seen
	require.Equal(t, []string{"content-type", "accept", "host", "content-length"}, row.names)
	require.True(t, strings.HasPrefix(row.body, "grant_type=authorization_code&client_id="))
}

func TestCodexLoginClientConnectionIsNotSharedAcrossLogins(t *testing.T) {
	t.Setenv(codexDeviceClientDisabledEnv, "")
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	t.Setenv("SSL_CERT_FILE", "")
	origin, seen := auxiliaryRawServer(t)
	first, err := newCodexAuthCodeReqClient("", time.Second)
	require.NoError(t, err)
	second, err := newCodexAuthCodeReqClient("", time.Second)
	require.NoError(t, err)
	post := func(clientIndex int) auxiliaryRawRequest {
		t.Helper()
		client := first
		if clientIndex == 2 {
			client = second
		}
		_, err := client.R().SetHeader("Content-Type", "application/x-www-form-urlencoded").SetBodyString("grant_type=synthetic").Post(origin + "/oauth/token")
		require.NoError(t, err)
		return <-seen
	}
	a, b := post(1), post(1)
	require.Equal(t, a.connection, b.connection, "related calls on one login client reuse its socket")
	require.NotEqual(t, a.connection, post(2).connection, "independent login sessions cannot share")
	require.Equal(t, []string{"content-type", "accept", "host", "content-length"}, a.names)
}

func TestCodexQuotaH1ParallelCallsAndCancellation(t *testing.T) {
	t.Setenv(codexDeviceClientDisabledEnv, "")
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	t.Setenv("SSL_CERT_FILE", "")
	started := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/backend-api/wham/usage" {
			started <- r.RemoteAddr
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, `{"peer":"`+r.RemoteAddr+`"}`)
	}))
	defer srv.Close()
	client, err := newCodexDeviceReqClient("", 3*time.Second)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.R().SetContext(ctx).SetHeader("Authorization", "Bearer synthetic-a").Get(srv.URL + "/backend-api/wham/usage")
		done <- err
	}()
	firstPeer := <-started
	resp, err := client.R().SetHeader("Authorization", "Bearer synthetic-a").Get(srv.URL + "/backend-api/wham/rate-limit-reset-credits")
	require.NoError(t, err)
	require.NotContains(t, resp.String(), firstPeer, "parallel HTTP1 response bodies must own separate sockets")
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("canceled quota request did not exit")
	}
}

func TestCodexLoginCustomCAUsesPrivateH2WithoutAuthManagerHeaders(t *testing.T) {
	t.Setenv(codexDeviceClientDisabledEnv, "")
	t.Setenv("SSL_CERT_FILE", "")
	type observation struct {
		peer, userAgent, originator string
		proto                       int
	}
	seen := make(chan observation, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		seen <- observation{r.RemoteAddr, r.Header.Get("User-Agent"), r.Header.Get("originator"), r.ProtoMajor}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "login-ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	first, err := newCodexAuthCodeReqClient("", time.Second)
	require.NoError(t, err)
	second, err := newCodexAuthCodeReqClient("", time.Second)
	require.NoError(t, err)
	var peers []string
	for _, client := range []int{1, 1, 2} {
		selected := first
		if client == 2 {
			selected = second
		}
		_, err := selected.R().SetHeader("Content-Type", "application/x-www-form-urlencoded").SetBodyString("grant_type=synthetic").Post(srv.URL + "/oauth/token")
		require.NoError(t, err)
		got := <-seen
		require.Equal(t, 2, got.proto)
		require.Empty(t, got.userAgent)
		require.Empty(t, got.originator)
		peers = append(peers, got.peer)
	}
	require.Equal(t, peers[0], peers[1])
	require.NotEqual(t, peers[0], peers[2])
	badCA := filepath.Join(t.TempDir(), "invalid.pem")
	require.NoError(t, os.WriteFile(badCA, []byte("not a certificate"), 0600))
	t.Setenv("CODEX_CA_CERTIFICATE", badCA)
	_, err = newCodexAuthCodeReqClient("", time.Second)
	require.Error(t, err, "login's strict CA builder does not use AuthManager's construction fallback")
}
