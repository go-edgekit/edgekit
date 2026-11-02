package bodylimit_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/bodylimit"
)

type completionFunc func(edgekit.Outcome)

func (f completionFunc) OnComplete(_ context.Context, _ *edgekit.RequestContext, outcome edgekit.Outcome) {
	f(outcome)
}

func TestGatewayBodyLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		known  bool
		limit  int64
		status int
	}{
		{"empty", "", true, 0, 200},
		{"at limit", "abcd", true, 4, 200},
		{"known oversize", "abcde", true, 4, 413},
		{"unknown at limit", "abcd", false, 4, 200},
		{"unknown oversize", "abcde", false, 4, 413},
		{"unknown zero limit", "a", false, 0, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var received atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				n, err := io.Copy(io.Discard, r.Body)
				received.Add(n)
				if err == nil {
					_, _ = io.WriteString(w, "ok")
				}
			}))
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			defer transport.CloseIdleConnections()
			completions := 0
			var outcome edgekit.Outcome
			unwound := false
			app, err := edgekit.NewWithConfig(edgekit.Config{
				Routes:    []edgekit.Route{{Prefix: "/api/", Upstream: target, Middlewares: []edgekit.Middleware{bodylimit.New(tc.limit)}}},
				Transport: transport,
				Middlewares: []edgekit.Middleware{func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer func() { unwound = true }()
						next.ServeHTTP(w, r)
					})
				}},
				Extensions: []any{completionFunc(func(got edgekit.Outcome) {
					completions++
					outcome = got
					if !unwound {
						t.Error("completion preceded unwind")
					}
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/upload", strings.NewReader(tc.body))
			if !tc.known {
				r.ContentLength = -1
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != tc.status || completions != 1 || outcome.Aborted {
				t.Fatalf("status=%d body=%q completions=%d outcome=%+v", w.Code, w.Body.String(), completions, outcome)
			}
			if tc.status == 200 && (w.Body.String() != "ok" || received.Load() != int64(len(tc.body))) {
				t.Fatalf("allowed body changed: response=%q received=%d", w.Body.String(), received.Load())
			}
			if tc.status == 413 && w.Body.String() != "Request Entity Too Large\n" {
				t.Fatalf("unexpected rejection body %q", w.Body.String())
			}
			if tc.known && tc.status == 413 && (calls.Load() != 0 || outcome.Err != nil) {
				t.Fatal("known oversized body reached upstream or invented a gateway error")
			}
			if !tc.known && tc.status == 413 {
				var tooLarge *http.MaxBytesError
				if !errors.As(outcome.Err, &tooLarge) || tooLarge.Limit != tc.limit {
					t.Fatalf("missing read-limit execution error: %v", outcome.Err)
				}
			}
			if received.Load() > tc.limit {
				t.Fatalf("forwarded %d bytes beyond limit %d", received.Load(), tc.limit)
			}
		})
	}
}

func TestMiddlewareDoesNotReadAheadOrWrapResponseWriter(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	r := httptest.NewRequest(http.MethodPost, "/", reader)
	w := httptest.NewRecorder()
	called := false
	bodylimit.New(4)(http.HandlerFunc(func(got http.ResponseWriter, request *http.Request) {
		called = true
		if got != w || request == r || request.Body == r.Body || request.Context() != r.Context() {
			t.Error("writer capabilities, request isolation, or context changed")
		}
		// No bytes have been supplied. Entering next must not wait for EOF.
	})).ServeHTTP(w, r)
	if !called {
		t.Fatal("next was not called")
	}
}

func TestNegativeLimitPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("negative configuration did not panic")
		}
	}()
	bodylimit.New(-1)
}
