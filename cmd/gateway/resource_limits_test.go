package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

func TestInboundHeaderLimitRejectsBeforeGatewayEntry(t *testing.T) {
	var entered, completed atomic.Int32
	target, _ := url.Parse("http://unused.test")
	app, err := edgekit.NewWithConfig(edgekit.Config{
		Routes: []edgekit.Route{{Prefix: "/api/", Upstream: target}},
		Middlewares: []edgekit.Middleware{func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered.Add(1)
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Errorf("allowed body was interrupted: %v", err)
				}
				w.WriteHeader(http.StatusNoContent)
			})
		}},
		Extensions: []any{completionObserver(func(edgekit.Outcome) { completed.Add(1) })},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(app)
	server.Config = newHTTPServer(app)
	server.Start()
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	for _, tc := range []struct {
		name                   string
		headerBytes, bodyBytes int
		status                 int
	}{
		{"small headers", 1024, 0, 204},
		// Exceed both the configured budget and Go's parser look-ahead allowance.
		{"oversized headers", 128 << 10, 0, 431},
		{"body is not a header", 1024, 128 << 10, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeEntry, beforeComplete := entered.Load(), completed.Load()
			r, err := http.NewRequest(http.MethodPost, server.URL+"/api/test", strings.NewReader(strings.Repeat("b", tc.bodyBytes)))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("X-Test", strings.Repeat("h", tc.headerBytes))
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != tc.status || readErr != nil {
				t.Fatalf("response=%d err=%v", response.StatusCode, readErr)
			}
			wantDelta := int32(1)
			if tc.status == 431 {
				wantDelta = 0
			}
			if entered.Load()-beforeEntry != wantDelta || completed.Load()-beforeComplete != wantDelta {
				t.Fatalf("pre-handler rejection entered/completed gateway: entry delta=%d completion delta=%d", entered.Load()-beforeEntry, completed.Load()-beforeComplete)
			}
		})
	}
}

type responseObserver func(*http.Response) error

func (f responseObserver) OnUpstreamResponse(_ context.Context, _ *edgekit.RequestContext, response *http.Response) error {
	return f(response)
}

func TestUpstreamHeaderLimitBeforeResponseHooks(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http1", true: "http2"}[secure], func(t *testing.T) {
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if secure && r.ProtoMajor != 2 {
					t.Errorf("expected HTTP/2, got %s", r.Proto)
				}
				size := 1024
				if strings.Contains(r.URL.Path, "large") {
					size = 128 << 10
				}
				w.Header().Set("X-Large", strings.Repeat("h", size))
				_, _ = io.WriteString(w, "ok")
			}))
			if secure {
				upstream.EnableHTTP2 = true
				upstream.StartTLS()
			} else {
				upstream.Start()
			}
			defer upstream.Close()
			transport := newUpstreamTransport()
			if secure {
				roots := x509.NewCertPool()
				roots.AddCert(upstream.Certificate())
				transport.TLSClientConfig = &tls.Config{RootCAs: roots}
			}
			defer transport.CloseIdleConnections()
			target, _ := url.Parse(upstream.URL)
			responses := 0
			var outcome edgekit.Outcome
			app, err := edgekit.NewWithConfig(edgekit.Config{
				Routes: []edgekit.Route{{Prefix: "/api/", Upstream: target}}, Transport: transport,
				Extensions: []any{
					responseObserver(func(*http.Response) error { responses++; return nil }),
					completionObserver(func(got edgekit.Outcome) { outcome = got }),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/api/small", "/api/large", "/api/small-again"} {
				w := httptest.NewRecorder()
				before := responses
				app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				if path == "/api/large" {
					if w.Code != 502 || w.Body.String() != "Bad Gateway\n" || w.Header().Get("X-Large") != "" || outcome.Err == nil || outcome.Aborted || responses != before {
						t.Fatalf("oversized headers escaped limit: response=%d %q outcome=%+v hooks=%d", w.Code, w.Body.String(), outcome, responses-before)
					}
				} else if w.Code != 200 || w.Body.String() != "ok" || len(w.Header().Get("X-Large")) != 1024 || outcome.Err != nil || outcome.Aborted || responses != before+1 {
					t.Fatalf("allowed response changed: response=%d %q outcome=%+v hooks=%d", w.Code, w.Body.String(), outcome, responses-before)
				}
			}
		})
	}
}
