package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func publicationFixture(t *testing.T) (*nativeReasoningChat, NativeReasoningReplayRequest, NativeReasoningReplayCompletion) {
	engine, _, request, completion := nativeReplayFixture(t)
	runtime := &nativeReasoningRuntime{engine: engine, limits: engine.limits}
	return &nativeReasoningChat{runtime: runtime, request: request, eligible: true}, request, completion
}

func TestNativeReasoningReplayPublicationMatchesExactPreAssistantPrefix(t *testing.T) {
	producer, request, completion := publicationFixture(t)
	p, admitted := producer.preparePublication(context.Background())
	require.True(t, admitted)
	defer producer.releasePublication()
	consumer := &nativeReasoningChat{runtime: producer.runtime, request: nativeReplayContinuation(request, completion)}
	prepared, err := producer.runtime.engine.prepare(consumer.request)
	require.NoError(t, err)
	candidates := nativeReplayRestoreCandidates(prepared.messages, 32)
	require.Equal(t, []int{len(request.Messages)}, candidates)
	require.Equal(t, p.keys[len(p.keys)-1], prepared.keys[candidates[0]-1])
	require.NotEqual(t, producer.publication.entry.key, prepared.keys[len(prepared.keys)-1], "a request cannot wait on its own whole-history key")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err = consumer.restoreAfterPublication(ctx, prepared)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	store := producer.runtime.engine.store.(*nativeReplayTestStore)
	require.Zero(t, store.gets, "a pending predecessor must block before the first cache lookup")
	require.NoError(t, producer.runtime.engine.Commit(context.Background(), request, completion))
	producer.releasePublication()
	replacements, err := consumer.restoreAfterPublication(context.Background(), prepared)
	require.NoError(t, err)
	require.Len(t, replacements, 1)
	require.Equal(t, prepared.keys[candidates[0]], replacements[0].Reference.Key, "the actual completed assistant key remains unchanged")
}

func TestNativeReasoningReplayPublicationScopeParametersAndHistoryDoNotWait(t *testing.T) {
	for _, fault := range []string{"user", "key", "session", "model", "credential", "parameters", "history", "incomplete_pair"} {
		t.Run(fault, func(t *testing.T) {
			producer, request, completion := publicationFixture(t)
			_, admitted := producer.preparePublication(context.Background())
			require.True(t, admitted)
			defer producer.releasePublication()
			next := nativeReplayContinuation(request, completion)
			switch fault {
			case "user":
				next.Scope.UserID++
			case "key":
				next.Scope.APIKeyID++
			case "session":
				next.Scope.SessionID += "other"
			case "model":
				next.Scope.Model += "other"
			case "credential":
				next.Scope.CredentialIdentity += "other"
			case "parameters":
				next.Parameters = json.RawMessage(`{"unknown_semantic_parameter":true}`)
			case "history":
				next.Messages[0] = json.RawMessage(`{"role":"system","content":"edited"}`)
			case "incomplete_pair":
				next.Messages = next.Messages[:len(next.Messages)-1]
			}
			p, err := producer.runtime.engine.prepare(next)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			require.NoError(t, producer.runtime.publications.wait(ctx, p, 32), "an unrelated or unpaired history has no publication dependency")
			require.NoError(t, ctx.Err())
		})
	}
}

func TestNativeReasoningReplayPublicationParallelProducersReleaseOnlyLastTicket(t *testing.T) {
	first, request, completion := publicationFixture(t)
	second := &nativeReasoningChat{runtime: first.runtime, request: request}
	_, ok := first.preparePublication(context.Background())
	require.True(t, ok)
	_, ok = second.preparePublication(context.Background())
	require.True(t, ok)
	require.Same(t, first.publication.entry, second.publication.entry)
	require.Equal(t, 2, first.publication.entry.pending)
	_, ok = first.preparePublication(context.Background())
	require.True(t, ok)
	require.Equal(t, 2, first.publication.entry.pending, "same request retry owns just one ticket")
	p, err := first.runtime.engine.prepare(nativeReplayContinuation(request, completion))
	require.NoError(t, err)
	first.releasePublication()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, first.runtime.publications.wait(ctx, p, 32), context.DeadlineExceeded)
	second.releasePublication()
	require.NoError(t, first.runtime.publications.wait(context.Background(), p, 32))
	second.releasePublication()
	require.Empty(t, first.runtime.publications.entries)
	require.Empty(t, first.runtime.publications.perOwner)
}

func TestNativeReasoningReplayPublicationCapacityDoesNotEvictOrBlockOtherOwners(t *testing.T) {
	var registry nativeReasoningPublications
	limits := DefaultNativeReasoningReplayLimits()
	limits.MaxEntriesGlobal, limits.MaxEntriesPerUser = 3, 2
	first := registry.acquire(context.Background(), "owner-a", "key-a", limits)
	second := registry.acquire(context.Background(), "owner-a", "key-b", limits)
	require.NotNil(t, first)
	require.NotNil(t, second)
	require.Nil(t, registry.acquire(context.Background(), "owner-a", "key-over-user", limits))
	other := registry.acquire(context.Background(), "owner-b", "key-other", limits)
	require.NotNil(t, other)
	require.Nil(t, registry.acquire(context.Background(), "owner-c", "key-over-global", limits))
	require.Len(t, registry.entries, 3)
	parallel := registry.acquire(context.Background(), "owner-a", "key-a", limits)
	require.NotNil(t, parallel, "a parallel producer shares an already admitted entry")
	require.Same(t, first.entry, parallel.entry)
	first.release()
	second.release()
	other.release()
	parallel.release()
	require.Empty(t, registry.entries)
	require.Empty(t, registry.perOwner)
}

func TestNativeReasoningReplayPublicationHardLimitsAndCanceledCleanup(t *testing.T) {
	var registry nativeReasoningPublications
	limits := DefaultNativeReasoningReplayLimits()
	leases := make([]*nativeReasoningPublicationLease, 0, 1024)
	for owner := 0; owner < 16; owner++ {
		for entry := 0; entry < 64; entry++ {
			lease := registry.acquire(context.Background(), fmt.Sprint(owner), fmt.Sprintf("%d/%d", owner, entry), limits)
			require.NotNil(t, lease)
			leases = append(leases, lease)
		}
		require.Nil(t, registry.acquire(context.Background(), fmt.Sprint(owner), fmt.Sprintf("%d/overflow", owner), limits))
	}
	require.Len(t, registry.entries, 1024)
	require.Nil(t, registry.acquire(context.Background(), "new-user", "new-key", limits))
	for _, lease := range leases {
		lease.release()
	}
	ctx, cancel := context.WithCancel(context.Background())
	lease := registry.acquire(ctx, "cancel-owner", "cancel-key", limits)
	require.NotNil(t, lease)
	cancel()
	select {
	case <-lease.entry.done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release publication")
	}
	lease.release()
	registry.mu.Lock()
	require.Empty(t, registry.entries)
	require.Empty(t, registry.perOwner)
	registry.mu.Unlock()
	require.Nil(t, registry.acquire(ctx, "cancel-owner", "cancel-key", limits))
}

type publicationDeadlineStore struct {
	NativeReasoningReplayStore
	deadline chan time.Time
}

func (s *publicationDeadlineStore) GetNativeReasoningReplay(ctx context.Context, _ string, _ []string, _ int) (map[string]NativeReasoningReplayValue, error) {
	deadline, _ := ctx.Deadline()
	s.deadline <- deadline
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestNativeReasoningReplayPublicationWaitAndStoreReadShareOneBudget(t *testing.T) {
	producer, request, completion := publicationFixture(t)
	_, admitted := producer.preparePublication(context.Background())
	require.True(t, admitted)
	store := &publicationDeadlineStore{NativeReasoningReplayStore: producer.runtime.engine.store, deadline: make(chan time.Time, 1)}
	producer.runtime.engine.store = store
	consumer := &nativeReasoningChat{runtime: producer.runtime, request: nativeReplayContinuation(request, completion)}
	prepared, err := producer.runtime.engine.prepare(consumer.request)
	require.NoError(t, err)
	start := time.Now()
	timer := time.AfterFunc(30*time.Millisecond, producer.publication.release)
	defer timer.Stop()
	_, err = consumer.restoreAfterPublication(context.Background(), prepared)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var deadline time.Time
	select {
	case deadline = <-store.deadline:
	case <-time.After(time.Second):
		t.Fatal("store was not reached after publication release")
	}
	require.WithinDuration(t, start.Add(100*time.Millisecond), deadline, 5*time.Millisecond, "Get must inherit the original deadline, not start a second 100ms budget")
}

func TestNativeReasoningReplayPublicationMultiplePredecessorsShareWaitBudget(t *testing.T) {
	first, request, completion := publicationFixture(t)
	_, ok := first.preparePublication(context.Background())
	require.True(t, ok)
	secondRequest := nativeReplayContinuation(request, completion)
	second := &nativeReasoningChat{runtime: first.runtime, request: secondRequest}
	_, ok = second.preparePublication(context.Background())
	require.True(t, ok)
	defer first.releasePublication()
	defer second.releasePublication()
	next := secondRequest
	next.Messages = append(append([]json.RawMessage(nil), secondRequest.Messages...),
		json.RawMessage(strings.ReplaceAll(string(completion.DeliveredAssistant), "call_synthetic", "call_second")),
		json.RawMessage(`{"role":"tool","tool_call_id":"call_second","content":"synthetic second result"}`))
	prepared, err := first.runtime.engine.prepare(next)
	require.NoError(t, err)
	require.Len(t, nativeReplayRestoreCandidates(prepared.messages, 32), 2)
	store := &publicationDeadlineStore{NativeReasoningReplayStore: first.runtime.engine.store, deadline: make(chan time.Time, 1)}
	first.runtime.engine.store = store
	consumer := &nativeReasoningChat{runtime: first.runtime, request: next}
	start := time.Now()
	a := time.AfterFunc(20*time.Millisecond, first.publication.release)
	b := time.AfterFunc(45*time.Millisecond, second.publication.release)
	defer a.Stop()
	defer b.Stop()
	_, err = consumer.restoreAfterPublication(context.Background(), prepared)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var deadline time.Time
	select {
	case deadline = <-store.deadline:
	case <-time.After(time.Second):
		t.Fatal("store was not reached after both predecessors released")
	}
	require.WithinDuration(t, start.Add(100*time.Millisecond), deadline, 5*time.Millisecond)
}

func TestNativeReasoningReplayPublicationSameVisibleHistoryForkRemainsAmbiguous(t *testing.T) {
	first, request, completion := publicationFixture(t)
	second := &nativeReasoningChat{runtime: first.runtime, request: request}
	_, ok := first.preparePublication(context.Background())
	require.True(t, ok)
	_, ok = second.preparePublication(context.Background())
	require.True(t, ok)
	require.NoError(t, first.runtime.engine.Commit(context.Background(), request, completion))
	first.releasePublication()
	changed := completion
	changed.Response = json.RawMessage(strings.ReplaceAll(string(completion.Response), "opaque-synthetic-reasoning", "opaque-different-branch"))
	require.ErrorIs(t, second.runtime.engine.Commit(context.Background(), request, changed), ErrNativeReasoningReplayAmbiguous)
	second.releasePublication()
	next := nativeReplayContinuation(request, completion)
	p, err := first.runtime.engine.prepare(next)
	require.NoError(t, err)
	consumer := &nativeReasoningChat{runtime: first.runtime, request: next}
	values, err := consumer.restoreAfterPublication(context.Background(), p)
	require.NoError(t, err)
	require.Empty(t, values, "waiting never chooses one branch or weakens the store poison rule")
}

func TestNativeReasoningReplayPublicationNewRuntimeSafelyMissesAndFailedFinishReleases(t *testing.T) {
	producer, request, completion := publicationFixture(t)
	_, admitted := producer.preparePublication(context.Background())
	require.True(t, admitted)
	p, err := producer.runtime.engine.prepare(nativeReplayContinuation(request, completion))
	require.NoError(t, err)
	var otherProcess nativeReasoningPublications
	require.NoError(t, otherProcess.wait(context.Background(), p, 32), "cross-process absence must not guess or poll for publication")
	producer.finish(context.Background(), nil, nil, nil, nil, errors.New("synthetic failure"))
	require.Empty(t, producer.runtime.publications.entries)
	require.Empty(t, producer.runtime.publications.perOwner)
}

func TestNativeReasoningReplayPublicationDefaultOffAllocatesNoRegistry(t *testing.T) {
	engine, _, _, _ := nativeReplayFixture(t)
	svc := &OpenAIGatewayService{}
	svc.SetNativeReasoningReplayRuntime(nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) {
		return NativeReasoningReplayRuntimeConfig{Mode: NativeReasoningReplayModeOff}, nil
	}), engine)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("api_key", &APIKey{ID: 202, UserID: 101})
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.Nil(t, svc.beginNativeReasoningChat(c, account, []byte(`{"messages":[{"role":"user","content":"synthetic"}]}`)))
	allocations := testing.AllocsPerRun(100, func() {
		if svc.beginNativeReasoningChat(c, account, nil) != nil {
			panic("off runtime")
		}
	})
	require.Zero(t, allocations)
	require.Nil(t, svc.nativeReplayRuntime.publications.entries)
	require.Nil(t, svc.nativeReplayRuntime.publications.perOwner)
}

func TestNativeReasoningReplayPublicationParallelCancelAndRelease(t *testing.T) {
	var registry nativeReasoningPublications
	limits := DefaultNativeReasoningReplayLimits()
	ctx, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	for i := 0; i < 64; i++ {
		lease := registry.acquire(ctx, "owner", strings.Repeat("a", 64), limits)
		require.NotNil(t, lease)
		group.Add(1)
		go func() { defer group.Done(); lease.release(); lease.release() }()
	}
	cancel()
	group.Wait()
	require.Eventually(t, func() bool {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		return len(registry.entries) == 0 && len(registry.perOwner) == 0
	}, time.Second, time.Millisecond)
}

func TestNativeReasoningReplayPublicationCancelDuringRegistrationDoesNotLeak(t *testing.T) {
	var registry nativeReasoningPublications
	limits := DefaultNativeReasoningReplayLimits()
	for i := 0; i < 64; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		var cancelDone sync.WaitGroup
		cancelDone.Add(1)
		go func() { defer cancelDone.Done(); <-started; cancel() }()
		close(started)
		lease := registry.acquire(ctx, "owner", fmt.Sprint(i), limits)
		cancelDone.Wait()
		lease.release()
	}
	require.Eventually(t, func() bool {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		return len(registry.entries) == 0 && len(registry.perOwner) == 0
	}, time.Second, time.Millisecond)
}

type publicationCanceledPutStore struct {
	NativeReasoningReplayStore
	entered, release chan struct{}
}

func (s *publicationCanceledPutStore) PutNativeReasoningReplay(ctx context.Context, _ NativeReasoningReplayWrite, _ NativeReasoningReplayLimits) error {
	close(s.entered)
	<-s.release // Controlled storage completion, even after cancellation.
	return ctx.Err()
}

func TestNativeReasoningReplayPublicationCanceledProducerPendingPutSafelyMisses(t *testing.T) {
	producer, request, completion := publicationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, ok := producer.preparePublication(ctx)
	require.True(t, ok)
	lease := producer.publication
	store := &publicationCanceledPutStore{NativeReasoningReplayStore: producer.runtime.engine.store, entered: make(chan struct{}), release: make(chan struct{})}
	producer.runtime.engine.store = store
	done := make(chan error, 1)
	go func() { done <- producer.runtime.engine.Commit(ctx, request, completion) }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("controlled Put did not start")
	}
	cancel()
	select {
	case <-lease.entry.done:
	case <-time.After(time.Second):
		t.Fatal("canceled producer did not release its lease")
	}
	consumer := &nativeReasoningChat{runtime: producer.runtime, request: nativeReplayContinuation(request, completion)}
	p, err := producer.runtime.engine.prepare(consumer.request)
	require.NoError(t, err)
	values, err := consumer.restoreAfterPublication(context.Background(), p)
	require.NoError(t, err)
	require.Empty(t, values, "unfinished canceled output must not be invented from a publication ticket")
	close(store.release)
	require.ErrorIs(t, <-done, context.Canceled)
	producer.releasePublication()
}

func TestNativeReasoningReplayPublicationRetryKeyChangeAndPrepareFailureRelease(t *testing.T) {
	producer, _, _ := publicationFixture(t)
	_, ok := producer.preparePublication(context.Background())
	require.True(t, ok)
	first := producer.publication
	producer.request.Scope.CredentialIdentity += "/new-owner"
	_, ok = producer.preparePublication(context.Background())
	require.True(t, ok)
	require.NotSame(t, first, producer.publication)
	select {
	case <-first.entry.done:
	default:
		t.Fatal("credential change did not release old prefix")
	}
	require.Len(t, producer.runtime.publications.entries, 1)
	producer.request.Parameters = json.RawMessage(`not-json`)
	_, ok = producer.preparePublication(context.Background())
	require.False(t, ok)
	require.Empty(t, producer.runtime.publications.entries)
	require.Empty(t, producer.runtime.publications.perOwner)
}

func TestNativeReasoningReplayPublicationObserveRegistersWithoutRestoring(t *testing.T) {
	producer, request, _ := publicationFixture(t)
	producer.mode = NativeReasoningReplayModeObserve
	producer.original = json.RawMessage(`{"model":"gpt-5.5","messages":[{"role":"user","content":"synthetic"}]}`)
	body := json.RawMessage(`{"model":"gpt-5.5","input":[{"role":"user","content":"synthetic"}]}`)
	account := &Account{ID: 51, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "synthetic-owner"}}
	producer.request.Scope = request.Scope
	require.Equal(t, body, json.RawMessage(producer.prepare(context.Background(), nil, account, "synthetic-token", body)))
	require.NotNil(t, producer.publication)
	require.True(t, producer.eligible)
	require.Zero(t, producer.runtime.engine.store.(*nativeReplayTestStore).gets)
	producer.releasePublication()
	require.Empty(t, producer.runtime.publications.entries)
}

func TestNativeReasoningReplayPublicationConfirmedHandoffSurvivesCloseAndExpires(t *testing.T) {
	var registry nativeReasoningPublications
	limits := DefaultNativeReasoningReplayLimits()
	ctx, cancel := context.WithCancel(context.Background())
	clientCtx, cancelClient := context.WithCancel(context.Background())
	lease := registry.acquire(ctx, "owner", "confirmed", limits)
	defer lease.release()
	bounded, stop, ok := lease.handoff(ctx, clientCtx, 30*time.Millisecond)
	require.True(t, ok)
	defer stop()
	cancel()
	cancelClient()
	require.NoError(t, bounded.Err(), "only the confirmed publication survives closing a completed response")
	_, _, again := lease.handoff(context.Background(), context.Background(), limits.IOTimeout)
	require.False(t, again, "handoff cannot restart the commit budget")
	select {
	case <-lease.entry.done:
		t.Fatal("completed response close canceled the confirmed publication")
	default:
	}
	select {
	case <-lease.entry.done:
	case <-time.After(time.Second):
		t.Fatal("publication timeout did not release the lease")
	}
	require.ErrorIs(t, bounded.Err(), context.DeadlineExceeded)
	registry.mu.Lock()
	require.Empty(t, registry.entries)
	registry.mu.Unlock()
}

func TestNativeReasoningReplayPublicationHandoffRejectsCanceledReleasedAndStartedCallback(t *testing.T) {
	for _, fault := range []string{"forward_canceled", "client_canceled", "released", "callback_started", "over_budget"} {
		t.Run(fault, func(t *testing.T) {
			var registry nativeReasoningPublications
			limits := DefaultNativeReasoningReplayLimits()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clientCtx, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			lease := registry.acquire(ctx, "owner", "refused", limits)
			defer lease.release()
			budget := limits.IOTimeout
			var callbackRelease chan struct{}
			switch fault {
			case "forward_canceled":
				cancel()
			case "client_canceled":
				cancelClient()
			case "released":
				lease.release()
			case "callback_started":
				started := make(chan struct{})
				callbackRelease = make(chan struct{})
				trigger, fire := context.WithCancel(context.Background())
				lease.mu.Lock()
				require.True(t, lease.stop())
				lease.stop = context.AfterFunc(trigger, func() { close(started); <-callbackRelease; lease.release() })
				lease.mu.Unlock()
				fire()
				<-started
			case "over_budget":
				budget++
			}
			bounded, stop, ok := lease.handoff(ctx, clientCtx, budget)
			if callbackRelease != nil {
				close(callbackRelease)
			}
			require.False(t, ok)
			require.Nil(t, bounded)
			require.Nil(t, stop)
		})
	}
}

func TestNativeReasoningReplayPublicationHandoffCancelRace(t *testing.T) {
	var registry nativeReasoningPublications
	limits := DefaultNativeReasoningReplayLimits()
	for i := 0; i < 64; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		lease := registry.acquire(ctx, "owner", fmt.Sprint(i), limits)
		start, canceled := make(chan struct{}), make(chan struct{})
		go func() { <-start; cancel(); close(canceled) }()
		close(start)
		bounded, stop, ok := lease.handoff(ctx, ctx, limits.IOTimeout)
		<-canceled
		if ok {
			require.NoError(t, bounded.Err())
			stop()
		}
		lease.release()
	}
	registry.mu.Lock()
	require.Empty(t, registry.entries)
	require.Empty(t, registry.perOwner)
	registry.mu.Unlock()
}

func TestNativeReasoningReplayPublicationFinishOnlyHandsOffConfirmedDelivery(t *testing.T) {
	for _, fault := range []string{"complete", "forward_canceled", "client_canceled", "partial_client", "short_write", "incomplete_native", "forward_error", "principal_changed", "commit_timeout"} {
		t.Run(fault, func(t *testing.T) {
			producer, _, completion := publicationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clientCtx, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			_, admitted := producer.preparePublication(ctx)
			require.True(t, admitted)
			lease := producer.publication
			producer.sourceInput, producer.toolReverse = completion.SourceInput, completion.ToolNameReverse
			producer.principal = nativeReasoningTokenPrincipal("synthetic-token")
			wire := append([]byte("data: "), nativeCaptureJSON(map[string]any{"type": "response.completed", "response": completion.Response})...)
			wire = append(wire, '\n', '\n')
			if fault == "incomplete_native" {
				wire = []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_synthetic\"}}\n\n")
			}
			native := NewNativeReasoningResponseCapture(io.NopCloser(bytes.NewReader(wire)), producer.runtime.limits)
			_, err := io.Copy(io.Discard, native)
			require.NoError(t, err)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(clientCtx)
			writer := c.Writer
			if fault == "short_write" {
				writer = &nativeCaptureFaultWriter{ResponseWriter: writer, short: true}
			}
			delivery := NewNativeReasoningChatCapture(writer, false, producer.runtime.limits)
			c.Writer = delivery
			chat := nativeCaptureJSON(map[string]any{"id": "chatcmpl_synthetic", "object": "chat.completion", "model": producer.request.Scope.Model, "choices": []any{map[string]any{"index": 0, "message": completion.DeliveredAssistant, "finish_reason": "tool_calls"}}})
			if fault == "partial_client" {
				chat = chat[:len(chat)-1]
			}
			_, err = delivery.Write(chat)
			require.NoError(t, err)
			response := &http.Response{StatusCode: http.StatusOK, Request: httptest.NewRequest(http.MethodPost, "/responses", nil)}
			response.Request.Header.Set("Authorization", "Bearer synthetic-token")
			var forwardErr error
			store := producer.runtime.engine.store.(*nativeReplayTestStore)
			switch fault {
			case "forward_canceled":
				cancel()
			case "client_canceled":
				cancelClient()
			case "forward_error":
				forwardErr = errors.New("synthetic forward failure")
			case "principal_changed":
				response.Request.Header.Set("Authorization", "Bearer different-synthetic-token")
			case "commit_timeout":
				store.block = true
			}
			producer.finish(ctx, c, response, native, delivery, forwardErr)
			store.mu.Lock()
			if fault == "complete" {
				require.Len(t, store.values, 1, "positive control must publish this exact complete fixture")
			} else {
				require.Empty(t, store.values)
			}
			store.mu.Unlock()
			select {
			case <-lease.entry.done:
			default:
				t.Fatal("finish did not release the publication")
			}
		})
	}
}

func TestNativeReasoningReplayPublicationConcurrentReleaseWaitsForRegistryCleanup(t *testing.T) {
	var registry nativeReasoningPublications
	lease := registry.acquire(context.Background(), "owner", "pending-prefix", DefaultNativeReasoningReplayLimits())
	require.NotNil(t, lease)
	entered := make(chan struct{})
	stop := lease.stop
	lease.stop = func() bool {
		close(entered)
		return stop()
	}
	// Hold cleanup at the registry boundary while the first cancellation or
	// completion path owns release. The second caller must join that cleanup.
	registry.mu.Lock()
	first := make(chan struct{})
	go func() { lease.release(); close(first) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		registry.mu.Unlock()
		t.Fatal("first release did not reach the controlled cleanup boundary")
	}
	secondStarted, second := make(chan struct{}), make(chan struct{})
	go func() { close(secondStarted); lease.release(); close(second) }()
	<-secondStarted
	returnedEarly := false
	select {
	case <-second:
		returnedEarly = true
	case <-time.After(20 * time.Millisecond):
	}
	registry.mu.Unlock()
	for _, done := range []<-chan struct{}{first, second} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent release did not complete after registry unlock")
		}
	}
	require.False(t, returnedEarly, "release returned before the registered publication was removed")
	select {
	case <-lease.entry.done:
	default:
		t.Fatal("completed releases did not signal the publication")
	}
	require.Empty(t, registry.entries)
	require.Empty(t, registry.perOwner)
}
