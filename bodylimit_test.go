package edgekit_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// unknownLength hides the length of a reader, so a request with it as body
// has no declared length.
type unknownLength struct {
	io.Reader
}

// bodyRequest returns a POST request to /api/upload. When known is false, the
// request has no declared length.
func bodyRequest(body string, known bool) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/upload", strings.NewReader(body))
	if !known {
		r.Body = io.NopCloser(unknownLength{strings.NewReader(body)})
		r.ContentLength = -1
	}

	return r
}

// readingUpstream returns a started upstream that reads the complete body,
// counts the bytes and the requests, and answers "ok" when the read ends
// without an error.
func readingUpstream(t *testing.T, received *atomic.Int64, requests *atomic.Int32) *httptest.Server {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		n, err := io.Copy(io.Discard, r.Body)
		received.Add(n)
		if err == nil {
			_, _ = io.WriteString(w, "ok")
		}
	}))
	t.Cleanup(upstream.Close)

	return upstream
}

// limitedRequest is what one request saw through a gateway with body limits.
type limitedRequest struct {
	recorder *httptest.ResponseRecorder

	// completions counts the RequestCompleted calls; completion is the last.
	completions int
	completion  edgekit.Completion

	// unwoundFirst is true when the shared middleware unwound before
	// completion ran.
	unwoundFirst bool

	// received and requests are the bytes and the requests that the upstream
	// got.
	received int64
	requests int32
}

// sendWithBodyLimits sends a POST request with body, with or without a
// declared length, through a gateway with the limits, to an upstream that
// reads the complete body.
func sendWithBodyLimits(t *testing.T, gatewayLimit, routeLimit edgekit.BodyLimit, body string, known bool) limitedRequest {
	t.Helper()

	var received atomic.Int64
	var requests atomic.Int32
	upstream := readingUpstream(t, &received, &requests)

	unwound := false
	got := limitedRequest{
		recorder: httptest.NewRecorder(),
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/api/",
				Upstream:  static(t, upstream.URL),
				BodyLimit: routeLimit,
			},
		},
		BodyLimit:        gatewayLimit,
		SharedMiddleware: []edgekit.Middleware{unwinding(&unwound)},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{
				edgekit.RequestCompletedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
					got.completions++
					got.completion = c
					got.unwoundFirst = unwound
				}),
			},
		},
	})

	g.ServeHTTP(got.recorder, bodyRequest(body, known))
	got.received = received.Load()
	got.requests = requests.Load()

	return got
}

func TestBodyWithinLimitIsForwarded(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		known        bool
		gatewayLimit edgekit.BodyLimit
		routeLimit   edgekit.BodyLimit
	}{
		{
			name:         "empty body with empty-only limit",
			body:         "",
			known:        true,
			gatewayLimit: edgekit.MaxBodyBytes(0),
		},
		{
			name:         "declared length at limit",
			body:         "abcd",
			known:        true,
			gatewayLimit: edgekit.MaxBodyBytes(4),
		},
		{
			name:         "unknown length at limit",
			body:         "abcd",
			gatewayLimit: edgekit.MaxBodyBytes(4),
		},
		{
			name:         "route limit larger than gateway limit",
			body:         "abcdefg",
			gatewayLimit: edgekit.MaxBodyBytes(4),
			routeLimit:   edgekit.MaxBodyBytes(8),
		},
		{
			name:         "route disables the limit",
			body:         "abcdefgh",
			known:        true,
			gatewayLimit: edgekit.MaxBodyBytes(4),
			routeLimit:   edgekit.NoBodyLimit(),
		},
		{
			name:         "largest limit",
			body:         "abcde",
			gatewayLimit: edgekit.MaxBodyBytes(math.MaxInt64),
		},
		{
			name: "omitted limit at default",
			body: strings.Repeat("a", edgekit.DefaultBodyLimit),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sendWithBodyLimits(t, tc.gatewayLimit, tc.routeLimit, tc.body, tc.known)
			w, c := got.recorder, got.completion

			if w.Code != http.StatusOK || w.Body.String() != "ok" {
				t.Errorf("ServeHTTP() response = %d %q, want 200 ok", w.Code, w.Body.String())
			}

			if got.received != int64(len(tc.body)) {
				t.Errorf("upstream got %d bytes, want %d", got.received, len(tc.body))
			}

			if got.completions != 1 || !got.unwoundFirst || c.Aborted || c.RoutePrefix != "/api/" {
				t.Errorf("RequestCompleted() ran %d times, after unwind %v, last with %+v; want one normal completion on /api/ after unwind", got.completions, got.unwoundFirst, c)
			}

			if c.Failure != nil || c.RejectionReason != "" {
				t.Errorf("RequestCompleted() = %+v, want no failure and no rejection", c)
			}
		})
	}
}

func TestDeclaredLengthOverLimitIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		gatewayLimit edgekit.BodyLimit
		routeLimit   edgekit.BodyLimit
	}{
		{
			name:         "gateway limit",
			body:         "abcde",
			gatewayLimit: edgekit.MaxBodyBytes(4),
		},
		{
			name:         "route limit smaller than gateway limit",
			body:         "abcde",
			gatewayLimit: edgekit.MaxBodyBytes(8),
			routeLimit:   edgekit.MaxBodyBytes(4),
		},
		{
			name: "omitted limit",
			body: strings.Repeat("a", edgekit.DefaultBodyLimit+1),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sendWithBodyLimits(t, tc.gatewayLimit, tc.routeLimit, tc.body, true)
			w, c := got.recorder, got.completion

			want := `{"type":"about:blank","title":"Request Entity Too Large","status":413,"reason":"body_too_large"}`
			if w.Code != http.StatusRequestEntityTooLarge || w.Body.String() != want {
				t.Errorf("ServeHTTP() response = %d %q, want 413 %q", w.Code, w.Body.String(), want)
			}

			if got.requests != 0 || c.SelectedUpstream != nil {
				t.Errorf("upstream got %d requests, RequestCompleted() SelectedUpstream = %v; want no selection and no upstream request", got.requests, c.SelectedUpstream)
			}

			if got.completions != 1 || got.unwoundFirst || c.Aborted || c.RoutePrefix != "/api/" {
				t.Errorf("RequestCompleted() ran %d times, middleware unwound %v, last with %+v; want one completion before processing middleware", got.completions, got.unwoundFirst, c)
			}

			if c.RejectionReason != edgekit.ReasonBodyTooLarge || c.Failure != nil {
				t.Errorf("RequestCompleted() = %+v, want the reason body_too_large and no failure", c)
			}
		})
	}
}

func TestStreamingOverflowFailsTheRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		gatewayLimit edgekit.BodyLimit
		routeLimit   edgekit.BodyLimit
		limit        int64
	}{
		{
			name:         "unknown length over limit",
			body:         "abcde",
			gatewayLimit: edgekit.MaxBodyBytes(4),
			limit:        4,
		},
		{
			name:         "unknown length with empty-only limit",
			body:         "a",
			gatewayLimit: edgekit.MaxBodyBytes(0),
			limit:        0,
		},
		{
			name:         "gateway disabled with route limits",
			body:         "abcde",
			gatewayLimit: edgekit.NoBodyLimit(),
			routeLimit:   edgekit.MaxBodyBytes(4),
			limit:        4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sendWithBodyLimits(t, tc.gatewayLimit, tc.routeLimit, tc.body, false)
			w, c := got.recorder, got.completion

			want := `{"type":"about:blank","title":"Request Entity Too Large","status":413}`
			if w.Code != http.StatusRequestEntityTooLarge || w.Body.String() != want {
				t.Errorf("ServeHTTP() response = %d %q, want 413 %q", w.Code, w.Body.String(), want)
			}

			if got.received > tc.limit {
				t.Errorf("upstream got %d bytes, more than the limit %d", got.received, tc.limit)
			}

			if got.completions != 1 || !got.unwoundFirst || c.Aborted || c.RoutePrefix != "/api/" {
				t.Errorf("RequestCompleted() ran %d times, after unwind %v, last with %+v; want one normal completion on /api/ after unwind", got.completions, got.unwoundFirst, c)
			}

			if c.RejectionReason != "" {
				t.Errorf("RequestCompleted() RejectionReason = %q, want empty", c.RejectionReason)
			}

			if c.Failure == nil {
				t.Fatal("RequestCompleted() Failure = nil, want a round-trip failure")
			}

			if c.Failure.Phase != edgekit.PhaseRoundTrip {
				t.Errorf("RequestCompleted() Failure.Phase = %v, want %v", c.Failure.Phase, edgekit.PhaseRoundTrip)
			}

			if !errors.Is(c.Failure.Err, edgekit.ErrRequestBodyTooLarge) {
				t.Errorf("RequestCompleted() Failure.Err = %v, want an error that wraps ErrRequestBodyTooLarge", c.Failure.Err)
			}

			if tooLarge, ok := errors.AsType[*http.MaxBytesError](c.Failure.Err); !ok || tooLarge.Limit != tc.limit {
				t.Errorf("RequestCompleted() Failure.Err = %v, want a *http.MaxBytesError with the limit %d", c.Failure.Err, tc.limit)
			}
		})
	}
}

// pipeBody is a request body that never gets data. It records a close.
type pipeBody struct {
	reads  atomic.Int32
	closed atomic.Bool
}

// Read records the call and blocks no caller: it reports that no data is
// there yet.
func (b *pipeBody) Read([]byte) (int, error) {
	b.reads.Add(1)

	return 0, nil
}

// Close records the call.
func (b *pipeBody) Close() error {
	b.closed.Store(true)

	return nil
}

func TestBodyLimitDoesNotReadAheadOrWrapWriter(t *testing.T) {
	body := &pipeBody{}
	r := httptest.NewRequest(http.MethodPost, "/api/upload", nil)
	r.Body = body
	r.ContentLength = -1
	w := httptest.NewRecorder()

	called := false
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/api/",
				Upstream:  static(t, "http://upstream.test"),
				BodyLimit: edgekit.MaxBodyBytes(4),
				Middleware: []edgekit.Middleware{
					func(http.Handler) http.Handler {
						return http.HandlerFunc(func(got http.ResponseWriter, request *http.Request) {
							called = true
							if got != w {
								t.Error("route middleware got another response writer, want the original one")
							}

							if body.reads.Load() != 0 {
								t.Errorf("body got %d reads before the route middleware, want 0", body.reads.Load())
							}

							if request.Body == r.Body {
								t.Error("route middleware got the original body, want the limited body")
							}

							if err := request.Body.Close(); err != nil || !body.closed.Load() {
								t.Errorf("Body.Close() = %v, original closed = %v; want the close to reach the original body", err, body.closed.Load())
							}

							got.WriteHeader(http.StatusNoContent)
						})
					},
				},
			},
		},
	})

	g.ServeHTTP(w, r)

	if !called || w.Code != http.StatusNoContent {
		t.Errorf("ServeHTTP() status = %d, route middleware called = %v; want 204 from the route middleware", w.Code, called)
	}
}

func TestBodyLimitKeepsEmptyBodies(t *testing.T) {
	for _, tc := range []struct {
		name string
		body io.ReadCloser
	}{
		{
			name: "nil body",
		},
		{
			name: "NoBody",
			body: http.NoBody,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
						Middleware: []edgekit.Middleware{
							func(http.Handler) http.Handler {
								return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									if r.Body != tc.body {
										t.Errorf("route middleware got the body %T, want the original %T", r.Body, tc.body)
									}

									w.WriteHeader(http.StatusNoContent)
								})
							},
						},
					},
				},
			})

			r := httptest.NewRequest(http.MethodGet, "/api/", nil)
			r.Body = tc.body
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)

			if w.Code != http.StatusNoContent {
				t.Errorf("ServeHTTP() status = %d, want 204", w.Code)
			}
		})
	}
}

func TestInvalidBodyLimit(t *testing.T) {
	selector := static(t, "http://upstream.test")
	for _, tc := range []struct {
		name string
		cfg  edgekit.Config
	}{
		{
			name: "negative gateway limit",
			cfg: edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/",
						Upstream: selector,
					},
				},
				BodyLimit: edgekit.MaxBodyBytes(-1),
			},
		},
		{
			name: "negative route limit",
			cfg: edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:    "/",
						Upstream:  selector,
						BodyLimit: edgekit.MaxBodyBytes(math.MinInt64),
					},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := edgekit.New(tc.cfg)
			if !errors.Is(err, edgekit.ErrInvalidConfiguration) || !strings.Contains(err.Error(), "body limit: is negative") {
				t.Errorf("New() error = %v, want an invalid body limit", err)
			}
		})
	}
}

// preReadRequest is what one request saw when the shared middleware read a
// part of the body in shared processing.
type preReadRequest struct {
	recorder     *httptest.ResponseRecorder
	completion   edgekit.Completion
	routeReached bool

	// received and requests are the bytes and the requests that the upstream
	// got.
	received int64
	requests int32
}

// sendAfterPreRead sends the 6-byte body "abcdef" without a declared length
// through a gateway with the limits. Shared middleware reads preRead bytes
// and ignores a read error, so the transport must still enforce overflow.
func sendAfterPreRead(t *testing.T, gatewayLimit, routeLimit int64, preRead int) preReadRequest {
	t.Helper()

	var received atomic.Int64
	var requests atomic.Int32
	upstream := readingUpstream(t, &received, &requests)

	got := preReadRequest{
		recorder: httptest.NewRecorder(),
	}
	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/api/",
				Upstream:  static(t, upstream.URL),
				BodyLimit: edgekit.MaxBodyBytes(routeLimit),
				Middleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							got.routeReached = true
							next.ServeHTTP(w, r)
						})
					},
				},
			},
		},
		BodyLimit: edgekit.MaxBodyBytes(gatewayLimit),
		SharedMiddleware: []edgekit.Middleware{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.ReadFull(r.Body, make([]byte, preRead))
					next.ServeHTTP(w, r)
				})
			},
		},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{
				collector,
			},
		},
	})

	g.ServeHTTP(got.recorder, bodyRequest("abcdef", false))
	got.completion = receive(t, completed)
	got.received = received.Load()
	got.requests = requests.Load()

	return got
}

func TestSharedBodyReadsUseMatchedLimit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		gatewayLimit int64
		routeLimit   int64
		preRead      int
		wantStatus   int
		wantOverflow bool
	}{
		{
			name:         "smaller route limit",
			gatewayLimit: 8,
			routeLimit:   2,
			preRead:      3,
			wantStatus:   http.StatusRequestEntityTooLarge,
			wantOverflow: true,
		},
		{
			name:         "larger route limit",
			gatewayLimit: 2,
			routeLimit:   8,
			preRead:      6,
			wantStatus:   http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sendAfterPreRead(t, tc.gatewayLimit, tc.routeLimit, tc.preRead)
			c := got.completion

			if got.recorder.Code != tc.wantStatus {
				t.Errorf("ServeHTTP() status = %d, want %d", got.recorder.Code, tc.wantStatus)
			}

			if !got.routeReached {
				t.Error("route middleware did not run after shared processing")
			}

			if c.RejectionReason != "" || (c.Failure != nil) != tc.wantOverflow {
				t.Errorf("RequestCompleted() = %+v, want overflow failure %v, no rejection", c, tc.wantOverflow)
			}
			if tc.wantOverflow && (c.Failure == nil || !errors.Is(c.Failure.Err, edgekit.ErrRequestBodyTooLarge)) {
				t.Errorf("RequestCompleted() failure = %+v, want typed overflow", c.Failure)
			}
		})
	}
}

func TestSharedReadCountsTowardFixedRouteLimit(t *testing.T) {
	const preRead, routeLimit = 3, 4
	got := sendAfterPreRead(t, 8, routeLimit, preRead)
	c := got.completion

	if got.recorder.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("ServeHTTP() status = %d, want 413", got.recorder.Code)
	}

	if !got.routeReached {
		t.Error("route middleware was not reached, want the overflow while the body streams")
	}

	if c.Failure == nil || !errors.Is(c.Failure.Err, edgekit.ErrRequestBodyTooLarge) {
		t.Errorf("RequestCompleted() = %+v, want a body overflow failure", c)
	}

	if read := preRead + got.received; read > routeLimit {
		t.Errorf("readers got %d bytes, more than the route limit %d", read, routeLimit)
	}
}

func TestSharedAndTransportReadsWithinFixedLimit(t *testing.T) {
	const preRead = 3
	got := sendAfterPreRead(t, 8, 8, preRead)
	c := got.completion

	if got.recorder.Code != http.StatusOK {
		t.Errorf("ServeHTTP() status = %d, want 200", got.recorder.Code)
	}

	if !got.routeReached {
		t.Error("route middleware was not reached")
	}

	if got.received != 6-preRead {
		t.Errorf("upstream got %d bytes, want %d", got.received, 6-preRead)
	}

	if c.Failure != nil || c.RejectionReason != "" {
		t.Errorf("RequestCompleted() = %+v, want no failure and no rejection", c)
	}
}

func TestDeclaredBodyRejectedBeforeContinue(t *testing.T) {
	var requests atomic.Int32
	upstream := countingUpstream(t, &requests)

	routeReached := false
	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/api/",
				Upstream:  static(t, upstream.URL),
				BodyLimit: edgekit.MaxBodyBytes(10),
				Middleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							routeReached = true
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
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	conn := ts.dial(t)

	writeClientData(t, conn, "POST /api/upload HTTP/1.1\r\nHost: gateway.test\r\nContent-Length: 11\r\nExpect: 100-continue\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("http.ReadResponse() error = %v", err)
	}
	_ = resp.Body.Close()

	// ReadResponse would return a 100 response as the response.
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("POST /api/upload with Expect: 100-continue: status = %d, want 413 without 100 Continue", resp.StatusCode)
	}

	c := receive(t, completed)
	if routeReached || requests.Load() != 0 || c.RejectionReason != edgekit.ReasonBodyTooLarge || c.RoutePrefix != "/api/" || c.SelectedUpstream != nil {
		t.Errorf("route middleware reached = %v, upstream got %d requests, RequestCompleted() = %+v; want a rejection on /api/ before the route middleware", routeReached, requests.Load(), c)
	}
}

// readingTransport reads the complete request body and then returns the error
// that fail makes from the read error.
type readingTransport struct {
	fail func(readErr error) error
}

// RoundTrip reads the body and fails.
func (t readingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	_, readErr := io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()

	return nil, t.fail(readErr)
}

func TestBodyOverflowStatusNeedsEvidence(t *testing.T) {
	unrelated := fmt.Errorf("other limit: %w", edgekit.ErrRequestBodyTooLarge)
	other := errors.New("other failure")
	for _, tc := range []struct {
		name     string
		body     string
		cancel   bool
		fail     func(readErr error) error
		response func(readErr error) error
		status   int
		wantErr  error
	}{
		{
			name: "overflow of the request body",
			body: "abcde",
			fail: func(readErr error) error {
				return readErr
			},
			status:  http.StatusRequestEntityTooLarge,
			wantErr: edgekit.ErrRequestBodyTooLarge,
		},
		{
			name: "transport sentinel without overflow",
			body: "abc",
			fail: func(error) error {
				return unrelated
			},
			status:  http.StatusBadGateway,
			wantErr: unrelated,
		},
		{
			name: "overflow without the sentinel",
			body: "abcde",
			fail: func(error) error {
				return other
			},
			status:  http.StatusBadGateway,
			wantErr: other,
		},
		{
			name: "overflow before a timeout",
			body: "abcde",
			fail: func(readErr error) error {
				return errors.Join(readErr, timeoutError{})
			},
			status:  http.StatusRequestEntityTooLarge,
			wantErr: edgekit.ErrRequestBodyTooLarge,
		},
		{
			name:   "client gone before overflow",
			body:   "abcde",
			cancel: true,
			fail: func(readErr error) error {
				return errors.Join(readErr, context.Canceled)
			},
			status:  499,
			wantErr: edgekit.ErrRequestBodyTooLarge,
		},
		{
			name: "response hook sentinel without overflow",
			body: "abc",
			response: func(error) error {
				return unrelated
			},
			status:  http.StatusInternalServerError,
			wantErr: unrelated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var transport http.RoundTripper = readingTransport{
				fail: tc.fail,
			}

			var hooks []edgekit.UpstreamResponseHook
			if tc.response != nil {
				transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					_ = r.Body.Close()

					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       http.NoBody,
					}, nil
				})
				hooks = append(hooks, edgekit.UpstreamResponseHookFunc(func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
					return nil, tc.response(nil)
				}))
			}

			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
					},
				},
				Transport: transport,
				BodyLimit: edgekit.MaxBodyBytes(4),
				Hooks: edgekit.Hooks{
					UpstreamResponse: hooks,
					RequestCompleted: []edgekit.RequestCompletedHook{
						collector,
					},
				},
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			w := httptest.NewRecorder()
			g.ServeHTTP(w, bodyRequest(tc.body, false).WithContext(ctx))
			c := receive(t, completed)

			if w.Code != tc.status {
				t.Errorf("ServeHTTP() status = %d, want %d", w.Code, tc.status)
			}

			if c.Failure == nil {
				t.Fatal("RequestCompleted() Failure = nil, want a failure")
			}

			if !errors.Is(c.Failure.Err, tc.wantErr) {
				t.Errorf("RequestCompleted() Failure.Err = %v, want an error that wraps %v", c.Failure.Err, tc.wantErr)
			}
		})
	}
}

// echoBody is a response body that reads the request body, as an upstream
// that echoes a stream does.
type echoBody struct {
	request io.ReadCloser
}

// Read reads from the request body.
func (b echoBody) Read(p []byte) (int, error) {
	return b.request.Read(p)
}

// Close closes the request body.
func (b echoBody) Close() error {
	return b.request.Close()
}

// echoGateway returns a gateway with a route "/api/" whose transport sends
// the request body back as a streamed response body, a renderer that counts
// its calls, and the completions. A read failure of the request body then
// happens after the response is committed.
func echoGateway(t *testing.T, limit edgekit.BodyLimit) (*edgekit.Gateway, *atomic.Int32, <-chan edgekit.Completion) {
	t.Helper()

	rendered := new(atomic.Int32)
	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        make(http.Header),
				Body:          echoBody{request: r.Body},
				ContentLength: -1,
			}, nil
		}),
		BodyLimit: limit,
		Renderer: func(context.Context, edgekit.Problem) (string, []byte) {
			rendered.Add(1)

			return "text/plain", nil
		},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{
				collector,
			},
		},
	})

	return g, rendered, completed
}

// serverRequest marks r as a request that an http.Server serves, so
// ReverseProxy aborts the handler when the copy of the response body fails.
// The server value only marks the request; it serves nothing.
func serverRequest(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, &http.Server{
		ReadHeaderTimeout: time.Second,
	}))
}

func TestBodyOverflowAfterCommitIsNotRendered(t *testing.T) {
	g, rendered, completed := echoGateway(t, edgekit.MaxBodyBytes(4))
	r := serverRequest(bodyRequest("abcdefgh", false))
	w := httptest.NewRecorder()
	panicked := panics(func() {
		g.ServeHTTP(w, r)
	})

	c := receive(t, completed)
	if !panicked || !c.Aborted {
		t.Errorf("ServeHTTP() panicked = %v, RequestCompleted() Aborted = %v; want true, true", panicked, c.Aborted)
	}

	if w.Code != http.StatusOK || rendered.Load() != 0 || c.RejectionReason != "" {
		t.Errorf("ServeHTTP() status = %d, renderer calls = %d, RequestCompleted() RejectionReason = %q; want the committed 200 and no rendering", w.Code, rendered.Load(), c.RejectionReason)
	}

	if w.Body.String() != "abcd" {
		t.Errorf("ServeHTTP() body = %q, want the 4 bytes within the limit", w.Body.String())
	}

	if c.Failure == nil {
		t.Fatal("RequestCompleted() Failure = nil, want a response-body failure")
	}

	if got := c.Failure.Phase; got != edgekit.PhaseResponseBody {
		t.Errorf("RequestCompleted() Failure.Phase = %v, want %v", got, edgekit.PhaseResponseBody)
	}

	if !errors.Is(c.Failure.Err, edgekit.ErrRequestBodyTooLarge) {
		t.Errorf("RequestCompleted() Failure.Err = %v, want an error that wraps ErrRequestBodyTooLarge", c.Failure.Err)
	}

	if tooLarge, ok := errors.AsType[*http.MaxBytesError](c.Failure.Err); !ok {
		t.Errorf("RequestCompleted() Failure.Err = %v, want an error that wraps *http.MaxBytesError", c.Failure.Err)
	} else if got := tooLarge.Limit; got != 4 {
		t.Errorf("RequestCompleted() MaxBytesError.Limit = %d, want 4", got)
	}
}

func TestBodyLimitThroughServer(t *testing.T) {
	for _, protocol := range []struct {
		name  string
		http2 bool
		major int
	}{
		{
			name:  "http1",
			major: 1,
		},
		{
			name:  "http2",
			http2: true,
			major: 2,
		},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			var received atomic.Int64
			var requests atomic.Int32
			upstream := readingUpstream(t, &received, &requests)

			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:    "/api/",
						Upstream:  static(t, upstream.URL),
						BodyLimit: edgekit.MaxBodyBytes(4),
					},
				},
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{
						collector,
					},
				},
			})
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{
				tls: true,
			})

			// uploaded is what the client and completion saw of one upload.
			type uploaded struct {
				proto      string
				protoMajor int
				status     int
				completion edgekit.Completion
			}

			// upload sends body without a declared length.
			upload := func(t *testing.T, body string) uploaded {
				t.Helper()

				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", unknownLength{strings.NewReader(body)})
				if err != nil {
					t.Fatalf("http.NewRequestWithContext() error = %v", err)
				}

				resp, err := ts.client(t, protocol.http2).Do(req)
				if err != nil {
					t.Fatalf("POST /api/upload error = %v", err)
				}

				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()

				return uploaded{
					proto:      resp.Proto,
					protoMajor: resp.ProtoMajor,
					status:     resp.StatusCode,
					completion: receive(t, completed),
				}
			}

			t.Run("body at limit", func(t *testing.T) {
				got := upload(t, "abcd")

				if got.protoMajor != protocol.major || got.status != http.StatusOK {
					t.Errorf("POST /api/upload = %s %d, want HTTP/%d with status 200", got.proto, got.status, protocol.major)
				}

				if got.completion.Failure != nil {
					t.Errorf("RequestCompleted() Failure = %v, want nil", got.completion.Failure)
				}
			})

			t.Run("body over limit", func(t *testing.T) {
				got := upload(t, "abcdefgh")

				if got.protoMajor != protocol.major || got.status != http.StatusRequestEntityTooLarge {
					t.Errorf("POST /api/upload = %s %d, want HTTP/%d with status 413", got.proto, got.status, protocol.major)
				}

				if c := got.completion; c.Failure == nil || !errors.Is(c.Failure.Err, edgekit.ErrRequestBodyTooLarge) {
					t.Errorf("RequestCompleted() = %+v, want a body overflow failure", c)
				}
			})

			if received.Load() > 8 {
				t.Errorf("upstream got %d bytes, want at most 4 for each request", received.Load())
			}
		})
	}
}
