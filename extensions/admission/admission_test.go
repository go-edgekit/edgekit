package admission_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit/extensions/admission"
)

// wait bounds the time that a test waits for handler activity.
func wait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()

	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for admission activity")

		var zero T

		return zero
	}
}

// serve runs a handler without blocking the test on admission or release.
func serve(handler http.Handler, r *http.Request) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		done <- w
	}()

	return done
}

func TestPositiveLimitRequired(t *testing.T) {
	for _, limit := range []int{
		0,
		-1,
	} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid limit did not panic")
				}
			}()

			admission.New(limit)
		})
	}
}

// unreadBody records whether a rejected request body was read or closed.
type unreadBody struct {
	reads  int
	closes int
}

// Read records the call without supplying body data.
func (b *unreadBody) Read([]byte) (int, error) {
	b.reads++

	return 0, io.EOF
}

// Close records the call.
func (b *unreadBody) Close() error {
	b.closes++

	return nil
}

func TestNoQueueAndNoBodyReadOnSaturation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(release)
		})
	}
	defer finish()

	middleware := admission.New(1)
	first := middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	done := serve(first, httptest.NewRequest(http.MethodGet, "/first", nil))
	wait(t, entered)

	// Reusing a middleware shares its budget across wrapped handlers.
	second := middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("overflow was delegated")
	}))
	body := &unreadBody{}
	r := httptest.NewRequest(http.MethodPost, "/overflow?secret=value", body)
	w := wait(t, serve(second, r))
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "Service Unavailable\n" {
		t.Fatalf("response = %d %q, want 503 Service Unavailable", w.Code, w.Body.String())
	}

	if body.reads != 0 || body.closes != 0 {
		t.Fatalf("body reads = %d, closes = %d; want 0, 0", body.reads, body.closes)
	}

	// A separate constructor creates an independent budget.
	independent := admission.New(1)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if w := wait(t, serve(independent, httptest.NewRequest(http.MethodGet, "/", nil))); w.Code != http.StatusNoContent {
		t.Fatal("independent middleware shared a global budget")
	}

	finish()
	wait(t, done)

	probe := middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if w := wait(t, serve(probe, httptest.NewRequest(http.MethodGet, "/after", nil))); w.Code != http.StatusNoContent {
		t.Fatal("slot leaked after normal return")
	}
}

func TestRequestAndResponseWriterArePassedThrough(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	calls := 0
	admission.New(1)(http.HandlerFunc(func(got http.ResponseWriter, request *http.Request) {
		calls++
		if got != w || request != r {
			t.Error("middleware wrapped the writer or changed the request")
		}

		got.(http.Flusher).Flush()
	})).ServeHTTP(w, r)

	if calls != 1 || !w.Flushed {
		t.Fatal("downstream call or flushing capability changed")
	}
}

func TestPanicUnwindsAndReleasesSlot(t *testing.T) {
	for _, value := range []any{
		"panic",
		http.ErrAbortHandler,
	} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			handler := admission.New(1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/panic" {
					panic(value)
				}

				w.WriteHeader(http.StatusNoContent)
			}))
			recovered := func() (recovered any) {
				defer func() {
					recovered = recover()
				}()

				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))

				return nil
			}()

			if recovered != value {
				t.Errorf("recovered %v, want the original panic %v", recovered, value)
			}

			if w := wait(t, serve(handler, httptest.NewRequest(http.MethodGet, "/after", nil))); w.Code != http.StatusNoContent {
				t.Fatalf("slot leaked after panic: status = %d", w.Code)
			}
		})
	}
}

func TestCancellationHoldsSlotUntilDownstreamReturns(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(release)
		})
	}
	defer finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := admission.New(1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/active" {
			close(entered)
			<-r.Context().Done()
			close(canceled)
			<-release
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	done := serve(handler, httptest.NewRequest(http.MethodGet, "/active", nil).WithContext(ctx))
	wait(t, entered)
	cancel()
	wait(t, canceled)

	if w := wait(t, serve(handler, httptest.NewRequest(http.MethodGet, "/overflow", nil))); w.Code != http.StatusServiceUnavailable {
		t.Fatal("cancellation released the slot before downstream unwound")
	}

	finish()
	if w := wait(t, done); w.Code != http.StatusNoContent {
		t.Fatalf("canceled handler status = %d", w.Code)
	}

	if w := wait(t, serve(handler, httptest.NewRequest(http.MethodGet, "/after", nil))); w.Code != http.StatusNoContent {
		t.Fatal("slot leaked after canceled handler returned")
	}
}

func TestActiveCallsStayWithinLimit(t *testing.T) {
	const limit = 3
	entered, release := make(chan struct{}, limit), make(chan struct{})
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(release)
		})
	}
	defer finish()

	var active, peak atomic.Int32
	handler := admission.New(limit)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)

		for previous := peak.Load(); n > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, n) {
				break
			}
		}

		entered <- struct{}{}
		<-release
	}))
	var occupied []<-chan *httptest.ResponseRecorder
	for range limit {
		occupied = append(occupied, serve(handler, httptest.NewRequest(http.MethodGet, "/active", nil)))
		wait(t, entered)
	}

	var overflow []<-chan *httptest.ResponseRecorder
	for range 32 {
		overflow = append(overflow, serve(handler, httptest.NewRequest(http.MethodGet, "/overflow", nil)))
	}

	for _, done := range overflow {
		if w := wait(t, done); w.Code != http.StatusServiceUnavailable {
			t.Errorf("overflow status = %d, want 503", w.Code)
		}
	}

	if active.Load() != limit {
		t.Fatalf("active calls = %d, want %d", active.Load(), limit)
	}

	if peak.Load() != limit {
		t.Fatalf("peak calls = %d, want %d", peak.Load(), limit)
	}

	finish()
	for _, done := range occupied {
		wait(t, done)
	}

	if active.Load() != 0 {
		t.Fatal("active calls remained after all handlers returned")
	}
}
