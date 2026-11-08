package edgekit_test

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/go-edgekit/edgekit"
)

// roundTripperFunc is an http.RoundTripper made from a function.
type roundTripperFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// The benchmarks in this file run the same request cases without network
// I/O, so the difference between two of them is the work of one layer:
//
//   - BenchmarkHandler: an http.Handler that writes the response itself, the
//     cost of the harness.
//   - BenchmarkReverseProxy: httputil.ReverseProxy with the same forwarding
//     as the gateway, the cost of the standard proxy.
//   - BenchmarkServeHTTP: the gateway, so the difference to
//     BenchmarkReverseProxy is the work of edgekit.
//   - BenchmarkServeHTTPHooks: the gateway with hooks in all four phases.
//
// Compare them only with results of the same machine and Go version.

// BenchmarkHandler measures an http.Handler that writes the response of the
// test transport itself, so the result is the cost of the harness: the
// request cases, the response recorder, and the loop.
func BenchmarkHandler(b *testing.B) {
	runBenchmarkCases(b, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
}

// BenchmarkReverseProxy measures httputil.ReverseProxy with the test
// transport. Its Rewrite does the forwarding of the gateway, SetURL and
// SetXForwarded, and nothing more: no routing, no secure defaults, no header
// rules, and no completion.
func BenchmarkReverseProxy(b *testing.B) {
	target, err := url.Parse("http://upstream.test")
	if err != nil {
		b.Fatal(err)
	}

	runBenchmarkCases(b, &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
		},
		Transport: benchmarkTransport(),
	})
}

// BenchmarkServeHTTP measures the gateway without network I/O, so the
// gateway's own work is visible.
func BenchmarkServeHTTP(b *testing.B) {
	benchmarkServeHTTP(b, edgekit.Hooks{})
}

// BenchmarkServeHTTPHooks is BenchmarkServeHTTP with one hook provider for
// all four phases, so the difference is the cost of hook dispatch and of the
// header rules after the hooks.
func BenchmarkServeHTTPHooks(b *testing.B) {
	hook := &testHooks{
		received: func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
			return nil, nil
		},
		before: func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
			return nil, nil
		},
		response: func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
			return nil, nil
		},
		completed: func(context.Context, *edgekit.RequestContext, edgekit.Completion) {},
	}
	benchmarkServeHTTP(b, allPhases(hook))
}

// benchmarkTransport returns a transport that answers 200 "ok" without
// network I/O.
func benchmarkTransport() http.RoundTripper {
	return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})
}

// benchmarkServeHTTP runs the request cases for a gateway with hooks.
func benchmarkServeHTTP(b *testing.B, hooks edgekit.Hooks) {
	b.Helper()

	g, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(b, "http://upstream.test"),
			},
		},
		Hooks:     hooks,
		Transport: benchmarkTransport(),
	})
	if err != nil {
		b.Fatal(err)
	}

	runBenchmarkCases(b, g)
}

// runBenchmarkCases runs the request cases as sub-benchmarks of h, so all
// benchmarks of this file measure the same requests.
func runBenchmarkCases(b *testing.B, h http.Handler) {
	b.Helper()

	// browserHeaders is a typical set of request headers, so the work per
	// header is visible.
	browserHeaders := http.Header{
		"Accept":          {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		"Accept-Encoding": {"gzip, deflate, br"},
		"Accept-Language": {"en-US,en;q=0.9"},
		"Authorization":   {"Bearer abcdefghijklmnopqrstuvwxyz0123456789"},
		"Cache-Control":   {"no-cache"},
		"Cookie":          {"session=abcdefghijklmnopqrstuvwxyz; theme=dark"},
		"Referer":         {"https://gateway.example/app"},
		"User-Agent":      {"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36"},
		"X-Request-Id":    {"0123456789abcdef"},
	}

	for _, bc := range []struct {
		name   string
		target string
		header http.Header
	}{
		{
			name:   "no query",
			target: "/api/users/42",
		},
		{
			name:   "escaped query",
			target: "/api/users?q=caf%C3%A9&filter=a%20b&page=2&size=50",
		},
		{
			name:   "browser headers",
			target: "/api/users/42",
			header: browserHeaders,
		},
	} {
		b.Run(bc.name, func(b *testing.B) {
			r := httptest.NewRequest(http.MethodGet, bc.target, nil)
			maps.Copy(r.Header, bc.header)

			for b.Loop() {
				h.ServeHTTP(httptest.NewRecorder(), r)
			}
		})
	}
}
