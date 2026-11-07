package edgekit_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-edgekit/edgekit"
)

// budget returns a new admission budget, or stops the test.
func budget(t *testing.T, limit int) *edgekit.AdmissionBudget {
	t.Helper()

	b, err := edgekit.NewAdmissionBudget(limit)
	if err != nil {
		t.Fatalf("NewAdmissionBudget(%d) error = %v", limit, err)
	}

	return b
}

// serveAsync serves r in a new goroutine and sends the recorder when the
// handler returned.
func serveAsync(g http.Handler, r *http.Request) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		done <- w
	}()

	return done
}

// blockingTransport answers each request after release is closed. It sends
// on entered when a request arrives.
func blockingTransport(entered chan<- struct{}, release <-chan struct{}) http.RoundTripper {
	return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-release

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})
}

// releaser closes a channel once, also when the test fails early.
func releaser(t *testing.T) (chan struct{}, func()) {
	t.Helper()

	release := make(chan struct{})
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(release)
		})
	}
	t.Cleanup(finish)

	return release, finish
}

// countedBody records reads and closes of a request body.
type countedBody struct {
	reads  atomic.Int32
	closes atomic.Int32
}

// Read records the call and gives no data.
func (b *countedBody) Read([]byte) (int, error) {
	b.reads.Add(1)

	return 0, io.EOF
}

// Close records the call.
func (b *countedBody) Close() error {
	b.closes.Add(1)

	return nil
}

func TestNewAdmissionBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		valid bool
	}{
		{
			name:  "negative",
			limit: -1,
		},
		{
			name:  "zero",
			limit: 0,
		},
		{
			name:  "one",
			limit: 1,
			valid: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := edgekit.NewAdmissionBudget(tc.limit)
			if tc.valid {
				if err != nil || b == nil {
					t.Errorf("NewAdmissionBudget(%d) = %v, %v; want a budget", tc.limit, b, err)
				}

				return
			}

			if b != nil || !errors.Is(err, edgekit.ErrInvalidConfiguration) {
				t.Errorf("NewAdmissionBudget(%d) = %v, %v; want ErrInvalidConfiguration", tc.limit, b, err)
			}
		})
	}
}

func TestInvalidAdmissionConfiguration(t *testing.T) {
	selector := static(t, "http://upstream.test")
	shared := budget(t, 1)
	for _, tc := range []struct {
		name    string
		cfg     edgekit.Config
		problem string
	}{
		{
			name: "gateway budget not made with NewAdmissionBudget",
			cfg: edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/",
						Upstream: selector,
					},
				},
				Admission: &edgekit.AdmissionBudget{},
			},
			problem: "admission budget: is not made with NewAdmissionBudget",
		},
		{
			name: "route budget not made with NewAdmissionBudget",
			cfg: edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:    "/",
						Upstream:  selector,
						Admission: &edgekit.AdmissionBudget{},
					},
				},
			},
			problem: "route 0: admission budget: is not made with NewAdmissionBudget",
		},
		{
			name: "same budget for the gateway and a route",
			cfg: edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/",
						Upstream: selector,
					},
					{
						Prefix:    "/api/",
						Upstream:  selector,
						Admission: shared,
					},
				},
				Admission: shared,
			},
			problem: "route 1: admission budget: is also the gateway budget",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := edgekit.New(tc.cfg)
			if !errors.Is(err, edgekit.ErrInvalidConfiguration) || !strings.Contains(err.Error(), tc.problem) {
				t.Errorf("New() error = %v, want %q", err, tc.problem)
			}
		})
	}

	g, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/a/",
				Upstream:  selector,
				Admission: shared,
			},
			{
				Prefix:    "/b/",
				Upstream:  selector,
				Admission: shared,
			},
		},
	})
	if err != nil || g == nil {
		t.Errorf("New() with a budget shared by routes = %v, %v; want a gateway", g, err)
	}
}

// statusRecorder is middleware that observes the status of the response, as
// an access log does.
type statusRecorder struct {
	statuses chan int
}

// recordingWriter keeps the status that the handler wrote.
type recordingWriter struct {
	http.ResponseWriter

	status int
}

// WriteHeader keeps the first status.
func (w *recordingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}

	w.ResponseWriter.WriteHeader(status)
}

// Middleware sends the observed status after the next handler returns.
func (s statusRecorder) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recordingWriter{
			ResponseWriter: w,
		}
		next.ServeHTTP(rw, r)
		s.statuses <- rw.status
	})
}

func TestSaturationRejectsWithoutQueueOrBodyRead(t *testing.T) {
	entered := make(chan struct{}, 1)
	release, finish := releaser(t)
	observer := statusRecorder{
		statuses: make(chan int, 2),
	}

	unwoundKey := edgekit.NewStateKey[bool]()
	var received atomic.Int32
	type observed struct {
		completion edgekit.Completion
		unwound    bool
	}
	completed := make(chan observed, 2)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		Transport: blockingTransport(entered, release),
		Admission: budget(t, 1),
		Middleware: []edgekit.Middleware{
			observer.Middleware,
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					rc, _ := edgekit.FromContext(r.Context())
					defer edgekit.SetState(rc, unwoundKey, true)
					next.ServeHTTP(w, r)
				})
			},
		},
		Hooks: edgekit.Hooks{
			RequestReceived: []edgekit.RequestReceivedHook{
				edgekit.RequestReceivedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
					received.Add(1)

					return nil, nil
				}),
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				edgekit.RequestCompletedHookFunc(func(_ context.Context, rc *edgekit.RequestContext, c edgekit.Completion) {
					unwound, _ := edgekit.State(rc, unwoundKey)
					completed <- observed{
						completion: c,
						unwound:    unwound,
					}
				}),
			},
		},
	})

	first := serveAsync(g, httptest.NewRequest(http.MethodGet, "/api/active", nil))
	receive(t, entered)

	body := &countedBody{}
	r := httptest.NewRequest(http.MethodPost, "/api/overflow", nil)
	r.Body = body
	w := receive(t, serveAsync(g, r))
	got := receive(t, completed)

	want := `{"type":"about:blank","title":"Service Unavailable","status":503,"reason":"admission_saturated"}`
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != want || w.Header().Get("Retry-After") != "" {
		t.Errorf("ServeHTTP() response = %d %q, Retry-After %q; want the 503 problem without Retry-After", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}

	if body.reads.Load() != 0 || body.closes.Load() != 0 || received.Load() != 1 {
		t.Errorf("request body reads = %d, closes = %d, RequestReceived calls = %d; want 0, 0, 1", body.reads.Load(), body.closes.Load(), received.Load())
	}

	c := got.completion
	if !got.unwound || c.RejectionReason != edgekit.ReasonAdmissionSaturated || c.Failure != nil || c.Aborted || c.RoutePrefix != "" || c.SelectedUpstream != nil {
		t.Errorf("RequestCompleted() after unwind %v = %+v; want a saturation rejection after unwind, without route or upstream", got.unwound, c)
	}

	if status := receive(t, observer.statuses); status != http.StatusServiceUnavailable {
		t.Errorf("observing middleware saw %d, want 503", status)
	}

	finish()
	if w := receive(t, first); w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Errorf("ServeHTTP() of the active request = %d %q, want 200 ok", w.Code, w.Body.String())
	}

	got = receive(t, completed)
	if !got.unwound || got.completion.RejectionReason != "" || got.completion.Failure != nil || got.completion.RoutePrefix != "/api/" {
		t.Errorf("RequestCompleted() of the active request = %+v, want a normal completion on /api/", got.completion)
	}
}

func TestAdmissionPassesRequestAndWriter(t *testing.T) {
	var outerRequest *http.Request
	var outerWriter http.ResponseWriter
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/api/",
				Upstream:  static(t, "http://upstream.test"),
				Admission: budget(t, 1),
				Middleware: []edgekit.Middleware{
					func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if w != outerWriter || r != outerRequest {
								t.Error("route middleware got another request or response writer, want the original ones")
							}

							w.WriteHeader(http.StatusNoContent)
						})
					},
				},
			},
		},
		Admission: budget(t, 1),
		Middleware: []edgekit.Middleware{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					outerRequest, outerWriter = r, w
					next.ServeHTTP(w, r)
				})
			},
		},
		Hooks: edgekit.Hooks{
			RequestReceived: []edgekit.RequestReceivedHook{
				edgekit.RequestReceivedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, r *http.Request) (*edgekit.Rejection, error) {
					if r != outerRequest {
						t.Error("RequestReceived hook got another request, want the original one")
					}

					return nil, nil
				}),
			},
		},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/", nil))

	if w.Code != http.StatusNoContent {
		t.Errorf("ServeHTTP() status = %d, want 204", w.Code)
	}
}

func TestPanicReleasesAdmissionSlot(t *testing.T) {
	for _, gatewayBudget := range []bool{false, true} {
		name := "route budget"
		if gatewayBudget {
			name = "gateway budget"
		}

		t.Run(name, func(t *testing.T) {
			b := budget(t, 1)
			route := edgekit.Route{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
				Middleware: []edgekit.Middleware{
					func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == "/api/panic" {
								panic(http.ErrAbortHandler)
							}

							w.WriteHeader(http.StatusNoContent)
						})
					},
				},
			}
			cfg := edgekit.Config{}
			if gatewayBudget {
				cfg.Admission = b
			} else {
				route.Admission = b
			}

			collector, completed := completions()
			cfg.Routes = []edgekit.Route{route}
			cfg.Hooks = edgekit.Hooks{
				RequestCompleted: []edgekit.RequestCompletedHook{
					collector,
				},
			}
			g := newGateway(t, cfg)

			var recovered any
			func() {
				defer func() {
					recovered = recover()
				}()

				g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/panic", nil))
			}()

			if err, ok := recovered.(error); !ok || !errors.Is(err, http.ErrAbortHandler) || !receive(t, completed).Aborted {
				t.Fatalf("ServeHTTP() panicked with %v, want http.ErrAbortHandler and an aborted completion", recovered)
			}

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/next", nil))

			if w.Code != http.StatusNoContent {
				t.Errorf("ServeHTTP() after the panic: status = %d, want 204: the slot was not released", w.Code)
			}
		})
	}
}

func TestCancellationHoldsAdmissionSlot(t *testing.T) {
	entered := make(chan struct{}, 1)
	release, finish := releaser(t)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/api/",
				Upstream:  static(t, "http://upstream.test"),
				Admission: budget(t, 1),
				Middleware: []edgekit.Middleware{
					// The middleware ignores the cancellation, as slow
					// downstream work can.
					func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == "/api/slow" {
								entered <- struct{}{}
								<-release
							}

							w.WriteHeader(http.StatusNoContent)
						})
					},
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(t.Context())
	first := serveAsync(g, httptest.NewRequest(http.MethodGet, "/api/slow", nil).WithContext(ctx))
	receive(t, entered)
	cancel()

	if w := receive(t, serveAsync(g, httptest.NewRequest(http.MethodGet, "/api/next", nil))); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("ServeHTTP() while the canceled request runs: status = %d, want 503", w.Code)
	}

	finish()
	receive(t, first)

	if w := receive(t, serveAsync(g, httptest.NewRequest(http.MethodGet, "/api/next", nil))); w.Code != http.StatusNoContent {
		t.Errorf("ServeHTTP() after the canceled request returned: status = %d, want 204", w.Code)
	}
}

func TestActiveRequestsStayWithinLimit(t *testing.T) {
	const (
		limit    = 3
		requests = 20
	)

	release, finish := releaser(t)
	var active, peak atomic.Int32
	entered := make(chan struct{}, requests)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/api/",
				Upstream:  static(t, "http://upstream.test"),
				Admission: budget(t, limit),
				Middleware: []edgekit.Middleware{
					func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							n := active.Add(1)
							for {
								old := peak.Load()
								if n <= old || peak.CompareAndSwap(old, n) {
									break
								}
							}

							entered <- struct{}{}
							<-release
							active.Add(-1)
							w.WriteHeader(http.StatusNoContent)
						})
					},
				},
			},
		},
	})

	results := make(chan int, requests)
	for range requests {
		go func() {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/", nil))
			results <- w.Code
		}()
	}

	// The admitted requests wait for the release, so all other requests
	// return first, without a queue.
	for range limit {
		receive(t, entered)
	}

	for range requests - limit {
		if code := receive(t, results); code != http.StatusServiceUnavailable {
			t.Errorf("ServeHTTP() before the release: status = %d, want 503", code)
		}
	}

	finish()
	for range limit {
		if code := receive(t, results); code != http.StatusNoContent {
			t.Errorf("ServeHTTP() after the release: status = %d, want 204", code)
		}
	}

	if peak.Load() != limit {
		t.Errorf("upstream saw a peak of %d active requests, want %d", peak.Load(), limit)
	}
}

func TestRouteAdmissionBudgets(t *testing.T) {
	entered := make(chan struct{}, 1)
	release, finish := releaser(t)
	shared := budget(t, 1)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/a/",
				Upstream:  static(t, "http://upstream.test"),
				Admission: shared,
			},
			{
				Prefix:    "/b/",
				Upstream:  static(t, "http://upstream.test"),
				Admission: shared,
			},
			{
				Prefix:    "/c/",
				Upstream:  static(t, "http://upstream.test"),
				Admission: budget(t, 1),
			},
			{
				Prefix:   "/d/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/a/slow" {
				entered <- struct{}{}
				<-release
			}

			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     make(http.Header),
				Body:       http.NoBody,
			}, nil
		}),
		Admission: budget(t, 2),
	})

	first := serveAsync(g, httptest.NewRequest(http.MethodGet, "/a/slow", nil))
	receive(t, entered)

	for _, tc := range []struct {
		name   string
		path   string
		status int
	}{
		{
			name:   "same route budget saturated",
			path:   "/a/next",
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "shared route budget saturated",
			path:   "/b/next",
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "separate route budget available",
			path:   "/c/next",
			status: http.StatusNoContent,
		},
		{
			// The route rejections above released their gateway slots.
			name:   "gateway slot released after route saturation",
			path:   "/d/next",
			status: http.StatusNoContent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := receive(t, serveAsync(g, httptest.NewRequest(http.MethodGet, tc.path, nil))); w.Code != tc.status {
				t.Errorf("ServeHTTP(GET %s) status = %d, want %d", tc.path, w.Code, tc.status)
			}
		})
	}

	finish()
	receive(t, first)
}

func TestCancellationDuringPoolWaitReleasesSlot(t *testing.T) {
	entered := make(chan struct{})
	release, finish := releaser(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	transport := testTransport(t)
	transport.MaxConnsPerHost = 1
	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		Transport: transport,
		Admission: budget(t, 2),
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{
				collector,
			},
		},
	})

	first := serveAsync(g, httptest.NewRequest(http.MethodGet, "/api/active", nil))
	receive(t, entered)

	for range 2 {
		waiting := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(t.Context())
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GetConn: func(string) {
				select {
				case waiting <- struct{}{}:
				default:
				}
			},
		})
		pending := serveAsync(g, httptest.NewRequest(http.MethodGet, "/api/queued", nil).WithContext(ctx))
		receive(t, waiting)

		if w := receive(t, serveAsync(g, httptest.NewRequest(http.MethodGet, "/api/overflow", nil))); w.Code != http.StatusServiceUnavailable {
			cancel()
			t.Fatalf("ServeHTTP() during the pool wait: status = %d, want 503", w.Code)
		}

		if c := receive(t, completed); c.RejectionReason != edgekit.ReasonAdmissionSaturated {
			cancel()
			t.Fatalf("RequestCompleted() of the saturated request = %+v, want the reason admission_saturated", c)
		}

		cancel()
		if w := receive(t, pending); w.Code != 499 {
			t.Fatalf("ServeHTTP() canceled in the pool wait: status = %d, want 499", w.Code)
		}

		if c := receive(t, completed); c.Failure == nil || !errors.Is(c.Failure.Err, context.Canceled) {
			t.Fatalf("RequestCompleted() of the canceled request = %+v, want context.Canceled", c)
		}
	}

	if calls.Load() != 1 {
		t.Errorf("upstream got %d calls, want 1", calls.Load())
	}

	finish()
	if w := receive(t, first); w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Errorf("ServeHTTP() of the active request = %d %q, want 200 ok", w.Code, w.Body.String())
	}
}

func TestSlowUploadHoldsAdmissionSlot(t *testing.T) {
	first := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 3)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			first <- "read failed"

			return
		}

		first <- string(buf)
		rest, err := io.ReadAll(r.Body)
		if err != nil || string(rest) != "def" {
			t.Errorf("upstream read the rest of the body as %q, %v; want def", rest, err)
		}

		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/api/",
				Upstream:  static(t, upstream.URL),
				BodyLimit: edgekit.MaxBodyBytes(6),
			},
		},
		Admission: budget(t, 1),
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	client := ts.client(t, false)

	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = writer.Close()
	})

	upload := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", reader)
		if err != nil {
			upload <- err

			return
		}

		resp, err := client.Do(req)
		if err != nil {
			upload <- err

			return
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err == nil && (resp.StatusCode != http.StatusOK || string(body) != "ok") {
			err = errors.New(resp.Status)
		}

		upload <- err
	}()

	if _, err := io.WriteString(writer, "abc"); err != nil {
		t.Fatalf("writing the upload body: error = %v", err)
	}

	if got := receive(t, first); got != "abc" {
		t.Fatalf("upstream got the first bytes %q, want abc", got)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.url()+"/api/other", nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() error = %v", err)
	}

	resp, err := ts.client(t, false).Do(req)
	if err != nil {
		t.Fatalf("GET /api/other during the upload: error = %v", err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /api/other during the upload: status = %d, want 503", resp.StatusCode)
	}

	if _, err := io.WriteString(writer, "def"); err != nil {
		t.Fatalf("writing the upload body: error = %v", err)
	}

	_ = writer.Close()
	if err := receive(t, upload); err != nil {
		t.Errorf("POST /api/upload error = %v", err)
	}
}

func TestUpgradedConnectionHoldsAdmissionSlot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gateway bool
	}{
		{
			name:    "gateway budget",
			gateway: true,
		},
		{
			name: "route budget",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready := make(chan struct{})
			close(ready)
			upstream, upstreamClosed := switchingUpstream(t, ready, 0)
			collector, completed := completions()
			cfg := edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, upstream.URL),
						Middleware: []edgekit.Middleware{
							func(next http.Handler) http.Handler {
								return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									// A probe uses the same admission budget without a protocol switch.
									if r.URL.Path == "/api/probe" {
										w.WriteHeader(http.StatusNoContent)

										return
									}

									next.ServeHTTP(w, r)
								})
							},
						},
					},
				},
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{
						collector,
					},
				},
			}
			if tc.gateway {
				cfg.Admission = budget(t, 1)
			} else {
				cfg.Routes[0].Admission = budget(t, 1)
			}

			ts := startServer(t, newGateway(t, cfg), edgekit.ServerConfig{}, serverOptions{})
			conn := ts.dial(t)
			reader := upgrade(t, conn)

			client := ts.client(t, false)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.url()+"/api/probe", nil)
			if err != nil {
				t.Fatalf("http.NewRequestWithContext() error = %v", err)
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET /api/probe during the upgrade: error = %v", err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("GET /api/probe during the upgrade: status = %d, want 503", resp.StatusCode)
			}

			if c := receive(t, completed); c.RejectionReason != edgekit.ReasonAdmissionSaturated || c.Failure != nil {
				t.Errorf("RequestCompleted() of the probe = %+v, want admission_saturated without a failure", c)
			}

			writeClientData(t, conn, "ping\n")
			if line, readErr := reader.ReadString('\n'); line != "ping\n" || readErr != nil {
				t.Fatalf("echo after saturation = %q, %v; want ping", line, readErr)
			}

			if closeErr := conn.Close(); closeErr != nil {
				t.Fatalf("closing the upgraded connection: error = %v", closeErr)
			}
			receive(t, upstreamClosed)
			if c := receive(t, completed); c.RejectionReason != "" || c.Failure != nil || c.Aborted {
				t.Errorf("RequestCompleted() of the upgrade = %+v, want a normal completion", c)
			}

			// Completion starts after the admission slot was released.
			resp, err = client.Do(req.Clone(t.Context()))
			if err != nil {
				t.Fatalf("GET /api/probe after the upgrade: error = %v", err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("GET /api/probe after the upgrade: status = %d, want 204", resp.StatusCode)
			}
			receive(t, completed)
		})
	}
}

func TestHTTP2StreamsShareAdmissionBudget(t *testing.T) {
	release, finish := releaser(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		_ = http.NewResponseController(w).Flush()

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	t.Cleanup(upstream.Close)

	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		Admission: budget(t, 2),
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{
		tls: true,
	})
	client := ts.client(t, true)

	get := func() *http.Response {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.url()+"/api/events", nil)
		if err != nil {
			t.Fatalf("http.NewRequestWithContext() error = %v", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /api/events error = %v", err)
		}

		if resp.ProtoMajor != 2 {
			t.Fatalf("GET /api/events protocol = %s, want HTTP/2", resp.Proto)
		}

		return resp
	}

	var streams []*bufio.Reader
	for range 2 {
		resp := get()
		t.Cleanup(func() {
			_ = resp.Body.Close()
		})

		reader := bufio.NewReader(resp.Body)
		if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
			t.Fatalf("GET /api/events first event = %q, %v; want data: first", line, err)
		}

		streams = append(streams, reader)
	}

	resp := get()
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || ts.listener.accepted.Load() != 1 {
		t.Errorf("GET /api/events third stream: status = %d, connections = %d; want 503 on one connection", resp.StatusCode, ts.listener.accepted.Load())
	}

	finish()
	for _, reader := range streams {
		if rest, err := io.ReadAll(reader); err != nil || string(rest) != "\ndata: last\n\n" {
			t.Errorf("GET /api/events rest of the stream = %q, %v; want the last event", rest, err)
		}
	}
}
