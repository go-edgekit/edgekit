package edgekit_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-edgekit/edgekit"
)

func TestNonPrintableUpgradeIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		upgrade string
	}{
		{
			name:    "tab",
			upgrade: "web\tsocket",
		},
		{
			name:    "obsolete text",
			upgrade: "w\xe9bsocket",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := countingUpstream(t, &requests)
			completionHooks, completed := completions()
			server := hookServer(t, upstream.URL, completionHooks)

			status, _, reader, _ := responseHead(t, server, "GET /api/echo HTTP/1.1\r\n"+
				"Host: gateway.test\r\n"+
				"Connection: Upgrade\r\n"+
				"Upgrade: "+tc.upgrade+"\r\n"+
				"\r\n")

			if status != "HTTP/1.1 400 Bad Request" {
				t.Errorf("status = %q, want 400", status)
			}

			body := make([]byte, len(problem(http.StatusBadRequest, edgekit.ReasonInvalidUpgrade)))
			if _, err := io.ReadFull(reader, body); err != nil {
				t.Errorf("ReadFull(response body) error = %v, want nil", err)
			}

			if want := problem(http.StatusBadRequest, edgekit.ReasonInvalidUpgrade); string(body) != want {
				t.Errorf("ServeHTTP() body = %q, want %q", body, want)
			}

			c := receive(t, completed)
			if c.RejectionReason != edgekit.ReasonInvalidUpgrade {
				t.Errorf("RequestCompleted() RejectionReason = %q, want %q", c.RejectionReason, edgekit.ReasonInvalidUpgrade)
			}

			if c.Failure != nil {
				t.Errorf("RequestCompleted() Failure = %+v, want nil", c.Failure)
			}

			if got := requests.Load(); got != 0 {
				t.Errorf("ServeHTTP() upstream requests = %d, want 0", got)
			}
		})
	}
}

func TestMiddlewareUpgradeValueFails(t *testing.T) {
	var requests atomic.Int32
	upstream := countingUpstream(t, &requests)
	completionHooks, completed := completions()
	middleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.Clone(r.Context())
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "web\tsocket")
			next.ServeHTTP(w, r)
		})
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:     "/api/",
			Upstream:   static(t, upstream.URL),
			Middleware: []edgekit.Middleware{middleware},
		}},
		Hooks: []any{completionHooks},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("ServeHTTP() status = %d, want 500", w.Code)
	}

	if got, want := w.Body.String(), problem(http.StatusInternalServerError, ""); got != want {
		t.Errorf("ServeHTTP() body = %q, want %q", got, want)
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("ServeHTTP() upstream requests = %d, want 0", got)
	}

	c := receive(t, completed)
	if c.Failure == nil {
		t.Fatal("RequestCompleted() Failure = nil, want a failure before the upstream")
	}

	if c.Failure.Phase != edgekit.PhaseBeforeUpstream {
		t.Errorf("RequestCompleted() Failure.Phase = %v, want BeforeUpstream", c.Failure.Phase)
	}
}

// upgradeServeHTTP sends an upgrade request for the echo protocol directly to
// g with w.
func upgradeServeHTTP(g *edgekit.Gateway, w http.ResponseWriter) {
	r := httptest.NewRequest(http.MethodGet, "/api/echo", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "echo")
	g.ServeHTTP(w, r)
}

func TestUpgradeBodyNotWritable(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		hookRan := false
		hooks := upstreamResponseFunc(func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
			hookRan = true

			return nil, nil
		})
		completionHooks, completed := completions()
		g := newGateway(t, edgekit.Config{
			Routes: []edgekit.Route{{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			}},
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusSwitchingProtocols,
					Header:     http.Header{"Connection": {"Upgrade"}, "Upgrade": {"echo"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			}),
			Hooks: []any{completionHooks, hooks},
		})

		w := httptest.NewRecorder()
		upgradeServeHTTP(g, w)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("ServeHTTP() status = %d, want 500", w.Code)
		}

		if got, want := w.Body.String(), problem(http.StatusInternalServerError, ""); got != want {
			t.Errorf("ServeHTTP() body = %q, want %q", got, want)
		}

		if hookRan {
			t.Error("response hook ran for a response that broke the transport contract")
		}

		c := receive(t, completed)
		if c.Failure == nil {
			t.Fatal("RequestCompleted() Failure = nil, want a round-trip failure")
		}

		if c.Failure.Phase != edgekit.PhaseRoundTrip {
			t.Errorf("RequestCompleted() Failure.Phase = %v, want RoundTrip", c.Failure.Phase)
		}
	})

	for _, tc := range []struct {
		name string
		body io.ReadCloser
	}{
		{
			name: "response hook",
			body: io.NopCloser(strings.NewReader("")),
		},
		{
			name: "response hook with nil body",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := upgradeUpstream(t, "Connection: Upgrade\r\nUpgrade: echo\r\n")
			hooks := upstreamResponseFunc(func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) (*edgekit.Rejection, error) {
				_ = resp.Body.Close()
				resp.Body = tc.body

				return nil, nil
			})
			completionHooks, completed := completions()
			server := hookServer(t, upstream.URL, completionHooks, hooks)

			status, _, _, _ := responseHead(t, server, upgradeRequest)

			if status != "HTTP/1.1 500 Internal Server Error" {
				t.Errorf("status = %q, want 500", status)
			}

			c := receive(t, completed)
			if c.Failure == nil {
				t.Fatal("RequestCompleted() Failure = nil, want an upstream-response failure")
			}

			if c.Failure.Phase != edgekit.PhaseUpstreamResponse {
				t.Errorf("RequestCompleted() Failure.Phase = %v, want UpstreamResponse", c.Failure.Phase)
			}
		})
	}
}

// hijackRecorder is a recorder whose Hijack returns err.
type hijackRecorder struct {
	*httptest.ResponseRecorder

	err          error
	beforeHijack func()
}

// Hijack runs the configured action before it fails with r.err.
func (r *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if r.beforeHijack != nil {
		r.beforeHijack()
	}

	return nil, nil, r.err
}

func TestHijackFailureStatus(t *testing.T) {
	upstream := upgradeUpstream(t, "Connection: Upgrade\r\nUpgrade: echo\r\n")

	for _, tc := range []struct {
		name        string
		err         error
		hijacker    bool
		cancelEntry bool
		status      int
	}{
		{
			name:   "writer without hijack",
			err:    http.ErrNotSupported,
			status: http.StatusInternalServerError,
		},
		{
			name:     "other hijack error",
			err:      io.ErrClosedPipe,
			hijacker: true,
			status:   http.StatusBadGateway,
		},
		{
			name:        "cancellation while the client is gone",
			err:         fmt.Errorf("hijack: %w", context.Canceled),
			hijacker:    true,
			cancelEntry: true,
			status:      499,
		},
		{
			name:     "cancellation while the client is present",
			err:      fmt.Errorf("hijack: %w", context.Canceled),
			hijacker: true,
			status:   http.StatusBadGateway,
		},
		{
			name:        "other error while the client is gone",
			err:         io.ErrClosedPipe,
			hijacker:    true,
			cancelEntry: true,
			status:      http.StatusBadGateway,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			completionHooks, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, upstream.URL),
				}},
				Transport: testTransport(t),
				Hooks:     []any{completionHooks},
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			recorder := httptest.NewRecorder()
			var w http.ResponseWriter = recorder
			hijackCalled := false
			if tc.hijacker {
				w = &hijackRecorder{
					ResponseRecorder: recorder,
					err:              tc.err,
					beforeHijack: func() {
						hijackCalled = true
						if tc.cancelEntry {
							cancel()
						}
					},
				}
			}

			r := httptest.NewRequest(http.MethodGet, "/api/echo", nil).WithContext(ctx)
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "echo")
			g.ServeHTTP(w, r)

			if hijackCalled != tc.hijacker {
				t.Errorf("ServeHTTP() writer Hijack() called = %v, want %v", hijackCalled, tc.hijacker)
			}

			var wantContextErr error
			if tc.cancelEntry {
				wantContextErr = context.Canceled
			}

			if err := ctx.Err(); !errors.Is(err, wantContextErr) {
				t.Errorf("ServeHTTP() entry context error = %v, want %v", err, wantContextErr)
			}

			if recorder.Code != tc.status {
				t.Errorf("ServeHTTP() status = %d, want %d", recorder.Code, tc.status)
			}

			if got, want := recorder.Body.String(), problem(tc.status, ""); got != want {
				t.Errorf("ServeHTTP() body = %q, want %q", got, want)
			}

			c := receive(t, completed)
			if c.Failure == nil {
				t.Fatal("RequestCompleted() Failure = nil, want a protocol-switch failure")
			}

			if c.Failure.Phase != edgekit.PhaseProtocolSwitch {
				t.Errorf("RequestCompleted() Failure.Phase = %v, want ProtocolSwitch", c.Failure.Phase)
			}

			if !errors.Is(c.Failure.Err, tc.err) {
				t.Errorf("RequestCompleted() Failure.Err = %v, want an error that matches %v", c.Failure.Err, tc.err)
			}
		})
	}
}

// brokenConnWriter hijacks to a connection whose writes fail, and records
// the calls that would write a response.
type brokenConnWriter struct {
	header http.Header
	writes int
}

// Header returns the header map.
func (w *brokenConnWriter) Header() http.Header {
	return w.header
}

// Write counts the call.
func (w *brokenConnWriter) Write(b []byte) (int, error) {
	w.writes++

	return len(b), nil
}

// WriteHeader counts the call.
func (w *brokenConnWriter) WriteHeader(int) {
	w.writes++
}

// Hijack returns a closed connection.
func (w *brokenConnWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	client, server := net.Pipe()
	_ = server.Close()

	return client, bufio.NewReadWriter(bufio.NewReader(client), bufio.NewWriter(client)), nil
}

func TestFailureAfterHijackIsNotRendered(t *testing.T) {
	upstream := upgradeUpstream(t, "Connection: Upgrade\r\nUpgrade: echo\r\n")
	rendered := false
	completionHooks, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		Transport: testTransport(t),
		Hooks:     []any{completionHooks},
		Renderer: func(context.Context, edgekit.Problem) (string, []byte) {
			rendered = true

			return "text/plain", nil
		},
	})

	w := &brokenConnWriter{
		header: make(http.Header),
	}
	upgradeServeHTTP(g, w)

	if rendered {
		t.Error("ServeHTTP() renderer called = true, want false after hijack")
	}

	if w.writes != 0 {
		t.Errorf("ServeHTTP() response writer calls = %d, want 0 after hijack", w.writes)
	}

	c := receive(t, completed)
	if c.Failure == nil {
		t.Fatal("RequestCompleted() Failure = nil, want a protocol-switch write failure")
	}

	if c.Failure.Phase != edgekit.PhaseProtocolSwitch {
		t.Errorf("RequestCompleted() Failure.Phase = %v, want ProtocolSwitch", c.Failure.Phase)
	}

	if errors.Is(c.Failure.Err, http.ErrNotSupported) {
		t.Errorf("RequestCompleted() Failure.Err = %v, want a write failure, not ErrNotSupported", c.Failure.Err)
	}
}

func TestResponseHookNilBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{
			name:   "with an error",
			err:    errors.New("inspect failed"),
			status: http.StatusInternalServerError,
		},
		{
			name:   "without an error",
			status: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := countingUpstream(t, &requests)
			hooks := upstreamResponseFunc(func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) (*edgekit.Rejection, error) {
				_ = resp.Body.Close()
				resp.Body = nil
				resp.Header.Del("Content-Length")

				return nil, tc.err
			})
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, upstream.URL),
				}},
				Hooks: []any{hooks},
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != tc.status {
				t.Errorf("status = %d, want %d", w.Code, tc.status)
			}

			if tc.err == nil && w.Body.Len() != 0 {
				t.Errorf("body = %q, want an empty body", w.Body.String())
			}
		})
	}
}

// hijackedWriter is a writer whose connection another component hijacked
// already. It counts the calls that would write a response.
type hijackedWriter struct {
	header http.Header
	writes int
}

// Header returns the header map.
func (w *hijackedWriter) Header() http.Header {
	return w.header
}

// Write counts the call.
func (w *hijackedWriter) Write([]byte) (int, error) {
	w.writes++

	return 0, http.ErrHijacked
}

// WriteHeader counts the call.
func (w *hijackedWriter) WriteHeader(int) {
	w.writes++
}

// FlushError counts the call.
func (w *hijackedWriter) FlushError() error {
	w.writes++

	return http.ErrHijacked
}

// Hijack fails, because the connection is hijacked already.
func (w *hijackedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrHijacked
}

func TestHijackedConnectionGetsNoResponse(t *testing.T) {
	upstream := upgradeUpstream(t, "Connection: Upgrade\r\nUpgrade: echo\r\n")
	renders := 0
	completionHooks, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		Transport: testTransport(t),
		Hooks:     []any{completionHooks},
		Renderer: func(context.Context, edgekit.Problem) (string, []byte) {
			renders++

			return "text/plain", nil
		},
	})

	w := &hijackedWriter{
		header: make(http.Header),
	}
	upgradeServeHTTP(g, w)

	if renders != 0 {
		t.Errorf("ServeHTTP() renderer calls = %d, want 0", renders)
	}

	if w.writes != 0 {
		t.Errorf("ServeHTTP() response writer calls = %d, want 0", w.writes)
	}

	c := receive(t, completed)
	if c.Failure == nil {
		t.Fatal("RequestCompleted() Failure = nil, want a hijack failure")
	}

	if c.Failure.Phase != edgekit.PhaseProtocolSwitch {
		t.Errorf("RequestCompleted() Failure.Phase = %v, want ProtocolSwitch", c.Failure.Phase)
	}

	if !errors.Is(c.Failure.Err, http.ErrHijacked) {
		t.Errorf("RequestCompleted() Failure.Err = %v, want http.ErrHijacked", c.Failure.Err)
	}
}

// trackedBody reports the close of an upstream 101 body.
type trackedBody struct {
	io.ReadWriteCloser

	once   sync.Once
	closed chan struct{}
}

// Close reports the call and closes the body.
func (b *trackedBody) Close() error {
	b.once.Do(func() {
		close(b.closed)
	})

	return b.ReadWriteCloser.Close()
}

// trackingTransport returns a transport that reports the close of the body of
// a 101 response.
func trackingTransport(t *testing.T) (http.RoundTripper, <-chan struct{}) {
	t.Helper()

	closed := make(chan struct{})
	base := testTransport(t)

	return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err != nil {
			return nil, err
		}

		if body, ok := resp.Body.(io.ReadWriteCloser); ok && resp.StatusCode == http.StatusSwitchingProtocols {
			resp.Body = &trackedBody{
				ReadWriteCloser: body,
				closed:          closed,
			}
		}

		return resp, nil
	}), closed
}

func TestFailedProtocolSwitchClosesUpstreamBody(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
		writer   func() http.ResponseWriter
	}{
		{
			name:     "upstream does not accept the switch",
			protocol: "other",
			writer: func() http.ResponseWriter {
				return httptest.NewRecorder()
			},
		},
		{
			name:     "writer without hijack",
			protocol: "echo",
			writer: func() http.ResponseWriter {
				return httptest.NewRecorder()
			},
		},
		{
			name:     "other hijack error",
			protocol: "echo",
			writer: func() http.ResponseWriter {
				return &hijackRecorder{
					ResponseRecorder: httptest.NewRecorder(),
					err:              io.ErrClosedPipe,
				}
			},
		},
		{
			name:     "connection hijacked already",
			protocol: "echo",
			writer: func() http.ResponseWriter {
				return &hijackedWriter{
					header: make(http.Header),
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := upgradeUpstream(t, "Connection: Upgrade\r\nUpgrade: "+tc.protocol+"\r\n")
			transport, closed := trackingTransport(t)
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, upstream.URL),
				}},
				Transport: transport,
			})

			upgradeServeHTTP(g, tc.writer())

			receive(t, closed)
		})
	}
}
