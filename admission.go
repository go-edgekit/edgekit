package edgekit

import (
	"sync/atomic"
)

// AdmissionBudget limits the number of requests that are active at the same
// time in the part of request handling that it protects. It is a
// configurable protection (ADR-0025): set it in [Config.Admission], in
// [Route.Admission], or in both, to enable it. Make it with
// [NewAdmissionBudget].
//
// There is no queue. When all slots are in use, the gateway rejects the
// request with 503 Service Unavailable and [ReasonAdmissionSaturated], and
// does not read or close its body. A request holds its slot until the
// protected part of handling returns or unwinds: also while the body streams,
// while the transport waits for a pooled connection, and while an upgraded
// connection is active. A cancellation alone does not release the slot. A
// panic releases the slot, and the panic continues to unwind.
//
// Each request counts, not each connection, so the streams of one HTTP/2
// connection share a budget. Routes that refer to the same budget share it,
// and so do gateways. A budget limits concurrency only; it is not a rate
// limit.
//
// The budget holds runtime state. It is safe for concurrent use. Do not copy
// it.
type AdmissionBudget struct {
	limit  int64
	active atomic.Int64
}

// NewAdmissionBudget returns a budget with limit slots. The limit must be
// positive. Otherwise NewAdmissionBudget returns an error that wraps
// [ErrInvalidConfiguration].
func NewAdmissionBudget(limit int) (*AdmissionBudget, error) {
	if limit <= 0 {
		return nil, invalid("limit: is not positive")
	}

	return &AdmissionBudget{
		limit: int64(limit),
	}, nil
}

// tryAcquire takes a slot and reports false when no slot is free. It never
// waits.
func (b *AdmissionBudget) tryAcquire() bool {
	for {
		active := b.active.Load()
		if active >= b.limit {
			return false
		}

		if b.active.CompareAndSwap(active, active+1) {
			return true
		}
	}
}

// release returns a slot that tryAcquire took.
func (b *AdmissionBudget) release() {
	b.active.Add(-1)
}
