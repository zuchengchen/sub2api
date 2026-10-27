package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Grok upstreams (cli-chat-proxy.grok.com and api.x.ai) can keep a stalled
// turn alive for the whole 58m wall clock by streaming keepalive bytes: vendor
// `event: ping` frames, SSE comments, blank lines, or repeated
// response.in_progress. A byte-level idle timer is refreshed by every one of
// those lines, so it never fires. The Grok idle timeout is therefore measured
// against model progress: an event that advances the response state.

const (
	// grokStreamIdleCheckMax caps how long an idle condition can go unnoticed
	// after the deadline passes. A ticker of the full idle interval detects a
	// stall only after up to 2x the interval.
	grokStreamIdleCheckMax = 15 * time.Second
	grokStreamIdleCheckMin = 100 * time.Millisecond

	// grokStreamDisconnectedDrainIdleMax bounds the no-progress window once the
	// client is gone. Draining continues while the upstream makes progress so
	// usage stays billable, but a stalled upstream no longer holds the account
	// slot for the full idle interval.
	grokStreamDisconnectedDrainIdleMax = 120 * time.Second
)

// grokStreamIdleCheckInterval returns the ticker period used to evaluate the
// Grok no-progress deadline.
func grokStreamIdleCheckInterval(idle time.Duration) time.Duration {
	if idle <= 0 {
		return 0
	}
	check := idle / 4
	if check > grokStreamIdleCheckMax {
		check = grokStreamIdleCheckMax
	}
	if check < grokStreamIdleCheckMin {
		check = grokStreamIdleCheckMin
	}
	return check
}

// grokStreamEffectiveIdle returns the no-progress limit that applies now.
func grokStreamEffectiveIdle(idle time.Duration, clientDisconnected bool) time.Duration {
	if clientDisconnected && idle > grokStreamDisconnectedDrainIdleMax {
		return grokStreamDisconnectedDrainIdleMax
	}
	return idle
}

// grokSSEEventIsProgress reports whether one upstream Responses SSE event moves
// the model output forward. Keepalive-shaped events only prove the socket is
// open and must not postpone the idle deadline.
func grokSSEEventIsProgress(eventType string, payload []byte) bool {
	eventType = strings.TrimSpace(eventType)
	switch eventType {
	case "", "ping", "keepalive", "heartbeat", "response.queued", "response.in_progress":
		return false
	}
	if strings.HasSuffix(eventType, ".delta") {
		// A delta with an empty body carries no new output.
		delta := gjson.GetBytes(payload, "delta")
		if delta.Exists() && delta.Type == gjson.String && delta.Str == "" {
			return false
		}
	}
	return true
}

// grokStreamProgress tracks upstream activity for one Grok stream. It is used
// only from the stream's processing goroutine; all methods tolerate a nil
// receiver so non-Grok paths can pass nil.
type grokStreamProgress struct {
	start          time.Time
	lastProgressAt time.Time
	lastEventType  string
	createdSeen    bool

	lines          int
	commentLines   int
	pingEvents     int
	progressEvents int
	idleEvents     int
}

func newGrokStreamProgress(start time.Time) *grokStreamProgress {
	if start.IsZero() {
		start = time.Now()
	}
	return &grokStreamProgress{start: start, lastProgressAt: start}
}

// ObserveLine records one raw upstream SSE line for diagnostics.
func (p *grokStreamProgress) ObserveLine(line string) {
	if p == nil {
		return
	}
	p.lines++
	if strings.HasPrefix(line, ":") {
		p.commentLines++
	}
}

// ObserveEvent classifies one parsed upstream event and refreshes the progress
// deadline when the event advances the response.
func (p *grokStreamProgress) ObserveEvent(eventType string, payload []byte) {
	if p == nil {
		return
	}
	eventType = strings.TrimSpace(eventType)
	if eventType != "" {
		p.lastEventType = eventType
	}
	switch eventType {
	case "ping", "keepalive", "heartbeat":
		p.pingEvents++
		return
	case "response.created":
		// Some upstreams re-announce the response; only the first one counts.
		if p.createdSeen {
			p.idleEvents++
			return
		}
		p.createdSeen = true
	}
	if !grokSSEEventIsProgress(eventType, payload) {
		p.idleEvents++
		return
	}
	p.progressEvents++
	p.lastProgressAt = time.Now()
}

// SinceProgress returns how long the stream has gone without model progress.
func (p *grokStreamProgress) SinceProgress(now time.Time) time.Duration {
	if p == nil {
		return 0
	}
	return now.Sub(p.lastProgressAt)
}

// Summary renders counters for operator logs and ops error details.
func (p *grokStreamProgress) Summary(now time.Time, lastRead time.Time) string {
	if p == nil {
		return ""
	}
	sinceRead := "n/a"
	if !lastRead.IsZero() {
		sinceRead = now.Sub(lastRead).Round(time.Second).String()
	}
	lastEvent := p.lastEventType
	if lastEvent == "" {
		lastEvent = "none"
	}
	return fmt.Sprintf(
		"elapsed=%s since_progress=%s since_read=%s last_event=%s lines=%d comments=%d pings=%d idle_events=%d progress_events=%d",
		now.Sub(p.start).Round(time.Second),
		now.Sub(p.lastProgressAt).Round(time.Second),
		sinceRead,
		lastEvent,
		p.lines,
		p.commentLines,
		p.pingEvents,
		p.idleEvents,
		p.progressEvents,
	)
}
