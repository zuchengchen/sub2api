package service

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func cookiePoolTestRequest(generation string) openAIWSAcquireRequest {
	h := make(http.Header)
	h.Set(openAICookieWSGenerationHeader, generation)
	h.Set(openAICookieWSExpiresHeader, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	h.Set(openAICookieWSScopeHeader, "user-session-a")
	h.Set("Cookie", "test_cookie=private")
	h.Set("session_id", "stable-harvest-session")
	return openAIWSAcquireRequest{
		Account: &Account{ID: 41, Type: AccountTypeOAuth, Platform: PlatformOpenAI, Concurrency: 20},
		WSURL:   "wss://example.com/responses", Headers: h,
	}
}

func cookiePoolSlotRequest(slot int, generation string) openAIWSAcquireRequest {
	req := cookiePoolTestRequest(generation)
	req.Headers.Set(openAICookieWSSlotHeader, strconv.Itoa(slot))
	return req
}

func cookiePoolTestPool(t *testing.T, max int) *openAIWSConnPool {
	t.Helper()
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = max
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = max
	cfg.Gateway.OpenAIWS.PoolTargetUtilization = 1
	p := newOpenAIWSConnPool(cfg)
	p.setClientDialerForTest(&openAIWSFakeDialer{})
	t.Cleanup(p.Close)
	return p
}

func TestOpenAIWSCookiePool_GenerationAndScopeIsolationWithoutFingerprint(t *testing.T) {
	p := cookiePoolTestPool(t, 8)
	old := cookiePoolTestRequest("old")
	first, err := p.Acquire(context.Background(), old)
	require.NoError(t, err)
	first.Release()
	p.RotateCookieGeneration(old.Account.ID, "old")
	second, err := p.Acquire(context.Background(), old)
	require.NoError(t, err)
	require.Equal(t, first.ConnID(), second.ConnID())
	second.Release()

	otherScope := cloneOpenAIWSAcquireRequest(old)
	otherScope.Headers.Set(openAICookieWSScopeHeader, "user-session-b")
	isolated, err := p.Acquire(context.Background(), otherScope)
	require.NoError(t, err)
	require.NotEqual(t, first.ConnID(), isolated.ConnID())
	isolated.Release()
	otherScope.PreferredConnID = first.ConnID()
	otherScope.ForcePreferredConn = true
	_, err = p.Acquire(context.Background(), otherScope)
	require.ErrorIs(t, err, errOpenAIWSPreferredConnUnavailable)

	candidate, err := p.Acquire(context.Background(), cookiePoolTestRequest("new"))
	require.NoError(t, err)
	require.NotEqual(t, first.ConnID(), candidate.ConnID())
	candidate.MarkBroken()
	candidate.Release()
	// A failed candidate neither rotates nor invalidates the current version.
	stillOld, err := p.Acquire(context.Background(), old)
	require.NoError(t, err)
	require.Equal(t, first.ConnID(), stillOld.ConnID())
	stillOld.Release()
}

func TestOpenAIWSCookiePool_CandidateDoesNotEvictServingCapacity(t *testing.T) {
	p := cookiePoolTestPool(t, 1)
	old := cookiePoolTestRequest("old")
	lease, err := p.Acquire(context.Background(), old)
	require.NoError(t, err)
	lease.Release()
	p.RotateCookieGeneration(old.Account.ID, "old")
	next := cookiePoolTestRequest("candidate")
	next.ForceNewConn = true
	_, err = p.Acquire(context.Background(), next)
	require.ErrorIs(t, err, errOpenAIWSConnQueueFull)
	require.False(t, lease.conn.isClosed())
	current, err := p.Acquire(context.Background(), old)
	require.NoError(t, err)
	require.Equal(t, lease.ConnID(), current.ConnID())
	current.Release()
}

func TestOpenAIWSCookiePool_ReservesValidationSlotAtBusinessCapacity(t *testing.T) {
	p := cookiePoolTestPool(t, 3)
	old := cookiePoolTestRequest("old")
	p.RotateCookieGeneration(old.Account.ID, "old")
	first, err := p.Acquire(context.Background(), old)
	require.NoError(t, err)
	second, err := p.Acquire(context.Background(), old)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = p.Acquire(ctx, old)
	require.ErrorIs(t, err, context.DeadlineExceeded, "business cannot occupy refresh reserve")
	candidate := cookiePoolTestRequest("new")
	candidate.ForceNewConn = true
	candidate.Headers.Set(openAICookieWSProbeHeader, "1")
	probe, err := p.Acquire(context.Background(), candidate)
	require.NoError(t, err, "validation retains a slot despite business saturation")
	inflight, _, conns := p.AccountPoolLoad(old.Account.ID)
	require.Equal(t, 3, inflight)
	require.Equal(t, 3, conns)
	probe.MarkBroken()
	probe.Release()
	first.Release()
	second.Release()
}

func TestOpenAIWSCookiePool_RotationDrainsActiveAndPreservesPinnedContinuation(t *testing.T) {
	p := cookiePoolTestPool(t, 8)
	old := cookiePoolTestRequest("old")
	active, err := p.Acquire(context.Background(), old)
	require.NoError(t, err)
	p.RotateCookieGeneration(old.Account.ID, "old")
	require.True(t, p.PinConn(old.Account.ID, active.ConnID()))
	next := cookiePoolTestRequest("new")
	candidate, err := p.Acquire(context.Background(), next)
	require.NoError(t, err)
	candidate.Release()
	p.RotateCookieGeneration(old.Account.ID, "new")
	require.False(t, active.conn.isClosed(), "rotation must not interrupt active output")
	require.NoError(t, active.WriteJSON(map[string]string{"type": "response.create"}, time.Second))
	active.Release()
	require.False(t, active.conn.isClosed(), "pinned continuation retains original connection")

	continuation := cloneOpenAIWSAcquireRequest(next)
	continuation.PreferredConnID = active.ConnID()
	continuation.ForcePreferredConn = true
	bound, err := p.Acquire(context.Background(), continuation)
	require.NoError(t, err)
	require.Equal(t, active.ConnID(), bound.ConnID())
	bound.Release()
	p.UnpinConn(old.Account.ID, active.ConnID())
	require.True(t, active.conn.isClosed())
	_, err = p.Acquire(context.Background(), old)
	require.ErrorIs(t, err, errOpenAIWSCookieRetired)
	current, err := p.Acquire(context.Background(), next)
	require.NoError(t, err)
	require.Equal(t, candidate.ConnID(), current.ConnID())
	current.Release()
}

func TestOpenAIWSCookiePool_RotatingOneSlotLeavesOtherSlotAvailable(t *testing.T) {
	p := cookiePoolTestPool(t, 24)
	zero := cookiePoolSlotRequest(0, "zero-old")
	one := cookiePoolSlotRequest(1, "one-current")
	first, err := p.Acquire(context.Background(), zero)
	require.NoError(t, err)
	p.RotateCookieSlot(zero.Account.ID, 0, "zero-old")
	second, err := p.Acquire(context.Background(), one)
	require.NoError(t, err)
	second.Release()
	p.RotateCookieSlot(one.Account.ID, 1, "one-current")
	newZero := cookiePoolSlotRequest(0, "zero-new")
	probe := cloneOpenAIWSAcquireRequest(newZero)
	probe.Headers.Set(openAICookieWSProbeHeader, "1")
	probe.ForceNewConn = true
	validated, err := p.Acquire(context.Background(), probe)
	require.NoError(t, err)
	p.RotateCookieSlot(zero.Account.ID, 0, "zero-new")
	require.False(t, first.conn.isClosed(), "old slot's executing response drains")
	require.False(t, second.conn.isClosed(), "other slot is not retired")
	reused, err := p.Acquire(context.Background(), one)
	require.NoError(t, err)
	require.Equal(t, second.ConnID(), reused.ConnID())
	reused.Release()
	first.Release()
	require.True(t, first.conn.isClosed())
	validated.MarkBroken()
	validated.Release()
	newLease, err := p.Acquire(context.Background(), newZero)
	require.NoError(t, err)
	require.NotEqual(t, first.ConnID(), newLease.ConnID())
	newLease.Release()
}

func TestOpenAIWSCookiePool_ExpiryBlocksPinnedAndLongLivedNewTurns(t *testing.T) {
	p := cookiePoolTestPool(t, 4)
	req := cookiePoolTestRequest("old")
	lease, err := p.Acquire(context.Background(), req)
	require.NoError(t, err)
	require.True(t, p.PinConn(req.Account.ID, lease.ConnID()))
	lease.conn.cookieExpiresAt = time.Now().Add(-time.Second)
	require.True(t, lease.CookieExpired())
	require.ErrorIs(t, lease.WriteJSON(map[string]string{"type": "response.create"}, time.Second), errOpenAIWSCookieExpired)
	// Responses already started before expiration can finish reading.
	_, err = lease.ReadMessage(time.Second)
	require.NoError(t, err)
	lease.Release()
	req.PreferredConnID, req.ForcePreferredConn = lease.ConnID(), true
	_, err = p.Acquire(context.Background(), req)
	require.ErrorIs(t, err, errOpenAIWSPreferredConnUnavailable)
	_, _, conns := p.AccountPoolLoad(req.Account.ID)
	require.Zero(t, conns)

	expired := cookiePoolTestRequest("expired")
	expired.Headers.Set(openAICookieWSExpiresHeader, strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10))
	_, err = p.Acquire(context.Background(), expired)
	require.ErrorIs(t, err, errOpenAIWSCookieExpired)
}

func TestOpenAIWSCookiePool_NoActivePingWithPassiveReaderAndPeerClose(t *testing.T) {
	p := cookiePoolTestPool(t, 4)
	d := &openAIWSReaderLoopFakeDialer{}
	p.setClientDialerForTest(d)
	req := cookiePoolTestRequest("cookie")
	lease, err := p.Acquire(context.Background(), req)
	require.NoError(t, err)
	fake := d.dialed()[0]
	require.Eventually(t, func() bool { return fake.readers.Load() == 1 }, time.Second, time.Millisecond)
	require.False(t, lease.SupportsIdlePingWithoutReader())
	require.NoError(t, lease.PingWithTimeout(time.Second))
	lease.Release()
	lease.conn.lastUsedNano.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	p.runBackgroundPingSweep()
	require.False(t, p.shouldHealthCheckConn(lease.conn))
	require.Zero(t, fake.pings.Load())
	p.runBackgroundCleanupSweep(time.Now())
	require.False(t, fake.isClosed(), "reader loop remains alive despite no proactive pings")
	fake.failReads(errOpenAIWSConnClosed)
	require.Eventually(t, func() bool { _, _, count := p.AccountPoolLoad(req.Account.ID); return count == 0 }, time.Second, time.Millisecond)
}

func TestOpenAIWSCookiePool_TwoSlotsOfTenIndependentConcurrentLeases(t *testing.T) {
	p := cookiePoolTestPool(t, 24)
	requests := [2]openAIWSAcquireRequest{cookiePoolSlotRequest(0, "shared-zero"), cookiePoolSlotRequest(1, "shared-one")}
	for slot, req := range requests {
		p.RotateCookieSlot(req.Account.ID, slot, req.Headers.Get(openAICookieWSGenerationHeader))
	}
	start := make(chan struct{})
	type result struct {
		lease *openAIWSConnLease
		err   error
	}
	results := make(chan result, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		req := requests[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			lease, err := p.Acquire(ctx, req)
			results <- result{lease, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	ids := map[string]bool{}
	var leases []*openAIWSConnLease
	for result := range results {
		require.NoError(t, result.err)
		require.False(t, ids[result.lease.ConnID()], "concurrent requests must receive independent leases")
		ids[result.lease.ConnID()] = true
		leases = append(leases, result.lease)
	}
	inflight, waiters, conns := p.AccountPoolLoad(requests[0].Account.ID)
	require.Equal(t, 20, inflight)
	require.Zero(t, waiters)
	require.GreaterOrEqual(t, conns, 20)
	require.LessOrEqual(t, conns, 24)
	slots := [2]int{}
	for _, lease := range leases {
		slot, ok := p.ConnCookieSlot(requests[0].Account.ID, lease.ConnID())
		require.True(t, ok)
		slots[slot]++
	}
	require.Equal(t, [2]int{10, 10}, slots)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Acquire(ctx, requests[0])
	require.ErrorIs(t, err, context.DeadlineExceeded, "one slot cannot exceed ten business connections")
	probeReq := cookiePoolSlotRequest(0, "next-zero")
	probeReq.ForceNewConn = true
	probeReq.Headers.Set(openAICookieWSProbeHeader, "1")
	probe, err := p.Acquire(context.Background(), probeReq)
	require.NoError(t, err)
	inflight, _, conns = p.AccountPoolLoad(requests[0].Account.ID)
	require.Equal(t, 21, inflight, "twenty business sockets allow one temporary validation socket")
	require.Equal(t, 21, conns)
	anotherProbe := cookiePoolSlotRequest(1, "next-one")
	anotherProbe.ForceNewConn = true
	anotherProbe.Headers.Set(openAICookieWSProbeHeader, "1")
	_, err = p.Acquire(context.Background(), anotherProbe)
	require.ErrorIs(t, err, errOpenAIWSConnQueueFull, "only one temporary validation socket per account")
	probe.MarkBroken()
	probe.Release()
	for _, lease := range leases {
		lease.Release()
	}
}

func TestOpenAIWSCookiePool_StalePrewarmCannotReenterAfterRotation(t *testing.T) {
	p := cookiePoolTestPool(t, 4)
	d := newOpenAIWSFirstDialBlockingCaptureDialer()
	p.setClientDialerForTest(d)
	req := cookiePoolTestRequest("old")
	ap := p.getOrCreateAccountPool(req.Account.ID)
	ap.mu.Lock()
	ap.cookieGenerations[0] = "old"
	ap.cookieExpiresAts[0], _ = openAIWSCookieExpiry(req.Headers)
	ap.lastAcquire = cloneOpenAIWSAcquireRequestPtr(&req)
	ap.creating, ap.prewarmActive = 1, true
	ap.cookieCreating[0] = 1
	generation := ap.generation
	ap.mu.Unlock()
	done := make(chan struct{})
	go func() { p.prewarmConns(req.Account.ID, req, 1, generation); close(done) }()
	<-d.firstStarted
	p.RotateCookieGeneration(req.Account.ID, "new")
	close(d.releaseFirst)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("prewarm did not finish")
	}
	_, _, conns := p.AccountPoolLoad(req.Account.ID)
	require.Zero(t, conns)
	d.mu.Lock()
	sent := d.headers[0]
	d.mu.Unlock()
	require.Empty(t, sent.Get(openAICookieWSGenerationHeader))
	require.Empty(t, sent.Get(openAICookieWSExpiresHeader))
	require.Empty(t, sent.Get(openAICookieWSScopeHeader))
	require.Empty(t, sent.Get(openAICookieWSSlotHeader))
	require.Empty(t, sent.Get(openAICookieWSProbeHeader))
	require.Equal(t, req.Headers.Get("Cookie"), sent.Get("Cookie"))
}

func TestCoderOpenAIWSClientDialer_DirectTransportIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	d := newDefaultOpenAIWSClientDialer().(*coderOpenAIWSClientDialer)
	c := d.directHTTPClient()
	transport, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	require.Nil(t, transport.Proxy)
	require.Same(t, c, d.directHTTPClient())
}

func TestOpenAIWSCookiePool_CleanupKeepsTwentyCookieSocketsAndTrimsLegacyIdle(t *testing.T) {
	p := cookiePoolTestPool(t, 24)
	p.cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 2
	ap := p.getOrCreateAccountPool(41)
	for slot := 0; slot < 2; slot++ {
		generation := "slot-" + strconv.Itoa(slot)
		ap.cookieGenerations[slot] = generation
		req := cookiePoolSlotRequest(slot, generation)
		for i := 0; i < 10; i++ {
			conn := newOpenAIWSConn(generation+"-"+strconv.Itoa(i), 41, &openAIWSFakeConn{}, nil)
			conn.handshakeCompatibility = normalizeOpenAIWSHandshakeCompatibility(req.Account, req.Headers)
			conn.cookieExpiresAt = time.Now().Add(time.Hour)
			ap.conns[conn.id] = conn
		}
	}
	for i := 0; i < 4; i++ {
		conn := newOpenAIWSConn("legacy-"+strconv.Itoa(i), 41, &openAIWSFakeConn{}, nil)
		ap.conns[conn.id] = conn
	}
	ap.mu.Lock()
	evicted := p.cleanupAccountLocked(ap, time.Now(), 24)
	cookieCount, legacyCount := 0, 0
	for _, conn := range ap.conns {
		if conn.cookieGeneration() != "" {
			cookieCount++
		} else {
			legacyCount++
		}
	}
	ap.mu.Unlock()
	closeOpenAIWSConns(evicted)
	require.Equal(t, 20, cookieCount)
	require.Equal(t, 2, legacyCount)
	require.Len(t, evicted, 2)
	// Cookie expiry still takes precedence over preservation of idle sockets.
	ap.mu.Lock()
	evicted = p.cleanupAccountLocked(ap, time.Now().Add(61*time.Minute), 24)
	for _, conn := range ap.conns {
		require.Empty(t, conn.cookieGeneration())
	}
	ap.mu.Unlock()
	closeOpenAIWSConns(evicted)
}
