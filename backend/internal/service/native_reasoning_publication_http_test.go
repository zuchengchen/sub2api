package service_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// This wrapper holds the first publication before the backing store's lock.
// A premature Get therefore returns a real miss instead of accidentally being
// serialized behind Put by the test store itself.
type nativePublicationHTTPStore struct {
	*nativeTrafficStore
	once      sync.Once
	entered   chan nativePublicationHTTPWrite
	release   chan struct{}
	finished  chan error
	lookups   chan []string
	premature chan []string
}

type nativePublicationHTTPWrite struct {
	value service.NativeReasoningReplayWrite
	ctx   context.Context
}

func (s *nativePublicationHTTPStore) PutNativeReasoningReplay(ctx context.Context, value service.NativeReasoningReplayWrite, limits service.NativeReasoningReplayLimits) error {
	controlled := false
	s.once.Do(func() { controlled = true })
	if controlled {
		s.entered <- nativePublicationHTTPWrite{value: value, ctx: ctx}
		select {
		case <-s.release:
		case <-ctx.Done():
			s.finished <- ctx.Err()
			return ctx.Err()
		}
	}
	err := s.nativeTrafficStore.PutNativeReasoningReplay(ctx, value, limits)
	if controlled {
		s.finished <- errors.Join(err, ctx.Err())
	}
	return err
}

func (s *nativePublicationHTTPStore) GetNativeReasoningReplay(ctx context.Context, owner string, keys []string, maxBytes int) (map[string]service.NativeReasoningReplayValue, error) {
	observed := append([]string(nil), keys...)
	s.lookups <- observed
	select {
	case <-s.release:
	default:
		select {
		case s.premature <- observed:
		default:
		}
	}
	return s.nativeTrafficStore.GetNativeReasoningReplay(ctx, owner, keys, maxBytes)
}

func TestNativeReasoningReplayPublicationImmediateNextTurnWaitsForCompletePut(t *testing.T) {
	t.Run("body_open", func(t *testing.T) { testNativePublicationHTTP(t, false, false) })
	t.Run("done_close_and_cancel", func(t *testing.T) { testNativePublicationHTTP(t, true, false) })
}

func TestNativeReasoningReplayAssistantMessageHTTPPublicationAfterDONEClose(t *testing.T) {
	testNativePublicationHTTP(t, true, true)
}

func testNativePublicationHTTP(t *testing.T, closeAfterDONE, assistantOnly bool) {
	store := &nativePublicationHTTPStore{
		entered: make(chan nativePublicationHTTPWrite, 1), release: make(chan struct{}),
		finished: make(chan error, 1), lookups: make(chan []string, 4), premature: make(chan []string, 1),
	}
	h := newNativeTrafficHarness(t, "replay", func(base service.NativeReasoningReplayStore) service.NativeReasoningReplayStore {
		store.nativeTrafficStore = base.(*nativeTrafficStore)
		return store
	})
	if assistantOnly {
		h.mu.Lock()
		h.outputOverrides = map[int][]json.RawMessage{1: nativeTrafficAssistantOutput(1, true), 2: nativeTrafficAssistantOutput(2, true)}
		h.mu.Unlock()
	}
	firstContext := make(chan context.Context, 1)
	oldHandler := h.server.Config.Handler
	var firstRequest sync.Once
	h.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstRequest.Do(func() { firstContext <- r.Context() })
		oldHandler.ServeHTTP(w, r)
	})
	client := h.server.Client()
	client.Timeout = 3 * time.Second
	released := false
	defer func() {
		if !released {
			close(store.release)
		}
	}()
	tools := []any{}
	for _, name := range []string{"lookup_temperature", "lookup_humidity"} {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": name, "parameters": map[string]any{"type": "object", "properties": map[string]any{"location": map[string]any{"type": "string"}}, "required": []string{"location"}}}})
	}
	request := func(history []json.RawMessage) *http.Request {
		body := nativeTrafficJSON(map[string]any{"model": "gpt-6-astra", "messages": history, "stream": true, "tools": tools})
		r, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", bytes.NewReader(body))
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("session-id", "deterministic-publication-session")
		if !assistantOnly {
			r.Header.Set("turn-id", "019a4e08-b000-7000-8000-000000000001")
			r.Header.Set("x-codex-turn-metadata", `{"turn_id":"019a4e08-b000-7000-8000-000000000001","turn_started_at_unix_ms":1793980000000}`)
		}
		return r
	}
	history := nativeTrafficInitialHistory()
	callCtx, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()
	firstConnection := make(chan net.Conn, 1)
	var firstConnectionOnce sync.Once
	callCtx = httptrace.WithClientTrace(callCtx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		firstConnectionOnce.Do(func() { firstConnection <- info.Conn })
	}})
	response, err := client.Do(request(history).WithContext(callCtx))
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	reader := bufio.NewReader(response.Body)
	var received bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		require.NoError(t, err)
		received.Write(line)
		if bytes.Equal(bytes.TrimSpace(line), []byte("data: [DONE]")) {
			blank, err := reader.ReadBytes('\n')
			require.NoError(t, err)
			received.Write(blank)
			break
		}
	}
	assistant, err := nativeTrafficDecodeChatSSE(received.Bytes())
	require.NoError(t, err)
	nextHistory := nativeTrafficContinue(history, assistant)
	if assistantOnly {
		nextHistory = append(nextHistory, json.RawMessage(`{"role":"user","content":"Continue after the completed assistant."}`))
	}
	follow := request(nextHistory)
	followConnection := make(chan net.Conn, 1)
	var followConnectionOnce sync.Once
	follow = follow.WithContext(httptrace.WithClientTrace(follow.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		followConnectionOnce.Do(func() { followConnection <- info.Conn })
	}}))
	secondDone := make(chan error, 1)
	go func() {
		second, err := client.Do(follow)
		if err == nil {
			_, err = io.Copy(io.Discard, second.Body)
			_ = second.Body.Close()
			if err == nil && second.StatusCode != http.StatusOK {
				err = errors.New("continuation returned non-success status")
			}
		}
		secondDone <- err
	}()
	var firstWrite nativePublicationHTTPWrite
	select {
	case firstWrite = <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("production Commit did not enter controlled Put after client DONE")
	}
	// The next request was already started after DONE, without waiting for Put
	// or cancellation. Reaching this store proves that the first publication has
	// handed off to its original bounded context before cancellation is induced.
	if closeAfterDONE {
		firstSocket := <-firstConnection
		select {
		case nextSocket := <-followConnection:
			require.NotSame(t, firstSocket, nextSocket, "the pending response must not share its connection with the immediate next request")
		case <-firstWrite.ctx.Done():
			t.Fatal("next request did not connect within the original publication budget")
		}
		// Body.Close may return before Go's optional response draining and peer
		// cancellation. Close this exact client socket, then observe cancellation
		// on the actual first server request rather than assuming a fixed delay.
		require.NoError(t, firstSocket.Close())
		require.NoError(t, response.Body.Close())
		cancelCall()
		serverContext := <-firstContext
		select {
		case <-serverContext.Done():
			require.ErrorIs(t, serverContext.Err(), context.Canceled)
		case keys := <-store.premature:
			t.Fatalf("next lookup bypassed the pending publication before cancellation was observed: %v", keys)
		case <-firstWrite.ctx.Done():
			t.Fatal("real first ingress cancellation was not observed within the original publication budget")
		}
	}
	var premature []string
	select {
	case premature = <-store.premature:
	case <-time.After(20 * time.Millisecond):
	}
	require.Empty(t, premature, "next lookup must wait for the matching publication")
	close(store.release)
	released = true
	select {
	case err := <-store.finished:
		require.NoError(t, err, "publication must finish inside its original 100ms I/O budget")
	case <-time.After(time.Second):
		t.Fatal("controlled Put did not finish")
	}
	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("immediate continuation did not finish")
	}
	if !closeAfterDONE {
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
	}
	h.mu.Lock()
	rows := append([]nativeTrafficRow(nil), h.rows...)
	h.mu.Unlock()
	require.Len(t, rows, 2)
	if assistantOnly {
		require.Equal(t, 0, nativeTrafficCompareOutputBlock(rows[1].Body, nativeTrafficAssistantOutput(1, true), true, false))
		nativeAssistantRequireNewTurnStablePrefix(t, rows[0], rows[1])
	} else {
		require.Equal(t, 0, nativeTrafficComparisonExit(rows[1].Body, nativeTrafficOutput(1)), "the actual next upstream request must retain the whole published native array")
	}
	var observed []string
	for len(store.lookups) > 0 {
		observed = append(observed, (<-store.lookups)...)
	}
	require.Contains(t, observed, firstWrite.value.Key, "eventual lookup must request the exact completed assistant key")
}
