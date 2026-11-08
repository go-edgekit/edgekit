package accesslog_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/accesslog"
)

// The benchmarks in this file use the same gateway and request, so the
// difference between two of them is the work of one access log:
//
//   - BenchmarkServeHTTP: the gateway without an access log.
//   - BenchmarkServeHTTPStatusMiddleware: a minimal access-log middleware
//     that records the status and the bytes, and discards them.
//   - BenchmarkServeHTTPRecorder: the recorder with a sink that discards the
//     records, so the difference to BenchmarkServeHTTPStatusMiddleware is
//     the work that the recorder adds: the request ID, the hooks, and the
//     record.
//   - BenchmarkServeHTTPRecorderSlog: the recorder with a sink that writes
//     the safe fields with slog as JSON, so the difference to
//     BenchmarkServeHTTPRecorder is the cost of the log output.
//
// Compare them only with results of the same machine and Go version.

// roundTripperFunc is an http.RoundTripper made from a function.
type roundTripperFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// BenchmarkServeHTTP measures a gateway without an access log, as the base
// for the other benchmarks of this file.
func BenchmarkServeHTTP(b *testing.B) {
	benchmarkServeHTTP(b, edgekit.Config{})
}

// BenchmarkServeHTTPStatusMiddleware measures a minimal access-log
// middleware: a response writer that records the status and the bytes, and
// a report that discards them. It has no request ID, no hooks, and no
// optional interfaces of the response writer.
func BenchmarkServeHTTPStatusMiddleware(b *testing.B) {
	var reported int64

	benchmarkServeHTTP(b, edgekit.Config{
		SharedMiddleware: []edgekit.Middleware{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sw := &statusWriter{
						ResponseWriter: w,
					}
					next.ServeHTTP(sw, r)
					reported += int64(sw.status) + sw.bytes
				})
			},
		},
	})

	if reported == 0 {
		b.Fatal("status middleware reported nothing")
	}
}

// BenchmarkServeHTTPRecorder measures the recorder with a sink that discards
// the records.
func BenchmarkServeHTTPRecorder(b *testing.B) {
	benchmarkServeHTTP(b, recorderConfig(b, accesslog.SinkFunc(func(accesslog.Record) {})))
}

// BenchmarkServeHTTPRecorderSlog measures the recorder with a sink that
// writes the safe fields of each record with slog as JSON. The output goes
// to a byteCounter, not to slog.DiscardHandler, because the JSON encoding is
// part of the cost.
func BenchmarkServeHTTPRecorderSlog(b *testing.B) {
	var output byteCounter
	logger := slog.New(slog.NewJSONHandler(&output, nil))

	benchmarkServeHTTP(b, recorderConfig(b, accesslog.SinkFunc(func(r accesslog.Record) {
		logger.LogAttrs(context.Background(), slog.LevelInfo, "request", slog.Any("record", r))
	})))

	if output == 0 {
		b.Fatal("sink wrote no log output")
	}
}

// byteCounter is an io.Writer that counts the bytes and keeps none.
type byteCounter int64

// Write counts the bytes of p.
func (c *byteCounter) Write(p []byte) (int, error) {
	*c += byteCounter(len(p))

	return len(p), nil
}

// statusWriter records the status and the body bytes of a response.
type statusWriter struct {
	http.ResponseWriter

	status int
	bytes  int64
}

// WriteHeader records the first status.
func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}

	w.ResponseWriter.WriteHeader(status)
}

// Write records the implicit 200 and the bytes.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)

	return n, err
}

// recorderConfig returns the registration of a recorder for sink.
func recorderConfig(b *testing.B, sink accesslog.Sink) edgekit.Config {
	b.Helper()

	recorder, err := accesslog.New(accesslog.Config{
		Sink: sink,
	})
	if err != nil {
		b.Fatal(err)
	}

	return edgekit.Config{
		SharedMiddleware: []edgekit.Middleware{
			recorder.Middleware,
		},
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				recorder,
			},
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				recorder,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				recorder,
			},
		},
		HeaderOwners: []edgekit.HeaderOwner{
			recorder,
		},
	}
}

// benchmarkServeHTTP measures a gateway without network I/O. cfg gives the
// middleware, the hooks, and the header owners; the route and the transport
// are the same for all benchmarks.
func benchmarkServeHTTP(b *testing.B, cfg edgekit.Config) {
	b.Helper()

	cfg.Routes = []edgekit.Route{
		apiRoute(b, "http://upstream.test"),
	}
	cfg.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": {"text/plain"},
			},
			Body:    io.NopCloser(strings.NewReader("ok")),
			Request: r,
		}, nil
	})

	g, err := edgekit.New(cfg)
	if err != nil {
		b.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/users/42", nil)
	for b.Loop() {
		g.ServeHTTP(httptest.NewRecorder(), r)
	}
}
