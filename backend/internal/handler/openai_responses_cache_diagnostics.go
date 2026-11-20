package handler

import (
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// A proxy attempt numbers its turns from one. Account failover starts a new
// attempt on the same client socket and can replay the current turn. Keep its
// original sample until that turn has a result or the connection ends.
type responsesWSCacheDiagnostics struct {
	observer *service.ResponsesCacheDiagnosticObserver
	lastErr  error

	mu        sync.Mutex // passthrough reads new frames concurrently with terminal events
	offset    int
	completed int
	attempt   int
	accountID int64
	pending   map[int]*responsesWSCacheTurn
}

type responsesWSCacheTurn struct {
	raw             []byte // retained only while a differently mapped retry could enter the sample
	model           string
	sample          *service.ResponsesCacheDiagnostic
	observedAttempt int
}

func newResponsesWSCacheDiagnostics(observer *service.ResponsesCacheDiagnosticObserver, first []byte) *responsesWSCacheDiagnostics {
	d := &responsesWSCacheDiagnostics{observer: observer, pending: make(map[int]*responsesWSCacheTurn, 2)}
	if observer.CanRetain(first) {
		d.pending[1] = &responsesWSCacheTurn{raw: first}
	}
	return d
}

func (d *responsesWSCacheDiagnostics) startAttempt(accountID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.accountID = accountID
	d.offset = d.completed
	d.attempt++
}

func (d *responsesWSCacheDiagnostics) observeTurn(turn int, raw []byte, model string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.offset + turn
	current := d.pending[id]
	if current == nil {
		// Frame 1 of a restarted proxy may be a reconstructed replay.
		if turn == 1 || !d.observer.CanRetain(raw) {
			return
		}
		current = &responsesWSCacheTurn{raw: raw}
		d.pending[id] = current
	}
	if current.sample == nil && current.raw != nil && current.model != model {
		current.model = model
		current.sample = d.observer.Begin(current.raw, model, "ws")
		if current.sample != nil || !d.observer.CanRetain(current.raw) {
			current.raw = nil
		}
		current.sample.RecordWSClientTurn(id)
	}
	if current.sample != nil && current.observedAttempt != d.attempt {
		current.sample.RecordAttempt(d.accountID, model)
		current.observedAttempt = d.attempt
	}
}

func (d *responsesWSCacheDiagnostics) finishTurn(turn int, result *service.OpenAIForwardResult, err error) {
	if result == nil && err != nil {
		// A transport can return a retryable error after AfterTurn. Finalize
		// from close if no subsequent attempt produces a result.
		return
	}
	d.mu.Lock()
	id := d.offset + turn
	current := d.pending[id]
	delete(d.pending, id)
	d.completed = max(d.completed, id)
	accountID := d.accountID
	d.mu.Unlock()
	if current != nil {
		current.sample.Finish(accountID, result, err)
	}
}

func (d *responsesWSCacheDiagnostics) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, turn := range d.pending {
		if turn.observedAttempt == d.attempt {
			turn.sample.Finish(d.accountID, nil, d.lastErr)
		}
	}
}
