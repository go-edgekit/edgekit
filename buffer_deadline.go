package edgekit

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// errBufferDeadlineSupport does not expose writer or deployment details.
var errBufferDeadlineSupport = errors.New("request buffering requires scoped inbound read deadlines")

// bufferReadScope preserves the transfer manager's owner and caller deadline.
// All fields except manager are protected by manager.mu.
type bufferReadScope struct {
	manager     *transfer
	idle        time.Duration
	total       time.Time
	applied     time.Time
	canceled    bool
	interrupted bool

	// The first HTTP/1.1 body read can block while writing 100 Continue.
	continueDeadline time.Time
	writeScoped      bool
	continueReading  bool
}

// earlier ignores an absent deadline.
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}

	return a
}

// deadline never extends another applicable read bound. The caller holds
// s.manager.mu.
func (s *bufferReadScope) deadline(now time.Time) time.Time {
	d := earlier(s.total, now.Add(s.idle))
	if s.manager.idle > 0 {
		d = earlier(d, now.Add(s.manager.idle))
	}
	if s.manager.readOwned {
		d = earlier(d, s.manager.readDeadline)
	}
	if s.canceled {
		d = earlier(d, now)
	}

	return d
}

// bufferReadStarter is a writer that can install a scoped buffering
// deadline. Only the Server writer implements it.
type bufferReadStarter interface {
	beginBufferRead(idle, total time.Duration) (*bufferReadScope, error)
}

// beginBufferRead does not use public deadline takeover: net/http has no
// operation that reports an existing owner or deadline for restoration.
func beginBufferRead(w http.ResponseWriter, idle, total time.Duration) (*bufferReadScope, error) {
	for {
		if scoped, ok := w.(bufferReadStarter); ok {
			return scoped.beginBufferRead(idle, total)
		}

		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil, errBufferDeadlineSupport
		}

		w = wrapper.Unwrap()
	}
}

// beginBufferRead installs a temporary scope without a public takeover.
func (w *transferWriter) beginBufferRead(idle, total time.Duration) (*bufferReadScope, error) {
	t := w.transfer
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.done || t.buffer != nil || t.readers != 0 {
		return nil, errBufferDeadlineSupport
	}

	now := time.Now()
	s := &bufferReadScope{
		manager: t,
		idle:    idle,
		total:   now.Add(total),
	}
	s.applied = s.deadline(now)
	if err := t.controller.SetReadDeadline(s.applied); err != nil {
		return nil, errors.Join(errBufferDeadlineSupport, err)
	}

	t.buffer = s

	return s, nil
}

// cancel interrupts I/O only while the scope owns its temporary deadlines.
func (s *bufferReadScope) cancel() {
	t := s.manager
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	// net/http can cancel its context as a consequence of a read timeout.
	// Only cancellation that shortened a still-future deadline is evidence
	// that this callback interrupted the read.
	s.canceled = true
	// An existing stricter deadline must remain in force. Failed calls
	// establish no evidence that cancellation interrupted the read.
	deadline := earlier(s.applied, now)
	if err := t.controller.SetReadDeadline(deadline); err == nil {
		s.interrupted = s.interrupted || now.Before(s.applied)
		s.applied = deadline
	}
	if s.continueReading {
		deadline = earlier(s.continueDeadline, now)
		if err := t.controller.SetWriteDeadline(deadline); err == nil {
			s.interrupted = s.interrupted || now.Before(s.continueDeadline)
			s.continueDeadline = deadline
		}
	}
}

// finish restores both the recorded deadline and its existing owner.
func (s *bufferReadScope) finish() error {
	t := s.manager
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buffer = nil
	deadline := time.Time{}
	if t.readOwned {
		deadline = t.readDeadline
	} else if t.drainers != 0 && t.idle > 0 {
		deadline = time.Now().Add(t.idle)
	}

	readErr := t.controller.SetReadDeadline(deadline)
	var writeErr error
	if s.writeScoped {
		deadline = time.Time{}
		if t.writeOwned {
			deadline = t.ownedWriteDeadline
		}
		writeErr = t.controller.SetWriteDeadline(deadline)
	}

	if readErr != nil || writeErr != nil {
		// Fail closed in each direction that cannot restore its deadline.
		if readErr != nil {
			_ = t.controller.SetReadDeadline(time.Now())
		}
		if writeErr != nil {
			_ = t.controller.SetWriteDeadline(time.Now())
		}

		return errors.Join(errBufferDeadlineSupport, readErr, writeErr)
	}

	return nil
}

// inboundReadTimeoutError carries evidence from an applied inbound deadline.
type inboundReadTimeoutError struct {
	err error
}

// Error contains no request or underlying error data.
func (e *inboundReadTimeoutError) Error() string {
	return "inbound read deadline reached"
}

// Unwrap preserves the original typed read failure.
func (e *inboundReadTimeoutError) Unwrap() error {
	return e.err
}

// canceledRead needs both an interrupted deadline and a deadline read error.
func (s *bufferReadScope) canceledRead(err error) bool {
	s.manager.mu.Lock()
	defer s.manager.mu.Unlock()

	_, timeout := errors.AsType[*inboundReadTimeoutError](err)

	return s.interrupted && timeout
}

// read applies the total deadline also to reads that already buffered data
// satisfies, and attaches deadline evidence to a read failure.
func (s *bufferReadScope) read(r io.Reader, p []byte) (int, error) {
	if err := s.totalError(); err != nil {
		return 0, err
	}

	n, err := r.Read(p)
	if err == nil {
		err = s.totalError()
	}

	return n, s.readError(err)
}

// totalError reports an expired total deadline.
func (s *bufferReadScope) totalError() error {
	s.manager.mu.Lock()
	defer s.manager.mu.Unlock()

	if !time.Now().Before(s.total) {
		return &inboundReadTimeoutError{
			err: context.DeadlineExceeded,
		}
	}

	return nil
}

// readError attaches evidence only after an applied deadline expired.
func (s *bufferReadScope) readError(err error) error {
	if err == nil {
		return nil
	}

	s.manager.mu.Lock()
	defer s.manager.mu.Unlock()

	deadline := s.applied
	if s.continueReading {
		deadline = earlier(deadline, s.continueDeadline)
	}
	if deadline.IsZero() || time.Now().Before(deadline) {
		return err
	}

	if timeout, ok := errors.AsType[net.Error](err); ok && timeout.Timeout() {
		return &inboundReadTimeoutError{
			err: err,
		}
	}

	return err
}
