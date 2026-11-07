package edgekit_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// protocolUpstream returns a started upstream for handler and a transport
// that uses HTTP/2 to it when http2 is true.
func protocolUpstream(t *testing.T, http2 bool, handler http.Handler) (*httptest.Server, *http.Transport) {
	t.Helper()

	upstream := httptest.NewUnstartedServer(handler)
	transport := testTransport(t)
	if http2 {
		upstream.EnableHTTP2 = true
		upstream.StartTLS()

		roots := x509.NewCertPool()
		roots.AddCert(upstream.Certificate())
		transport.TLSClientConfig = &tls.Config{
			RootCAs:    roots,
			MinVersion: tls.VersionTLS12,
		}
	} else {
		upstream.Start()
	}
	t.Cleanup(upstream.Close)

	return upstream, transport
}

// timeoutError is a net.Error that reports a timeout.
type timeoutError struct{}

// Error returns fixed text.
func (timeoutError) Error() string { return "i/o timeout" }

// Timeout reports true.
func (timeoutError) Timeout() bool { return true }

// Temporary reports true.
func (timeoutError) Temporary() bool { return true }

func TestRoundTripTimeoutStatus(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("response header timeout/HTTP2=%v", http2), func(t *testing.T) {
			released := make(chan struct{})
			upstream, transport := protocolUpstream(t, http2, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
				close(released)
			}))
			transport.ResponseHeaderTimeout = 50 * time.Millisecond

			completionHooks, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, upstream.URL),
				}},
				Transport: transport,
				Hooks:     []any{completionHooks},
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != http.StatusGatewayTimeout {
				t.Errorf("ServeHTTP() status = %d, want 504", w.Code)
			}

			if got, want := w.Body.String(), problem(http.StatusGatewayTimeout, ""); got != want {
				t.Errorf("ServeHTTP() body = %q, want %q", got, want)
			}

			c := receive(t, completed)
			if !c.ConnectionObtained {
				t.Error("RequestCompleted() ConnectionObtained = false, want true")
			}

			if !c.RequestWritten {
				t.Error("RequestCompleted() RequestWritten = false, want true")
			}

			if c.Failure == nil {
				t.Fatal("RequestCompleted() Failure = nil, want a round-trip failure")
			}

			if c.Failure.Phase != edgekit.PhaseRoundTrip {
				t.Errorf("RequestCompleted() Failure.Phase = %v, want RoundTrip", c.Failure.Phase)
			}

			receive(t, released)
		})
	}

	t.Run("dial timeout", func(t *testing.T) {
		transport := testTransport(t)
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: timeoutError{},
			}
		}

		completionHooks, completed := completions()
		g := newGateway(t, edgekit.Config{
			Routes: []edgekit.Route{{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			}},
			Transport: transport,
			Hooks:     []any{completionHooks},
		})

		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

		if w.Code != http.StatusGatewayTimeout {
			t.Errorf("status = %d, want 504", w.Code)
		}

		c := receive(t, completed)
		if c.ConnectionObtained {
			t.Error("RequestCompleted() ConnectionObtained = true, want false")
		}

		if c.RequestWritten {
			t.Error("RequestCompleted() RequestWritten = true, want false")
		}

		if c.Failure == nil {
			t.Fatal("RequestCompleted() Failure = nil, want a dial failure")
		}

		if _, ok := errors.AsType[*net.OpError](c.Failure.Err); !ok {
			t.Errorf("RequestCompleted() Failure.Err = %v, want a *net.OpError", c.Failure.Err)
		}
	})

	t.Run("timeout without trace events", func(t *testing.T) {
		for _, err := range []error{
			timeoutError{},
			fmt.Errorf("pool: %w", context.DeadlineExceeded),
		} {
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, "http://upstream.test"),
				}},
				Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return nil, err
				}),
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != http.StatusGatewayTimeout {
				t.Errorf("%v: status = %d, want 504", err, w.Code)
			}
		}
	})
}

// failingStages returns a gateway whose stage in phase stage fails with err.
// The other stages pass.
func failingStages(t *testing.T, stage edgekit.Phase, err error, middleware []edgekit.Middleware, providers ...any) *edgekit.Gateway {
	t.Helper()

	hooks := &testHooks{}
	switch stage {
	case edgekit.PhaseRequestReceived:
		hooks.received = func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
			return nil, err
		}
	case edgekit.PhaseBeforeUpstream:
		hooks.before = func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
			return nil, err
		}
	case edgekit.PhaseUpstreamResponse:
		hooks.response = func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
			return nil, err
		}
	case edgekit.PhaseUpstreamSelection, edgekit.PhaseRoundTrip:
		// The selector and the transport below fail.
	case edgekit.PhaseUnknown, edgekit.PhaseProtocolSwitch, edgekit.PhaseResponseBody:
		t.Fatalf("failingStages() does not support %v", stage)
	}

	selector := edgekit.UpstreamSelectorFunc(func(*http.Request) (*url.URL, error) {
		if stage == edgekit.PhaseUpstreamSelection {
			return nil, err
		}

		return parseURL(t, "http://upstream.test"), nil
	})
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if stage == edgekit.PhaseRoundTrip {
			return nil, err
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})

	return newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
		}},
		Middleware: middleware,
		Transport:  transport,
		Hooks:      append(providers, hooks),
	})
}

func TestClientGoneStatus(t *testing.T) {
	for _, stage := range []edgekit.Phase{
		edgekit.PhaseRequestReceived,
		edgekit.PhaseUpstreamSelection,
		edgekit.PhaseBeforeUpstream,
		edgekit.PhaseRoundTrip,
		edgekit.PhaseUpstreamResponse,
	} {
		t.Run(stage.String(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			completionHooks, completed := completions()
			g := failingStages(t, stage, fmt.Errorf("stopped: %w", context.Canceled), nil, completionHooks)

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil).WithContext(ctx))

			if w.Code != 499 {
				t.Errorf("ServeHTTP() status = %d, want 499", w.Code)
			}

			if got, want := w.Body.String(), problem(499, ""); got != want {
				t.Errorf("ServeHTTP() body = %q, want %q", got, want)
			}

			c := receive(t, completed)
			if c.Failure == nil {
				t.Fatal("RequestCompleted() Failure = nil, want a cancellation")
			}

			if c.Failure.Phase != stage {
				t.Errorf("RequestCompleted() Failure.Phase = %v, want %v", c.Failure.Phase, stage)
			}

			if !errors.Is(c.Failure.Err, context.Canceled) {
				t.Errorf("RequestCompleted() Failure.Err = %v, want context.Canceled", c.Failure.Err)
			}
		})
	}
}

func TestCanceledContextDoesNotChangeOtherFailures(t *testing.T) {
	for _, tc := range []struct {
		name               string
		stage              edgekit.Phase
		err                error
		entryCanceled      bool
		middlewareCanceled bool
		status             int
	}{
		{
			name:          "hook error while the client is gone",
			stage:         edgekit.PhaseRequestReceived,
			err:           errors.New("database down"),
			entryCanceled: true,
			status:        http.StatusInternalServerError,
		},
		{
			name:          "selector error while the client is gone",
			stage:         edgekit.PhaseUpstreamSelection,
			err:           errors.New("discovery down"),
			entryCanceled: true,
			status:        http.StatusInternalServerError,
		},
		{
			name:          "connection reset while the client is gone",
			stage:         edgekit.PhaseRoundTrip,
			err:           io.ErrUnexpectedEOF,
			entryCanceled: true,
			status:        http.StatusBadGateway,
		},
		{
			name:          "timeout while the client is gone",
			stage:         edgekit.PhaseRoundTrip,
			err:           timeoutError{},
			entryCanceled: true,
			status:        http.StatusGatewayTimeout,
		},
		{
			name:               "cancellation by middleware in a hook",
			stage:              edgekit.PhaseBeforeUpstream,
			err:                context.Canceled,
			middlewareCanceled: true,
			status:             http.StatusInternalServerError,
		},
		{
			name:               "cancellation by middleware in the round trip",
			stage:              edgekit.PhaseRoundTrip,
			err:                context.Canceled,
			middlewareCanceled: true,
			status:             http.StatusBadGateway,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if tc.entryCanceled {
				cancel()
			}

			var stageContextErr error
			middlewareRan := false
			middleware := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					stageContext := r.Context()
					if tc.middlewareCanceled {
						derived, cancelDerived := context.WithCancel(r.Context())
						defer cancelDerived()
						cancelDerived()
						stageContext = derived
					}

					middlewareRan = true
					stageContextErr = stageContext.Err()
					next.ServeHTTP(w, r.WithContext(stageContext))
				})
			}
			g := failingStages(t, tc.stage, tc.err, []edgekit.Middleware{middleware})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil).WithContext(ctx))

			var wantEntryErr error
			if tc.entryCanceled {
				wantEntryErr = context.Canceled
			}

			if err := ctx.Err(); !errors.Is(err, wantEntryErr) {
				t.Errorf("entry context error = %v, want %v", err, wantEntryErr)
			}

			if !middlewareRan {
				t.Fatal("middleware did not run")
			}

			if !errors.Is(stageContextErr, context.Canceled) {
				t.Errorf("stage context error = %v, want context.Canceled", stageContextErr)
			}

			if w.Code != tc.status {
				t.Errorf("status = %d, want %d", w.Code, tc.status)
			}
		})
	}
}

func TestClientGoneDuringRoundTripWithAnotherError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// The transport sees the cancellation and then fails with an error that
	// does not report it, as for a connection reset at the same time.
	entered := make(chan struct{})
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()

		return nil, io.ErrUnexpectedEOF
	})
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, "http://upstream.test"),
		}},
		Transport: transport,
	})

	status := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil).WithContext(ctx))
		status <- w.Code
	}()
	receive(t, entered)
	cancel()

	if got := receive(t, status); got != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", got)
	}
}

func TestTypedFailureSentinels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  edgekit.Phase
		err    error
		status int
	}{
		{
			name:   "no upstream from the selector",
			stage:  edgekit.PhaseUpstreamSelection,
			err:    fmt.Errorf("pool: %w", edgekit.ErrNoUpstreamAvailable),
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "invalid response from a response hook",
			stage:  edgekit.PhaseUpstreamResponse,
			err:    fmt.Errorf("signature: %w", edgekit.ErrInvalidUpstreamResponse),
			status: http.StatusBadGateway,
		},
		{
			name:   "no upstream from a request hook",
			stage:  edgekit.PhaseRequestReceived,
			err:    edgekit.ErrNoUpstreamAvailable,
			status: http.StatusInternalServerError,
		},
		{
			name:   "no upstream from the transport",
			stage:  edgekit.PhaseRoundTrip,
			err:    edgekit.ErrNoUpstreamAvailable,
			status: http.StatusBadGateway,
		},
		{
			name:   "invalid response from the selector",
			stage:  edgekit.PhaseUpstreamSelection,
			err:    edgekit.ErrInvalidUpstreamResponse,
			status: http.StatusInternalServerError,
		},
		{
			name:   "invalid response from an outbound hook",
			stage:  edgekit.PhaseBeforeUpstream,
			err:    edgekit.ErrInvalidUpstreamResponse,
			status: http.StatusInternalServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			completionHooks, completed := completions()
			g := failingStages(t, tc.stage, tc.err, nil, completionHooks)

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != tc.status {
				t.Errorf("status = %d, want %d", w.Code, tc.status)
			}

			c := receive(t, completed)
			if c.Failure == nil {
				t.Fatal("RequestCompleted() Failure = nil, want the original failure")
			}

			if c.Failure.Phase != tc.stage {
				t.Errorf("RequestCompleted() Failure.Phase = %v, want %v", c.Failure.Phase, tc.stage)
			}

			if !errors.Is(c.Failure.Err, tc.err) {
				t.Errorf("RequestCompleted() Failure.Err = %v, want an error that matches %v", c.Failure.Err, tc.err)
			}

			if c.Failure.Err == nil {
				t.Fatal("RequestCompleted() Failure.Err = nil, want the original error")
			}

			if got, want := c.Failure.Err.Error(), tc.err.Error(); got != want {
				t.Errorf("RequestCompleted() Failure.Err.Error() = %q, want %q", got, want)
			}
		})
	}
}

func TestRoundTripFactsAtCompletion(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	// The upstream closes the connection after it read the request, so the
	// transport wrote it and then fails without a response.
	reset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer reset.Close()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer ok.Close()

	for _, tc := range []struct {
		name      string
		upstream  string
		status    int
		connected bool
		written   bool
	}{
		{
			name:     "closed port",
			upstream: closed.URL,
			status:   http.StatusBadGateway,
		},
		{
			name:      "written then reset",
			upstream:  reset.URL,
			status:    http.StatusBadGateway,
			connected: true,
			written:   true,
		},
		{
			name:      "response",
			upstream:  ok.URL,
			status:    http.StatusOK,
			connected: true,
			written:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			completionHooks, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, tc.upstream),
				}},
				Transport: testTransport(t),
				Hooks:     []any{completionHooks},
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != tc.status {
				t.Errorf("status = %d, want %d", w.Code, tc.status)
			}

			c := receive(t, completed)
			if c.ConnectionObtained != tc.connected {
				t.Errorf("RequestCompleted() ConnectionObtained = %v, want %v", c.ConnectionObtained, tc.connected)
			}

			if c.RequestWritten != tc.written {
				t.Errorf("RequestCompleted() RequestWritten = %v, want %v", c.RequestWritten, tc.written)
			}
		})
	}
}

func TestRoundTripFactsOnHTTP2(t *testing.T) {
	upstream, transport := protocolUpstream(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			w.WriteHeader(http.StatusHTTPVersionNotSupported)
		}
	}))

	completionHooks, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		Transport: transport,
		Hooks:     []any{completionHooks},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader("body")))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 over HTTP/2", w.Code)
	}

	c := receive(t, completed)
	if !c.ConnectionObtained {
		t.Error("RequestCompleted() ConnectionObtained = false, want true")
	}

	if !c.RequestWritten {
		t.Error("RequestCompleted() RequestWritten = false, want true")
	}
}

func TestCompletionHooksShareOneFactSnapshot(t *testing.T) {
	// The transport keeps the trace and reports the write only after
	// completion started, as a late callback of another goroutine can.
	var trace *httptrace.ClientTrace
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		trace = httptrace.ContextClientTrace(r.Context())

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    r,
		}, nil
	})

	var got []edgekit.Completion
	record := edgekit.RequestCompletedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
		got = append(got, c)
	})

	// Completion hooks run from the last provider to the first, so late
	// runs first.
	late := edgekit.RequestCompletedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
		trace.GotConn(httptrace.GotConnInfo{})
		trace.WroteRequest(httptrace.WroteRequestInfo{})
		got = append(got, c)
	})
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, "http://upstream.test"),
		}},
		Transport: transport,
		Hooks:     []any{record, late},
	})

	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))

	if len(got) != 2 {
		t.Fatalf("RequestCompleted() snapshots = %d, want 2", len(got))
	}

	for i, c := range got {
		if c.ConnectionObtained {
			t.Errorf("RequestCompleted() snapshot %d ConnectionObtained = true, want false", i)
		}

		if c.RequestWritten {
			t.Errorf("RequestCompleted() snapshot %d RequestWritten = true, want false", i)
		}
	}
}

func TestResponseHookRequestDoesNotSetFacts(t *testing.T) {
	// The hook sends its own request with its context, such as to fetch a
	// key. The gateway transport does not report trace events, so only the
	// request of the hook could set the facts.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer other.Close()

	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    r,
		}, nil
	})

	client := &http.Client{
		Transport: testTransport(t),
	}
	fetch := func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, other.URL, nil)
		if err != nil {
			return err
		}

		resp, err := client.Do(req)
		if err != nil {
			return err
		}

		return resp.Body.Close()
	}

	var hookErr error
	hooks := &testHooks{
		response: func(ctx context.Context, _ *edgekit.RequestContext, _ *http.Response) (*edgekit.Rejection, error) {
			hookErr = fetch(ctx)

			return nil, nil
		},
	}
	completionHooks, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, "http://upstream.test"),
		}},
		Transport: transport,
		Hooks:     []any{completionHooks, hooks},
	})

	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))

	if hookErr != nil {
		t.Fatalf("hook request failed: %v", hookErr)
	}

	c := receive(t, completed)
	if c.ConnectionObtained {
		t.Error("RequestCompleted() ConnectionObtained = true, want false")
	}

	if c.RequestWritten {
		t.Error("RequestCompleted() RequestWritten = true, want false")
	}
}
