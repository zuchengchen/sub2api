package repository

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
)

// Only a request-owned H2 client has this shutdown lifetime. Shared quota/auth
// clients remain reusable after each response; web transports keep their body.
type codexOwnedH2Body struct {
	io.ReadCloser
	ctx        context.Context
	cancel     context.CancelFunc
	stopAbort  func() bool
	complete   func() bool
	pool       *codexwire.H2Pool
	once       sync.Once
	readMu     sync.Mutex
	eof        atomic.Bool
	readFailed atomic.Bool
	closed     bool
	err        error
}

func (b *codexOwnedH2Body) Read(p []byte) (int, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.eof.Store(true)
	} else if err != nil {
		b.readFailed.Store(true)
	}
	return n, err
}
func (b *codexOwnedH2Body) Close() error {
	b.once.Do(func() {
		complete := b.eof.Load()
		if b.complete != nil {
			complete = b.complete()
		}
		normal := complete && !b.readFailed.Load() && b.ctx.Err() == nil
		if normal {
			normal = b.stopAbort()
		}
		if !normal {
			b.stopAbort()
			b.cancel()
			b.pool.CloseAll()
		}
		// Cancellation precedes this lock for early/concurrent reads. A fully read
		// response can release its stream first, before normal client retirement.
		b.readMu.Lock()
		b.closed = true
		b.err = b.ReadCloser.Close()
		b.readMu.Unlock()
		if normal {
			if b.ctx.Err() != nil || b.err != nil || b.readFailed.Load() {
				b.cancel()
				b.pool.CloseAll()
			} else {
				b.pool.RetireAll()
				b.cancel()
			}
		}
	})
	return b.err
}
