package edgekit_test

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
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

// BenchmarkServeHTTP measures the gateway without network I/O, so the
// gateway's own work is visible.
func BenchmarkServeHTTP(b *testing.B) {
	benchmarkServeHTTP(b, nil)
}

// BenchmarkServeHTTPHooks is BenchmarkServeHTTP with one hook provider for
// all four phases, so the difference is the cost of hook dispatch and of the
// header rules after the hooks.
func BenchmarkServeHTTPHooks(b *testing.B) {
	benchmarkServeHTTP(b, []any{&testHooks{
		received: func(context.Context, *edgekit.RequestContext, *http.Request) error {
			return nil
		},
		before: func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) error {
			return nil
		},
		response: func(context.Context, *edgekit.RequestContext, *http.Response) error {
			return nil
		},
		completed: func(context.Context, *edgekit.RequestContext, edgekit.Completion) {},
	}})
}

// benchmarkServeHTTP runs the sub-benchmarks for a gateway with providers.
func benchmarkServeHTTP(b *testing.B, providers []any) {
	b.Helper()

	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})
	g, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(b, "http://upstream.test"),
		}},
		Hooks:     providers,
		Transport: transport,
	})
	if err != nil {
		b.Fatal(err)
	}

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
				g.ServeHTTP(httptest.NewRecorder(), r)
			}
		})
	}
}
