package accesslog_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/accesslog"
)

// plainWriter is a response writer without optional interfaces.
type plainWriter struct {
	header   http.Header
	statuses []int
	body     bytes.Buffer
}

// newPlainWriter returns a plainWriter with an empty header map.
func newPlainWriter() *plainWriter {
	return &plainWriter{
		header: make(http.Header),
	}
}

// Header returns the header map.
func (w *plainWriter) Header() http.Header {
	return w.header
}

// WriteHeader records status.
func (w *plainWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}

// Write appends b to the body.
func (w *plainWriter) Write(b []byte) (int, error) {
	return w.body.Write(b)
}

// capabilities counts the calls of the optional interfaces.
type capabilities struct {
	flushes int
	hijacks int
	pushes  int
}

// Flush counts the call.
func (c *capabilities) Flush() {
	c.flushes++
}

// Hijack counts the call and returns no connection.
func (c *capabilities) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c.hijacks++

	return nil, nil, nil
}

// Push counts the call.
func (c *capabilities) Push(string, *http.PushOptions) error {
	c.pushes++

	return nil
}

// writerCaps selects the optional interfaces of a test writer.
type writerCaps struct {
	flush  bool
	hijack bool
	push   bool
}

// writerWith returns base with the optional interfaces of caps. c implements
// them.
func writerWith(base *plainWriter, c *capabilities, caps writerCaps) http.ResponseWriter {
	flush, hijack, push := caps.flush, caps.hijack, caps.push

	switch {
	case flush && hijack && push:
		return struct {
			*plainWriter
			http.Flusher
			http.Hijacker
			http.Pusher
		}{
			plainWriter: base,
			Flusher:     c,
			Hijacker:    c,
			Pusher:      c,
		}
	case flush && hijack:
		return struct {
			*plainWriter
			http.Flusher
			http.Hijacker
		}{
			plainWriter: base,
			Flusher:     c,
			Hijacker:    c,
		}
	case flush && push:
		return struct {
			*plainWriter
			http.Flusher
			http.Pusher
		}{
			plainWriter: base,
			Flusher:     c,
			Pusher:      c,
		}
	case hijack && push:
		return struct {
			*plainWriter
			http.Hijacker
			http.Pusher
		}{
			plainWriter: base,
			Hijacker:    c,
			Pusher:      c,
		}
	case flush:
		return struct {
			*plainWriter
			http.Flusher
		}{
			plainWriter: base,
			Flusher:     c,
		}
	case hijack:
		return struct {
			*plainWriter
			http.Hijacker
		}{
			plainWriter: base,
			Hijacker:    c,
		}
	case push:
		return struct {
			*plainWriter
			http.Pusher
		}{
			plainWriter: base,
			Pusher:      c,
		}
	default:
		return base
	}
}

// inspectingGateway returns a gateway whose global middleware after the
// recorder runs inspect instead of the gateway handling.
func inspectingGateway(t *testing.T, sink accesslog.Sink, inspect http.HandlerFunc) *edgekit.Gateway {
	t.Helper()

	recorder := newRecorder(t, accesslog.Config{
		Sink: sink,
	})

	return newGateway(t, edgekit.Config{
		Middleware: []edgekit.Middleware{
			recorder.Middleware,
			func(http.Handler) http.Handler {
				return inspect
			},
		},
		Hooks: []any{
			recorder,
		},
		Routes: []edgekit.Route{
			apiRoute(t, "http://upstream.test"),
		},
	})
}

func TestResponseWriterCapabilityCombinations(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps writerCaps
	}{
		{
			name: "none",
		},
		{
			name: "flush",
			caps: writerCaps{
				flush: true,
			},
		},
		{
			name: "hijack",
			caps: writerCaps{
				hijack: true,
			},
		},
		{
			name: "push",
			caps: writerCaps{
				push: true,
			},
		},
		{
			name: "flush_hijack",
			caps: writerCaps{
				flush:  true,
				hijack: true,
			},
		},
		{
			name: "flush_push",
			caps: writerCaps{
				flush: true,
				push:  true,
			},
		},
		{
			name: "hijack_push",
			caps: writerCaps{
				hijack: true,
				push:   true,
			},
		},
		{
			name: "flush_hijack_push",
			caps: writerCaps{
				flush:  true,
				hijack: true,
				push:   true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &capabilities{}
			base := writerWith(newPlainWriter(), c, tc.caps)

			var gotFlush, gotHijack, gotPush bool
			sink := newCollector(t, 1)
			g := inspectingGateway(t, sink, func(w http.ResponseWriter, _ *http.Request) {
				var flusher http.Flusher
				var hijacker http.Hijacker
				var pusher http.Pusher
				flusher, gotFlush = w.(http.Flusher)
				hijacker, gotHijack = w.(http.Hijacker)
				pusher, gotPush = w.(http.Pusher)

				if gotPush {
					_ = pusher.Push("/style.css", nil)
				}

				if gotFlush {
					flusher.Flush()
				}

				if gotHijack {
					_, _, _ = hijacker.Hijack()
				}
			})

			g.ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/api/data", nil))

			if gotFlush != tc.caps.flush {
				t.Errorf("writer is http.Flusher = %t, want %t", gotFlush, tc.caps.flush)
			}

			if gotHijack != tc.caps.hijack {
				t.Errorf("writer is http.Hijacker = %t, want %t", gotHijack, tc.caps.hijack)
			}

			if gotPush != tc.caps.push {
				t.Errorf("writer is http.Pusher = %t, want %t", gotPush, tc.caps.push)
			}

			if want := callCount(tc.caps.flush); c.flushes != want {
				t.Errorf("original Flush calls = %d, want %d", c.flushes, want)
			}

			if want := callCount(tc.caps.hijack); c.hijacks != want {
				t.Errorf("original Hijack calls = %d, want %d", c.hijacks, want)
			}

			if want := callCount(tc.caps.push); c.pushes != want {
				t.Errorf("original Push calls = %d, want %d", c.pushes, want)
			}

			record, err := receive(t.Context(), sink.signal)
			if err != nil {
				t.Fatalf("wait for capability observation record: %v", err)
			}

			if record.Hijacked != tc.caps.hijack {
				t.Errorf("Hijacked = %t, want %t", record.Hijacked, tc.caps.hijack)
			}
		})
	}
}

// callCount is the expected number of calls of an optional interface: one
// when the original writer has it.
func callCount(has bool) int {
	if has {
		return 1
	}

	return 0
}

// unwrapWriter exposes its writer only through Unwrap.
type unwrapWriter struct {
	http.ResponseWriter
}

// Unwrap returns the writer.
func (w unwrapWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func TestControllerHijackThroughUnwrapIsRecorded(t *testing.T) {
	c := &capabilities{}
	base := unwrapWriter{
		ResponseWriter: writerWith(newPlainWriter(), c, writerCaps{
			hijack: true,
		}),
	}

	sink := newCollector(t, 1)
	g := inspectingGateway(t, sink, func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Hijacker); ok {
			t.Error("writer has Hijack, want the interfaces of the original writer")
		}

		if _, _, err := http.NewResponseController(w).Hijack(); err != nil {
			t.Errorf("controller Hijack: %v", err)
		}
	})

	g.ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/api/data", nil))

	record, err := receive(t.Context(), sink.signal)
	if err != nil {
		t.Fatalf("wait for controller hijack record: %v", err)
	}

	if c.hijacks != 1 {
		t.Errorf("original Hijack calls = %d, want 1", c.hijacks)
	}

	if !record.Hijacked {
		t.Error("Hijacked = false, want true")
	}

	if record.Status != 0 {
		t.Errorf("Status = %d, want 0 after a hijack", record.Status)
	}
}

func TestResponseStatusesAndRequestID(t *testing.T) {
	t.Run("statuses", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			write      []int
			wantStatus int
		}{
			{
				name: "informational then final",
				write: []int{
					http.StatusEarlyHints,
					http.StatusCreated,
					http.StatusInternalServerError,
				},
				wantStatus: http.StatusCreated,
			},
			{
				name: "explicit switch",
				write: []int{
					http.StatusSwitchingProtocols,
				},
				wantStatus: http.StatusSwitchingProtocols,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				base := newPlainWriter()
				sink := newCollector(t, 1)
				g := inspectingGateway(t, sink, func(w http.ResponseWriter, _ *http.Request) {
					for _, status := range tc.write {
						w.WriteHeader(status)
					}
				})

				g.ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/api/data", nil))

				record, err := receive(t.Context(), sink.signal)
				if err != nil {
					t.Fatalf("wait for response status record: %v", err)
				}

				if record.Status != tc.wantStatus || !slices.Equal(base.statuses, tc.write) {
					t.Errorf("Status = %d, writes %v; want %d and all writes forwarded", record.Status, base.statuses, tc.wantStatus)
				}

				if got := base.header.Values(accesslog.RequestIDHeader); !slices.Equal(got, []string{record.RequestID}) {
					t.Errorf("request IDs = %q, want only %q", got, record.RequestID)
				}
			})
		}
	})

	for _, proto := range protocols {
		t.Run("proxied "+proto.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Link", "</style.css>; rel=preload")
				w.WriteHeader(http.StatusEarlyHints)
				w.Header().Del("Link")
				w.Header()[accesslog.RequestIDHeader] = []string{"upstream-1", "upstream-2"}
				w.WriteHeader(http.StatusCreated)
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

			var hints []textproto.MIMEHeader
			trace := &httptrace.ClientTrace{
				Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
					if code == http.StatusEarlyHints {
						hints = append(hints, header)
					}

					return nil
				},
			}

			req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, ts.url("/api/data"), nil)
			if err != nil {
				t.Fatalf("make request: %v", err)
			}

			resp, err := ts.client(t, proto.http2).Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			_ = resp.Body.Close()

			record, err := receive(t.Context(), sink.signal)
			if err != nil {
				t.Fatalf("wait for proxied response record: %v", err)
			}

			if len(hints) != 1 || hints[0].Get("Link") == "" || len(hints[0].Values(accesslog.RequestIDHeader)) != 0 {
				t.Errorf("103 headers = %v, want Link and no request ID", hints)
			}

			if got := resp.Header.Values(accesslog.RequestIDHeader); resp.StatusCode != http.StatusCreated || !slices.Equal(got, []string{record.RequestID}) {
				t.Errorf("response %d with request IDs %q, want 201 with only %q", resp.StatusCode, got, record.RequestID)
			}

			if record.Status != http.StatusCreated || record.UpstreamStatus != http.StatusCreated {
				t.Errorf("record = %+v, want status 201 and upstream 201", record)
			}
		})
	}
}

// controllerWriter supports the operations of http.ResponseController
// without the Flusher interface.
type controllerWriter struct {
	*plainWriter

	flushErr      error
	flushes       int
	readDeadline  time.Time
	writeDeadline time.Time
	duplex        bool
}

// FlushError counts the call and returns flushErr.
func (w *controllerWriter) FlushError() error {
	w.flushes++

	return w.flushErr
}

// SetReadDeadline records deadline.
func (w *controllerWriter) SetReadDeadline(deadline time.Time) error {
	w.readDeadline = deadline

	return nil
}

// SetWriteDeadline records deadline.
func (w *controllerWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline

	return nil
}

// EnableFullDuplex records the call.
func (w *controllerWriter) EnableFullDuplex() error {
	w.duplex = true

	return nil
}

func TestResponseControllerOperationsAndFlushError(t *testing.T) {
	t.Run("supported", func(t *testing.T) {
		base := &controllerWriter{
			plainWriter: newPlainWriter(),
			flushErr:    io.ErrClosedPipe,
		}
		deadline := time.Now().Add(time.Minute)

		var flushErrs []error
		sink := newCollector(t, 1)
		g := inspectingGateway(t, sink, func(w http.ResponseWriter, _ *http.Request) {
			if _, ok := w.(http.Flusher); ok {
				t.Error("writer has Flush, want the interfaces of the original writer")
			}

			controller := http.NewResponseController(w)
			if err := controller.SetReadDeadline(deadline); err != nil {
				t.Errorf("SetReadDeadline: %v", err)
			}

			if err := controller.SetWriteDeadline(deadline); err != nil {
				t.Errorf("SetWriteDeadline: %v", err)
			}

			if err := controller.EnableFullDuplex(); err != nil {
				t.Errorf("EnableFullDuplex: %v", err)
			}

			flushErrs = append(flushErrs, controller.Flush())
			base.flushErr = io.ErrUnexpectedEOF
			flushErrs = append(flushErrs, controller.Flush())
		})

		g.ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/api/data", nil))

		if !base.readDeadline.Equal(deadline) || !base.writeDeadline.Equal(deadline) || !base.duplex {
			t.Errorf("writer = %+v, want the deadlines and full duplex through Unwrap", base)
		}

		if base.flushes != 2 || len(flushErrs) != 2 || !errors.Is(flushErrs[0], io.ErrClosedPipe) || !errors.Is(flushErrs[1], io.ErrUnexpectedEOF) {
			t.Errorf("flushes %d with errors %v, want 2 with the errors of the writer", base.flushes, flushErrs)
		}

		record, err := receive(t.Context(), sink.signal)
		if err != nil {
			t.Fatalf("wait for flush failure record: %v", err)
		}

		if !errors.Is(record.WriteErr, io.ErrClosedPipe) || record.Status != http.StatusOK {
			t.Errorf("record = %+v, want the first flush error and the implicit 200", record)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		var flushErr error
		sink := newCollector(t, 1)
		g := inspectingGateway(t, sink, func(w http.ResponseWriter, _ *http.Request) {
			flushErr = http.NewResponseController(w).Flush()
			w.WriteHeader(http.StatusNoContent)
		})

		g.ServeHTTP(newPlainWriter(), httptest.NewRequest(http.MethodGet, "/api/data", nil))

		record, err := receive(t.Context(), sink.signal)
		if err != nil {
			t.Fatalf("wait for unsupported flush record: %v", err)
		}

		if !errors.Is(flushErr, http.ErrNotSupported) || record.WriteErr != nil || record.Status != http.StatusNoContent {
			t.Errorf("flush %v, record %+v; want ErrNotSupported without a commit or a write error", flushErr, record)
		}
	})
}

func TestUpstreamRequestIDDoesNotReachClient(t *testing.T) {
	for _, proto := range protocols {
		for _, tc := range []struct {
			name     string
			declared bool
		}{
			{
				name:     "declared trailer",
				declared: true,
			},
			{
				name:     "undeclared trailer",
				declared: false,
			},
		} {
			t.Run(proto.name+" "+tc.name, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					h := w.Header()
					h.Set(accesslog.RequestIDHeader, "upstream-hint-id")
					h.Set("Link", "</style.css>; rel=preload")
					w.WriteHeader(http.StatusEarlyHints)

					h.Del("Link")
					h.Set(accesslog.RequestIDHeader, "upstream-header-id")
					if tc.declared {
						h.Set("Trailer", "X-Request-Id, X-Other")
					} else {
						h.Set("Trailer", "X-Other")
					}

					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "body")

					h.Set("X-Other", "kept")
					if tc.declared {
						h.Set(accesslog.RequestIDHeader, "upstream-trailer-id")
					} else {
						h.Set(http.TrailerPrefix+accesslog.RequestIDHeader, "upstream-trailer-id")
					}
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

				var hints []textproto.MIMEHeader
				trace := &httptrace.ClientTrace{
					Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
						if code == http.StatusEarlyHints {
							hints = append(hints, header)
						}

						return nil
					},
				}

				req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, ts.url("/api/data"), nil)
				if err != nil {
					t.Fatalf("make request: %v", err)
				}

				resp, err := ts.client(t, proto.http2).Do(req)
				if err != nil {
					t.Fatalf("GET: %v", err)
				}

				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || string(body) != "body" {
					t.Fatalf("body = %q, %v; want the complete body", body, err)
				}

				record, err := receive(t.Context(), sink.signal)
				if err != nil {
					t.Fatalf("wait for request-ID ownership record: %v", err)
				}

				if len(hints) != 1 || hints[0].Get("Link") == "" || len(hints[0].Values(accesslog.RequestIDHeader)) != 0 {
					t.Errorf("103 headers = %v, want Link and no request ID", hints)
				}

				if got := resp.Header.Values(accesslog.RequestIDHeader); !slices.Equal(got, []string{record.RequestID}) {
					t.Errorf("response request IDs = %q, want only %q", got, record.RequestID)
				}

				if got := resp.Trailer.Values(accesslog.RequestIDHeader); len(got) != 0 {
					t.Errorf("trailer request IDs = %q, want none", got)
				}

				if got := resp.Trailer.Get("X-Other"); got != "kept" {
					t.Errorf("trailer X-Other = %q, want the other trailers unchanged", got)
				}
			})
		}
	}
}

// snapshotWriter is a plainWriter that keeps a copy of the header map at
// each WriteHeader call.
type snapshotWriter struct {
	*plainWriter

	headers []http.Header
}

// WriteHeader keeps a copy of the header map and records status.
func (w *snapshotWriter) WriteHeader(status int) {
	w.headers = append(w.headers, w.header.Clone())
	w.plainWriter.WriteHeader(status)
}

func TestLocalInformationalResponseHasNoRequestID(t *testing.T) {
	base := &snapshotWriter{
		plainWriter: newPlainWriter(),
	}
	sink := newCollector(t, 1)
	g := inspectingGateway(t, sink, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add(accesslog.RequestIDHeader, "local-hint-id")
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusNoContent)
	})

	g.ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/api/data", nil))

	record, err := receive(t.Context(), sink.signal)
	if err != nil {
		t.Fatalf("wait for informational response record: %v", err)
	}

	if len(base.headers) != 2 {
		t.Fatalf("WriteHeader calls = %d, want 2", len(base.headers))
	}

	if got := base.headers[0].Values(accesslog.RequestIDHeader); len(got) != 0 {
		t.Errorf("103 request IDs = %q, want none", got)
	}

	if got := base.headers[1].Values(accesslog.RequestIDHeader); !slices.Equal(got, []string{record.RequestID}) {
		t.Errorf("final request IDs = %q, want only %q", got, record.RequestID)
	}
}
