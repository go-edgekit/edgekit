package edgekit

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// transferChunk is the largest write that one deadline covers. A deadline
// before one large write would limit its total time, not its progress.
const transferChunk = 16 << 10

// transfer applies the transfer idle deadlines to one request. Its methods
// are safe for concurrent use, because the transport reads the request body
// in a different goroutine than the handler writes the response.
type transfer struct {
	// controller is the controller of the writer of net/http. The server
	// uses it directly, so its own deadlines do not count as a takeover.
	controller *http.ResponseController
	idle       time.Duration
	http1      bool

	mu sync.Mutex

	// done is true after the handler returned or took the connection.
	// Then the writer of net/http must not be used.
	done bool

	// readOwned and writeOwned are true after the handler took over the
	// deadline of that direction.
	readOwned          bool
	writeOwned         bool
	readDeadline       time.Time
	ownedWriteDeadline time.Time
	buffer             *bufferReadScope

	// readers counts the reads of the request body that are in progress.
	readers int

	// drainers counts the writes in progress that can make net/http read the
	// unread request body before it writes the response headers.
	drainers int

	// fullDuplex is true after the handler enabled full duplex on HTTP/1.1.
	// Then net/http does not read the unread body before a write.
	fullDuplex bool

	// expectContinue is true until the first read of a request body with
	// "Expect: 100-continue" on HTTP/1.1. That read makes net/http write
	// "100 Continue".
	expectContinue bool

	// bodyDone is true when the request body has no unread data that
	// net/http reads itself: there is no body, or it returned EOF or was
	// closed.
	bodyDone bool
}

// beginRead sets the read deadline, and reports whether endRead must
// remove it. Before the first read that makes net/http write "100 Continue",
// it also sets a write deadline. It leaves that deadline set, because a write
// of the handler can be active at the same time; each later write sets its
// own deadline. It also returns the buffering scope of the read, or nil for
// a streaming read, so streaming reads skip the buffering checks.
func (t *transfer) beginRead() (armed bool, scope *bufferReadScope, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.expectContinue {
		t.expectContinue = false
		if !t.done && t.buffer != nil {
			deadline := t.buffer.deadline(time.Now())
			if t.writeOwned {
				deadline = earlier(deadline, t.ownedWriteDeadline)
			}
			if err := t.controller.SetWriteDeadline(deadline); err != nil {
				return false, nil, errors.Join(errBufferDeadlineSupport, err)
			}
			t.buffer.continueDeadline = deadline
			t.buffer.writeScoped = true
			t.buffer.continueReading = true
		} else if !t.done && !t.writeOwned && t.idle > 0 {
			_ = t.controller.SetWriteDeadline(time.Now().Add(t.idle))
		}
	}

	if !t.done && t.buffer != nil {
		deadline := t.buffer.deadline(time.Now())
		if err := t.controller.SetReadDeadline(deadline); err != nil {
			return false, nil, errors.Join(errBufferDeadlineSupport, err)
		}

		if t.buffer.canceled && deadline.Before(t.buffer.applied) {
			t.buffer.interrupted = true
		}
		t.buffer.applied = deadline
		t.readers++

		return true, t.buffer, nil
	}

	if t.done || t.readOwned || t.idle == 0 {
		return false, nil, nil
	}

	t.readers++
	_ = t.controller.SetReadDeadline(time.Now().Add(t.idle))

	return true, nil, nil
}

// endRead removes the read deadline when no other call needs it. When a
// write is in progress that can make net/http read the unread body, it sets
// a new deadline instead, because that read can start now.
func (t *transfer) endRead(armed bool) {
	if !armed {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.readers--
	if t.readers == 0 && t.buffer != nil {
		t.buffer.continueReading = false
	}
	if t.readers != 0 || t.done || t.readOwned || t.buffer != nil {
		return
	}

	if t.drainers != 0 {
		_ = t.controller.SetReadDeadline(time.Now().Add(t.idle))
	} else {
		_ = t.controller.SetReadDeadline(time.Time{})
	}
}

// beginWrite sets the write deadline. On HTTP/1.1 with an unread request
// body and without full duplex, net/http can read that body before it
// writes the response headers. Then beginWrite also gives the write one more
// timeout, so the write can start after a read that used its full time, and
// sets the read deadline when no read of the body is in progress. It does not
// change the deadline of a read in progress, so writes cannot hide a stalled
// upload.
func (t *transfer) beginWrite() writeDeadlines {
	t.mu.Lock()
	defer t.mu.Unlock()

	drain := t.http1 && !t.bodyDone && !t.fullDuplex
	set := writeDeadlines{
		write: !t.done && !t.writeOwned && t.idle > 0,
		read:  drain && !t.done && !t.readOwned && t.idle > 0 && t.buffer == nil,
	}

	if set.write {
		_ = t.controller.SetWriteDeadline(t.writeDeadline(drain))
	}

	if set.read {
		t.drainers++
		if t.readers == 0 {
			_ = t.controller.SetReadDeadline(time.Now().Add(t.idle))
		}
	}

	return set
}

// writeDeadlines are the deadlines that beginWrite set and endWrite removes.
type writeDeadlines struct {
	write bool
	read  bool
}

// writeDeadline returns the write deadline from now. t.mu must be held.
func (t *transfer) writeDeadline(drain bool) time.Time {
	if drain && !t.readOwned {
		return time.Now().Add(2 * t.idle)
	}

	return time.Now().Add(t.idle)
}

// endBody records that the request body has no unread data.
func (t *transfer) endBody() {
	t.mu.Lock()
	t.bodyDone = true
	t.mu.Unlock()
}

// endWrite removes the deadlines that beginWrite set. It keeps the read
// deadline of a read of the body that is in progress.
func (t *transfer) endWrite(set writeDeadlines) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if set.read {
		t.drainers--
		if t.drainers == 0 && t.readers == 0 && !t.done && !t.readOwned {
			_ = t.controller.SetReadDeadline(time.Time{})
		}
	}

	if set.write && !t.done && !t.writeOwned {
		_ = t.controller.SetWriteDeadline(time.Time{})
	}
}

// enableFullDuplex enables full duplex, and records it when it succeeds.
func (t *transfer) enableFullDuplex() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	err := t.controller.EnableFullDuplex()
	if err == nil {
		t.fullDuplex = true
	}

	return err
}

// setReadDeadline is a read deadline call of the handler. A successful call
// takes over the read direction; a failed call keeps the protection.
func (t *transfer) setReadDeadline(deadline time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	err := t.controller.SetReadDeadline(deadline)
	if err == nil {
		t.readOwned = true
		t.readDeadline = deadline
	}

	return err
}

// setWriteDeadline is a write deadline call of the handler. A successful call
// takes over the write direction; a failed call keeps the protection.
func (t *transfer) setWriteDeadline(deadline time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	err := t.controller.SetWriteDeadline(deadline)
	if err == nil {
		t.writeOwned = true
		t.ownedWriteDeadline = deadline
	}

	return err
}

// stop ends the deadline management after a successful hijack, because the
// connection then belongs to the hijacker.
func (t *transfer) stop() {
	t.mu.Lock()
	t.done = true
	t.mu.Unlock()
}

// finish sets the deadlines for the work that net/http does after the
// handler returns: the final write, and on HTTP/1.1 the read of the unread
// request body before and after that write. It leaves them set: net/http
// sets its own read deadline for the next request, and the server removes
// the write deadline when the connection is idle.
func (t *transfer) finish() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.done {
		return
	}

	t.done = true
	drain := t.http1 && !t.bodyDone

	if !t.writeOwned && t.idle > 0 {
		_ = t.controller.SetWriteDeadline(t.writeDeadline(drain))
	}

	if drain && !t.readOwned && t.idle > 0 {
		_ = t.controller.SetReadDeadline(time.Now().Add(t.idle))
	}
}

// transferBody is a request body with read deadlines. Close needs a deadline
// too, because net/http reads the unread body on close to reuse the
// connection.
type transferBody struct {
	io.ReadCloser

	transfer *transfer
}

// Read reads with a read deadline.
func (b *transferBody) Read(p []byte) (int, error) {
	armed, scope, err := b.transfer.beginRead()
	if err != nil {
		return 0, err
	}

	defer b.transfer.endRead(armed)

	var n int
	if scope == nil {
		n, err = b.ReadCloser.Read(p)
	} else {
		n, err = scope.read(b.ReadCloser, p)
	}

	if errors.Is(err, io.EOF) {
		b.transfer.endBody()
	}

	return n, err
}

// Close closes with a read deadline.
func (b *transferBody) Close() error {
	armed, _, err := b.transfer.beginRead()
	if err != nil {
		return err
	}

	defer b.transfer.endRead(armed)

	err = b.ReadCloser.Close()
	b.transfer.endBody()

	return err
}

// transferWriter is a response writer with write deadlines. Its variants
// add Hijack and Push only when the writer of net/http has them, so the
// optional interfaces stay the same.
type transferWriter struct {
	http.ResponseWriter

	transfer *transfer
}

// hijackTransferWriter is a transferWriter for an HTTP/1.1 writer.
type hijackTransferWriter struct {
	*transferWriter
}

// pushTransferWriter is a transferWriter for an HTTP/2 writer.
type pushTransferWriter struct {
	*transferWriter
}

// hijackPushTransferWriter is a transferWriter for a writer with both.
type hijackPushTransferWriter struct {
	*transferWriter
}

// newTransferWriter returns the variant with the optional interfaces of w.
func newTransferWriter(w http.ResponseWriter, t *transfer) http.ResponseWriter {
	tw := &transferWriter{
		ResponseWriter: w,
		transfer:       t,
	}

	_, hijacker := w.(http.Hijacker)
	_, pusher := w.(http.Pusher)

	switch {
	case hijacker && pusher:
		return hijackPushTransferWriter{tw}
	case hijacker:
		return hijackTransferWriter{tw}
	case pusher:
		return pushTransferWriter{tw}
	default:
		return tw
	}
}

// WriteHeader writes with deadlines, because a 1xx status is sent
// immediately.
func (w *transferWriter) WriteHeader(status int) {
	set := w.transfer.beginWrite()
	w.ResponseWriter.WriteHeader(status)
	w.transfer.endWrite(set)
}

// Write writes b in parts, each with its own deadline.
func (w *transferWriter) Write(b []byte) (int, error) {
	written := 0
	for {
		part := b[:min(len(b), transferChunk)]
		set := w.transfer.beginWrite()
		n, err := w.ResponseWriter.Write(part)
		w.transfer.endWrite(set)

		written += n
		b = b[len(part):]

		if err != nil || len(b) == 0 {
			return written, err
		}
	}
}

// Flush flushes with deadlines. http.Flusher has no error result.
func (w *transferWriter) Flush() {
	_ = w.FlushError()
}

// FlushError flushes with deadlines. http.ResponseController prefers it to
// Flush.
func (w *transferWriter) FlushError() error {
	set := w.transfer.beginWrite()
	err := w.transfer.controller.Flush()
	w.transfer.endWrite(set)

	return err
}

// EnableFullDuplex enables full duplex. The server records it, because then
// net/http does not read the unread request body before a write.
func (w *transferWriter) EnableFullDuplex() error {
	return w.transfer.enableFullDuplex()
}

// SetReadDeadline takes over the read deadline when it succeeds.
func (w *transferWriter) SetReadDeadline(deadline time.Time) error {
	return w.transfer.setReadDeadline(deadline)
}

// SetWriteDeadline takes over the write deadline when it succeeds.
func (w *transferWriter) SetWriteDeadline(deadline time.Time) error {
	return w.transfer.setWriteDeadline(deadline)
}

// Unwrap gives access to the underlying writer without the deadline
// ownership checks.
func (w *transferWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// hijack takes the connection and ends the deadline management.
func (w *transferWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := w.transfer.controller.Hijack()
	if err == nil {
		w.transfer.stop()
	}

	return conn, buffered, err
}

// push starts an HTTP/2 server push.
func (w *transferWriter) push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}

	return pusher.Push(target, opts)
}

// Hijack takes the connection.
func (w hijackTransferWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.hijack()
}

// Push starts a server push.
func (w pushTransferWriter) Push(target string, opts *http.PushOptions) error {
	return w.push(target, opts)
}

// Hijack takes the connection.
func (w hijackPushTransferWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.hijack()
}

// Push starts a server push.
func (w hijackPushTransferWriter) Push(target string, opts *http.PushOptions) error {
	return w.push(target, opts)
}
