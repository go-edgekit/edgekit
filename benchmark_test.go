package edgekit_test

import (
	"io"
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
		Transport: transport,
	})
	if err != nil {
		b.Fatal(err)
	}

	for _, bc := range []struct {
		name   string
		target string
	}{
		{
			name:   "no query",
			target: "/api/users/42",
		},
		{
			name:   "escaped query",
			target: "/api/users?q=caf%C3%A9&filter=a%20b&page=2&size=50",
		},
	} {
		b.Run(bc.name, func(b *testing.B) {
			r := httptest.NewRequest(http.MethodGet, bc.target, nil)
			for b.Loop() {
				g.ServeHTTP(httptest.NewRecorder(), r)
			}
		})
	}
}
