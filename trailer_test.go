package edgekit_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-edgekit/edgekit"
)

// trailerValues returns the trailers that have values, as "Name=value" in
// sorted order, so tests can compare them.
func trailerValues(trailer http.Header) string {
	var fields []string
	for name, values := range trailer {
		if len(values) != 0 {
			fields = append(fields, name+"="+strings.Join(values, ","))
		}
	}

	sort.Strings(fields)

	return strings.Join(fields, " ")
}

// trailerUpstream returns an upstream that reads the complete body and
// sends the trailers that it got, and a transport to it. With http2, the
// upstream uses HTTP/2. Close of the upstream waits for its handlers, so a
// test can then check the channel without a wait.
func trailerUpstream(t *testing.T, http2 bool) (*httptest.Server, http.RoundTripper, <-chan string) {
	t.Helper()

	received := make(chan string, 4)
	upstream, transport := protocolUpstream(t, http2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			received <- "read failed"

			return
		}

		received <- trailerValues(r.Trailer)
		_, _ = io.WriteString(w, "ok")
	}))

	return upstream, transport, received
}

// chunkedRequest is an HTTP/1.1 request with a chunked body "abc" and the
// trailer section trailers. declared is the value of the Trailer header, or
// empty.
func chunkedRequest(declared, trailers string, header ...string) string {
	var b strings.Builder
	b.WriteString("POST /api/upload HTTP/1.1\r\nHost: gateway.test\r\nTransfer-Encoding: chunked\r\n")
	if declared != "" {
		b.WriteString("Trailer: " + declared + "\r\n")
	}

	for _, h := range header {
		b.WriteString(h + "\r\n")
	}

	b.WriteString("\r\n3\r\nabc\r\n0\r\n" + trailers + "\r\n")

	return b.String()
}

// sendRaw writes request on a new connection to ts and returns the status.
func sendRaw(t *testing.T, ts *testServer, request string) int {
	t.Helper()

	conn := ts.dial(t)
	writeClientData(t, conn, request)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("http.ReadResponse() error = %v", err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return resp.StatusCode
}

// lateTrailerBody gives "abc" and sets the trailer values of its request at
// the end, as a client does for trailers that it computes from the body.
type lateTrailerBody struct {
	io.Reader

	request *http.Request
	values  map[string]string
}

// Read sets the trailer values at the end of the body.
func (b *lateTrailerBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		for name, value := range b.values {
			b.request.Trailer.Set(name, value)
		}
	}

	return n, err
}

// trailerSetup selects the protocols and the middleware of a
// trailerForwarding gateway.
type trailerSetup struct {
	inboundHTTP2    bool
	upstreamHTTP2   bool
	middlewareReads bool
}

// trailerForwarding is a Server with a gateway in drop mode, with the managed
// header X-Customer, in front of an upstream that reports the trailers that
// it got. When the middleware reads the body, it reports the trailers that it
// saw in seen and forwards a copy of the body.
type trailerForwarding struct {
	ts        *testServer
	received  <-chan string
	seen      <-chan string
	completed <-chan edgekit.Completion
}

func newTrailerForwarding(t *testing.T, setup trailerSetup) *trailerForwarding {
	t.Helper()

	upstream, transport, received := trailerUpstream(t, setup.upstreamHTTP2)
	seen := make(chan string, 1)
	var middleware []edgekit.Middleware
	if setup.middlewareReads {
		middleware = append(middleware, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					seen <- "read failed"
					w.WriteHeader(http.StatusInternalServerError)

					return
				}

				seen <- trailerValues(r.Trailer)
				forwarded := r.Clone(r.Context())
				forwarded.Body = io.NopCloser(bytes.NewReader(body))
				next.ServeHTTP(w, forwarded)
			})
		})
	}

	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		Transport:        transport,
		ManagedHeaders:   []string{"X-Customer"},
		HeaderNamePolicy: edgekit.HeaderNameDrop,
		Middleware:       middleware,
		Hooks:            []any{collector},
	})

	return &trailerForwarding{
		ts: startServer(t, g, edgekit.ServerConfig{}, serverOptions{
			tls: setup.inboundHTTP2,
		}),
		received:  received,
		seen:      seen,
		completed: completed,
	}
}

// The client sends Kept, a managed header, an identity header, Host,
// Content-Length, and the invalid name X_late. Only Kept and the undeclared
// Late reach the upstream; an HTTP/2 upstream keeps only declared trailers.
func TestHTTP1RequestTrailersAreCheckedAndForwarded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    trailerSetup
		declared string
		header   []string
		upstream string
	}{
		{
			name:     "to http1",
			declared: "Kept, X-Customer",
			upstream: "Kept=v Late=w",
		},
		{
			name:     "undeclared trailers only",
			declared: "",
			upstream: "Kept=v Late=w",
		},
		{
			name: "undeclared trailers only and middleware that reads the body",
			setup: trailerSetup{
				middlewareReads: true,
			},
			declared: "",
			upstream: "Kept=v Late=w",
		},
		{
			name:     "to http1 with stripped headers",
			declared: "Kept, X-Customer",
			header: []string{
				"X-Forwarded-For: 203.0.113.9",
			},
			upstream: "Kept=v Late=w",
		},
		{
			name: "to http2",
			setup: trailerSetup{
				upstreamHTTP2: true,
			},
			declared: "Kept, X-Customer",
			upstream: "Kept=v",
		},
		{
			name: "to http1 with middleware that reads the body",
			setup: trailerSetup{
				middlewareReads: true,
			},
			declared: "Kept, X-Customer",
			upstream: "Kept=v Late=w",
		},
		{
			name: "middleware that reads the body and stripped headers",
			setup: trailerSetup{
				middlewareReads: true,
			},
			declared: "Kept, X-Customer",
			header: []string{
				"X-Forwarded-For: 203.0.113.9",
			},
			upstream: "Kept=v Late=w",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrailerForwarding(t, tc.setup)

			status := sendRaw(t, f.ts, chunkedRequest(
				tc.declared,
				"Kept: v\r\nX-Customer: spoofed\r\nLate: w\r\nX-Real-Ip: 203.0.113.9\r\nHost: evil.test\r\nContent-Length: 9\r\nX_late: bad\r\n",
				tc.header...,
			))

			if status != http.StatusOK {
				t.Fatalf("POST /api/upload status = %d, want 200", status)
			}

			if c := receive(t, f.completed); c.Failure != nil || c.RejectionReason != "" {
				t.Errorf("RequestCompleted() = %+v, want no failure and no rejection", c)
			}

			if got := receive(t, f.received); got != tc.upstream {
				t.Errorf("upstream got trailers %q, want %q", got, tc.upstream)
			}

			// Only middleware that reads the body to EOF can read the
			// trailers before the upstream gets them.
			if tc.setup.middlewareReads {
				if got := receive(t, f.seen); got != tc.upstream {
					t.Errorf("middleware saw trailers %q, want %q", got, tc.upstream)
				}
			}
		})
	}
}

// An HTTP/2 server keeps only declared trailers. The client declares Kept, a
// managed header, and an identity header; only Kept reaches the upstream.
func TestHTTP2RequestTrailersAreCheckedAndForwarded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    trailerSetup
		header   http.Header
		upstream string
	}{
		{
			name: "to http1",
			setup: trailerSetup{
				inboundHTTP2: true,
			},
			upstream: "Kept=v",
		},
		{
			name: "to http2 with stripped headers",
			setup: trailerSetup{
				inboundHTTP2:  true,
				upstreamHTTP2: true,
			},
			header: http.Header{
				"X-Forwarded-For": {"203.0.113.9"},
			},
			upstream: "Kept=v",
		},
		{
			name: "middleware that reads the body",
			setup: trailerSetup{
				inboundHTTP2:    true,
				middlewareReads: true,
			},
			upstream: "Kept=v",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrailerForwarding(t, tc.setup)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.ts.url()+"/api/upload", nil)
			if err != nil {
				t.Fatalf("http.NewRequestWithContext() error = %v", err)
			}

			maps.Copy(req.Header, tc.header)

			req.Trailer = http.Header{
				"Kept":            nil,
				"X-Customer":      nil,
				"X-Forwarded-For": nil,
			}
			req.Body = io.NopCloser(&lateTrailerBody{
				Reader:  strings.NewReader("abc"),
				request: req,
				values: map[string]string{
					"Kept":            "v",
					"X-Customer":      "spoofed",
					"X-Forwarded-For": "203.0.113.9",
				},
			})
			req.ContentLength = -1

			resp, err := f.ts.client(t, true).Do(req)
			if err != nil {
				t.Fatalf("POST /api/upload error = %v", err)
			}

			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("POST /api/upload status = %d, want 200", resp.StatusCode)
			}

			if c := receive(t, f.completed); c.Failure != nil || c.RejectionReason != "" {
				t.Errorf("RequestCompleted() = %+v, want no failure and no rejection", c)
			}

			if got := receive(t, f.received); got != tc.upstream {
				t.Errorf("upstream got trailers %q, want %q", got, tc.upstream)
			}

			// Only middleware that reads the body to EOF can read the
			// trailers before the upstream gets them.
			if tc.setup.middlewareReads {
				if got := receive(t, f.seen); got != tc.upstream {
					t.Errorf("middleware saw trailers %q, want %q", got, tc.upstream)
				}
			}
		})
	}
}

// lateTrailerResult is what one request with the invalid late trailer
// X_late saw in reject mode.
type lateTrailerResult struct {
	status     int
	completion edgekit.Completion

	// upstream is what the upstream reported, or "" when the request did not
	// reach it.
	upstream string
}

// sendLateInvalidTrailer sends a chunked body whose trailers include the
// invalid name X_late, through a gateway in reject mode with middleware, which
// can be nil.
func sendLateInvalidTrailer(t *testing.T, middleware edgekit.Middleware) lateTrailerResult {
	t.Helper()

	upstream, transport, received := trailerUpstream(t, false)
	collector, completed := completions()
	cfg := edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		Transport: transport,
		Hooks:     []any{collector},
	}
	if middleware != nil {
		cfg.Middleware = []edgekit.Middleware{middleware}
	}

	ts := startServer(t, newGateway(t, cfg), edgekit.ServerConfig{}, serverOptions{})
	got := lateTrailerResult{
		status: sendRaw(t, ts, chunkedRequest("Kept", "Kept: v\r\nX_late: bad\r\n")),
	}
	got.completion = receive(t, completed)

	// Close waits for the handlers of the upstream, so each request that
	// reached it has sent its result.
	upstream.Close()
	select {
	case got.upstream = <-received:
	default:
	}

	return got
}

func TestLateTrailerNameFailsTheRoundTrip(t *testing.T) {
	got := sendLateInvalidTrailer(t, nil)
	c := got.completion

	if got.status != http.StatusBadRequest {
		t.Errorf("POST /api/upload status = %d, want 400", got.status)
	}

	if got.upstream != "" && got.upstream != "read failed" {
		t.Errorf("upstream got the body with trailers %q, want a failed read", got.upstream)
	}

	// A late failure is not a rejection before the middleware: the facts
	// that handling established stay.
	if c.RejectionReason != "" {
		t.Errorf("RequestCompleted() RejectionReason = %q, want empty", c.RejectionReason)
	}

	if c.RoutePrefix != "/api/" {
		t.Errorf("RequestCompleted() RoutePrefix = %q, want /api/", c.RoutePrefix)
	}

	if c.SelectedUpstream == nil {
		t.Error("RequestCompleted() SelectedUpstream = nil, want the selected upstream")
	}

	if c.Failure == nil {
		t.Fatal("RequestCompleted() Failure = nil, want a round-trip failure")
	}

	if c.Failure.Phase != edgekit.PhaseRoundTrip {
		t.Errorf("RequestCompleted() Failure.Phase = %v, want %v", c.Failure.Phase, edgekit.PhaseRoundTrip)
	}

	if !errors.Is(c.Failure.Err, edgekit.ErrInvalidTrailerName) {
		t.Errorf("RequestCompleted() Failure.Err = %v, want an error that wraps ErrInvalidTrailerName", c.Failure.Err)
	}
}

func TestLateTrailerNameFailsTheMiddlewareRead(t *testing.T) {
	type observation struct {
		err      error
		trailers string
	}

	observed := make(chan observation, 1)
	got := sendLateInvalidTrailer(t, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := io.ReadAll(r.Body)
			observed <- observation{
				err:      err,
				trailers: trailerValues(r.Trailer),
			}
			w.WriteHeader(http.StatusBadRequest)
		})
	})
	c := got.completion

	if got.status != http.StatusBadRequest {
		t.Errorf("POST /api/upload status = %d, want 400", got.status)
	}

	if got.upstream != "" {
		t.Errorf("upstream got %q, want no request", got.upstream)
	}

	read := receive(t, observed)
	if !errors.Is(read.err, edgekit.ErrInvalidTrailerName) || read.trailers != "" {
		t.Errorf("middleware io.ReadAll() error = %v with trailers %q, want ErrInvalidTrailerName and no trailers", read.err, read.trailers)
	}

	// The middleware made the response, so the gateway records no failure of
	// its own.
	if c.Failure != nil || c.RejectionReason != "" {
		t.Errorf("RequestCompleted() = %+v, want no failure and no rejection", c)
	}
}

// replacedBody wraps a body, so the request no longer has the body of the
// gateway.
type replacedBody struct {
	io.ReadCloser
}

func TestMiddlewareOwnsOutboundTrailers(t *testing.T) {
	for _, tc := range []struct {
		name              string
		change            func(r *http.Request) *http.Request
		readBeforeForward bool
		upstream          string
	}{
		{
			name: "request unchanged",
			change: func(r *http.Request) *http.Request {
				return r
			},
			upstream: "Kept=v Late=w",
		},
		{
			name: "request copied before EOF",
			change: func(r *http.Request) *http.Request {
				return r.Clone(r.Context())
			},
			readBeforeForward: true,
			upstream:          "Kept=v Late=w",
		},
		{
			name: "declared trailer deleted from a copy before EOF",
			change: func(r *http.Request) *http.Request {
				out := r.Clone(r.Context())
				delete(out.Trailer, "Kept")

				return out
			},
			readBeforeForward: true,
			upstream:          "Late=w",
		},
		{
			name: "mixed-case value set on a copy before EOF",
			change: func(r *http.Request) *http.Request {
				out := r.Clone(r.Context())
				out.Trailer["kept"] = []string{"middleware"}

				return out
			},
			readBeforeForward: true,
			upstream:          "Kept=middleware Late=w",
		},
		{
			name: "body replaced",
			change: func(r *http.Request) *http.Request {
				out := r.Clone(r.Context())
				out.Body = replacedBody{
					ReadCloser: r.Body,
				}

				return out
			},
			upstream: "",
		},
		{
			name: "trailer map removed",
			change: func(r *http.Request) *http.Request {
				out := r.Clone(r.Context())
				out.Trailer = nil

				return out
			},
			upstream: "",
		},
		{
			name: "declared trailer deleted by middleware",
			change: func(r *http.Request) *http.Request {
				out := r.Clone(r.Context())
				delete(out.Trailer, "Kept")

				return out
			},
			upstream: "Late=w",
		},
		{
			name: "trailer value set by middleware under a name in lowercase",
			change: func(r *http.Request) *http.Request {
				out := r.Clone(r.Context())
				out.Trailer["kept"] = []string{"middleware"}

				return out
			},
			upstream: "Kept=middleware Late=w",
		},
		{
			name: "declared trailer deleted and set again in mixed case",
			change: func(r *http.Request) *http.Request {
				out := r.Clone(r.Context())
				delete(out.Trailer, "Kept")
				out.Trailer["kEPT"] = []string{"middleware"}

				return out
			},
			upstream: "Kept=middleware Late=w",
		},
		{
			name: "trailer value set by middleware",
			change: func(r *http.Request) *http.Request {
				out := r.Clone(r.Context())
				out.Trailer["Kept"] = []string{"middleware"}

				return out
			},
			upstream: "Kept=middleware Late=w",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, transport, received := trailerUpstream(t, false)
			readErrors := make(chan error, 1)
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, upstream.URL),
					},
				},
				Transport: transport,
				Middleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							out := tc.change(r)
							var err error
							if tc.readBeforeForward {
								_, err = io.Copy(io.Discard, out.Body)
							}

							readErrors <- err
							if err != nil {
								w.WriteHeader(http.StatusBadRequest)

								return
							}

							next.ServeHTTP(w, out)
						})
					},
				},
			})
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})

			status := sendRaw(t, ts, chunkedRequest("Kept", "Kept: v\r\nLate: w\r\n"))

			if err := receive(t, readErrors); err != nil {
				t.Fatalf("middleware read of the copied body: error = %v", err)
			}

			if status != http.StatusOK {
				t.Fatalf("POST /api/upload status = %d, want 200", status)
			}

			if got := receive(t, received); got != tc.upstream {
				t.Errorf("upstream got trailers %q, want %q", got, tc.upstream)
			}
		})
	}
}

func TestTrailersNeedSuccessfulEndOfBody(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request string
		limit   edgekit.BodyLimit
		close   bool
		read    bool
	}{
		{
			name:    "malformed chunk",
			request: "POST /api/upload HTTP/1.1\r\nHost: gateway.test\r\nTransfer-Encoding: chunked\r\nTrailer: Kept\r\n\r\n3\r\nabc\r\nzz\r\nKept: v\r\n\r\n",
			read:    true,
		},
		{
			name:    "truncated body",
			request: "POST /api/upload HTTP/1.1\r\nHost: gateway.test\r\nTransfer-Encoding: chunked\r\nTrailer: Kept\r\n\r\n3\r\nabc\r\n",
			close:   true,
			read:    true,
		},
		{
			name:    "body over the limit",
			request: chunkedRequest("Kept", "Kept: v\r\n"),
			limit:   edgekit.MaxBodyBytes(2),
			read:    true,
		},
		{
			name:    "close without a read",
			request: chunkedRequest("Kept", "Kept: v\r\n"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type observation struct {
				err      error
				trailers string
			}

			observed := make(chan observation, 1)
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:    "/api/",
						Upstream:  static(t, "http://upstream.test"),
						BodyLimit: tc.limit,
						Middleware: []edgekit.Middleware{
							func(http.Handler) http.Handler {
								return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									var err error
									if tc.read {
										_, err = io.ReadAll(r.Body)
									}

									closeErr := r.Body.Close()
									observed <- observation{
										err:      errors.Join(err, closeErr),
										trailers: trailerValues(r.Trailer),
									}
									w.WriteHeader(http.StatusNoContent)
								})
							},
						},
					},
				},
			})
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})

			conn := ts.dial(t)
			writeClientData(t, conn, tc.request)
			if tc.close {
				// The client goes away in the middle of the body.
				if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatalf("CloseWrite() error = %v", err)
				}
			}

			got := receive(t, observed)

			// A read fails in each case; Close alone does not fail.
			if gotErr := got.err != nil; gotErr != tc.read {
				t.Errorf("middleware read and close: error = %v, want an error: %t", got.err, tc.read)
			}

			if got.trailers != "" {
				t.Errorf("middleware saw trailers %q, want none after a failed or missing read", got.trailers)
			}
		})
	}
}

// eofBody gives its data and io.EOF in the same call, and sets the trailers
// of its request before that call, as a server does.
type eofBody struct {
	data    string
	request *http.Request
	done    bool
}

// Read returns all data with io.EOF.
func (b *eofBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}

	b.done = true
	b.request.Trailer["Kept"] = []string{"v"}
	b.request.Trailer["X_late"] = []string{"bad"}

	return copy(p, b.data), io.EOF
}

// Close has no effect.
func (*eofBody) Close() error {
	return nil
}

func TestTrailersArePublishedAtEOFWithData(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   edgekit.HeaderNamePolicy
		trailers string
		wantErr  error
	}{
		{
			name:     "drop mode",
			policy:   edgekit.HeaderNameDrop,
			trailers: "Kept=v",
			wantErr:  io.EOF,
		},
		{
			name:    "reject mode",
			policy:  edgekit.HeaderNameReject,
			wantErr: edgekit.ErrInvalidTrailerName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type observation struct {
				n        int
				err      error
				trailers string
			}

			observed := make(chan observation, 1)
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
						Middleware: []edgekit.Middleware{
							func(http.Handler) http.Handler {
								return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									buf := make([]byte, 16)
									n, err := r.Body.Read(buf)
									observed <- observation{
										n:        n,
										err:      err,
										trailers: trailerValues(r.Trailer),
									}
									w.WriteHeader(http.StatusNoContent)
								})
							},
						},
					},
				},
				HeaderNamePolicy: tc.policy,
			})

			r := httptest.NewRequest(http.MethodPost, "/api/upload", nil)
			r.Trailer = http.Header{
				"Kept": nil,
			}
			r.Body = &eofBody{
				data:    "abc",
				request: r,
			}
			r.ContentLength = -1
			g.ServeHTTP(httptest.NewRecorder(), r)

			got := receive(t, observed)

			if got.n != 3 || !errors.Is(got.err, tc.wantErr) {
				t.Errorf("middleware Body.Read() = %d, %v; want 3, %v", got.n, got.err, tc.wantErr)
			}

			if got.trailers != tc.trailers {
				t.Errorf("middleware saw trailers %q, want %q", got.trailers, tc.trailers)
			}
		})
	}
}

func TestTrailerStatusNeedsEvidence(t *testing.T) {
	unrelated := fmt.Errorf("other check: %w", edgekit.ErrInvalidTrailerName)
	for _, tc := range []struct {
		name        string
		lateInvalid bool
		cancel      bool
		fail        func(readErr error) error
		status      int
	}{
		{
			name:        "late invalid name",
			lateInvalid: true,
			fail: func(readErr error) error {
				return readErr
			},
			status: http.StatusBadRequest,
		},
		{
			name:        "client gone first",
			lateInvalid: true,
			cancel:      true,
			fail: func(readErr error) error {
				return errors.Join(readErr, context.Canceled)
			},
			status: 499,
		},
		{
			name: "sentinel without evidence",
			fail: func(error) error {
				return unrelated
			},
			status: http.StatusBadGateway,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
					},
				},
				Transport: readingTransport{
					fail: tc.fail,
				},
				Hooks: []any{collector},
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			r := httptest.NewRequest(http.MethodPost, "/api/upload", nil).WithContext(ctx)
			r.Trailer = http.Header{}
			r.Body = io.NopCloser(strings.NewReader("abc"))
			if tc.lateInvalid {
				r.Body = &eofBody{
					data:    "abc",
					request: r,
				}
			}

			r.ContentLength = -1
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			c := receive(t, completed)

			if w.Code != tc.status {
				t.Errorf("ServeHTTP() status = %d, want %d", w.Code, tc.status)
			}

			if c.Failure == nil {
				t.Error("RequestCompleted() Failure = nil, want a failure")
			}
		})
	}
}

func TestLateTrailerFailureAfterCommitIsNotRendered(t *testing.T) {
	g, rendered, completed := echoGateway(t, edgekit.BodyLimit{})
	r := httptest.NewRequest(http.MethodPost, "/api/upload", nil)
	r.Trailer = http.Header{}
	r.Body = &eofBody{
		data:    "abc",
		request: r,
	}
	r.ContentLength = -1
	r = serverRequest(r)
	w := httptest.NewRecorder()
	panicked := panics(func() {
		g.ServeHTTP(w, r)
	})
	c := receive(t, completed)

	if !panicked || !c.Aborted || w.Code != http.StatusOK || rendered.Load() != 0 {
		t.Errorf("ServeHTTP() panicked = %v, status = %d, renderer calls = %d, RequestCompleted() Aborted = %v; want the committed 200 to abort without rendering", panicked, w.Code, rendered.Load(), c.Aborted)
	}

	if c.Failure == nil {
		t.Fatal("RequestCompleted() Failure = nil, want a response-body failure")
	}

	if got := c.Failure.Phase; got != edgekit.PhaseResponseBody {
		t.Errorf("RequestCompleted() Failure.Phase = %v, want %v", got, edgekit.PhaseResponseBody)
	}

	if !errors.Is(c.Failure.Err, edgekit.ErrInvalidTrailerName) {
		t.Errorf("RequestCompleted() Failure.Err = %v, want an error that wraps ErrInvalidTrailerName", c.Failure.Err)
	}
}

func TestEarlyResponseWithLateTrailers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   edgekit.HeaderNamePolicy
		upstream string
	}{
		{
			name:     "drop mode forwards the checked trailers",
			policy:   edgekit.HeaderNameDrop,
			upstream: "Kept=v",
		},
		{
			name:     "reject mode ends the upload",
			policy:   edgekit.HeaderNameReject,
			upstream: "read failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The upstream answers before it reads the body. Without full
				// duplex, an HTTP/1.1 server reads the body before it
				// responds.
				if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
					t.Errorf("upstream EnableFullDuplex() error = %v", err)
				}

				_, _ = io.WriteString(w, "early")
				_ = http.NewResponseController(w).Flush()

				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					received <- "read failed"

					return
				}

				received <- trailerValues(r.Trailer)
			}))
			t.Cleanup(upstream.Close)

			var rendered atomic.Int32
			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, upstream.URL),
					},
				},
				HeaderNamePolicy: tc.policy,
				Renderer: func(context.Context, edgekit.Problem) (string, []byte) {
					rendered.Add(1)

					return "text/plain", nil
				},
				Middleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
								t.Errorf("middleware EnableFullDuplex() error = %v", err)
							}

							next.ServeHTTP(w, r)
						})
					},
				},
				Hooks: []any{collector},
			})
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})

			conn := ts.dial(t)
			writeClientData(t, conn, "POST /api/upload HTTP/1.1\r\nHost: gateway.test\r\nTransfer-Encoding: chunked\r\nTrailer: Kept\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("http.ReadResponse() error = %v", err)
			}
			defer func() {
				_ = resp.Body.Close()
			}()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("POST /api/upload status = %d, want the early 200", resp.StatusCode)
			}

			// The trailers arrive after the response was committed.
			writeClientData(t, conn, "3\r\nabc\r\n0\r\nKept: v\r\nX_late: bad\r\n\r\n")
			if got := receive(t, received); got != tc.upstream {
				t.Errorf("upstream got %q, want %q", got, tc.upstream)
			}

			_, _ = io.Copy(io.Discard, resp.Body)
			c := receive(t, completed)
			if rendered.Load() != 0 || c.RejectionReason != "" {
				t.Errorf("renderer calls = %d, RequestCompleted() = %+v; want no rendering after the commit", rendered.Load(), c)
			}
		})
	}
}

func TestForbiddenTrailersAreRemoved(t *testing.T) {
	upstream, transport, received := trailerUpstream(t, false)
	declared := make(chan string, 1)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		Transport: transport,
		Middleware: []edgekit.Middleware{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					names := make([]string, 0, len(r.Trailer))
					for name := range r.Trailer {
						names = append(names, name)
					}

					sort.Strings(names)
					declared <- strings.Join(names, " ")
					next.ServeHTTP(w, r)
				})
			},
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})

	// RFC 9110, section 6.5.1: framing, routing, authentication, request
	// modifiers, response controls, and content format. Some names are in
	// lowercase on the wire.
	forbidden := strings.Join([]string{
		"Authorization: Bearer late",
		"proxy-authorization: Basic late",
		"Cookie: session=late",
		"Set-Cookie: session=late",
		"Www-Authenticate: Basic",
		"Realm: late",
		"Cache-Control: no-cache",
		"Pragma: no-cache",
		"Expect: 100-continue",
		"Max-Forwards: 0",
		"Range: bytes=0-1",
		"If-Match: \"late\"",
		"if-none-match: \"late\"",
		"If-Modified-Since: Thu, 01 Jan 1970 00:00:00 GMT",
		"Content-Type: text/evil",
		"Content-Encoding: gzip",
		"Content-Range: bytes 0-1/2",
		"Host: evil.test",
	}, "\r\n") + "\r\n"

	status := sendRaw(t, ts, chunkedRequest("Kept, Authorization, If-Match", "Kept: v\r\nLate: w\r\n"+forbidden))

	if status != http.StatusOK {
		t.Fatalf("POST /api/upload status = %d, want 200", status)
	}

	if got := receive(t, declared); got != "Kept" {
		t.Errorf("middleware saw declared trailers %q, want Kept", got)
	}

	if got := receive(t, received); got != "Kept=v Late=w" {
		t.Errorf("upstream got trailers %q, want only Kept=v Late=w", got)
	}
}
