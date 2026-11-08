package edgekit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/textproto"
	"slices"
	"sync"
	"time"
)

// RequestBuffering enables complete request buffering before processing
// middleware. Set it on [Config.RequestBuffering] as an inherited default,
// or on [Route.RequestBuffering] to replace that default. A nil route setting
// inherits the gateway setting; a nil gateway setting keeps streaming. Use
// [NoRequestBuffering] to select streaming explicitly. New copies each
// effective configuration and shares its Budget between requests. A non-nil
// zero value is invalid; it does not inherit individual fields. Do not change
// configuration values while New reads them.
//
// Buffering requires a finite effective [BodyLimit], a Budget large enough
// for that limit, and positive explicit timeouts. A request with a body also
// requires the scoped read-deadline support of [Server]: serve the gateway
// with Server, or through a writer wrapper that keeps that support available
// with Unwrap. Without it, the request fails with 500 before the gateway reads
// the body, and [Completion.Failure] holds the cause. [New] cannot check this,
// because it does not know the server. Streaming does not require it. There
// is no temporary-file spill or response buffering.
//
// A request without a body (a nil Body or [http.NoBody]) skips buffering: it
// reserves nothing, needs no deadline support, and gets a GetBody that
// returns [http.NoBody]. A request with a declared length reserves that
// length. A request with an unknown length reserves the complete effective
// limit before reading, even for a small body: on a 100 MiB route, it
// reserves 100 MiB but does not necessarily allocate it. A full Budget
// rejects immediately with 503 and [ReasonRequestBufferSaturated].
// Reservations bound stored-body capacity, not process memory, runtime
// overhead, or fatal Go allocation failures.
//
// Processing starts only after a checked EOF within the reservation. Body and
// GetBody then provide request-scoped cursors over shared immutable storage,
// without copying the complete body or counting inbound bytes again. Repeated
// and concurrent replays are supported, including concurrent access to one
// cursor. Reads consume a cursor; middleware must explicitly replace Body with
// a replay to forward content that it consumed. It must close the replaced
// cursor. Middleware that replaces the representation must also replace or
// clear GetBody and own its ContentLength and trailers. Replacing the body
// removes unchanged checked client trailer values, while explicit new values
// remain. The outbound transport always gets a nil GetBody, so this
// capability does not enable transport replay retries.
//
// When the outbound request still has an unread gateway cursor, an unknown
// length, no trailer fields, and a body that is not empty, the gateway sets
// its ContentLength to the stored size. The upstream then gets a declared
// length instead of chunked framing. The gateway never changes an unknown
// length to zero, because net/http can retry a request without a body.
//
// Storage and its reservation last until protected route handling returns or
// unwinds, including a stalled or streaming response or an upgraded
// connection. Closing one cursor does not release the reservation or
// invalidate other cursors. Cleanup invalidates all cursors and replay
// functions, drops their stored data, and then releases the reservation.
// Garbage collection and memory return to the operating system are not
// immediate. Invalid handles fail with [io.ErrClosedPipe]. Consumers must not
// retain bodies or replay functions after handling.
//
// Timeouts apply only during inbound buffering. Each read uses the earliest
// buffering total, buffering idle, normal server idle, or recorded caller
// deadline. The temporary scope preserves deadline ownership and restores it
// on every exit. The first HTTP/1.1 read's 100 Continue write is also bounded
// without a write-ownership takeover. Unknown body-reader errors are internal
// failures (500), not malformed-input rejections. See [PhaseRequestBuffering]
// and [Gateway].
type RequestBuffering struct {
	// Budget is required for enabled buffering and can be shared across
	// routes and gateways.
	Budget *RequestBufferBudget

	// IdleTimeout limits one blocking read. It must be positive.
	IdleTimeout time.Duration

	// TotalTimeout limits the complete inbound buffering phase. It must be
	// positive. Neither timeout has a default or a disabled value.
	TotalTimeout time.Duration

	// disabled distinguishes an explicit streaming choice from inheritance.
	disabled bool
}

// NoRequestBuffering returns a configuration that explicitly selects streaming.
// Use it on [Route.RequestBuffering] to override the gateway default, or on
// [Config.RequestBuffering] to keep streaming as the default. It does not
// disable the body limit or admission. Each call returns a new value owned by
// the caller; New copies it. Do not add a budget or timeouts to this value:
// New rejects such settings with [ErrInvalidConfiguration]. The returned
// configuration has no storage, background work, or Close operation.
func NoRequestBuffering() *RequestBuffering {
	return &RequestBuffering{
		disabled: true,
	}
}

// resolve selects the complete configuration; nil inherits fallback, and
// fields are never merged.
func (rb *RequestBuffering) resolve(fallback *RequestBuffering) *RequestBuffering {
	if rb == nil {
		rb = fallback
	}

	if rb == nil || rb.disabled {
		return nil
	}

	return rb
}

// RequestBufferBudget limits aggregate reserved stored-body bytes. Make it
// with [NewRequestBufferBudget] and share it through [RequestBuffering.Budget].
// It is safe for concurrent use. Its capacity is immutable. A reservation
// covers the declared length, or the complete effective route limit for an
// unknown length, until all gateway-owned replay capabilities are
// invalidated (see [RequestBuffering]). Do not copy a budget after first use.
// It is not a request acceptance limit or a complete process-memory bound.
// The zero value is not valid configuration.
type RequestBufferBudget struct {
	mu       sync.Mutex
	capacity int64
	used     int64
}

// NewRequestBufferBudget returns a shared byte budget with positive capacity.
// An invalid capacity returns an error wrapping [ErrInvalidConfiguration]
// without its value. The caller owns the budget and may share it between
// routes and gateways; there is no Close operation or background work.
func NewRequestBufferBudget(bytes int64) (*RequestBufferBudget, error) {
	if bytes <= 0 {
		return nil, invalid("request buffer budget: capacity is not positive")
	}

	return &RequestBufferBudget{
		capacity: bytes,
	}, nil
}

// reserve never waits for capacity and cannot overflow the counter.
func (b *RequestBufferBudget) reserve(bytes int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if bytes > b.capacity-b.used {
		return false
	}

	b.used += bytes

	return true
}

// release returns a reservation after storage invalidation.
func (b *RequestBufferBudget) release(bytes int64) {
	b.mu.Lock()
	b.used -= bytes
	b.mu.Unlock()
}

// checkBufferingSettings checks one configuration without a body limit.
func checkBufferingSettings(cfg *RequestBuffering) error {
	if cfg == nil {
		return nil
	}

	if cfg.disabled {
		if cfg.Budget != nil || cfg.IdleTimeout != 0 || cfg.TotalTimeout != 0 {
			return errors.New("streaming configuration contains buffering settings")
		}

		return nil
	}

	switch {
	case cfg.Budget == nil || cfg.Budget.capacity <= 0:
		return errors.New("requires a constructed byte budget")
	case cfg.IdleTimeout <= 0 || cfg.TotalTimeout <= 0:
		return errors.New("requires positive idle and total timeouts")
	}

	return nil
}

// checkRouteBuffering checks the route setting and the effective configuration
// against the effective route limit. The gateway setting must be valid or nil.
func checkRouteBuffering(route, gateway *RequestBuffering, limit int64) error {
	if err := checkBufferingSettings(route); err != nil {
		return err
	}

	cfg := route.resolve(gateway)
	if cfg == nil {
		return nil
	}

	switch {
	case limit == noBodyLimit:
		return errors.New("requires a finite effective body limit")
	case cfg.Budget.capacity < limit:
		return errors.New("byte budget is smaller than the effective body limit")
	}

	return nil
}

// bufferChunk bounds the temporary allocation of each storage segment.
const bufferChunk = 32 << 10

// maxEmptyReads stops a source that returns no data and no error, as
// bufio does.
const maxEmptyReads = 100

// errBufferOverrun reports a source that gives data after its reservation.
// net/http prevents it, so it is an internal failure, not client evidence.
var errBufferOverrun = errors.New("request body source gives data after its reservation")

// requestBuffer owns data independently of cursors, so invalidation removes
// all body-sized references before releasing capacity.
type requestBuffer struct {
	mu       sync.Mutex
	chunks   [][]byte
	size     int64
	closed   bool
	budget   *RequestBufferBudget
	reserved int64
}

// dispose revokes all handles before releasing reserved capacity.
func (b *requestBuffer) dispose() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}

	b.closed = true
	b.chunks = nil
	b.size = 0
	b.budget.release(b.reserved)
}

// fill runs before any cursor is published. It allocates one bounded segment
// before the reads that fill it, not one body-sized copy, so allocated
// segment capacity never exceeds the reservation. All segments except the
// last are full, which the cursor offset arithmetic requires.
func (b *requestBuffer) fill(body io.Reader) error {
	for b.size < b.reserved {
		part := make([]byte, min(int64(bufferChunk), b.reserved-b.size))
		used := 0
		stalls := 0
		for used < len(part) {
			n, err := body.Read(part[used:])
			used += n
			b.size += int64(n)
			if err != nil {
				b.chunks = append(b.chunks, part[:used])
				if err == io.EOF {
					return nil
				}

				return err
			}

			if n == 0 {
				stalls++
				if stalls == maxEmptyReads {
					return io.ErrNoProgress
				}
			} else {
				stalls = 0
			}
		}

		b.chunks = append(b.chunks, part)
	}

	// After the reserved bytes, the source must end. The inbound limiter
	// reads one more byte to tell an exact limit from overflow, and net/http
	// ends a body at its declared length. A byte after the reservation is
	// not stored or exposed: it fails the buffering.
	var probe [1]byte
	for range maxEmptyReads {
		n, err := body.Read(probe[:])
		if err != nil && err != io.EOF {
			return err
		}

		if n > 0 {
			return errBufferOverrun
		}

		if err == io.EOF {
			return nil
		}
	}

	return io.ErrNoProgress
}

// replay allocates a cursor, never a copy of stored data.
func (b *requestBuffer) replay() (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, io.ErrClosedPipe
	}

	return &bufferCursor{
		store: b,
	}, nil
}

// bufferCursor owns only an offset and a reference to the revocable store.
type bufferCursor struct {
	mu     sync.Mutex
	store  *requestBuffer
	offset int64
}

// Read serializes cursor access with storage invalidation.
func (c *bufferCursor) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.store == nil {
		return 0, io.ErrClosedPipe
	}

	b := c.store
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return 0, io.ErrClosedPipe
	}

	if c.offset == b.size {
		return 0, io.EOF
	}

	n := 0
	for n < len(p) && c.offset < b.size {
		segment := b.chunks[c.offset/bufferChunk]
		copied := copy(p[n:], segment[c.offset%bufferChunk:])
		c.offset += int64(copied)
		n += copied
	}

	return n, nil
}

// Close drops this cursor's store reference without affecting other cursors.
func (c *bufferCursor) Close() error {
	c.mu.Lock()
	c.store = nil
	c.mu.Unlock()

	return nil
}

// state reports whether c is a cursor of store, and whether it is still at
// the start of the content.
func (c *bufferCursor) state(store *requestBuffer) (live, unread bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.store != store {
		return false, false
	}

	return true, c.offset == 0
}

// bufferedOutbound returns the request for the transport, a copy when it
// changes r. Client trailer values stay only with a live gateway cursor,
// because a replacement representation owns its own trailer values. The
// declared-length rule is in the RequestBuffering contract.
func (rc *RequestContext) bufferedOutbound(r *http.Request) *http.Request {
	var live, unread bool
	if cursor, ok := r.Body.(*bufferCursor); ok {
		live, unread = cursor.state(rc.buffer)
	}

	out := r
	ensureCopy := func() {
		if out == r {
			out = r.Clone(r.Context())
		}
	}

	// A length of zero would let ReverseProxy drop the body, and net/http
	// can retry a request without a body. Trailer fields need chunked
	// framing on HTTP/1.1. The transport prefers the inbound chunked
	// TransferEncoding to ContentLength, so the copy removes it.
	if r.ContentLength < 0 && unread && len(r.Trailer) == 0 {
		if size := rc.buffer.storedSize(); size > 0 {
			ensureCopy()
			out.ContentLength = size
			out.TransferEncoding = nil
		}
	}

	if rc.body == nil || len(rc.body.checked) == 0 {
		return out
	}

	// The first pass finds every client name that middleware set itself, in
	// any letter case. The second pass then deletes unchanged client values.
	// Two passes keep the result independent of map iteration order.
	checked := canonicalTrailerNames(rc.body.checked)
	overridden := make(map[string]bool)
	for name, values := range r.Trailer {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if _, client := checked[canonical]; !client {
			continue
		}

		// A new key is middleware-owned even when its values equal the
		// client's values. The inbound map keeps the published entries.
		original, published := rc.body.inner[name]
		if !published || !slices.Equal(values, original) {
			overridden[canonical] = true
		}
	}

	for name, values := range r.Trailer {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if _, client := checked[canonical]; !client {
			continue
		}

		original, published := rc.body.inner[name]
		unchanged := published && slices.Equal(values, original)
		keep := live && !overridden[canonical]
		if !unchanged || keep {
			continue
		}

		ensureCopy()
		delete(out.Trailer, name)
	}

	return out
}

// storedSize reads the size under the store lock, because cleanup resets it.
func (b *requestBuffer) storedSize() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.size
}

// bufferRequest returns a live store only when it buffered a body. A request
// without a body gets an empty replay and no store. All failure paths revoke
// storage here, including panic; the successful caller defers disposal.
func (g *Gateway) bufferRequest(w http.ResponseWriter, r *http.Request, rc *RequestContext, rt *route) (_ *requestBuffer, ok bool) {
	if r.Body == nil || r.Body == http.NoBody {
		r.GetBody = emptyReplay

		return nil, true
	}

	// The declared-length check already rejected a length over the limit.
	reserved := rt.bodyLimit
	if r.ContentLength >= 0 {
		reserved = r.ContentLength
	}

	cfg := rt.buffering
	if !cfg.Budget.reserve(reserved) {
		g.rejectRequest(r.Context(), w, rc, http.StatusServiceUnavailable, ReasonRequestBufferSaturated)

		return nil, false
	}

	store := &requestBuffer{
		budget:   cfg.Budget,
		reserved: reserved,
	}
	defer func() {
		if !ok {
			store.dispose()
		}
	}()

	err := readBuffered(w, r, store, cfg)
	if err != nil {
		rc.fail(PhaseRequestBuffering, err)
		g.respond(r.Context(), w, rc)

		return nil, false
	}

	body, err := store.replay()
	if err != nil {
		rc.failDefect(PhaseRequestBuffering, err)
		g.respond(r.Context(), w, rc)

		return nil, false
	}

	r.Body = body
	r.GetBody = store.replay
	rc.buffer = store

	return store, true
}

// emptyReplay holds no storage, so it stays valid after handling.
func emptyReplay() (io.ReadCloser, error) {
	return http.NoBody, nil
}

// readBuffered keeps deadline restoration and source closure inside storage
// ownership, including panic unwinding.
func readBuffered(w http.ResponseWriter, r *http.Request, store *requestBuffer, cfg *RequestBuffering) (result error) {
	scope, err := beginBufferRead(w, cfg.IdleTimeout, cfg.TotalTimeout)
	if err != nil {
		return err
	}

	// Stop and join cancellation before restoring ownership. No reader runs
	// detached from this handler.
	finished := make(chan struct{})
	stop := context.AfterFunc(r.Context(), func() {
		scope.cancel()
		close(finished)
	})
	defer func() {
		if !stop() {
			<-finished
		}
		if err := scope.finish(); err != nil && result == nil {
			result = err
		}
	}()

	// bufferRequest handles a nil Body, so the source is a real body.
	body := r.Body
	defer func() { _ = body.Close() }()

	// The AfterFunc callback also runs for a context that is already done,
	// and the deferred function waits for it.
	if err := r.Context().Err(); err != nil {
		return err
	}

	if err := store.fill(body); err != nil {
		if scope.canceledRead(err) && r.Context().Err() != nil {
			return errors.Join(err, r.Context().Err())
		}

		return err
	}

	if r.ContentLength >= 0 && r.ContentLength != store.size {
		return io.ErrUnexpectedEOF
	}

	return scope.totalError()
}

// bufferedFailureStatus requires typed input evidence. An arbitrary reader's
// error, including a net.Error without deadline evidence, stays internal.
func bufferedFailureStatus(err error) int {
	if _, ok := errors.AsType[*inboundReadTimeoutError](err); ok {
		return http.StatusRequestTimeout
	}

	if errors.Is(err, io.ErrUnexpectedEOF) {
		return http.StatusBadRequest
	}

	return http.StatusInternalServerError
}
