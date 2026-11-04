package codexwire

import (
	"context"
	"sync"
)

// contextMutex keeps wire writes serialized without making a canceled caller
// wait for an unrelated blocked writer. The channel is allocated once per lock,
// not once per message. Its zero value is ready to use.
type contextMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *contextMutex) Lock() { _ = m.LockContext(context.Background()) }

func (m *contextMutex) LockContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.once.Do(func() {
		m.token = make(chan struct{}, 1)
		m.token <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	}
}

func (m *contextMutex) Unlock() { m.token <- struct{}{} }
