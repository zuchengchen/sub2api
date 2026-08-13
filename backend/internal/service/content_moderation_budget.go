package service

import "sync/atomic"

const DefaultContentModerationPendingBodyBudgetBytes int64 = 1 << 30

type ContentModerationPendingBodyBudget struct {
	inUse      atomic.Int64
	maxSeen    atomic.Int64
	rejections atomic.Int64
}

type ContentModerationPendingReservation struct {
	budget *ContentModerationPendingBodyBudget
	bytes  int64
	refs   atomic.Int64
}

func NewContentModerationPendingBodyBudget() *ContentModerationPendingBodyBudget {
	return &ContentModerationPendingBodyBudget{}
}

func (b *ContentModerationPendingBodyBudget) TryReserve(bytes, limit int64) (*ContentModerationPendingReservation, bool) {
	if b == nil {
		return nil, true
	}
	if bytes < 0 {
		bytes = 0
	}
	if limit <= 0 {
		limit = DefaultContentModerationPendingBodyBudgetBytes
	}
	for {
		current := b.inUse.Load()
		if bytes > limit-current {
			b.rejections.Add(1)
			return nil, false
		}
		if b.inUse.CompareAndSwap(current, current+bytes) {
			updateAtomicMaximum(&b.maxSeen, current+bytes)
			reservation := &ContentModerationPendingReservation{budget: b, bytes: bytes}
			reservation.refs.Store(1)
			return reservation, true
		}
	}
}

func (r *ContentModerationPendingReservation) Retain() bool {
	if r == nil {
		return false
	}
	for {
		refs := r.refs.Load()
		if refs <= 0 {
			return false
		}
		if r.refs.CompareAndSwap(refs, refs+1) {
			return true
		}
	}
}

func (r *ContentModerationPendingReservation) Release() {
	if r == nil {
		return
	}
	refs := r.refs.Add(-1)
	if refs == 0 && r.budget != nil {
		r.budget.inUse.Add(-r.bytes)
	}
}

func (b *ContentModerationPendingBodyBudget) InUse() int64 {
	if b == nil {
		return 0
	}
	return b.inUse.Load()
}

func (b *ContentModerationPendingBodyBudget) MaxSeen() int64 {
	if b == nil {
		return 0
	}
	return b.maxSeen.Load()
}

func (b *ContentModerationPendingBodyBudget) Rejections() int64 {
	if b == nil {
		return 0
	}
	return b.rejections.Load()
}

func updateAtomicMaximum(target *atomic.Int64, value int64) {
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}
