package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	openAIWSEdgeBackoffReason           = "edge_backoff"
	openAIWSEdgeBackoffFailureThreshold = 2
	openAIWSEdgeBackoffMaxEntries       = 4096
	openAIWSEdgeBackoffMaxDelay         = time.Minute
)

// This is local transport policy, not an upstream limit or account penalty.
// Integration must only return this error from a fresh dial whose request is
// already eligible for a pre-output HTTP fallback. It must never mark the
// logical WS session disabled, evict a healthy socket, or consume a 403 strike.
var errOpenAIWSEdgeBackoff = errors.New("upstream websocket edge backoff active")

type openAIWSEdgeBackoffKey [sha256.Size]byte

// Inputs are used transiently by the key builder and are never retained by the
// registry. Resolve these from the actual dial AFTER HeadersFactory/auth refresh.
// Egress must distinguish explicit direct, managed proxy (including credential
// changes), environment proxy, and a known transport route. Unknown is not a
// shared egress. Transport must be a stable fingerprint of the actual dialer,
// TLS/wire profile, relevant handshake settings and credential mode, not turn IDs.
// CredentialGeneration must change with the effective bearer/cookie credential.
// UserID/APIKeyID come from authenticated server context, never client headers.
type openAIWSEdgeBackoffScope struct {
	AccountID            int64
	CredentialOwnerID    int64
	CredentialNamespace  string
	UserID               int64
	APIKeyID             int64
	GroupID              int64
	Egress               string
	Target               string
	CredentialGeneration string
	Transport            string
}

// Incomplete attribution opts out instead of merging unrelated callers under
// zero IDs, empty proxy strings or an "unknown" route. Length prefixes prevent
// boundary collisions even when opaque input components contain separators.
func makeOpenAIWSEdgeBackoffKey(scope openAIWSEdgeBackoffScope) (openAIWSEdgeBackoffKey, bool) {
	if scope.AccountID <= 0 || scope.CredentialOwnerID <= 0 || scope.UserID <= 0 || scope.APIKeyID <= 0 || scope.GroupID < 0 ||
		strings.TrimSpace(scope.Egress) == "" || strings.EqualFold(strings.TrimSpace(scope.Egress), "unknown") ||
		strings.TrimSpace(scope.Target) == "" || strings.TrimSpace(scope.CredentialGeneration) == "" || strings.TrimSpace(scope.Transport) == "" {
		return openAIWSEdgeBackoffKey{}, false
	}
	h := sha256.New()
	_, _ = h.Write([]byte("openai-ws-edge-backoff-v1"))
	var buf [8]byte
	for _, id := range []int64{scope.AccountID, scope.CredentialOwnerID, scope.UserID, scope.APIKeyID, scope.GroupID} {
		binary.BigEndian.PutUint64(buf[:], uint64(id))
		_, _ = h.Write(buf[:])
	}
	for _, value := range []string{scope.CredentialNamespace, scope.Egress, scope.Target, scope.CredentialGeneration, scope.Transport} {
		binary.BigEndian.PutUint64(buf[:], uint64(len(value)))
		_, _ = h.Write(buf[:])
		_, _ = h.Write([]byte(value))
	}
	var key openAIWSEdgeBackoffKey
	copy(key[:], h.Sum(nil))
	return key, true
}

type openAIWSEdgeBackoffSettings struct {
	Disabled      bool
	FailureWindow time.Duration
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	JitterRatio   float64
	// ProbeTimeout should be the existing configured dial deadline. The lease
	// supplies a context enforcing this bound; callers MUST dial with it.
	ProbeTimeout time.Duration
	MaxEntries   int
}

type openAIWSEdgeBackoffEntry struct {
	epoch        uint64
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
	probeUntil   time.Time
	activeUntil  time.Time
	lastTouched  time.Time
	inFlight     int
}

// Only digests, counters and timestamps live in this service-local registry.
// No accounts, credentials, requests, responses, contexts or session flags are
// cached. It has no background goroutine and no relationship to the session
// disable registry or proxy/account scheduling health.
type openAIWSEdgeBackoff struct {
	mu       sync.Mutex
	settings openAIWSEdgeBackoffSettings
	entries  map[openAIWSEdgeBackoffKey]*openAIWSEdgeBackoffEntry
	epoch    uint64
}

func newOpenAIWSEdgeBackoff(settings openAIWSEdgeBackoffSettings) *openAIWSEdgeBackoff {
	if settings.FailureWindow <= 0 {
		settings.FailureWindow = time.Minute
	}
	if settings.MaxDelay <= 0 || settings.MaxDelay > openAIWSEdgeBackoffMaxDelay {
		settings.MaxDelay = openAIWSEdgeBackoffMaxDelay
	}
	if settings.BaseDelay <= 0 {
		settings.BaseDelay = 30 * time.Second
	}
	if settings.BaseDelay > settings.MaxDelay {
		settings.BaseDelay = settings.MaxDelay
	}
	if math.IsNaN(settings.JitterRatio) || settings.JitterRatio <= 0 || settings.JitterRatio >= 1 {
		settings.JitterRatio = 0.2
	}
	if settings.ProbeTimeout <= 0 {
		settings.ProbeTimeout = 10 * time.Second
	}
	if settings.MaxEntries <= 0 || settings.MaxEntries > openAIWSEdgeBackoffMaxEntries {
		settings.MaxEntries = openAIWSEdgeBackoffMaxEntries
	}
	return &openAIWSEdgeBackoff{settings: settings, entries: make(map[openAIWSEdgeBackoffKey]*openAIWSEdgeBackoffEntry)}
}

// A lease represents one actual fresh dial. Finish must be called exactly once
// on every exit, including cancellation and errors other than 403. It is safe
// to call it defensively more than once: repeated results cannot count twice.
// A lease never retains key-construction inputs or upstream errors.
type openAIWSEdgeDialLease struct {
	registry *openAIWSEdgeBackoff
	key      openAIWSEdgeBackoffKey
	entry    *openAIWSEdgeBackoffEntry
	epoch    uint64
	probe    bool
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
}

func (l *openAIWSEdgeDialLease) Context() context.Context {
	if l == nil || l.ctx == nil {
		return context.Background()
	}
	return l.ctx
}

// Acquire is called after connection reuse has been attempted, immediately
// before a fresh eligible WS handshake. bypass means this request may take its
// already-authorized HTTP fallback; it does not manufacture an upstream 403.
// Nil/disabled/unscoped/capacity-limited registries fail open. Canceled requests
// also keep their original context and must not become HTTP fallback attempts.
func (b *openAIWSEdgeBackoff) Acquire(ctx context.Context, key openAIWSEdgeBackoffKey, now time.Time) (lease *openAIWSEdgeDialLease, bypass bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	lease = &openAIWSEdgeDialLease{ctx: ctx}
	if b == nil || b.settings.Disabled || key == (openAIWSEdgeBackoffKey{}) || ctx.Err() != nil {
		return lease, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.entries[key]
	if entry == nil {
		if !b.makeRoomLocked(now) {
			return lease, false
		}
		b.epoch++
		entry = &openAIWSEdgeBackoffEntry{epoch: b.epoch}
		b.entries[key] = entry
	}
	entry.lastTouched = now
	if now.Before(entry.blockedUntil) || now.Before(entry.probeUntil) {
		return nil, true
	}
	probe := !entry.blockedUntil.IsZero()
	if probe {
		// An expired abandoned probe is superseded. Its context has the same
		// finite deadline, and any late completion belongs to an older epoch.
		b.epoch++
		entry.epoch = b.epoch
		entry.inFlight = 0
		entry.probeUntil = now.Add(b.settings.ProbeTimeout)
	} else if !entry.windowStart.IsZero() && (now.Before(entry.windowStart) || now.Sub(entry.windowStart) >= b.settings.FailureWindow) {
		entry.failures = 0
		entry.windowStart = time.Time{}
	}
	// A half-open probe always gets its own bounded context. Ordinary fresh
	// dials retain the caller's existing dial deadline; integration must pass
	// the lease context to the same dialer deadline used elsewhere.
	if probe {
		lease.ctx, lease.cancel = context.WithTimeout(ctx, b.settings.ProbeTimeout)
	} else {
		lease.ctx = ctx
	}
	// The registry uses this same finite lease bound only for capacity eviction;
	// it never cancels an ordinary dial or treats an old result as current.
	entry.activeUntil = now.Add(b.settings.ProbeTimeout)
	entry.inFlight++
	lease.registry, lease.key, lease.entry, lease.epoch, lease.probe = b, key, entry, entry.epoch, probe
	return lease, false
}

func (l *openAIWSEdgeDialLease) Finish(err error, now time.Time) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		canceled := l.ctx != nil && l.ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		if l.cancel != nil {
			defer l.cancel()
		}
		b := l.registry
		if b == nil {
			return
		}
		edge := !canceled && openAIWSEdgeBackoffEligibleFailure(err)
		b.mu.Lock()
		defer b.mu.Unlock()
		entry := b.entries[l.key]
		if entry != l.entry || entry.epoch != l.epoch {
			return
		}
		entry.inFlight--
		entry.lastTouched = now
		if l.probe {
			entry.probeUntil = time.Time{}
		}
		if !canceled && err == nil {
			delete(b.entries, l.key)
			return
		}
		if !edge {
			// Cancellations and other failure classes neither count nor extend
			// the cooldown. A failed/canceled probe releases its exclusive slot.
			if l.probe || (entry.inFlight == 0 && entry.failures == 0 && entry.blockedUntil.IsZero()) {
				delete(b.entries, l.key)
			}
			return
		}
		if !l.probe && (entry.windowStart.IsZero() || now.Before(entry.windowStart) || now.Sub(entry.windowStart) >= b.settings.FailureWindow) {
			entry.failures = 0
			entry.windowStart = now
		}
		entry.failures++
		if l.probe || entry.failures >= openAIWSEdgeBackoffFailureThreshold {
			entry.failures = openAIWSEdgeBackoffFailureThreshold
			b.epoch++
			entry.epoch = b.epoch
			entry.inFlight = 0
			entry.blockedUntil = now.Add(b.delay(l.key, entry.epoch))
		}
	})
}

// A structured 403 must retain the existing credential/permission policy even
// if a proxy also added cf-mitigated. Treat truncated JSON as unknown rather
// than promoting a clipped authorization failure into an edge transport hint.
func openAIWSEdgeBackoffEligibleFailure(err error) bool {
	dialErr, ok := openAIWSDialForbidden(err)
	if !ok || dialErr.StatusCode != http.StatusForbidden || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	body := bytes.TrimSpace(dialErr.ResponseBody)
	if len(body) == 0 || gjson.ValidBytes(body) || body[0] == '{' || body[0] == '[' {
		return false
	}
	// Cross-request suppression requires affirmative edge evidence. The older
	// per-request fallback deliberately also handles unknown text/empty 403s;
	// those rejections must not acquire a broader transport-health meaning.
	lower := strings.ToLower(string(body))
	isHTML := strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<head")
	if !isHTML {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(dialErr.ResponseHeaders.Get("cf-mitigated")), "challenge") ||
		isCloudflareBotBlockResponse(body) || isOpenAIEdgeChallenge403(body)
}

// Deterministic keyed jitter avoids shared RNG state and synchronizing routes.
// The epoch changes on each trip; retries never grow into a long-lived disable.
func (b *openAIWSEdgeBackoff) delay(key openAIWSEdgeBackoffKey, epoch uint64) time.Duration {
	var seed [sha256.Size + 8]byte
	copy(seed[:], key[:])
	binary.BigEndian.PutUint64(seed[sha256.Size:], epoch)
	digest := sha256.Sum256(seed[:])
	unit := float64(binary.BigEndian.Uint64(digest[:8])>>11) / (1 << 53)
	factor := 1 + b.settings.JitterRatio*(2*unit-1)
	delay := time.Duration(float64(b.settings.BaseDelay) * factor)
	if delay <= 0 {
		delay = time.Nanosecond
	}
	if delay > b.settings.MaxDelay {
		delay = b.settings.MaxDelay
	}
	return delay
}

func (b *openAIWSEdgeBackoff) makeRoomLocked(now time.Time) bool {
	if len(b.entries) < b.settings.MaxEntries {
		return true
	}
	// Never evict a live probe or dial: evicting it could admit a second probe
	// for the same key. Other idle entries use LRU; eviction only permits a
	// fresh dial, so capacity pressure cannot suppress a different caller.
	var oldestKey openAIWSEdgeBackoffKey
	var oldest *openAIWSEdgeBackoffEntry
	for key, entry := range b.entries {
		if entry.inFlight > 0 && now.Before(entry.activeUntil) {
			continue
		}
		if oldest == nil || entry.lastTouched.Before(oldest.lastTouched) {
			oldest, oldestKey = entry, key
		}
	}
	if oldest == nil {
		return false
	}
	delete(b.entries, oldestKey)
	return true
}
