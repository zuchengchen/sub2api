package service

import (
	"context"
	"sync"
	"time"
)

// A publication owns only framed digests and a completion signal. It does not
// cache output, authorize replay, or serialize upstream work. The persistent
// store remains the sole source of completed native state.
type nativeReasoningPublications struct {
	mu       sync.Mutex
	entries  map[string]*nativeReasoningPublicationEntry
	perOwner map[string]int
}

type nativeReasoningPublicationEntry struct {
	owner, key string
	pending    int
	done       chan struct{}
}

type nativeReasoningPublicationLease struct {
	registry  *nativeReasoningPublications
	entry     *nativeReasoningPublicationEntry
	mu        sync.Mutex
	released  bool
	handedOff bool
	stop      func() bool
}

func (p *nativeReasoningPublications) acquire(ctx context.Context, owner, key string, limits NativeReasoningReplayLimits) *nativeReasoningPublicationLease {
	if ctx.Err() != nil {
		return nil
	}
	p.mu.Lock()
	entry := p.entries[key]
	if entry != nil && entry.owner != owner {
		p.mu.Unlock()
		return nil
	}
	if entry == nil {
		if len(p.entries) >= limits.MaxEntriesGlobal || p.perOwner[owner] >= limits.MaxEntriesPerUser {
			p.mu.Unlock()
			return nil
		}
		if p.entries == nil {
			p.entries = make(map[string]*nativeReasoningPublicationEntry)
			p.perOwner = make(map[string]int)
		}
		entry = &nativeReasoningPublicationEntry{owner: owner, key: key, done: make(chan struct{})}
		p.entries[key] = entry
		p.perOwner[owner]++
	}
	entry.pending++
	p.mu.Unlock()
	lease := &nativeReasoningPublicationLease{registry: p, entry: entry}
	stop := context.AfterFunc(ctx, lease.release)
	lease.mu.Lock()
	if lease.released {
		lease.mu.Unlock()
		stop()
	} else {
		lease.stop = stop
		lease.mu.Unlock()
	}
	return lease
}

func (l *nativeReasoningPublicationLease) matches(owner, key string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.released && l.entry.owner == owner && l.entry.key == key
}

// handoff is called only after finish verifies complete native output and
// successful client delivery. Cancellation before this point still discards the
// output. Once confirmed, closing the completed HTTP response must not interrupt
// the bounded cache publication. A cancellation callback that already started
// wins: stop returning false prevents transferring an already retiring lease.
func (l *nativeReasoningPublicationLease) handoff(ctx, clientCtx context.Context, budget time.Duration) (context.Context, context.CancelFunc, bool) {
	if l == nil {
		return nil, nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.handedOff || l.stop == nil || ctx.Err() != nil || clientCtx.Err() != nil || budget <= 0 || budget > 100*time.Millisecond {
		return nil, nil, false
	}
	if !l.stop() {
		return nil, nil, false
	}
	l.stop = nil
	if ctx.Err() != nil || clientCtx.Err() != nil {
		return nil, nil, false
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	l.handedOff = true
	// Also release on the publication deadline if a store fails to return
	// promptly; the normal finish defer remains the primary release path.
	l.stop = context.AfterFunc(bounded, l.release)
	return bounded, cancel, true
}

func (l *nativeReasoningPublicationLease) release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	// A concurrent caller must not observe release completion before the
	// registry ticket is removed and its completion signal is published.
	l.released = true
	stop := l.stop
	if stop != nil {
		stop()
	}
	p := l.registry
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[l.entry.key] != l.entry {
		return
	}
	l.entry.pending--
	if l.entry.pending == 0 {
		delete(p.entries, l.entry.key)
		p.perOwner[l.entry.owner]--
		if p.perOwner[l.entry.owner] == 0 {
			delete(p.perOwner, l.entry.owner)
		}
		close(l.entry.done)
	}
}

// Eligible assistant turns share Restore's candidate selector. A producer registers the
// complete input history before its future assistant; that same prefix is the
// candidate assistant's immediately preceding key. It is strictly shorter than
// this request's whole history and therefore cannot wait on its own lease.
func (p *nativeReasoningPublications) wait(ctx context.Context, prepared nativeReasoningReplayPrepared, maxLookups int) error {
	candidates := nativeReplayRestoreCandidates(prepared.messages, maxLookups)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var pending []<-chan struct{}
		p.mu.Lock()
		for _, index := range candidates {
			if index > 0 {
				if entry := p.entries[prepared.keys[index-1]]; entry != nil && entry.owner == prepared.owner {
					pending = append(pending, entry.done)
				}
			}
		}
		p.mu.Unlock()
		if len(pending) == 0 {
			return nil
		}
		for _, done := range pending {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// Another matching producer can join while earlier ones complete. Recheck
		// it within the same deadline, never restarting a per-key wait budget.
	}
}

func (n *nativeReasoningChat) releasePublication() {
	if n != nil {
		n.publication.release()
		n.publication = nil
	}
}

func (n *nativeReasoningChat) preparePublication(ctx context.Context) (nativeReasoningReplayPrepared, bool) {
	p, err := n.runtime.engine.prepare(n.request)
	if err != nil {
		n.releasePublication()
		return p, false
	}
	key := p.keys[len(p.keys)-1]
	if !n.publication.matches(p.owner, key) {
		n.releasePublication()
		n.publication = n.runtime.publications.acquire(ctx, p.owner, key, n.runtime.limits)
	}
	return p, n.publication != nil
}

func (n *nativeReasoningChat) restoreAfterPublication(ctx context.Context, prepared nativeReasoningReplayPrepared) ([]NativeReasoningReplayReplacement, error) {
	// Waiting and the subsequent single store read share the existing I/O budget.
	// Timeout/cancellation is a safe miss; Commit still requires complete delivery.
	bounded, cancel := context.WithTimeout(ctx, n.runtime.limits.IOTimeout)
	defer cancel()
	if err := n.runtime.publications.wait(bounded, prepared, n.runtime.limits.MaxLookups); err != nil {
		return nil, err
	}
	return n.runtime.engine.Restore(bounded, n.request)
}
