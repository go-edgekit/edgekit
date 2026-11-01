package outboundsign_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gateway "example.com/go-gateway-starter"
	"example.com/go-gateway-starter/extensions/outboundsign"
)

type observer struct {
	complete func(context.Context, *gateway.RequestContext, gateway.Outcome)
	response func()
	outbound func(*http.Request)
}

func (o observer) OnBeforeUpstream(_ context.Context, _ *gateway.RequestContext, r *http.Request) error {
	if o.outbound != nil {
		o.outbound(r)
	}
	return nil
}

type countedBody struct {
	io.ReadCloser
	reads int
}

func (b *countedBody) Read(p []byte) (int, error) {
	b.reads++
	return b.ReadCloser.Read(p)
}

func (o observer) OnComplete(ctx context.Context, rc *gateway.RequestContext, outcome gateway.Outcome) {
	if o.complete != nil {
		o.complete(ctx, rc, outcome)
	}
}

func (o observer) OnUpstreamResponse(context.Context, *gateway.RequestContext, *http.Response) error {
	if o.response != nil {
		o.response()
	}
	return nil
}

func parseURL(t *testing.T, address string) *url.URL {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Calculate from the independently observed wire fields, not the extension's
// canonicalization code. The destination uses the test server's known scheme.
func wireSignature(key string, r *http.Request) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = fmt.Fprintf(mac, "%s\nhttp\n%s\n%s\n%s", r.Method, r.Host, r.Host, r.RequestURI)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestSignsRewrittenDestinationAndEscapedURIWithoutChangingInboundRequest(t *testing.T) {
	for _, tc := range []struct {
		name, connection string
	}{
		{name: "client signature"},
		{name: "hop-by-hop client signature", connection: "X-Gateway-Signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type receivedRequest struct {
				method, host, uri, signature, expected, body string
				err                                          error
			}
			received := make(chan receivedRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				received <- receivedRequest{
					method: r.Method, host: r.Host, uri: r.RequestURI,
					signature: r.Header.Get("X-Gateway-Signature"), expected: wireSignature("original key", r),
					body: string(body), err: err,
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()

			signer := outboundsign.New()
			secret := []byte("original key")
			routeMiddleware := signer.WithKey(secret)
			secret[0] = 'X' // Middleware must own a copy of configuration supplied by callers.
			target := parseURL(t, upstream.URL+"/base%2Fv1?fixed=1")
			var unwound bool
			var body *countedBody
			completions := 0
			app, err := gateway.NewWithConfig(gateway.Config{
				Routes: []gateway.Route{{
					Prefix: "/api/", Upstream: target,
					Middlewares: []gateway.Middleware{routeMiddleware, func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							rc, _ := gateway.FromContext(r.Context())
							if prefix, ok := rc.RoutePrefix(); !ok || prefix != "/api/" {
								t.Fatal("route metadata missing before signing middleware unwinds")
							}
							if _, ok := rc.Upstream(); ok {
								t.Fatal("upstream selected before route middleware")
							}
							defer func() { unwound = true }()
							next.ServeHTTP(w, r)
						})
					}},
				}},
				Extensions: []any{signer, observer{outbound: func(r *http.Request) {
					if body.reads != 0 || r.Header.Get("X-Gateway-Signature") == "" {
						t.Fatal("signer read the body or failed to set a signature before transport")
					}
				}, complete: func(_ context.Context, rc *gateway.RequestContext, outcome gateway.Outcome) {
					completions++
					if !unwound || outcome.Err != nil || outcome.Aborted {
						t.Fatalf("unexpected completion: unwound=%v outcome=%+v", unwound, outcome)
					}
					if selected, ok := rc.Upstream(); !ok || selected.String() != target.String() {
						t.Fatal("selected upstream missing at completion")
					}
				}}},
			})
			if err != nil {
				t.Fatal(err)
			}

			r := httptest.NewRequest(http.MethodPost, "http://client.example/api/object%2Fpart?q=a%2Fb&x=2", strings.NewReader("streamed body"))
			body = &countedBody{ReadCloser: r.Body}
			r.Body = body
			r.Header.Set("X-Gateway-Signature", "forged client signature")
			// A client cannot nominate the new signature for removal: hop-by-hop
			// cleanup happens before the outbound hook runs.
			if tc.connection != "" {
				r.Header.Set("Connection", tc.connection)
			}
			originalURL, originalHost := r.URL.String(), r.Host
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent || completions != 1 {
				t.Fatalf("status=%d body=%q completions=%d", w.Code, w.Body.String(), completions)
			}
			got := <-received
			if got.err != nil || got.method != http.MethodPost || got.host != target.Host ||
				got.uri != "/base%2Fv1/api/object%2Fpart?fixed=1&q=a%2Fb&x=2" || got.body != "streamed body" {
				t.Fatalf("unexpected upstream request: %+v", got)
			}
			if got.signature == "" || got.signature != got.expected {
				t.Fatalf("signature=%q want=%q", got.signature, got.expected)
			}
			if r.URL.String() != originalURL || r.Host != originalHost || r.Header.Get("X-Gateway-Signature") != "forged client signature" {
				t.Fatal("signing changed the inbound request metadata")
			}
		})
	}
}

func TestSigningFailurePreventsForwardingAndCompletesAfterUnwind(t *testing.T) {
	for _, name := range []string{"missing route state", "empty key", "canceled context"} {
		t.Run(name, func(t *testing.T) {
			var received atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			signer := outboundsign.New()
			route := gateway.Route{Prefix: "/api/", Upstream: parseURL(t, upstream.URL)}
			if name == "empty key" {
				route.Middlewares = append(route.Middlewares, signer.WithKey(nil))
			}
			if name == "canceled context" {
				route.Middlewares = append(route.Middlewares, signer.WithKey([]byte("key")), func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						ctx, cancel := context.WithCancel(r.Context())
						cancel()
						next.ServeHTTP(w, r.WithContext(ctx))
					})
				})
			}
			unwound := false
			route.Middlewares = append(route.Middlewares, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer func() { unwound = true }()
					next.ServeHTTP(w, r)
				})
			})
			completions := 0
			app, err := gateway.NewWithConfig(gateway.Config{
				Routes: []gateway.Route{route},
				Extensions: []any{signer, observer{
					response: func() { t.Fatal("response hook ran after signing failed") },
					complete: func(ctx context.Context, rc *gateway.RequestContext, outcome gateway.Outcome) {
						completions++
						if !unwound || outcome.Err == nil || outcome.Aborted {
							t.Fatalf("unexpected completion: unwound=%v outcome=%+v", unwound, outcome)
						}
						if name == "canceled context" && (!errors.Is(outcome.Err, context.Canceled) || ctx.Err() != nil) {
							t.Fatal("cancellation cause or completion entry context was lost")
						}
						if _, ok := rc.Upstream(); !ok {
							t.Fatal("selected upstream missing despite completed rewriting")
						}
					},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader("not forwarded")))
			if w.Code != http.StatusBadGateway || w.Body.String() != "Bad Gateway\n" || received.Load() != 0 || completions != 1 {
				t.Fatalf("status=%d body=%q upstream requests=%d completions=%d", w.Code, w.Body.String(), received.Load(), completions)
			}
		})
	}
}

func TestConcurrentRoutesUseTheirOwnSigningKeys(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := "first key"
		if strings.HasPrefix(r.URL.Path, "/two/") {
			key = "second key"
		}
		if got, want := r.Header.Get("X-Gateway-Signature"), wireSignature(key, r); got != want {
			t.Errorf("request %s: signature=%q want=%q", r.RequestURI, got, want)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	signer := outboundsign.New()
	target := parseURL(t, upstream.URL)
	app, err := gateway.NewWithConfig(gateway.Config{
		Routes: []gateway.Route{
			{Prefix: "/one/", Upstream: target, Middlewares: []gateway.Middleware{signer.WithKey([]byte("first key"))}},
			{Prefix: "/two/", Upstream: target, Middlewares: []gateway.Middleware{signer.WithKey([]byte("second key"))}},
		},
		Extensions: []any{signer}, // This extension implements only the outbound hook.
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prefix := "/one/"
			if i%2 != 0 {
				prefix = "/two/"
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("%sobject?id=%d", prefix, i), nil))
			if w.Code != http.StatusNoContent {
				t.Errorf("request %d: status=%d body=%q", i, w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
}
