package repository

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type deviceGoldenRecord struct {
	Kind    string `json:"kind"`
	Method  string `json:"method"`
	Target  string `json:"target"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
}

func loadDeviceGolden(t *testing.T) []deviceGoldenRecord {
	t.Helper()
	var out []deviceGoldenRecord
	for _, name := range []string{"http_unauthorized_refresh.jsonl", "ws_tool_loop.jsonl"} {
		data, err := os.ReadFile(filepath.Join("..", "pkg", "codexwire", "testdata", "golden", "0.163.0", name))
		if err != nil {
			t.Fatalf("read golden: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var r deviceGoldenRecord
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
	}
	return out
}

// captureDeviceRequest sends req through the device lane to a plain-HTTP
// local server and returns the header names in wire order.
func captureDeviceRequest(t *testing.T, method, path string, set func(h http.Header), body string) []string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	names := make(chan []string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			names <- nil
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(c)
		_, _ = br.ReadString('\n')
		var got []string
		cl := 0
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			name, value, _ := strings.Cut(line, ":")
			got = append(got, name)
			if strings.EqualFold(name, "content-length") {
				cl = len(strings.TrimSpace(value))
				_ = cl
			}
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: 2\r\n\r\n{}")
		names <- got
	}()
	client, err := newCodexDeviceReqClient("", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := client.R()
	h := http.Header{}
	set(h)
	for k, v := range h {
		r.SetHeader(k, v[0])
	}
	if body != "" {
		r.SetBodyString(body)
	}
	url := "http://" + ln.Addr().String() + path
	if _, err := r.Send(method, url); err != nil {
		t.Fatalf("send: %v", err)
	}
	return <-names
}

func goldenNames(t *testing.T, recs []deviceGoldenRecord, method, targetSuffix string) []string {
	t.Helper()
	for _, r := range recs {
		if r.Kind == "http_request" && r.Method == method && strings.HasSuffix(strings.SplitN(r.Target, "?", 2)[0], targetSuffix) {
			var names []string
			for _, h := range r.Headers {
				names = append(names, h.Name)
			}
			return names
		}
	}
	t.Fatalf("golden has no %s %s", method, targetSuffix)
	return nil
}

func TestCodexDeviceLane_MatchesGoldenTokenRefresh(t *testing.T) {
	want := goldenNames(t, loadDeviceGolden(t), "POST", "/oauth/token")
	got := captureDeviceRequest(t, "POST", "/oauth/token", func(h http.Header) {
		h.Set("User-Agent", "codex-tui/0.163.0 (Ubuntu 26.4.0; x86_64) xterm-256color")
		h.Set("originator", "codex-tui")
		h.Set("Content-Type", "application/json")
	}, `{"client_id":"x","grant_type":"refresh_token","refresh_token":"y"}`)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("token refresh header order:\n ours   %v\n golden %v", got, want)
	}
}

func TestCodexDeviceLane_MatchesGoldenAccountsCheck(t *testing.T) {
	want := goldenNames(t, loadDeviceGolden(t), "GET", "/wham/accounts/check")
	got := captureDeviceRequest(t, "GET", "/backend-api/wham/accounts/check", func(h http.Header) {
		h.Set("User-Agent", "codex-tui/0.163.0 (Ubuntu 26.4.0; x86_64) xterm-256color")
		h.Set("Authorization", "Bearer t")
		h.Set("ChatGPT-Account-Id", "acct")
	}, "")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("backend-client header order:\n ours   %v\n golden %v", got, want)
	}
}

func TestCodexDeviceLane_MatchesGoldenModels(t *testing.T) {
	want := goldenNames(t, loadDeviceGolden(t), "GET", "/codex/models")
	got := captureDeviceRequest(t, "GET", "/backend-api/codex/models?client_version=0.163.0", func(h http.Header) {
		h.Set("version", "0.163.0")
		h.Set("Authorization", "Bearer t")
		h.Set("ChatGPT-Account-Id", "acct")
		h.Set("originator", "codex-tui")
		h.Set("User-Agent", "codex-tui/0.163.0 (Ubuntu 26.4.0; x86_64) xterm-256color")
	}, "")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models header order:\n ours   %v\n golden %v", got, want)
	}
}

func TestCodexDeviceLane_DropsBrowserHeaders(t *testing.T) {
	got := captureDeviceRequest(t, "GET", "/backend-api/wham/usage", func(h http.Header) {
		h.Set("User-Agent", "codex-tui/0.163.0")
		h.Set("Authorization", "Bearer t")
		h.Set("Origin", "https://chatgpt.com")
		h.Set("sec-fetch-mode", "cors")
		h.Set("oai-language", "zh-CN")
	}, "")
	for _, n := range got {
		switch strings.ToLower(n) {
		case "origin", "sec-fetch-mode", "oai-language", "accept-encoding":
			t.Fatalf("browser header leaked: %v", got)
		}
	}
}
