package accesslog_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/accesslog"
)

// traceKey is the context key of the test tracer.
type traceKey struct{}

// span is a span of the test tracer. The tracing middleware owns it and
// ends it when it returns.
type span struct {
	id    string
	ended atomic.Bool
}

func TestRecordJoinsHTTPGatewayAndTraceObservations(t *testing.T) {
	for _, proto := range protocols {
		t.Run(proto.name, func(t *testing.T) {
			upstreamIDs := make(chan []string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamIDs <- r.Header.Values(accesslog.RequestIDHeader)
				w.Header().Set(accesslog.RequestIDHeader, "upstream-supplied")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, "ok")
			}))
			t.Cleanup(upstream.Close)

			// The span lives in a derived context. Completion runs after the
			// tracing middleware ended it, with the entry context.
			var current atomic.Pointer[span]
			tracing := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					s := &span{
						id: "trace-from-middleware",
					}
					current.Store(s)
					defer s.ended.Store(true)

					next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), traceKey{}, s)))
				})
			}

			records := make(chan accesslog.Record, 1)
			recorder := newRecorder(t, accesslog.Config{
				Sink: accesslog.SinkFunc(func(r accesslog.Record) {
					if s := current.Load(); s == nil || !s.ended.Load() {
						t.Error("Report ran before the tracing middleware returned")
					}

					records <- r
				}),
				TraceID: func(ctx context.Context) string {
					s, ok := ctx.Value(traceKey{}).(*span)
					if !ok {
						return ""
					}

					if s.ended.Load() {
						t.Error("TraceID ran after the span ended")
					}

					return s.id
				},
			})

			hookIDs := make(chan string, 1)
			checkHook := edgekit.BeforeUpstreamHookFunc(func(ctx context.Context, rc *edgekit.RequestContext, _ *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
				id, _ := recorder.RequestID(rc)
				hookIDs <- id

				if ctx.Value(traceKey{}) == nil {
					t.Error("BeforeUpstream context has no trace")
				}

				return nil, nil
			})
			checkCompletion := edgekit.RequestCompletedHookFunc(func(ctx context.Context, _ *edgekit.RequestContext, _ edgekit.Completion) {
				if ctx.Value(traceKey{}) != nil {
					t.Error("completion got the derived trace context")
				}
			})

			g := newGateway(t, edgekit.Config{
				Middleware: []edgekit.Middleware{
					tracing,
					recorder.Middleware,
				},
				Hooks: []any{
					recorder,
					checkHook,
					checkCompletion,
				},
				Routes: []edgekit.Route{
					apiRoute(t, upstream.URL),
				},
			})
			ts := startServer(t, g)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url("/api/data%20set?token=query-secret"), strings.NewReader("body-secret"))
			if err != nil {
				t.Fatalf("make request: %v", err)
			}

			req.Header.Set(accesslog.RequestIDHeader, "client-id-secret")
			req.Header.Set("Authorization", "Bearer authorization-secret")

			resp, err := ts.client(t, proto.http2).Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}

			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			record, err := receive(t.Context(), records)
			if err != nil {
				t.Fatalf("wait for completion record: %v", err)
			}

			wantMajor := 1
			if proto.http2 {
				wantMajor = 2
			}

			if resp.StatusCode != http.StatusCreated || string(body) != "ok" || resp.ProtoMajor != wantMajor {
				t.Fatalf("response = %d %q over HTTP/%d, want 201 \"ok\" over HTTP/%d", resp.StatusCode, body, resp.ProtoMajor, wantMajor)
			}

			if !validRequestID(record.RequestID) {
				t.Fatalf("RequestID = %q, want 32 lowercase hexadecimal digits", record.RequestID)
			}

			gotIDs, err := receive(t.Context(), upstreamIDs)
			if err != nil {
				t.Fatalf("wait for upstream request IDs: %v", err)
			}

			if !slices.Equal(gotIDs, []string{record.RequestID}) {
				t.Errorf("upstream request IDs = %q, want only %q", gotIDs, record.RequestID)
			}

			if got := resp.Header.Values(accesslog.RequestIDHeader); !slices.Equal(got, []string{record.RequestID}) {
				t.Errorf("response request IDs = %q, want only %q", got, record.RequestID)
			}

			hookID, err := receive(t.Context(), hookIDs)
			if err != nil {
				t.Fatalf("wait for hook request ID: %v", err)
			}

			if hookID != record.RequestID {
				t.Errorf("RequestID in hook = %q, want %q", hookID, record.RequestID)
			}

			if !record.Observed || record.TraceID != "trace-from-middleware" || record.Method != http.MethodPost || record.Path != "/api/data%20set" {
				t.Errorf("request observations = %+v, want the method, the escaped path, and the trace ID", record)
			}

			if record.Status != http.StatusCreated || record.BytesWritten != 2 || record.UpstreamStatus != http.StatusCreated || record.WriteErr != nil || record.Hijacked {
				t.Errorf("HTTP observations = %+v, want 201, 2 bytes, upstream 201, no write error", record)
			}

			c := record.Completion
			if c.RoutePrefix != "/api/" || c.SelectedUpstream == nil || c.SelectedUpstream.String() != upstream.URL || c.Duration <= 0 {
				t.Errorf("Completion = %+v, want the route, the upstream, and a duration", c)
			}

			if c.Failure != nil || c.RejectionReason != "" || c.Aborted || !c.ConnectionObtained || !c.RequestWritten {
				t.Errorf("Completion = %+v, want a written request without failure", c)
			}

			if text := fmt.Sprintf("%+v", record); strings.Contains(text, "secret") {
				t.Errorf("record contains request content: %s", text)
			}
		})
	}
}

func TestRecordsGatewayResponsesAndFailures(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "unavailable")
	}))
	t.Cleanup(upstream.Close)

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	denied := &edgekit.Rejection{
		Status: http.StatusForbidden,
		Reason: "test.denied",
	}
	invalid := &edgekit.Rejection{
		Status: http.StatusOK,
		Reason: "test.denied",
	}
	internal := errors.New("private failure details")

	type hookResult struct {
		rejection *edgekit.Rejection
		err       error
	}

	for _, tc := range []struct {
		name           string
		path           string
		upstream       string
		local          bool
		routeDeny      bool
		received       *hookResult
		response       *hookResult
		wantStatus     int
		wantUpstream   int
		wantRoute      bool
		wantSelected   bool
		wantReason     edgekit.RejectionReason
		wantPhase      edgekit.Phase
		wantFailure    bool
		wantCause      error
		wantConnection bool
	}{
		{
			name:       "global middleware response",
			local:      true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "route middleware response",
			routeDeny:  true,
			wantStatus: http.StatusForbidden,
			wantRoute:  true,
		},
		{
			name: "request hook rejection",
			received: &hookResult{
				rejection: denied,
			},
			wantStatus: http.StatusForbidden,
			wantReason: "test.denied",
		},
		{
			name: "request hook failure",
			received: &hookResult{
				err: internal,
			},
			wantStatus:  http.StatusInternalServerError,
			wantPhase:   edgekit.PhaseRequestReceived,
			wantFailure: true,
			wantCause:   internal,
		},
		{
			name: "invalid rejection",
			received: &hookResult{
				rejection: invalid,
			},
			wantStatus:  http.StatusInternalServerError,
			wantPhase:   edgekit.PhaseRequestReceived,
			wantFailure: true,
			wantCause:   edgekit.ErrInvalidRejection,
		},
		{
			name:       "routing miss",
			path:       "/missing",
			wantStatus: http.StatusNotFound,
			wantReason: edgekit.ReasonNoRoute,
		},
		{
			name:           "upstream error status",
			wantStatus:     http.StatusServiceUnavailable,
			wantUpstream:   http.StatusServiceUnavailable,
			wantRoute:      true,
			wantSelected:   true,
			wantConnection: true,
		},
		{
			name:         "round-trip failure",
			upstream:     closed.URL,
			wantStatus:   http.StatusBadGateway,
			wantRoute:    true,
			wantSelected: true,
			wantPhase:    edgekit.PhaseRoundTrip,
			wantFailure:  true,
		},
		{
			// The recorder is the first provider, so its response hook runs
			// after the rejecting hook and does not run (ADR-0021).
			name: "response hook rejection",
			response: &hookResult{
				rejection: denied,
			},
			wantStatus:     http.StatusForbidden,
			wantRoute:      true,
			wantSelected:   true,
			wantReason:     "test.denied",
			wantConnection: true,
		},
		{
			name: "response hook failure",
			response: &hookResult{
				err: internal,
			},
			wantStatus:     http.StatusInternalServerError,
			wantRoute:      true,
			wantSelected:   true,
			wantPhase:      edgekit.PhaseUpstreamResponse,
			wantFailure:    true,
			wantCause:      internal,
			wantConnection: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := newCollector(t, 1)
			recorder := newRecorder(t, accesslog.Config{
				Sink: sink,
			})

			middleware := []edgekit.Middleware{
				recorder.Middleware,
			}
			if tc.local {
				middleware = append(middleware, func(http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						http.Error(w, "local response", http.StatusUnauthorized)
					})
				})
			}

			hooks := []any{
				recorder,
			}
			if tc.received != nil {
				hooks = append(hooks, edgekit.RequestReceivedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
					return tc.received.rejection, tc.received.err
				}))
			}

			if tc.response != nil {
				hooks = append(hooks, edgekit.UpstreamResponseHookFunc(func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
					return tc.response.rejection, tc.response.err
				}))
			}

			upstreamURL := upstream.URL
			if tc.upstream != "" {
				upstreamURL = tc.upstream
			}

			route := apiRoute(t, upstreamURL)
			if tc.routeDeny {
				route.Middleware = []edgekit.Middleware{
					func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							http.Error(w, "denied", http.StatusForbidden)
						})
					},
				}
			}

			g := newGateway(t, edgekit.Config{
				Middleware: middleware,
				Hooks:      hooks,
				Routes: []edgekit.Route{
					route,
				},
			})

			path := "/api/data"
			if tc.path != "" {
				path = tc.path
			}

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))

			got := sink.all()
			if len(got) != 1 {
				t.Fatalf("records = %d, want 1", len(got))
			}

			record := got[0]
			c := record.Completion
			if !record.Observed {
				t.Error("Observed = false, want true")
			}

			if record.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", record.Status, tc.wantStatus)
			}

			if record.Status != w.Code {
				t.Errorf("Status = %d, want response status %d", record.Status, w.Code)
			}

			if record.BytesWritten != int64(w.Body.Len()) {
				t.Errorf("BytesWritten = %d, want response body length %d", record.BytesWritten, w.Body.Len())
			}

			if record.UpstreamStatus != tc.wantUpstream {
				t.Errorf("UpstreamStatus = %d, want %d", record.UpstreamStatus, tc.wantUpstream)
			}

			if record.WriteErr != nil {
				t.Errorf("WriteErr = %v, want nil", record.WriteErr)
			}

			if record.Hijacked {
				t.Error("Hijacked = true, want false")
			}

			if got := c.RoutePrefix != ""; got != tc.wantRoute {
				t.Errorf("route matched = %t, want %t; RoutePrefix = %q", got, tc.wantRoute, c.RoutePrefix)
			}

			if got := c.SelectedUpstream != nil; got != tc.wantSelected {
				t.Errorf("upstream selected = %t, want %t", got, tc.wantSelected)
			}

			if c.RejectionReason != tc.wantReason {
				t.Errorf("RejectionReason = %q, want %q", c.RejectionReason, tc.wantReason)
			}

			if got := c.Failure != nil; got != tc.wantFailure {
				t.Errorf("failure present = %t, want %t", got, tc.wantFailure)
			}

			if c.Aborted {
				t.Error("Aborted = true, want false")
			}

			if c.ConnectionObtained != tc.wantConnection {
				t.Errorf("ConnectionObtained = %t, want %t", c.ConnectionObtained, tc.wantConnection)
			}

			if c.Failure != nil && c.Failure.Phase != tc.wantPhase {
				t.Errorf("Failure.Phase = %v, want %v", c.Failure.Phase, tc.wantPhase)
			}

			if tc.wantCause != nil && (c.Failure == nil || !errors.Is(c.Failure.Err, tc.wantCause)) {
				t.Errorf("Failure = %+v, want the cause %v", c.Failure, tc.wantCause)
			}

			if got := w.Header().Values(accesslog.RequestIDHeader); !slices.Equal(got, []string{record.RequestID}) {
				t.Errorf("response request IDs = %q, want only %q", got, record.RequestID)
			}
		})
	}
}

func TestRecordsCoreProtectionRejections(t *testing.T) {
	t.Run("body too large", func(t *testing.T) {
		sink := newCollector(t, 1)
		recorder := newRecorder(t, accesslog.Config{
			Sink: sink,
		})

		route := apiRoute(t, "http://upstream.test")
		route.BodyLimit = edgekit.MaxBodyBytes(4)
		g := newGateway(t, edgekit.Config{
			Middleware: []edgekit.Middleware{
				recorder.Middleware,
			},
			Hooks: []any{
				recorder,
			},
			Routes: []edgekit.Route{
				route,
			},
		})

		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/upload", strings.NewReader("too large")))

		record, err := receive(t.Context(), sink.signal)
		if err != nil {
			t.Fatalf("wait for body-limit rejection record: %v", err)
		}

		if w.Code != http.StatusRequestEntityTooLarge || record.Status != w.Code || record.Completion.RejectionReason != edgekit.ReasonBodyTooLarge {
			t.Errorf("response %d, record status %d, reason %q; want 413 and body_too_large", w.Code, record.Status, record.Completion.RejectionReason)
		}

		if w.Header().Get(accesslog.RequestIDHeader) != record.RequestID || record.Completion.Failure != nil {
			t.Errorf("record = %+v, want the request ID in the response and no failure", record)
		}
	})

	t.Run("admission saturated", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}

			w.WriteHeader(http.StatusNoContent)
		}))
		// One cleanup releases the handler before the close, because Close
		// waits for active handlers and cleanups run in reverse order.
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(release) })
			upstream.Close()
		})

		budget, err := edgekit.NewAdmissionBudget(1)
		if err != nil {
			t.Fatalf("NewAdmissionBudget: %v", err)
		}

		sink := newCollector(t, 2)
		recorder := newRecorder(t, accesslog.Config{
			Sink: sink,
		})
		g := newGateway(t, edgekit.Config{
			Middleware: []edgekit.Middleware{
				recorder.Middleware,
			},
			Hooks: []any{
				recorder,
			},
			Admission: budget,
			Routes: []edgekit.Route{
				apiRoute(t, upstream.URL),
			},
		})

		first := make(chan int, 1)
		go func() {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/slow", nil))
			first <- w.Code
		}()
		if _, err = receive(t.Context(), entered); err != nil {
			t.Fatalf("wait for admitted request to reach the upstream: %v", err)
		}

		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fast", nil))

		saturated, err := receive(t.Context(), sink.signal)
		if err != nil {
			t.Fatalf("wait for admission rejection record: %v", err)
		}

		releaseOnce.Do(func() { close(release) })
		code, err := receive(t.Context(), first)
		if err != nil {
			t.Fatalf("wait for admitted request to return: %v", err)
		}

		if code != http.StatusNoContent {
			t.Errorf("first request status = %d, want 204", code)
		}

		admitted, err := receive(t.Context(), sink.signal)
		if err != nil {
			t.Fatalf("wait for admitted request record: %v", err)
		}
		if w.Code != http.StatusServiceUnavailable || saturated.Status != w.Code || saturated.Completion.RejectionReason != edgekit.ReasonAdmissionSaturated {
			t.Errorf("response %d, record %+v; want 503 and admission_saturated", w.Code, saturated)
		}

		if saturated.Completion.RoutePrefix != "" || saturated.Path != "/api/fast" {
			t.Errorf("record = %+v, want no route, because gateway admission is before routing", saturated)
		}

		if admitted.Status != http.StatusNoContent || admitted.UpstreamStatus != http.StatusNoContent {
			t.Errorf("admitted record = %+v, want 204", admitted)
		}
	})
}

func TestPanicDoesNotRecordSuccessfulStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		committed  bool
		wantStatus int
		wantBytes  int64
	}{
		{
			name:       "before a write",
			committed:  false,
			wantStatus: 0,
			wantBytes:  0,
		},
		{
			name:       "after a write",
			committed:  true,
			wantStatus: http.StatusAccepted,
			wantBytes:  int64(len("partial")),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := newCollector(t, 1)
			recorder := newRecorder(t, accesslog.Config{
				Sink: sink,
			})
			panicking := func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if tc.committed {
						w.WriteHeader(http.StatusAccepted)
						_, _ = io.WriteString(w, "partial")
					}

					panic(http.ErrAbortHandler)
				})
			}
			g := newGateway(t, edgekit.Config{
				Middleware: []edgekit.Middleware{
					recorder.Middleware,
					panicking,
				},
				Hooks: []any{
					recorder,
				},
				Routes: []edgekit.Route{
					apiRoute(t, "http://upstream.test"),
				},
			})

			recovered := func() (recovered any) {
				defer func() { recovered = recover() }()

				g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/data", nil))

				return nil
			}()
			if err, ok := recovered.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
				t.Fatalf("panic = %v, want http.ErrAbortHandler", recovered)
			}

			record, err := receive(t.Context(), sink.signal)
			if err != nil {
				t.Fatalf("wait for aborted request record: %v", err)
			}

			if !record.Completion.Aborted || record.Status != tc.wantStatus || record.BytesWritten != tc.wantBytes {
				t.Errorf("record = %+v, want aborted with status %d and %d bytes", record, tc.wantStatus, tc.wantBytes)
			}
		})
	}
}

func TestStreamingReadFailureKeepsPartialObservations(t *testing.T) {
	for _, proto := range protocols {
		t.Run(proto.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, "short")
			}))
			t.Cleanup(upstream.Close)

			sink := newCollector(t, 1)
			recorder := newRecorder(t, accesslog.Config{
				Sink: sink,
			})
			g := newGateway(t, edgekit.Config{
				Middleware: []edgekit.Middleware{
					recorder.Middleware,
				},
				Hooks: []any{
					recorder,
				},
				Routes: []edgekit.Route{
					apiRoute(t, upstream.URL),
				},
			})
			ts := startServer(t, g)

			// The abort can come before the buffered headers reach the
			// client, so the client error can come from Get or from the read.
			resp, err := ts.client(t, proto.http2).Get(ts.url("/api/data"))
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}

			if err == nil {
				t.Error("client read the complete body, want an aborted response")
			}

			// The writer accepted the status and the bytes, although the
			// client did not get them.
			record, err := receive(t.Context(), sink.signal)
			if err != nil {
				t.Fatalf("wait for response-body failure record: %v", err)
			}

			if record.Status != http.StatusOK || record.BytesWritten != int64(len("short")) || record.UpstreamStatus != http.StatusOK {
				t.Errorf("record = %+v, want status 200, 5 bytes, and upstream 200", record)
			}

			c := record.Completion
			if !c.Aborted || c.Failure == nil || c.Failure.Phase != edgekit.PhaseResponseBody || !errors.Is(c.Failure.Err, io.ErrUnexpectedEOF) {
				t.Errorf("Completion = %+v, want an abort with a failure in PhaseResponseBody", c)
			}

			if record.WriteErr != nil {
				t.Errorf("WriteErr = %v, want nil: the read failure is not a write error", record.WriteErr)
			}
		})
	}
}

// failingWriter accepts 2 bytes of each write and then fails.
type failingWriter struct {
	header http.Header
}

// Header returns the header map.
func (w *failingWriter) Header() http.Header {
	return w.header
}

// WriteHeader does nothing.
func (w *failingWriter) WriteHeader(int) {}

// Write accepts 2 bytes and fails.
func (w *failingWriter) Write(b []byte) (int, error) {
	return min(len(b), 2), io.ErrClosedPipe
}

func TestWriteErrorIsSeparateFromGatewayFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello world")
	}))
	t.Cleanup(upstream.Close)

	for _, tc := range []struct {
		name         string
		local        bool
		wantUpstream int
	}{
		{
			name:         "local response",
			local:        true,
			wantUpstream: 0,
		},
		{
			name:         "proxied response",
			local:        false,
			wantUpstream: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := newCollector(t, 1)
			recorder := newRecorder(t, accesslog.Config{
				Sink: sink,
			})

			middleware := []edgekit.Middleware{
				recorder.Middleware,
			}
			if tc.local {
				middleware = append(middleware, func(http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						_, _ = io.WriteString(w, "hello")
					})
				})
			}

			g := newGateway(t, edgekit.Config{
				Middleware: middleware,
				Hooks: []any{
					recorder,
				},
				Routes: []edgekit.Route{
					apiRoute(t, upstream.URL),
				},
			})

			g.ServeHTTP(&failingWriter{header: make(http.Header)}, httptest.NewRequest(http.MethodGet, "/api/data", nil))

			record, err := receive(t.Context(), sink.signal)
			if err != nil {
				t.Fatalf("wait for response-write failure record: %v", err)
			}

			if record.Status != http.StatusOK || record.BytesWritten != 2 || !errors.Is(record.WriteErr, io.ErrClosedPipe) {
				t.Errorf("record = %+v, want status 200, 2 bytes, and io.ErrClosedPipe", record)
			}

			if record.UpstreamStatus != tc.wantUpstream || record.Completion.Failure != nil {
				t.Errorf("record = %+v, want upstream %d and no gateway failure", record, tc.wantUpstream)
			}
		})
	}
}

func TestStreamingResponseFlushesBeforeCompletion(t *testing.T) {
	for _, proto := range protocols {
		t.Run(proto.name, func(t *testing.T) {
			release := make(chan struct{})
			var releaseOnce sync.Once
			finish := func() { releaseOnce.Do(func() { close(release) }) }

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
			// One cleanup releases the handler before the close, because
			// Close waits for active handlers and cleanups run in reverse
			// order.
			t.Cleanup(func() {
				finish()
				upstream.Close()
			})

			sink := newCollector(t, 1)
			recorder := newRecorder(t, accesslog.Config{
				Sink: sink,
			})
			g := newGateway(t, edgekit.Config{
				Middleware: []edgekit.Middleware{
					recorder.Middleware,
				},
				Hooks: []any{
					recorder,
				},
				Routes: []edgekit.Route{
					apiRoute(t, upstream.URL),
				},
			})
			ts := startServer(t, g)

			resp, err := ts.client(t, proto.http2).Get(ts.url("/api/events"))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			reader := bufio.NewReader(resp.Body)
			line, err := reader.ReadString('\n')
			if err != nil || line != "data: first\n" {
				t.Fatalf("first line = %q, %v; want the flushed event before the upstream ends", line, err)
			}

			// The upstream waits for release, so completion cannot have run.
			select {
			case record := <-sink.signal:
				t.Fatalf("record %+v before the stream ended", record)
			default:
			}

			finish()
			if _, err = io.ReadAll(reader); err != nil {
				t.Fatalf("read rest: %v", err)
			}

			record, err := receive(t.Context(), sink.signal)
			if err != nil {
				t.Fatalf("wait for completed stream record: %v", err)
			}

			if record.Status != http.StatusOK || record.BytesWritten != int64(len("data: first\n\ndata: last\n\n")) || record.WriteErr != nil || record.Completion.Failure != nil {
				t.Errorf("record = %+v, want 200 with all bytes", record)
			}
		})
	}
}

func TestProtocolUpgradeKeepsHijackingAndServerManagement(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)

		conn, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("upstream hijack: %v", err)

			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = io.WriteString(buffered, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\nX-Request-Id: upstream-id\r\n\r\n")
		if err := buffered.Flush(); err != nil {
			return
		}

		_, _ = io.Copy(conn, buffered)
	}))
	t.Cleanup(upstream.Close)

	sink := newCollector(t, 1)
	recorder := newRecorder(t, accesslog.Config{
		Sink: sink,
	})
	g := newGateway(t, edgekit.Config{
		Middleware: []edgekit.Middleware{
			recorder.Middleware,
		},
		Hooks: []any{
			recorder,
		},
		Routes: []edgekit.Route{
			apiRoute(t, upstream.URL),
		},
	})
	ts := startServer(t, g)

	conn := ts.dial(t)
	if _, err := io.WriteString(conn, "GET /api/socket HTTP/1.1\r\nHost: gateway.test\r\nConnection: Upgrade\r\nUpgrade: echo\r\nX-Request-Id: client-id\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	reader := bufio.NewReader(conn)
	hints, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read 103: %v", err)
	}
	_ = hints.Body.Close()

	if hints.StatusCode != http.StatusEarlyHints || hints.Header.Get("Link") == "" || hints.Header.Get(accesslog.RequestIDHeader) != "" {
		t.Fatalf("103 response = %d %v, want Link and no request ID", hints.StatusCode, hints.Header)
	}

	switched, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read 101: %v", err)
	}
	_ = switched.Body.Close()

	ids := switched.Header.Values(accesslog.RequestIDHeader)
	if switched.StatusCode != http.StatusSwitchingProtocols || len(ids) != 1 || !validRequestID(ids[0]) {
		t.Fatalf("101 response = %d with request IDs %q, want one generated ID", switched.StatusCode, ids)
	}

	if _, err = io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	message := make([]byte, 4)
	if _, err = io.ReadFull(reader, message); err != nil || string(message) != "ping" {
		t.Fatalf("echo = %q, %v; want ping", message, err)
	}

	// The server still manages the upgraded connection of the gateway: it
	// closes it at shutdown.
	if err = ts.shutdown(t); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// A close by the server ends the read with EOF, or with a reset when data
	// is still in transit.
	if _, err = io.ReadAll(reader); err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("read after shutdown: %v, want the server to close the connection", err)
	}

	record, err := receive(t.Context(), sink.signal)
	if err != nil {
		t.Fatalf("wait for protocol-switch record: %v", err)
	}

	if !record.Hijacked || record.Status != 0 || record.BytesWritten != 0 || record.UpstreamStatus != http.StatusSwitchingProtocols {
		t.Errorf("record = %+v, want a hijack, no status, no bytes, and upstream 101", record)
	}

	if record.RequestID != ids[0] {
		t.Errorf("RequestID = %q, want the ID of the 101 response %q", record.RequestID, ids[0])
	}
}

func TestConcurrentRecordsKeepRequestAndTraceIDsIsolated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Saw", r.Header.Get(accesslog.RequestIDHeader))
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	tracing := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), traceKey{}, "trace:"+r.URL.Path)))
		})
	}

	const requests = 40
	sink := newCollector(t, requests)
	recorder := newRecorder(t, accesslog.Config{
		Sink: sink,
		TraceID: func(ctx context.Context) string {
			id, ok := ctx.Value(traceKey{}).(string)
			if !ok {
				return ""
			}

			return id
		},
	})
	g := newGateway(t, edgekit.Config{
		Middleware: []edgekit.Middleware{
			tracing,
			recorder.Middleware,
		},
		Hooks: []any{
			recorder,
		},
		Routes: []edgekit.Route{
			{
				Prefix:   "/a/",
				Upstream: static(t, upstream.URL),
			},
			{
				Prefix:   "/b/",
				Upstream: static(t, upstream.URL),
			},
		},
	})

	type response struct {
		path   string
		header string
		saw    string
	}

	responses := make(chan response, requests)
	var wg sync.WaitGroup
	for i := range requests {
		path := fmt.Sprintf("/%c/%d", "ab"[i%2], i)
		wg.Go(func() {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			responses <- response{
				path:   path,
				header: w.Header().Get(accesslog.RequestIDHeader),
				saw:    w.Header().Get("X-Upstream-Saw"),
			}
		})
	}
	wg.Wait()
	close(responses)

	byID := make(map[string]accesslog.Record)
	for _, record := range sink.all() {
		if !validRequestID(record.RequestID) {
			t.Fatalf("request ID %q is not valid", record.RequestID)
		}

		if _, ok := byID[record.RequestID]; ok {
			t.Fatalf("request ID %q is not unique", record.RequestID)
		}

		byID[record.RequestID] = record
	}

	if len(byID) != requests {
		t.Fatalf("records = %d, want %d", len(byID), requests)
	}

	for resp := range responses {
		record, ok := byID[resp.header]
		if !ok || resp.saw != resp.header {
			t.Errorf("response %+v: the upstream and the client did not get the ID of a record", resp)

			continue
		}

		if record.Path != resp.path || record.TraceID != "trace:"+resp.path || record.Completion.RoutePrefix != resp.path[:3] {
			t.Errorf("record %+v does not belong to request %q", record, resp.path)
		}

		if record.Status != http.StatusOK || record.BytesWritten != 2 {
			t.Errorf("record %+v, want 200 and 2 bytes", record)
		}
	}
}

func TestMissingMiddlewareGivesDegradedRecord(t *testing.T) {
	upstreamIDs := make(chan []string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamIDs <- r.Header.Values(accesslog.RequestIDHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	t.Run("secure-default rejection", func(t *testing.T) {
		sink := newCollector(t, 1)
		recorder := newRecorder(t, accesslog.Config{
			Sink: sink,
		})
		g := newGateway(t, edgekit.Config{
			Middleware: []edgekit.Middleware{
				recorder.Middleware,
			},
			Hooks: []any{
				recorder,
			},
			Routes: []edgekit.Route{
				apiRoute(t, upstream.URL),
			},
		})

		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/%2e%2e/secret", nil))

		record, err := receive(t.Context(), sink.signal)
		if err != nil {
			t.Fatalf("wait for secure-default rejection record: %v", err)
		}

		if w.Code != http.StatusBadRequest || record.Observed || record.Completion.RejectionReason != edgekit.ReasonInvalidPath {
			t.Errorf("response %d, record %+v; want 400 and a degraded record with invalid_path", w.Code, record)
		}

		if record.RequestID != "" || record.Status != 0 || record.Path != "" || w.Header().Get(accesslog.RequestIDHeader) != "" {
			t.Errorf("record = %+v, want no middleware observations and no request ID", record)
		}
	})

	t.Run("middleware not registered", func(t *testing.T) {
		sink := newCollector(t, 1)
		recorder := newRecorder(t, accesslog.Config{
			Sink: sink,
		})
		g := newGateway(t, edgekit.Config{
			Hooks: []any{
				recorder,
			},
			Routes: []edgekit.Route{
				apiRoute(t, upstream.URL),
			},
		})

		r := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		r.Header.Set(accesslog.RequestIDHeader, "client-id")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)

		record, err := receive(t.Context(), sink.signal)
		if err != nil {
			t.Fatalf("wait for completion record without middleware: %v", err)
		}

		gotIDs, err := receive(t.Context(), upstreamIDs)
		if err != nil {
			t.Fatalf("wait for upstream request IDs without middleware: %v", err)
		}

		if len(gotIDs) != 0 {
			t.Errorf("upstream request IDs = %q, want none: the client value is a managed header", gotIDs)
		}

		if w.Code != http.StatusNoContent || record.Observed || record.RequestID != "" || record.Status != 0 || record.UpstreamStatus != 0 {
			t.Errorf("response %d, record %+v; want 204 and a degraded record", w.Code, record)
		}

		c := record.Completion
		if c.RoutePrefix != "/api/" || c.SelectedUpstream == nil || c.Failure != nil || !c.RequestWritten {
			t.Errorf("Completion = %+v, want the execution facts of the request", c)
		}
	})
}

func TestStandaloneMiddlewarePassesThrough(t *testing.T) {
	sink := newCollector(t, 1)
	recorder := newRecorder(t, accesslog.Config{
		Sink: sink,
	})

	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "value")
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	called := false
	handler := recorder.Middleware(http.HandlerFunc(func(gotW http.ResponseWriter, gotR *http.Request) {
		called = true
		if gotR != r || gotR.Context() != ctx {
			t.Error("middleware changed the request or its context outside a gateway")
		}

		if gotW != w {
			t.Error("middleware wrapped the response writer outside a gateway")
		}

		gotW.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(w, r)

	if !called || w.Code != http.StatusNoContent || w.Header().Get(accesslog.RequestIDHeader) != "" {
		t.Errorf("called %t, response %d %v; want the next handler without a request ID", called, w.Code, w.Header())
	}

	if got := sink.all(); len(got) != 0 {
		t.Errorf("records = %+v, want none outside a gateway", got)
	}
}

func TestSinkPanicIsDropped(t *testing.T) {
	// The recorder is the last provider, so its completion hook runs first,
	// and a panic that escaped it would skip the other completion hook.
	type abort struct{}

	for _, tc := range []struct {
		name      string
		panicking bool
	}{
		{
			name:      "normal return",
			panicking: false,
		},
		{
			name:      "during an abort",
			panicking: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := newRecorder(t, accesslog.Config{
				Sink: accesslog.SinkFunc(func(accesslog.Record) {
					panic("sink failure")
				}),
			})

			otherRan := false
			middleware := []edgekit.Middleware{
				recorder.Middleware,
			}
			if tc.panicking {
				middleware = append(middleware, func(http.Handler) http.Handler {
					return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
						panic(abort{})
					})
				})
			}

			g := newGateway(t, edgekit.Config{
				Middleware: middleware,
				Hooks: []any{
					edgekit.RequestCompletedHookFunc(func(context.Context, *edgekit.RequestContext, edgekit.Completion) {
						otherRan = true
					}),
					recorder,
				},
				Routes: []edgekit.Route{
					apiRoute(t, "http://upstream.test"),
				},
			})

			recovered := func() (recovered any) {
				defer func() { recovered = recover() }()

				g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))

				return nil
			}()

			var want any
			if tc.panicking {
				want = abort{}
			}

			if recovered != want {
				t.Errorf("panic = %v, want %v: the sink panic must not escape or replace the original panic", recovered, want)
			}

			if !otherRan {
				t.Error("the other completion hook did not run")
			}
		})
	}
}

func TestNewValidatesAndCopiesConfig(t *testing.T) {
	if _, err := accesslog.New(accesslog.Config{}); err == nil {
		t.Error("New without a sink: error = nil, want an error")
	}

	first := newCollector(t, 1)
	second := newCollector(t, 1)
	cfg := accesslog.Config{
		Sink: first,
	}
	recorder := newRecorder(t, cfg)
	cfg.Sink = second

	g := newGateway(t, edgekit.Config{
		Hooks: []any{
			recorder,
		},
		Routes: []edgekit.Route{
			apiRoute(t, "http://upstream.test"),
		},
	})
	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))

	if len(first.all()) != 1 || len(second.all()) != 0 {
		t.Errorf("records: first sink %d, second sink %d; want 1 and 0", len(first.all()), len(second.all()))
	}
}

func TestRecordLogValueOmitsSensitiveFields(t *testing.T) {
	record := accesslog.Record{
		Observed:       true,
		RequestID:      "0123456789abcdef0123456789abcdef",
		Method:         "METHOD-SECRET",
		Path:           "/path-secret",
		TraceID:        "trace-secret",
		Status:         http.StatusBadGateway,
		BytesWritten:   7,
		WriteErr:       errors.New("write-secret"),
		Hijacked:       false,
		UpstreamStatus: http.StatusOK,
		Completion: edgekit.Completion{
			Start:       time.Unix(0, 0),
			Duration:    time.Millisecond,
			RoutePrefix: "/route-secret/",
			SelectedUpstream: &url.URL{
				Scheme:   "https",
				User:     url.UserPassword("user", "password-secret"),
				Host:     "origin-secret",
				RawQuery: "token=query-secret",
			},
			Failure: &edgekit.Failure{
				Phase: edgekit.PhaseResponseBody,
				Err:   errors.New("failure-secret"),
			},
			RequestWritten: true,
		},
	}

	var output bytes.Buffer
	slog.New(slog.NewJSONHandler(&output, nil)).Info("request", slog.Any("record", record))

	if strings.Contains(strings.ToLower(output.String()), "secret") {
		t.Fatalf("log contains a sensitive field: %s", output.String())
	}

	var logged struct {
		Record map[string]any `json:"record"`
	}
	if err := json.Unmarshal(output.Bytes(), &logged); err != nil {
		t.Fatalf("decode log: %v", err)
	}

	want := []string{
		"aborted",
		"bytes",
		"connection_obtained",
		"duration",
		"failure_phase",
		"hijacked",
		"observed",
		"rejection_reason",
		"request_id",
		"request_written",
		"route_matched",
		"start",
		"status",
		"upstream_selected",
		"upstream_status",
		"write_failed",
	}
	if keys := slices.Sorted(maps.Keys(logged.Record)); !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}

	for key, value := range map[string]any{
		"request_id":        record.RequestID,
		"status":            float64(http.StatusBadGateway),
		"failure_phase":     "response_body",
		"write_failed":      true,
		"route_matched":     true,
		"upstream_selected": true,
		"request_written":   true,
	} {
		if logged.Record[key] != value {
			t.Errorf("%s = %v, want %v", key, logged.Record[key], value)
		}
	}
}
