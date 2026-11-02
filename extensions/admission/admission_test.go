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
	for _, limit := range []int{0, -1} {
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

type unreadBody struct{ reads, closes int }

func (b *unreadBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *unreadBody) Close() error             { b.closes++; return nil }

func TestNoQueueAndNoBodyReadOnSaturation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	middleware := admission.New(1)
	first := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}))
	done := serve(first, httptest.NewRequest(http.MethodGet, "/first", nil))
	wait(t, entered)
	// Reusing a middleware shares its budget across wrapped handlers.
	second := middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("overflow was delegated") }))
	body := &unreadBody{}
	r := httptest.NewRequest(http.MethodPost, "/overflow?secret=value", body)
	w := wait(t, serve(second, r))
	if w.Code != 503 || w.Body.String() != "Service Unavailable\n" || body.reads != 0 || body.closes != 0 {
		t.Fatalf("response=%d %q body=%+v", w.Code, w.Body.String(), body)
	}
	// A separate constructor creates an independent budget.
	independent := admission.New(1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	if w := wait(t, serve(independent, httptest.NewRequest(http.MethodGet, "/", nil))); w.Code != 204 {
		t.Fatal("independent middleware shared a global budget")
	}
	finish()
	wait(t, done)
	probe := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	if w := wait(t, serve(probe, httptest.NewRequest(http.MethodGet, "/after", nil))); w.Code != 204 {
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
	for _, value := range []any{"panic", http.ErrAbortHandler} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			handler := admission.New(1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/panic" {
					panic(value)
				}
				w.WriteHeader(204)
			}))
			func() {
				defer func() {
					if got := recover(); got != value {
						t.Errorf("panic changed: %v", got)
					}
				}()
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
			}()
			if w := wait(t, serve(handler, httptest.NewRequest(http.MethodGet, "/after", nil))); w.Code != 204 {
				t.Fatalf("slot leaked after panic: status=%d", w.Code)
			}
		})
	}
}

func TestCancellationHoldsSlotUntilDownstreamReturns(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
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
		w.WriteHeader(204)
	}))
	done := serve(handler, httptest.NewRequest(http.MethodGet, "/active", nil).WithContext(ctx))
	wait(t, entered)
	cancel()
	wait(t, canceled)
	if w := wait(t, serve(handler, httptest.NewRequest(http.MethodGet, "/overflow", nil))); w.Code != 503 {
		t.Fatal("cancellation released the slot before downstream unwound")
	}
	finish()
	if w := wait(t, done); w.Code != 204 {
		t.Fatalf("canceled handler status=%d", w.Code)
	}
	if w := wait(t, serve(handler, httptest.NewRequest(http.MethodGet, "/after", nil))); w.Code != 204 {
		t.Fatal("slot leaked after canceled handler returned")
	}
}

func TestActiveCallsStayWithinLimit(t *testing.T) {
	const limit = 3
	entered, release := make(chan struct{}, limit), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	var active, peak atomic.Int32
	handler := admission.New(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if w := wait(t, done); w.Code != 503 {
			t.Errorf("overflow status=%d", w.Code)
		}
	}
	if active.Load() != limit || peak.Load() != limit {
		t.Fatalf("active=%d peak=%d", active.Load(), peak.Load())
	}
	finish()
	for _, done := range occupied {
		wait(t, done)
	}
	if active.Load() != 0 {
		t.Fatal("active calls remained after all handlers returned")
	}
}
