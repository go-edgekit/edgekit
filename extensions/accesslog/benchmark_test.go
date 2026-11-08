package accesslog_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/accesslog"
)

// roundTripperFunc is an http.RoundTripper made from a function.
type roundTripperFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// BenchmarkServeHTTP measures a gateway without the recorder, as the base
// for BenchmarkServeHTTPRecorder.
func BenchmarkServeHTTP(b *testing.B) {
	benchmarkServeHTTP(b, nil)
}

// BenchmarkServeHTTPRecorder is BenchmarkServeHTTP with the recorder and a
// sink that discards the records, so the difference is the cost of the
// recorder: the request ID, the response writer, the record, and the hooks.
func BenchmarkServeHTTPRecorder(b *testing.B) {
	recorder, err := accesslog.New(accesslog.Config{
		Sink: accesslog.SinkFunc(func(accesslog.Record) {}),
	})
	if err != nil {
		b.Fatal(err)
	}

	benchmarkServeHTTP(b, recorder)
}

// benchmarkServeHTTP measures a gateway without network I/O. It registers
// recorder when it is not nil.
func benchmarkServeHTTP(b *testing.B, recorder *accesslog.Recorder) {
	b.Helper()

	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": {"text/plain"},
			},
			Body:    io.NopCloser(strings.NewReader("ok")),
			Request: r,
		}, nil
	})
	cfg := edgekit.Config{
		Routes: []edgekit.Route{
			apiRoute(b, "http://upstream.test"),
		},
		Transport: transport,
	}
	if recorder != nil {
		cfg.SharedMiddleware = []edgekit.Middleware{
			recorder.Middleware,
		}
		cfg.Hooks = edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				recorder,
			},
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				recorder,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				recorder,
			},
		}
		cfg.HeaderOwners = []edgekit.HeaderOwner{
			recorder,
		}
	}

	g, err := edgekit.New(cfg)
	if err != nil {
		b.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/users/42", nil)
	for b.Loop() {
		g.ServeHTTP(httptest.NewRecorder(), r)
	}
}
