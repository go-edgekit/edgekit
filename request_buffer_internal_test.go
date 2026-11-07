package edgekit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests inspect storage and deadline ownership, which have no public
// mutable surface. Public lifecycle tests are in request_buffer_test.go.

func TestRequestBufferStorageAndReplayBounds(t *testing.T) {
	const size = 3*bufferChunk + 7
	budget, err := NewRequestBufferBudget(size)
	if err != nil {
		t.Fatal(err)
	}
	if !budget.reserve(size) {
		t.Fatalf("reserve(%d) on an unused budget = false, want true", size)
	}
	store := &requestBuffer{
		budget:   budget,
		reserved: size,
	}
	defer store.dispose()
	data := strings.Repeat("x", size)
	source := &countingReader{
		Reader: strings.NewReader(data),
	}
	r := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(source))
	inner := r.Clone(r.Context())
	inner.Trailer = make(http.Header)
	inbound := (&Gateway{}).newRequestBody(r, inner, size)
	if err = store.fill(inbound); err != nil {
		t.Fatal(err)
	}

	allocated := 0
	for _, chunk := range store.chunks {
		allocated += cap(chunk)
	}
	if allocated != size || len(store.chunks) != 4 || budget.used != size {
		t.Errorf("fill(%d bytes): allocated/segments/reserved = %d/%d/%d, want %d/4/%d", size, allocated, len(store.chunks), budget.used, size, size)
	}

	var wg sync.WaitGroup
	handles := make([]io.ReadCloser, 32)
	for i := range handles {
		handles[i], err = store.replay()
		if err != nil {
			t.Fatal(err)
		}
		cursor, ok := handles[i].(*bufferCursor)
		if !ok || cursor.store != store {
			t.Fatalf("replay() %d: cursor %T does not refer to the store", i, handles[i])
		}
		wg.Go(func() {
			got, err := io.ReadAll(handles[i])
			if string(got) != data || err != nil {
				t.Errorf("ReadAll(replay %d) = %d bytes, %v; want %d bytes, nil", i, len(got), err, size)
			}
		})
	}
	wg.Wait()

	if source.read != size || inbound.bytesRead.Load() != size || inbound.overflowFound() {
		t.Errorf("after 32 replays: source read %d, inbound exposed %d, overflow %v; want %d, %d, false", source.read, inbound.bytesRead.Load(), inbound.overflowFound(), size, size)
	}
	if budget.reserve(1) {
		t.Error("reserve(1) on a full budget = true, want false")
	}

	store.dispose()
	if budget.used != 0 || store.chunks != nil {
		t.Errorf("after dispose(): segments %d, reserved %d; want nil, 0", len(store.chunks), budget.used)
	}
	for _, handle := range handles {
		if _, err := handle.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("retained replay Read() after dispose() error = %v, want %v", err, io.ErrClosedPipe)
		}
	}
	if _, err := store.replay(); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("replay() after dispose() error = %v, want %v", err, io.ErrClosedPipe)
	}
}

func TestConcurrentReadsOfOneReplayCursor(t *testing.T) {
	const size = 128
	budget, err := NewRequestBufferBudget(size)
	if err != nil {
		t.Fatal(err)
	}
	if !budget.reserve(size) {
		t.Fatalf("reserve(%d) on an unused budget = false, want true", size)
	}
	store := &requestBuffer{
		budget:   budget,
		reserved: size,
	}
	defer store.dispose()
	if err = store.fill(strings.NewReader(strings.Repeat("x", size))); err != nil {
		t.Fatal(err)
	}
	cursor, err := store.replay()
	if err != nil {
		t.Fatal(err)
	}
	var total atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			var part [3]byte
			for {
				n, err := cursor.Read(part[:])
				total.Add(int64(n))
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Errorf("concurrent cursor Read() error = %v, want nil or io.EOF", err)

					return
				}
			}
		})
	}
	wg.Wait()
	if total.Load() != size {
		t.Errorf("8 concurrent readers of one cursor read %d bytes, want %d", total.Load(), size)
	}
}

func TestRequestBufferConfiguration(t *testing.T) {
	budget, err := NewRequestBufferBudget(8)
	if err != nil {
		t.Fatal(err)
	}
	for _, bytes := range []int64{0, -1} {
		if _, err := NewRequestBufferBudget(bytes); !errors.Is(err, ErrInvalidConfiguration) {
			t.Errorf("NewRequestBufferBudget(%d) error = %v, want %v", bytes, err, ErrInvalidConfiguration)
		}
	}
	for _, tc := range []struct {
		name   string
		limit  BodyLimit
		budget *RequestBufferBudget
		idle   time.Duration
		total  time.Duration
		valid  bool
	}{
		{
			name:   "finite",
			limit:  MaxBodyBytes(8),
			budget: budget,
			idle:   time.Second,
			total:  time.Second,
			valid:  true,
		},
		{
			name:   "zero",
			limit:  MaxBodyBytes(0),
			budget: budget,
			idle:   time.Second,
			total:  time.Second,
			valid:  true,
		},
		{
			name:   "disabled limit",
			limit:  NoBodyLimit(),
			budget: budget,
			idle:   time.Second,
			total:  time.Second,
		},
		{
			name:   "inherited default limit",
			budget: budget,
			idle:   time.Second,
			total:  time.Second,
		},
		{
			name:  "missing budget",
			limit: MaxBodyBytes(8),
			idle:  time.Second,
			total: time.Second,
		},
		{
			name:   "unconstructed budget",
			limit:  MaxBodyBytes(8),
			budget: new(RequestBufferBudget),
			idle:   time.Second,
			total:  time.Second,
		},
		{
			name:   "insufficient budget",
			limit:  MaxBodyBytes(9),
			budget: budget,
			idle:   time.Second,
			total:  time.Second,
		},
		{
			name:   "omitted idle",
			limit:  MaxBodyBytes(8),
			budget: budget,
			total:  time.Second,
		},
		{
			name:   "disabled total",
			limit:  MaxBodyBytes(8),
			budget: budget,
			idle:   time.Second,
			total:  NoTimeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := New(Config{
				Routes: []Route{
					{
						Prefix: "/",
						Upstream: staticUpstream{url: url.URL{
							Scheme: "http",
							Host:   "unused.test",
						}},
						BodyLimit: tc.limit,
						RequestBuffering: &RequestBuffering{
							Budget:       tc.budget,
							IdleTimeout:  tc.idle,
							TotalTimeout: tc.total,
						},
					},
				},
			})
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				g.CloseIdleConnections()
			} else if !errors.Is(err, ErrInvalidConfiguration) {
				t.Errorf("New(%s) error = %v, want %v", tc.name, err, ErrInvalidConfiguration)
			}
		})
	}
}

// deadlineRecorder has deadline support without clock-driven I/O, so tests
// can inspect the exact ownership and failure transitions.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu             sync.Mutex
	deadlines      []time.Time
	failAt         int
	notify         func(time.Time)
	writeDeadlines []time.Time
	writeFailAt    int
}

func (w *deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.deadlines = append(w.deadlines, deadline)
	if w.notify != nil {
		w.notify(deadline)
	}
	if len(w.deadlines) == w.failAt {
		return errors.New("private deadline failure")
	}

	return nil
}

func (w *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.writeDeadlines = append(w.writeDeadlines, deadline)
	if len(w.writeDeadlines) == w.writeFailAt {
		return errors.New("private write deadline failure")
	}

	return nil
}

type bufferInput struct {
	read   func([]byte) (int, error)
	reads  int
	closes int
}

func (r *bufferInput) Read(p []byte) (int, error) {
	r.reads++

	return r.read(p)
}

func (r *bufferInput) Close() error {
	r.closes++

	return nil
}

type bufferHarness struct {
	gateway          *Gateway
	request          *http.Request
	writer           *deadlineRecorder
	manager          *transfer
	budget           *RequestBufferBudget
	gatewayAdmission *AdmissionBudget
	routeAdmission   *AdmissionBudget
	completion       Completion
	processed        bool
	afterBuffer      func(*http.Request)
}

func newBufferHarness(t *testing.T, source io.ReadCloser) *bufferHarness {
	t.Helper()

	budget, err := NewRequestBufferBudget(8)
	if err != nil {
		t.Fatal(err)
	}
	gatewayAdmission, err := NewAdmissionBudget(1)
	if err != nil {
		t.Fatal(err)
	}
	routeAdmission, err := NewAdmissionBudget(1)
	if err != nil {
		t.Fatal(err)
	}
	h := &bufferHarness{
		writer:           &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()},
		budget:           budget,
		gatewayAdmission: gatewayAdmission,
		routeAdmission:   routeAdmission,
	}
	h.manager = &transfer{
		controller: http.NewResponseController(h.writer),
		idle:       time.Minute,
	}
	h.request = httptest.NewRequest(http.MethodPost, "/", nil)
	h.request.ContentLength = -1
	h.request.Body = &transferBody{
		ReadCloser: source,
		transfer:   h.manager,
	}
	h.gateway, err = New(Config{
		Admission: gatewayAdmission,
		Routes: []Route{
			{
				Prefix: "/",
				Upstream: staticUpstream{url: url.URL{
					Scheme: "http",
					Host:   "unused.test",
				}},
				BodyLimit: MaxBodyBytes(8),
				Admission: routeAdmission,
				RequestBuffering: &RequestBuffering{
					Budget:       budget,
					IdleTimeout:  time.Minute,
					TotalTimeout: time.Minute,
				},
			},
		},
		SharedMiddleware: []Middleware{
			func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					h.processed = true
					if h.afterBuffer != nil {
						h.afterBuffer(r)
					}
					w.WriteHeader(http.StatusNoContent)
				})
			},
		},
		Hooks: Hooks{
			RequestCompleted: []RequestCompletedHook{
				RequestCompletedHookFunc(func(_ context.Context, _ *RequestContext, c Completion) {
					h.completion = c
				}),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.gateway.CloseIdleConnections)

	return h
}

// harnessState holds the comparable request and resource state of a
// bufferHarness after a request.
type harnessState struct {
	status    int
	processed bool
	reserved  int64
	scoped    bool
	readers   int
}

// state reads the harness state. It makes no assertion.
func (h *bufferHarness) state() harnessState {
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()

	return harnessState{
		status:    h.writer.Code,
		processed: h.processed,
		reserved:  h.budget.used,
		scoped:    h.manager.buffer != nil,
		readers:   h.manager.readers,
	}
}

func TestBufferedReadFailureNeedsEvidence(t *testing.T) {
	private := errors.New("secret request and storage details")
	for _, tc := range []struct {
		name    string
		err     error
		expired bool
		want    int
	}{
		{
			name: "unknown reader",
			err:  private,
			want: http.StatusInternalServerError,
		},
		{
			name: "unestablished timeout",
			err:  bufferTimeoutError{},
			want: http.StatusInternalServerError,
		},
		{
			name:    "applied inbound deadline",
			err:     bufferTimeoutError{},
			expired: true,
			want:    http.StatusRequestTimeout,
		},
		{
			name: "truncated body",
			err:  io.ErrUnexpectedEOF,
			want: http.StatusBadRequest,
		},
		{
			name: "other HTTP error",
			err:  http.ErrNotMultipart,
			want: http.StatusInternalServerError,
		},
		{
			name: "unrelated overflow sentinel",
			err:  ErrRequestBodyTooLarge,
			want: http.StatusInternalServerError,
		},
		{
			name: "unrelated trailer sentinel",
			err:  ErrInvalidTrailerName,
			want: http.StatusInternalServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &bufferInput{
				read: func([]byte) (int, error) { return 0, tc.err },
			}
			h := newBufferHarness(t, source)
			if tc.expired {
				if err := h.manager.setReadDeadline(time.Now().Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			h.gateway.ServeHTTP(newTransferWriter(h.writer, h.manager), h.request)

			want := harnessState{
				status: tc.want,
			}
			if got := h.state(); got != want {
				t.Errorf("ServeHTTP with source error %q = %+v, want %+v", tc.err, got, want)
			}
			if f := h.completion.Failure; f == nil || f.Phase != PhaseRequestBuffering || !errors.Is(f.Err, tc.err) {
				t.Errorf("ServeHTTP with source error %q: Completion.Failure = %+v, want %v wrapping the source error", tc.err, f, PhaseRequestBuffering)
			}
			if body := h.writer.Body.String(); strings.Contains(body, "private") || strings.Contains(body, "secret") {
				t.Errorf("ServeHTTP with source error %q: response body = %q, want no failure details", tc.err, body)
			}
			if source.closes != 1 {
				t.Errorf("ServeHTTP with source error %q: source closes = %d, want 1", tc.err, source.closes)
			}
			if !h.gatewayAdmission.tryAcquire() {
				t.Errorf("ServeHTTP with source error %q: gateway admission slot is still held", tc.err)
			}
			if !h.routeAdmission.tryAcquire() {
				t.Errorf("ServeHTTP with source error %q: route admission slot is still held", tc.err)
			}
		})
	}
}

type bufferTimeoutError struct{}

func (bufferTimeoutError) Error() string   { return "private timeout" }
func (bufferTimeoutError) Timeout() bool   { return true }
func (bufferTimeoutError) Temporary() bool { return false }

func TestBufferCleanupOnPanicCancellationAndCapacityFailure(t *testing.T) {
	for _, scenario := range []string{"panic", "cancellation", "capacity", "unsupported", "deadline failure", "read deadline failure", "restore failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			source := &bufferInput{
				read: func([]byte) (int, error) { return 0, io.EOF },
			}
			h := newBufferHarness(t, source)
			h.request = h.request.WithContext(ctx)
			w := newTransferWriter(h.writer, h.manager)
			switch scenario {
			case "panic":
				source.read = func([]byte) (int, error) { panic("private panic") }
			case "cancellation":
				cancel()
			case "capacity":
				if !h.budget.reserve(8) {
					t.Fatalf("reserve(%d) on an unused budget = false, want true", 8)
				}
			case "unsupported":
				w = h.writer
			case "deadline failure":
				h.writer.failAt = 1
			case "read deadline failure":
				h.writer.failAt = 2
			case "restore failure":
				h.writer.failAt = 4
			}
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				h.gateway.ServeHTTP(w, h.request)
			}()

			wantStatus := http.StatusInternalServerError
			switch scenario {
			case "panic":
				if recovered != "private panic" {
					t.Errorf("ServeHTTP(%s) panic = %v, want %q", scenario, recovered, "private panic")
				}
				if !h.completion.Aborted {
					t.Errorf("ServeHTTP(%s): Completion.Aborted = false, want true", scenario)
				}
				wantStatus = http.StatusOK
			case "cancellation":
				wantStatus = statusClientClosedRequest
			case "capacity":
				wantStatus = http.StatusServiceUnavailable
				if h.completion.RejectionReason != ReasonRequestBufferSaturated {
					t.Errorf("ServeHTTP(%s): Completion.RejectionReason = %q, want %q", scenario, h.completion.RejectionReason, ReasonRequestBufferSaturated)
				}
				if source.reads != 0 {
					t.Errorf("ServeHTTP(%s): source reads = %d, want 0", scenario, source.reads)
				}
				h.budget.release(8)
			case "unsupported", "deadline failure", "read deadline failure":
				if source.reads != 0 {
					t.Errorf("ServeHTTP(%s): source reads = %d, want 0", scenario, source.reads)
				}
			}
			want := harnessState{
				status: wantStatus,
			}
			if got := h.state(); got != want {
				t.Errorf("ServeHTTP(%s) = %+v, want %+v", scenario, got, want)
			}
			if !h.gatewayAdmission.tryAcquire() {
				t.Errorf("ServeHTTP(%s): gateway admission slot is still held", scenario)
			}
			if !h.routeAdmission.tryAcquire() {
				t.Errorf("ServeHTTP(%s): route admission slot is still held", scenario)
			}
		})
	}
}

func TestBufferDeadlineScopePreservesOwnershipAndStricterBounds(t *testing.T) {
	for _, scenario := range []string{"server idle", "owned deadline", "buffer total", "buffer idle", "disabled server idle"} {
		t.Run(scenario, func(t *testing.T) {
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			manager := &transfer{
				controller: http.NewResponseController(w),
				idle:       time.Minute,
			}
			idle, total := time.Hour, 2*time.Hour
			switch scenario {
			case "owned deadline":
				if err := manager.setReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
			case "buffer total":
				total = time.Second
			case "buffer idle":
				idle = time.Second
			case "disabled server idle":
				manager.idle = 0
			}
			owned, original := manager.readOwned, manager.readDeadline
			before := time.Now()
			scope, err := beginBufferRead(newTransferWriter(w, manager), idle, total)
			if err != nil {
				t.Fatal(err)
			}
			armed, readScope, err := manager.beginRead()
			if err != nil {
				t.Fatal(err)
			}
			after := time.Now()
			if readScope != scope {
				t.Errorf("%s: beginRead() scope = %p, want the active scope %p", scenario, readScope, scope)
			}
			applied := scope.applied
			bound := min(idle, total)
			if manager.idle > 0 {
				bound = min(bound, manager.idle)
			}
			if applied.After(after.Add(bound)) || applied.Before(before) {
				t.Errorf("%s: applied deadline = %v, want between %v and %v", scenario, applied, before, after.Add(bound))
			}
			if owned && !applied.Equal(original) {
				t.Errorf("%s: applied deadline = %v, want the stricter owned deadline %v", scenario, applied, original)
			}
			if manager.readOwned != owned || !manager.readDeadline.Equal(original) {
				t.Errorf("%s: during the scope, read ownership = %v at %v, want %v at %v", scenario, manager.readOwned, manager.readDeadline, owned, original)
			}
			manager.endRead(armed)
			if err = scope.finish(); err != nil {
				t.Fatal(err)
			}
			if manager.buffer != nil {
				t.Errorf("%s: after finish(), the transfer still has a buffering scope", scenario)
			}
			if manager.readOwned != owned || !manager.readDeadline.Equal(original) {
				t.Errorf("%s: after finish(), read ownership = %v at %v, want %v at %v", scenario, manager.readOwned, manager.readDeadline, owned, original)
			}
			last := w.deadlines[len(w.deadlines)-1]
			if !last.Equal(original) {
				t.Errorf("%s: last SetReadDeadline after finish() = %v, want %v", scenario, last, original)
			}
			armed, readScope, err = manager.beginRead()
			if err != nil {
				t.Fatal(err)
			}
			if readScope != nil {
				t.Errorf("%s: beginRead() after finish() scope = %p, want nil", scenario, readScope)
			}
			if wantArmed := !owned && manager.idle > 0; armed != wantArmed {
				t.Errorf("%s: beginRead() after finish() armed = %v, want %v", scenario, armed, wantArmed)
			}
			manager.endRead(armed)
		})
	}
}

func TestBufferAdmissionCoversInputAndProcessingPanicRevokesReplays(t *testing.T) {
	source := &bufferInput{}
	h := newBufferHarness(t, source)
	source.read = func([]byte) (int, error) {
		if h.gatewayAdmission.tryAcquire() {
			t.Error("during the buffering read: gateway admission slot is free, want held")
		}
		if h.routeAdmission.tryAcquire() {
			t.Error("during the buffering read: route admission slot is free, want held")
		}
		if h.budget.used != 8 {
			t.Errorf("during the buffering read: reserved = %d, want 8", h.budget.used)
		}

		return 0, io.EOF
	}
	var kept io.ReadCloser
	var getBody func() (io.ReadCloser, error)
	h.afterBuffer = func(r *http.Request) {
		var err error
		kept, err = r.GetBody()
		if err != nil {
			t.Errorf("GetBody() error = %v, want nil", err)
		}
		getBody = r.GetBody
		if h.budget.used != 8 {
			t.Errorf("during processing: reserved = %d, want 8", h.budget.used)
		}
		if h.gatewayAdmission.tryAcquire() {
			t.Error("during processing: gateway admission slot is free, want held")
		}
		if h.routeAdmission.tryAcquire() {
			t.Error("during processing: route admission slot is free, want held")
		}
		panic("processing panic")
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.gateway.ServeHTTP(newTransferWriter(h.writer, h.manager), h.request)
	}()

	if recovered != "processing panic" {
		t.Errorf("ServeHTTP panic = %v, want %q", recovered, "processing panic")
	}
	if !h.completion.Aborted {
		t.Error("after the processing panic: Completion.Aborted = false, want true")
	}
	if h.budget.used != 0 {
		t.Errorf("after the processing panic: reserved = %d, want 0", h.budget.used)
	}
	if kept == nil || getBody == nil {
		t.Fatal("processing did not obtain replay capabilities")
	}
	if _, err := kept.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("retained replay Read() after the panic error = %v, want %v", err, io.ErrClosedPipe)
	}
	if _, err := getBody(); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("retained GetBody() after the panic error = %v, want %v", err, io.ErrClosedPipe)
	}
	if !h.gatewayAdmission.tryAcquire() {
		t.Error("after the processing panic: gateway admission slot is still held")
	}
	if !h.routeAdmission.tryAcquire() {
		t.Error("after the processing panic: route admission slot is still held")
	}
}

func TestTotalBufferDeadlineAlsoBoundsAvailableData(t *testing.T) {
	source := &bufferInput{}
	h := newBufferHarness(t, source)
	source.read = func(p []byte) (int, error) {
		p[0] = 'x'
		// Model a read that completed after the total phase deadline,
		// without a blocking network call that can report a timeout.
		h.manager.mu.Lock()
		h.manager.buffer.total = time.Now().Add(-time.Hour)
		h.manager.mu.Unlock()

		return 1, io.EOF
	}
	h.gateway.ServeHTTP(newTransferWriter(h.writer, h.manager), h.request)

	want := harnessState{
		status: http.StatusRequestTimeout,
	}
	if got := h.state(); got != want {
		t.Errorf("ServeHTTP with a read after the total deadline = %+v, want %+v", got, want)
	}
	if f := h.completion.Failure; f == nil || !errors.Is(f.Err, context.DeadlineExceeded) {
		t.Errorf("ServeHTTP with a read after the total deadline: Completion.Failure = %+v, want an error wrapping %v", f, context.DeadlineExceeded)
	}
}

func TestUnknownBufferedErrorIsNotHiddenByCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	original := errors.New("unknown reader failure")
	source := &bufferInput{
		read: func([]byte) (int, error) {
			cancel()

			return 0, original
		},
	}
	h := newBufferHarness(t, source)
	h.request = h.request.WithContext(ctx)
	h.gateway.ServeHTTP(newTransferWriter(h.writer, h.manager), h.request)

	want := harnessState{
		status: http.StatusInternalServerError,
	}
	if got := h.state(); got != want {
		t.Errorf("ServeHTTP with an unknown error and cancellation = %+v, want %+v", got, want)
	}
	if f := h.completion.Failure; f == nil || !errors.Is(f.Err, original) {
		t.Errorf("ServeHTTP with an unknown error and cancellation: Completion.Failure = %+v, want an error wrapping %q", f, original)
	}
}

func TestBufferCancellationInterruptsReadAndRestoresServerOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	interrupted := make(chan struct{})
	var signal sync.Once
	source := &bufferInput{
		read: func([]byte) (int, error) {
			close(entered)
			<-interrupted

			return 0, bufferTimeoutError{}
		},
	}
	h := newBufferHarness(t, source)
	h.request = h.request.WithContext(ctx)
	h.writer.notify = func(deadline time.Time) {
		if !deadline.IsZero() && !deadline.After(time.Now()) {
			signal.Do(func() { close(interrupted) })
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.gateway.ServeHTTP(newTransferWriter(h.writer, h.manager), h.request)
	}()
	<-entered
	cancel()
	<-done

	want := harnessState{
		status: statusClientClosedRequest,
	}
	if got := h.state(); got != want {
		t.Errorf("ServeHTTP canceled during a read = %+v, want %+v", got, want)
	}
	if f := h.completion.Failure; f == nil || !errors.Is(f.Err, context.Canceled) {
		t.Errorf("ServeHTTP canceled during a read: Completion.Failure = %+v, want an error wrapping %v", f, context.Canceled)
	}
	if h.manager.readOwned {
		t.Error("ServeHTTP canceled during a read: read deadline is caller-owned, want server-owned")
	}
	if source.closes != 1 {
		t.Errorf("ServeHTTP canceled during a read: source closes = %d, want 1", source.closes)
	}
	if last := h.writer.deadlines[len(h.writer.deadlines)-1]; !last.IsZero() {
		t.Errorf("ServeHTTP canceled during a read: last SetReadDeadline = %v, want zero", last)
	}
}

func TestBufferScopeRestoresCallerOwnershipOnEveryExit(t *testing.T) {
	for _, scenario := range []string{"success", "read failure", "panic", "cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			source := &bufferInput{
				read: func([]byte) (int, error) { return 0, io.EOF },
			}
			h := newBufferHarness(t, source)
			original := time.Now().Add(10 * time.Second)
			if err := h.manager.setReadDeadline(original); err != nil {
				t.Fatal(err)
			}
			originalWrite := time.Now().Add(5 * time.Second)
			if err := h.manager.setWriteDeadline(originalWrite); err != nil {
				t.Fatal(err)
			}
			h.manager.expectContinue = true
			switch scenario {
			case "read failure":
				source.read = func([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
			case "panic":
				source.read = func([]byte) (int, error) { panic("input panic") }
			case "cancellation":
				cancel()
			}
			h.request = h.request.WithContext(ctx)
			func() {
				defer func() { _ = recover() }()
				h.gateway.ServeHTTP(newTransferWriter(h.writer, h.manager), h.request)
			}()

			if h.manager.buffer != nil {
				t.Errorf("ServeHTTP(%s): the transfer still has a buffering scope", scenario)
			}
			if !h.manager.readOwned || !h.manager.readDeadline.Equal(original) {
				t.Errorf("ServeHTTP(%s): read ownership = %v at %v, want caller-owned at %v", scenario, h.manager.readOwned, h.manager.readDeadline, original)
			}
			if last := h.writer.deadlines[len(h.writer.deadlines)-1]; !last.Equal(original) {
				t.Errorf("ServeHTTP(%s): last SetReadDeadline = %v, want %v", scenario, last, original)
			}
			if !h.manager.writeOwned || !h.manager.ownedWriteDeadline.Equal(originalWrite) {
				t.Errorf("ServeHTTP(%s): write ownership = %v at %v, want caller-owned at %v", scenario, h.manager.writeOwned, h.manager.ownedWriteDeadline, originalWrite)
			}
			if last := h.writer.writeDeadlines[len(h.writer.writeDeadlines)-1]; !last.Equal(originalWrite) {
				t.Errorf("ServeHTTP(%s): last SetWriteDeadline = %v, want %v", scenario, last, originalWrite)
			}
			if h.budget.used != 0 {
				t.Errorf("ServeHTTP(%s): reserved = %d, want 0", scenario, h.budget.used)
			}
		})
	}
}

func TestReplayRevocationWithConcurrentOutstandingReaders(t *testing.T) {
	const readers = 8
	budget, err := NewRequestBufferBudget(bufferChunk)
	if err != nil {
		t.Fatal(err)
	}
	if !budget.reserve(bufferChunk) {
		t.Fatalf("reserve(%d) on an unused budget = false, want true", bufferChunk)
	}
	store := &requestBuffer{
		budget:   budget,
		reserved: bufferChunk,
	}
	defer store.dispose()
	if err = store.fill(strings.NewReader(strings.Repeat("x", bufferChunk))); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, readers)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range readers {
		cursor, replayErr := store.replay()
		if replayErr != nil {
			t.Fatal(replayErr)
		}
		wg.Go(func() {
			var one [1]byte
			if n, err := cursor.Read(one[:]); n != 1 || err != nil {
				t.Errorf("cursor Read() before dispose() = %d, %v; want 1, nil", n, err)
			}
			ready <- struct{}{}
			<-release
			if _, err := cursor.Read(one[:]); !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("cursor Read() after dispose() error = %v, want %v", err, io.ErrClosedPipe)
			}
			_ = cursor.Close()
		})
	}
	for range readers {
		<-ready
	}
	store.dispose()
	if store.chunks != nil || budget.used != 0 {
		t.Errorf("dispose() with %d outstanding readers: segments %d, reserved %d; want nil, 0", readers, len(store.chunks), budget.used)
	}
	close(release)
	wg.Wait()
}

func TestBufferContinueWriteDeadlinePreservesOwnership(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprintf("owned=%t", owned), func(t *testing.T) {
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			manager := &transfer{
				controller:     http.NewResponseController(w),
				idle:           time.Minute,
				expectContinue: true,
			}
			original := time.Time{}
			if owned {
				original = time.Now().Add(time.Second)
				if err := manager.setWriteDeadline(original); err != nil {
					t.Fatal(err)
				}
			}
			scope, err := beginBufferRead(newTransferWriter(w, manager), time.Minute, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			armed, _, err := manager.beginRead()
			if err != nil {
				t.Fatal(err)
			}
			if !scope.writeScoped || !scope.continueReading {
				t.Errorf("first beginRead(): writeScoped = %v, continueReading = %v; want true, true", scope.writeScoped, scope.continueReading)
			}
			if scope.continueDeadline.After(scope.total) {
				t.Errorf("100 Continue write deadline = %v, want at most the total deadline %v", scope.continueDeadline, scope.total)
			}
			if owned && !scope.continueDeadline.Equal(original) {
				t.Errorf("100 Continue write deadline = %v, want the stricter caller deadline %v", scope.continueDeadline, original)
			}
			if manager.writeOwned != owned {
				t.Errorf("during the scope: writeOwned = %v, want %v", manager.writeOwned, owned)
			}
			manager.endRead(armed)
			if scope.continueReading {
				t.Error("after endRead(): continueReading = true, want false")
			}
			if err = scope.finish(); err != nil {
				t.Fatal(err)
			}
			if last := w.writeDeadlines[len(w.writeDeadlines)-1]; manager.writeOwned != owned || !last.Equal(original) {
				t.Errorf("after finish(): writeOwned = %v, last SetWriteDeadline = %v; want %v, %v", manager.writeOwned, last, owned, original)
			}
		})
	}
}

func TestBufferContinueWriteDeadlineFailureStopsBeforeRead(t *testing.T) {
	source := &bufferInput{
		read: func([]byte) (int, error) { return 0, io.EOF },
	}
	h := newBufferHarness(t, source)
	h.manager.expectContinue = true
	h.writer.writeFailAt = 1
	h.gateway.ServeHTTP(newTransferWriter(h.writer, h.manager), h.request)

	want := harnessState{
		status: http.StatusInternalServerError,
	}
	if got := h.state(); got != want {
		t.Errorf("ServeHTTP with a failed 100 Continue write deadline = %+v, want %+v", got, want)
	}
	if source.reads != 0 {
		t.Errorf("ServeHTTP with a failed 100 Continue write deadline: source reads = %d, want 0", source.reads)
	}
	if h.manager.readOwned || h.manager.writeOwned {
		t.Errorf("ServeHTTP with a failed 100 Continue write deadline: readOwned = %v, writeOwned = %v; want false, false", h.manager.readOwned, h.manager.writeOwned)
	}
}

func TestSharedRequestBufferBudgetConcurrentReservations(t *testing.T) {
	const requests, slots, reservation = 64, 3, 64
	budget, err := NewRequestBufferBudget(slots * reservation)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	attempted := make(chan bool, requests)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			<-start
			admitted := budget.reserve(reservation)
			attempted <- admitted
			if admitted {
				<-release
				budget.release(reservation)
			}
		})
	}
	close(start)
	admitted := 0
	for range requests {
		if <-attempted {
			admitted++
		}
	}
	if admitted != slots || budget.used != slots*reservation {
		t.Errorf("%d concurrent reserve(%d) calls: admitted %d, reserved %d; want %d, %d", requests, reservation, admitted, budget.used, slots, slots*reservation)
	}
	close(release)
	wg.Wait()
	if budget.used != 0 {
		t.Errorf("after all release() calls: reserved = %d, want 0", budget.used)
	}
}

func TestConservativeReservationIsNotActualBodyAllocation(t *testing.T) {
	const limit = 100 << 20
	budget, err := NewRequestBufferBudget(limit)
	if err != nil {
		t.Fatal(err)
	}
	if !budget.reserve(limit) {
		t.Fatalf("reserve(%d) on an unused budget = false, want true", limit)
	}
	store := &requestBuffer{
		budget:   budget,
		reserved: limit,
	}
	defer store.dispose()
	if err = store.fill(strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	type storage struct {
		reserved int64
		size     int64
		segments int
		capacity int
	}
	got := storage{
		reserved: budget.used,
		size:     store.size,
		segments: len(store.chunks),
		capacity: cap(store.chunks[0]),
	}
	want := storage{
		reserved: limit,
		size:     1,
		segments: 1,
		capacity: bufferChunk,
	}
	if got != want {
		t.Errorf("fill(1 byte) with a 100 MiB reservation = %+v, want %+v", got, want)
	}
}

func TestFailedBufferCancellationDeadlineProvidesNoEvidence(t *testing.T) {
	w := &deadlineRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		failAt:           2,
	}
	manager := &transfer{
		controller: http.NewResponseController(w),
		idle:       time.Minute,
	}
	scope, err := beginBufferRead(newTransferWriter(w, manager), time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	original := scope.applied
	scope.cancel()
	if scope.interrupted || !scope.applied.Equal(original) {
		t.Errorf("cancel() with a failed SetReadDeadline: interrupted = %v, applied = %v; want false, %v", scope.interrupted, scope.applied, original)
	}
	err = scope.readError(bufferTimeoutError{})
	if status := bufferedFailureStatus(err); status != http.StatusInternalServerError {
		t.Errorf("bufferedFailureStatus(readError(timeout)) = %d, want %d", status, http.StatusInternalServerError)
	}
	if scope.canceledRead(err) {
		t.Error("canceledRead(readError(timeout)) = true, want false")
	}
	if err = scope.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestBufferFillRejectsDataAfterReservation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     string
		wantErr    error
		wantStatus int
	}{
		{
			name:   "exact",
			source: "abc",
		},
		{
			name:       "overrun",
			source:     "abcd",
			wantErr:    errBufferOverrun,
			wantStatus: http.StatusInternalServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget, err := NewRequestBufferBudget(8)
			if err != nil {
				t.Fatal(err)
			}
			if !budget.reserve(3) {
				t.Fatalf("reserve(%d) on an unused budget = false, want true", 3)
			}
			store := &requestBuffer{
				budget:   budget,
				reserved: 3,
			}
			defer store.dispose()

			err = store.fill(strings.NewReader(tc.source))

			if !errors.Is(err, tc.wantErr) {
				t.Errorf("fill(%q) with a 3-byte reservation = %v, want %v", tc.source, err, tc.wantErr)
			}
			if tc.wantErr != nil && bufferedFailureStatus(err) != tc.wantStatus {
				t.Errorf("bufferedFailureStatus(%v) = %d, want %d", err, bufferedFailureStatus(err), tc.wantStatus)
			}
			if store.size != 3 {
				t.Errorf("fill(%q): stored size = %d, want 3", tc.source, store.size)
			}
		})
	}
}
