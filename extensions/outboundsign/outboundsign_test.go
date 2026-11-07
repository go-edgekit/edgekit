package outboundsign_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/outboundsign"
)

// newGateway returns a gateway for cfg and closes its idle connections when
// the test ends.
func newGateway(t *testing.T, cfg edgekit.Config) *edgekit.Gateway {
	t.Helper()

	g, err := edgekit.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(g.CloseIdleConnections)

	return g
}

// static returns a static selector for rawURL.
func static(t *testing.T, rawURL string) edgekit.UpstreamSelector {
	t.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	selector, err := edgekit.StaticUpstream(u)
	if err != nil {
		t.Fatalf("StaticUpstream: %v", err)
	}

	return selector
}

// withKey returns the route middleware of signer for key, or stops the test.
func withKey(t *testing.T, signer *outboundsign.Signer, key []byte) edgekit.Middleware {
	t.Helper()

	middleware, err := signer.WithKey(key)
	if err != nil {
		t.Fatalf("WithKey: %v", err)
	}

	return middleware
}

// receive waits at most 5s for a value from ch, or returns the context error.
func receive[T any](ctx context.Context, ch <-chan T) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	select {
	case value := <-ch:
		return value, nil
	case <-ctx.Done():
		var zero T

		return zero, ctx.Err()
	}
}

// wireSignature computes the signature from the request that the upstream
// got, independently of the extension. The gateway sends the upstream
// authority as Host. The scheme comes from the TLS state of the connection.
func wireSignature(key []byte, r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, r.Method+"\n"+scheme+"\n"+r.Host+"\n"+r.Host+"\n"+r.RequestURI)

	return hex.EncodeToString(mac.Sum(nil))
}

// received is what a test upstream observed.
type received struct {
	signatures []string
	valid      bool
	requestURI string
	body       string
}

func TestSignsRewrittenDestinationAndEscapedURI(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")

	for _, tc := range []struct {
		name        string
		http2       bool
		connection  bool
		upstreamTLS bool
	}{
		{
			name: "http1_plain_upstream",
		},
		{
			name:       "http1_signature_in_connection",
			connection: true,
		},
		{
			name:  "http2_plain_upstream",
			http2: true,
		},
		{
			name:        "http1_tls_upstream",
			upstreamTLS: true,
		},
		{
			name:        "http2_tls_upstream",
			http2:       true,
			upstreamTLS: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(chan struct{}, 1)
			results := make(chan received, 1)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The client sends the body only after this signal, so the
				// request can arrive only when signing does not read the body.
				headers <- struct{}{}

				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("upstream read body: %v", err)
				}

				results <- received{
					signatures: r.Header.Values(outboundsign.SignatureHeader),
					valid:      r.Header.Get(outboundsign.SignatureHeader) == wireSignature(key, r),
					requestURI: r.RequestURI,
					body:       string(body),
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			if tc.upstreamTLS {
				upstream.StartTLS()
			} else {
				upstream.Start()
			}
			t.Cleanup(upstream.Close)

			transport := edgekit.NewTransport()
			if tc.upstreamTLS {
				roots := x509.NewCertPool()
				roots.AddCert(upstream.Certificate())
				transport.TLSClientConfig = &tls.Config{
					RootCAs:    roots,
					MinVersion: tls.VersionTLS12,
				}
			}
			t.Cleanup(transport.CloseIdleConnections)

			signer := outboundsign.New()
			g := newGateway(t, edgekit.Config{
				Transport: transport,
				Hooks: edgekit.Hooks{
					BeforeUpstream: []edgekit.BeforeUpstreamHook{
						signer,
					},
				},
				HeaderOwners: []edgekit.HeaderOwner{
					signer,
				},
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, upstream.URL+"/base/v%31"),
						Middleware: []edgekit.Middleware{
							withKey(t, signer, key),
						},
					},
				},
			})

			server := httptest.NewUnstartedServer(g)
			server.EnableHTTP2 = tc.http2
			server.StartTLS()
			t.Cleanup(server.Close)

			body, bodyWriter := io.Pipe()
			t.Cleanup(func() { _ = bodyWriter.CloseWithError(errors.New("test ended")) })

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/caf%C3%A9/item%20one?q=a+b&z=%7E", body)
			if err != nil {
				t.Fatalf("make request: %v", err)
			}

			req.Header.Set(outboundsign.SignatureHeader, "forged client signature")
			if tc.connection {
				req.Header.Set("Connection", outboundsign.SignatureHeader)
			}

			type clientResponse struct {
				status     int
				protoMajor int
			}

			responses := make(chan clientResponse, 1)
			go func() {
				resp, requestErr := server.Client().Do(req)
				if requestErr != nil {
					t.Errorf("POST: %v", requestErr)
					responses <- clientResponse{}

					return
				}
				_ = resp.Body.Close()

				responses <- clientResponse{
					status:     resp.StatusCode,
					protoMajor: resp.ProtoMajor,
				}
			}()

			if _, err = receive(t.Context(), headers); err != nil {
				t.Fatalf("wait for upstream headers: %v", err)
			}

			if _, err = io.WriteString(bodyWriter, "request body"); err != nil {
				t.Fatalf("write body: %v", err)
			}
			_ = bodyWriter.Close()

			got, err := receive(t.Context(), results)
			if err != nil {
				t.Fatalf("wait for upstream observations: %v", err)
			}

			response, err := receive(t.Context(), responses)
			if err != nil {
				t.Fatalf("wait for client response: %v", err)
			}

			if response.status != http.StatusNoContent {
				t.Errorf("status = %d, want 204", response.status)
			}

			wantMajor := 1
			if tc.http2 {
				wantMajor = 2
			}

			if response.protoMajor != wantMajor {
				t.Errorf("response HTTP version = %d, want %d", response.protoMajor, wantMajor)
			}

			if len(got.signatures) != 1 || !got.valid {
				t.Errorf("signatures = %q, want one valid signature", got.signatures)
			}

			if got.requestURI != "/base/v%31/api/caf%C3%A9/item%20one?q=a+b&z=%7E" || got.body != "request body" {
				t.Errorf("upstream got %q with body %q, want the rewritten escaped URI and the body", got.requestURI, got.body)
			}
		})
	}
}

func TestSigningDoesNotChangeInboundRequest(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	valid := make(chan bool, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		valid <- r.Header.Get(outboundsign.SignatureHeader) == wireSignature(key, r)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	signer := outboundsign.New()
	g := newGateway(t, edgekit.Config{
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				signer,
			},
		},
		HeaderOwners: []edgekit.HeaderOwner{
			signer,
		},
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL+"/base"),
				Middleware: []edgekit.Middleware{
					withKey(t, signer, key),
				},
			},
		},
	})

	r := httptest.NewRequest(http.MethodGet, "/api/a%20b?q=1", nil)
	r.Host = "gateway.test"
	r.Header.Set(outboundsign.SignatureHeader, "forged client signature")
	originalURL := r.URL.String()
	originalHeader := r.Header.Clone()

	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	signatureValid, err := receive(t.Context(), valid)
	if err != nil {
		t.Fatalf("wait for upstream signature check: %v", err)
	}

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}

	if !signatureValid {
		t.Error("upstream signature is not valid, want a valid signature")
	}

	if r.URL.String() != originalURL || r.Host != "gateway.test" || !maps.EqualFunc(r.Header, originalHeader, slices.Equal) {
		t.Errorf("inbound request changed: %s %s %v", r.Host, r.URL, r.Header)
	}
}

func TestSigningFailurePreventsForwardingAndCompletesAfterUnwind(t *testing.T) {
	key := []byte("key-material-that-must-stay-secret")

	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	canceling := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	expired := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithDeadline(r.Context(), time.Unix(0, 0))
			defer cancel()

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}

	for _, tc := range []struct {
		name       string
		noKey      bool
		clientGone bool
		middleware edgekit.Middleware
		wantStatus int
		wantCause  error
	}{
		{
			name:       "missing key",
			noKey:      true,
			wantStatus: http.StatusInternalServerError,
			wantCause:  outboundsign.ErrNoKey,
		},
		{
			name:       "client gone",
			clientGone: true,
			wantStatus: 499,
			wantCause:  context.Canceled,
		},
		{
			name:       "canceled by middleware",
			middleware: canceling,
			wantStatus: http.StatusInternalServerError,
			wantCause:  context.Canceled,
		},
		{
			name:       "deadline of middleware",
			middleware: expired,
			wantStatus: http.StatusInternalServerError,
			wantCause:  context.DeadlineExceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forwarded.Store(0)

			signer := outboundsign.New()
			var routeMiddleware []edgekit.Middleware
			if tc.middleware != nil {
				routeMiddleware = append(routeMiddleware, tc.middleware)
			}

			if !tc.noKey {
				routeMiddleware = append(routeMiddleware, withKey(t, signer, key))
			}

			unwound := false
			var completions []edgekit.Completion
			var unwoundAtCompletion bool
			g := newGateway(t, edgekit.Config{
				SharedMiddleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							next.ServeHTTP(w, r)
							unwound = true
						})
					},
				},
				Hooks: edgekit.Hooks{
					BeforeUpstream: []edgekit.BeforeUpstreamHook{
						signer,
					},
					RequestCompleted: []edgekit.RequestCompletedHook{
						edgekit.RequestCompletedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
							unwoundAtCompletion = unwound
							completions = append(completions, c)
						}),
					},
				},
				HeaderOwners: []edgekit.HeaderOwner{
					signer,
				},
				Routes: []edgekit.Route{
					{
						Prefix:     "/api/",
						Upstream:   static(t, upstream.URL),
						Middleware: routeMiddleware,
					},
				},
			})

			r := httptest.NewRequest(http.MethodGet, "/api/data", nil)
			if tc.clientGone {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}

			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)

			if w.Code != tc.wantStatus || forwarded.Load() != 0 {
				t.Errorf("status %d, forwarded %d; want %d and no forwarded request", w.Code, forwarded.Load(), tc.wantStatus)
			}

			if len(completions) != 1 || !unwoundAtCompletion {
				t.Fatalf("completions = %d, after unwind %t; want one after the middleware returned", len(completions), unwoundAtCompletion)
			}

			failure := completions[0].Failure
			if failure == nil || failure.Phase != edgekit.PhaseBeforeUpstream || !errors.Is(failure.Err, tc.wantCause) {
				t.Fatalf("Failure = %+v, want %v in PhaseBeforeUpstream", failure, tc.wantCause)
			}

			if completions[0].SelectedUpstream == nil || completions[0].ConnectionObtained {
				t.Errorf("Completion = %+v, want a selected upstream without a connection", completions[0])
			}

			if strings.Contains(w.Body.String(), "secret") || strings.Contains(failure.Err.Error(), "secret") || w.Header().Get("Content-Type") != "application/problem+json" {
				t.Errorf("response %q, error %q: want a problem body without key data", w.Body.String(), failure.Err)
			}
		})
	}
}

func TestNewlineInMethodFailsClosed(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")

	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	completions := make(chan edgekit.Completion, 1)
	signer := outboundsign.New()
	g := newGateway(t, edgekit.Config{
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				signer,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				edgekit.RequestCompletedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
					completions <- c
				}),
			},
		},
		HeaderOwners: []edgekit.HeaderOwner{
			signer,
		},
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
				Middleware: []edgekit.Middleware{
					withKey(t, signer, key),
				},
			},
		},
	})

	// The method field is before the scheme field, so "GET\nhttp" with the
	// scheme "s" would give the same signed string as "GET" with "http\ns".
	r := httptest.NewRequest(http.MethodGet, "/api/item", nil)
	r.Method = "GET\nhttp"

	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	c, err := receive(t.Context(), completions)
	if err != nil {
		t.Fatalf("wait for completion: %v", err)
	}

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}

	if n := forwarded.Load(); n != 0 {
		t.Errorf("forwarded requests = %d, want 0", n)
	}

	if c.Failure == nil {
		t.Fatal("Completion.Failure = nil, want ErrInvalidField")
	}

	if c.Failure.Phase != edgekit.PhaseBeforeUpstream {
		t.Errorf("Failure.Phase = %v, want %v", c.Failure.Phase, edgekit.PhaseBeforeUpstream)
	}

	if !errors.Is(c.Failure.Err, outboundsign.ErrInvalidField) {
		t.Errorf("Failure.Err = %v, want ErrInvalidField", c.Failure.Err)
	}
}

func TestConcurrentRoutesUseTheirOwnSigningKeys(t *testing.T) {
	keys := map[string][]byte{
		"/a/": []byte("key-for-route-a-0123456789abcdef"),
		"/b/": []byte("key-for-route-b-0123456789abcdef"),
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keys[r.URL.Path[:3]]
		if r.Header.Get(outboundsign.SignatureHeader) != wireSignature(key, r) {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	signer := outboundsign.New()
	var routes []edgekit.Route
	for _, prefix := range []string{"/a/", "/b/"} {
		// WithKey copies the key, so a later change has no effect.
		key := append([]byte(nil), keys[prefix]...)
		middleware := withKey(t, signer, key)
		clear(key)

		routes = append(routes, edgekit.Route{
			Prefix:   prefix,
			Upstream: static(t, upstream.URL),
			Middleware: []edgekit.Middleware{
				middleware,
			},
		})
	}

	g := newGateway(t, edgekit.Config{
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				signer,
			},
		},
		HeaderOwners: []edgekit.HeaderOwner{
			signer,
		},
		Routes: routes,
	})

	const requests = 40
	statuses := make(chan string, requests)
	var wg sync.WaitGroup
	for i := range requests {
		path := fmt.Sprintf("/%c/%d", "ab"[i%2], i)
		wg.Go(func() {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusNoContent {
				statuses <- fmt.Sprintf("%s: %d", path, w.Code)
			}
		})
	}
	wg.Wait()
	close(statuses)

	for status := range statuses {
		t.Errorf("request %s, want 204 with the key of its route", status)
	}
}

func TestWithKeyRejectsShortKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  []byte
	}{
		{
			name: "nil",
			key:  nil,
		},
		{
			name: "empty",
			key:  []byte{},
		},
		{
			name: "one_byte_short",
			key:  bytes.Repeat([]byte{'k'}, outboundsign.MinKeySize-1),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			middleware, err := outboundsign.New().WithKey(tc.key)
			if !errors.Is(err, outboundsign.ErrShortKey) {
				t.Errorf("WithKey() error = %v, want ErrShortKey", err)
			}

			if middleware != nil {
				t.Error("WithKey() middleware != nil, want nil")
			}
		})
	}
}

func TestWithKeyAcceptsMinimumKey(t *testing.T) {
	middleware, err := outboundsign.New().WithKey(bytes.Repeat([]byte{'k'}, outboundsign.MinKeySize))
	if err != nil {
		t.Errorf("WithKey() error = %v, want nil", err)
	}

	if middleware == nil {
		t.Error("WithKey() middleware = nil, want middleware")
	}
}

func TestWithKeyOutsideGatewayPassesThrough(t *testing.T) {
	signer := outboundsign.New()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	called := false
	withKey(t, signer, []byte("0123456789abcdef0123456789abcdef"))(http.HandlerFunc(func(gotW http.ResponseWriter, gotR *http.Request) {
		called = gotW == w && gotR == r
	})).ServeHTTP(w, r)

	if !called {
		t.Error("middleware did not call next with the same writer and request")
	}
}
