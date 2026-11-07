package edgekit_test

import (
	"bytes"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
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

// parseURL returns the parsed rawURL, or stops the test.
func parseURL(tb testing.TB, rawURL string) *url.URL {
	tb.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		tb.Fatalf("parse URL: %v", err)
	}

	return u
}

// static returns a static selector for rawURL.
func static(tb testing.TB, rawURL string) edgekit.UpstreamSelector {
	tb.Helper()

	selector, err := edgekit.StaticUpstream(parseURL(tb, rawURL))
	if err != nil {
		tb.Fatalf("StaticUpstream: %v", err)
	}

	return selector
}

// receive returns the next value from ch, or stops the test after 5s.
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()

	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out while waiting for a signal")

		var zero T

		return zero
	}
}

// problem returns the body that the default renderer writes for status and
// reason.
func problem(status int, reason edgekit.RejectionReason) string {
	body := `{"type":"about:blank"`
	if title := http.StatusText(status); title != "" {
		body += `,"title":"` + title + `"`
	}

	body += `,"status":` + strconv.Itoa(status)
	if reason != "" {
		body += `,"reason":"` + string(reason) + `"`
	}

	return body + "}"
}

// pointerSelector is an UpstreamSelector with a pointer receiver, so a nil
// *pointerSelector is a typed nil selector.
type pointerSelector struct{}

// SelectUpstream always fails.
func (*pointerSelector) SelectUpstream(*http.Request) (*url.URL, error) {
	return nil, errors.New("not used")
}

// upstreamRequest is what a test upstream received.
type upstreamRequest struct {
	method     string
	requestURI string
	header     http.Header
	body       string
	readErr    error
}

func TestProxyForwardsRequestAndAppliesRouteMiddleware(t *testing.T) {
	received := make(chan upstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		received <- upstreamRequest{
			method:     r.Method,
			requestURI: r.RequestURI,
			header:     r.Header.Clone(),
			body:       string(body),
			readErr:    err,
		}
		_, _ = io.WriteString(w, "upstream response")
	}))
	defer upstream.Close()

	routeMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Route-Middleware", "ran")
			next.ServeHTTP(w, r)
		})
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:     "/api/",
				Upstream:   static(t, upstream.URL),
				Middleware: []edgekit.Middleware{routeMiddleware},
			},
		},
	})

	r := httptest.NewRequest(http.MethodPost, "/api/hello?q=1", strings.NewReader("payload"))
	r.Header.Set("X-Test", "value")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	got := receive(t, received)
	requestLineOK := got.method == http.MethodPost && got.requestURI == "/api/hello?q=1"
	bodyOK := got.body == "payload" && got.readErr == nil
	if !requestLineOK || !bodyOK {
		t.Errorf("upstream got %s %s with body %q, %v; want POST /api/hello?q=1 with body %q, <nil>",
			got.method, got.requestURI, got.body, got.readErr, "payload")
	}

	if got.header.Get("X-Test") != "value" {
		t.Errorf("upstream got X-Test %q, want %q", got.header.Get("X-Test"), "value")
	}

	if w.Code != http.StatusOK || w.Body.String() != "upstream response" {
		t.Errorf("response = %d %q, want 200 %q", w.Code, w.Body.String(), "upstream response")
	}

	if got := w.Header().Get("X-Route-Middleware"); got != "ran" {
		t.Errorf("response X-Route-Middleware = %q, want %q", got, "ran")
	}
}

func TestGlobalAndRouteMiddlewareOrder(t *testing.T) {
	var (
		mu    sync.Mutex
		steps []string
	)
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()

		steps = append(steps, step)
	}
	wrap := func(name string) edgekit.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record(name + ":before")
				next.ServeHTTP(w, r)
				record(name + ":after")
			})
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		record("upstream")
	}))
	defer upstream.Close()

	selector := static(t, upstream.URL)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix: "/api/",
				Upstream: edgekit.UpstreamSelectorFunc(func(r *http.Request) (*url.URL, error) {
					record("select")

					return selector.SelectUpstream(r)
				}),
				Middleware: []edgekit.Middleware{wrap("route1"), wrap("route2")},
			},
		},
		Middleware: []edgekit.Middleware{wrap("global1"), wrap("global2")},
	})

	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/example", nil))

	want := []string{
		"global1:before",
		"global2:before",
		"route1:before",
		"route2:before",
		"select",
		"upstream",
		"route2:after",
		"route1:after",
		"global2:after",
		"global1:after",
	}
	mu.Lock()
	got := slices.Clone(steps)
	mu.Unlock()

	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestGlobalMiddlewareRunsForRouteMiss(t *testing.T) {
	ran := false
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.invalid"),
			},
		},
		Middleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ran = true
				next.ServeHTTP(w, r)
			})
		}},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/other", nil))

	if !ran {
		t.Error("global middleware ran = false, want true")
	}

	if want := problem(http.StatusNotFound, edgekit.ReasonNoRoute); w.Code != http.StatusNotFound || w.Body.String() != want {
		t.Errorf("response = %d %q, want 404 %q", w.Code, w.Body.String(), want)
	}
}

// routingGateway returns a gateway with overlapping and escaped route prefixes
// to upstreamURL.
func routingGateway(t *testing.T, upstreamURL string) *edgekit.Gateway {
	t.Helper()

	return newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstreamURL+"/short"),
			},
			{
				Prefix:   "/api/v2/",
				Upstream: static(t, upstreamURL+"/long/"),
			},
			{
				Prefix:   "/caf%C3%A9/",
				Upstream: static(t, upstreamURL),
			},
		},
	})
}

func TestRouteMatching(t *testing.T) {
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- r.RequestURI
	}))
	defer upstream.Close()

	g := routingGateway(t, upstream.URL)

	for _, tc := range []struct {
		name   string
		target string
		want   string
	}{
		{
			name:   "prefix only",
			target: "/api/",
			want:   "/short/api/",
		},
		{
			name:   "below prefix",
			target: "/api/users?id=1",
			want:   "/short/api/users?id=1",
		},
		{
			name:   "longest prefix wins",
			target: "/api/v2/users",
			want:   "/long/api/v2/users",
		},
		{
			name:   "longer prefix without slash",
			target: "/api/v2",
			want:   "/short/api/v2",
		},
		{
			name:   "escaped path is preserved",
			target: "/api/a%20b",
			want:   "/short/api/a%20b",
		},
		{
			name:   "encoded percent is preserved",
			target: "/api/100%25",
			want:   "/short/api/100%25",
		},
		{
			name:   "empty segments are preserved",
			target: "/api//x",
			want:   "/short/api//x",
		},
		{
			name:   "escaped prefix",
			target: "/caf%C3%A9/menu",
			want:   "/caf%C3%A9/menu",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.target, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", tc.target, w.Code)
			}

			if got := receive(t, received); got != tc.want {
				t.Errorf("GET %s upstream request URI = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

func TestRouteMiss(t *testing.T) {
	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded.Add(1)
	}))
	defer upstream.Close()

	g := routingGateway(t, upstream.URL)

	for _, tc := range []struct {
		name   string
		target string
	}{
		{
			name:   "prefix without slash",
			target: "/api",
		},
		{
			name:   "prefix as part of a segment",
			target: "/apix/users",
		},
		{
			name:   "case differs",
			target: "/API/users",
		},
		{
			name:   "root",
			target: "/",
		},
		{
			name:   "double leading slash",
			target: "//api/x",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.target, nil))

			if w.Code != http.StatusNotFound {
				t.Errorf("GET %s status = %d, want 404", tc.target, w.Code)
			}
		})
	}

	if got := forwarded.Load(); got != 0 {
		t.Errorf("upstream got %d requests, want 0", got)
	}
}

func TestCustomUpstreamSelector(t *testing.T) {
	newUpstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, name)
		}))
	}
	blue, green := newUpstream("blue"), newUpstream("green")
	defer blue.Close()
	defer green.Close()

	blueURL, greenURL := parseURL(t, blue.URL), parseURL(t, green.URL)
	selector := edgekit.UpstreamSelectorFunc(func(r *http.Request) (*url.URL, error) {
		switch r.Header.Get("X-Pool") {
		case "blue":
			return blueURL, nil
		case "green":
			return greenURL, nil
		case "credentials":
			return url.Parse("http://user:secret@" + blueURL.Host)
		case "query":
			return url.Parse(blue.URL + "?q=1")
		case "nil":
			var none *url.URL

			return none, nil
		}

		return nil, errors.New("private selector detail")
	})
	// Route middleware sets the data that the selector reads.
	pool := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.Clone(r.Context())
			r.Header.Set("X-Pool", r.URL.Query().Get("pool"))
			next.ServeHTTP(w, r)
		})
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:     "/api/",
				Upstream:   selector,
				Middleware: []edgekit.Middleware{pool},
			},
		},
	})

	for _, tc := range []struct {
		pool   string
		status int
		body   string
	}{
		{
			pool:   "blue",
			status: http.StatusOK,
			body:   "blue",
		},
		{
			pool:   "green",
			status: http.StatusOK,
			body:   "green",
		},
		{
			pool:   "credentials",
			status: http.StatusInternalServerError,
			body:   problem(http.StatusInternalServerError, ""),
		},
		{
			pool:   "query",
			status: http.StatusInternalServerError,
			body:   problem(http.StatusInternalServerError, ""),
		},
		{
			pool:   "nil",
			status: http.StatusInternalServerError,
			body:   problem(http.StatusInternalServerError, ""),
		},
		{
			pool:   "error",
			status: http.StatusInternalServerError,
			body:   problem(http.StatusInternalServerError, ""),
		},
	} {
		t.Run(tc.pool, func(t *testing.T) {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x?pool="+tc.pool, nil))

			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Errorf("response = %d %q, want %d %q", w.Code, w.Body.String(), tc.status, tc.body)
			}
		})
	}

	if blueURL.String() != blue.URL || greenURL.String() != green.URL {
		t.Errorf("selected URLs = %s, %s; want %s, %s", blueURL, greenURL, blue.URL, green.URL)
	}
}

func TestStaticUpstreamAcceptsValidURL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream *url.URL
	}{
		{
			name:     "http",
			upstream: &url.URL{Scheme: "http", Host: "upstream.test"},
		},
		{
			name:     "https with port",
			upstream: &url.URL{Scheme: "https", Host: "upstream.test:8443"},
		},
		{
			name:     "base path",
			upstream: &url.URL{Scheme: "http", Host: "upstream.test", Path: "/base"},
		},
		{
			name:     "Punycode name",
			upstream: &url.URL{Scheme: "http", Host: "xn--bcher-kva.example"},
		},
		{
			name:     "underscore and upper case",
			upstream: &url.URL{Scheme: "http", Host: "Upstream_1.internal:8080"},
		},
		{
			name:     "IPv6 literal",
			upstream: &url.URL{Scheme: "http", Host: "[2001:db8::1]:8080"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector, err := edgekit.StaticUpstream(tc.upstream)

			if err != nil || selector == nil {
				t.Fatalf("StaticUpstream(%s) = %v, %v; want a selector, <nil>", tc.upstream, selector, err)
			}
		})
	}
}

func TestStaticUpstreamRejectsInvalidURL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream *url.URL
	}{
		{
			name:     "nil",
			upstream: nil,
		},
		{
			name:     "other scheme",
			upstream: &url.URL{Scheme: "ftp", Host: "upstream.test"},
		},
		{
			name:     "no scheme",
			upstream: &url.URL{Host: "upstream.test"},
		},
		{
			name:     "no host",
			upstream: &url.URL{Scheme: "http", Path: "/base"},
		},
		{
			name:     "opaque",
			upstream: &url.URL{Scheme: "http", Opaque: "secret"},
		},
		{
			name:     "user information",
			upstream: &url.URL{Scheme: "http", Host: "upstream.test", User: url.UserPassword("user", "secret")},
		},
		{
			name:     "query",
			upstream: &url.URL{Scheme: "http", Host: "upstream.test", RawQuery: "token=secret"},
		},
		{
			name:     "empty query",
			upstream: &url.URL{Scheme: "http", Host: "upstream.test", ForceQuery: true},
		},
		{
			name:     "fragment",
			upstream: &url.URL{Scheme: "http", Host: "upstream.test", Fragment: "secret"},
		},
		{
			name:     "relative path",
			upstream: &url.URL{Scheme: "http", Host: "upstream.test", Path: "secret"},
		},
		{
			name:     "internationalized name",
			upstream: &url.URL{Scheme: "http", Host: "secret-bücher.example"},
		},
		{
			name:     "IPv6 zone",
			upstream: &url.URL{Scheme: "http", Host: "[fe80::1%secret]:8080"},
		},
		{
			name:     "space in host",
			upstream: &url.URL{Scheme: "http", Host: "upstream.test secret"},
		},
		{
			name:     "slash in host",
			upstream: &url.URL{Scheme: "http", Host: "upstream.test/secret"},
		},
		{
			name:     "control character in host",
			upstream: &url.URL{Scheme: "http", Host: "upstream.test\rsecret"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector, err := edgekit.StaticUpstream(tc.upstream)

			if !errors.Is(err, edgekit.ErrInvalidConfiguration) || selector != nil {
				t.Fatalf("StaticUpstream = %v, %v; want nil, ErrInvalidConfiguration", selector, err)
			}

			if strings.Contains(err.Error(), "secret") {
				t.Errorf("StaticUpstream error = %q, want no configuration value", err)
			}
		})
	}
}

func TestInvalidConfiguration(t *testing.T) {
	upstream := static(t, "http://upstream.test")
	route := func(prefix string) edgekit.Route {
		return edgekit.Route{
			Prefix:   prefix,
			Upstream: upstream,
		}
	}
	nilHandler := func(http.Handler) http.Handler { return nil }
	passThrough := func(next http.Handler) http.Handler { return next }
	compressing := edgekit.NewTransport()
	compressing.DisableCompression = false

	// The helpers return a valid configuration with one change.
	withRoutes := func(routes ...edgekit.Route) edgekit.Config {
		return edgekit.Config{
			Routes: routes,
		}
	}
	withMiddleware := func(middleware ...edgekit.Middleware) edgekit.Config {
		return edgekit.Config{
			Routes:     []edgekit.Route{route("/api/")},
			Middleware: middleware,
		}
	}
	withRouteMiddleware := func(middleware ...edgekit.Middleware) edgekit.Config {
		r := route("/api/")
		r.Middleware = middleware

		return withRoutes(r)
	}
	withTransport := func(transport http.RoundTripper) edgekit.Config {
		return edgekit.Config{
			Routes:    []edgekit.Route{route("/api/")},
			Transport: transport,
		}
	}
	withManagedHeaders := func(names ...string) edgekit.Config {
		return edgekit.Config{
			Routes:         []edgekit.Route{route("/api/")},
			ManagedHeaders: names,
		}
	}

	// location is the start of the error text after the sentinel, when a case
	// checks it.
	for _, tc := range []struct {
		name     string
		cfg      edgekit.Config
		location string
	}{
		{
			name: "no routes",
			cfg:  edgekit.Config{},
		},
		{
			name: "prefix without leading slash",
			cfg:  withRoutes(route("api/")),
		},
		{
			name: "prefix without trailing slash",
			cfg:  withRoutes(route("/api")),
		},
		{
			name: "empty prefix",
			cfg:  withRoutes(route("")),
		},
		{
			name: "prefix not escaped",
			cfg:  withRoutes(route("/a secret/")),
		},
		{
			name: "prefix with dot-segment",
			cfg:  withRoutes(route("/api/../")),
		},
		{
			name: "prefix with encoded slash",
			cfg:  withRoutes(route("/a%2Fb/")),
		},
		{
			name: "duplicate prefix",
			cfg:  withRoutes(route("/api/"), route("/api/")),
		},
		{
			name: "nil upstream",
			cfg:  withRoutes(edgekit.Route{Prefix: "/api/"}),
		},
		{
			name: "nil upstream function",
			cfg:  withRoutes(edgekit.Route{Prefix: "/api/", Upstream: edgekit.UpstreamSelectorFunc(nil)}),
		},
		{
			name: "nil upstream pointer",
			cfg:  withRoutes(edgekit.Route{Prefix: "/api/", Upstream: (*pointerSelector)(nil)}),
		},
		{
			name:     "nil route middleware",
			cfg:      withRouteMiddleware(passThrough, nil),
			location: "route 0: middleware 1:",
		},
		{
			name:     "route middleware returns nil",
			cfg:      withRouteMiddleware(passThrough, nilHandler),
			location: "route 0: middleware 1:",
		},
		{
			name:     "nil global middleware",
			cfg:      withMiddleware(passThrough, nil),
			location: "middleware 1:",
		},
		{
			name:     "global middleware returns nil",
			cfg:      withMiddleware(passThrough, nilHandler),
			location: "middleware 1:",
		},
		{
			name: "compressing transport",
			cfg:  withTransport(compressing),
		},
		{
			name: "nil transport pointer",
			cfg:  withTransport((*http.Transport)(nil)),
		},
		{
			name: "nil transport function",
			cfg:  withTransport(roundTripperFunc(nil)),
		},
		{
			name: "empty managed header",
			cfg:  withManagedHeaders(""),
		},
		{
			name: "managed header with underscore",
			cfg:  withManagedHeaders("X_Secret"),
		},
		{
			name: "managed header with space",
			cfg:  withManagedHeaders("X Secret"),
		},
		{
			name: "duplicate managed header",
			cfg:  withManagedHeaders("X-User", "x-user"),
		},
		{
			name: "hop-by-hop managed header",
			cfg:  withManagedHeaders("Connection"),
		},
		{
			name: "framing managed header",
			cfg:  withManagedHeaders("content-length"),
		},
		{
			name: "Host managed header",
			cfg:  withManagedHeaders("Host"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := edgekit.New(tc.cfg)

			if !errors.Is(err, edgekit.ErrInvalidConfiguration) || g != nil {
				t.Fatalf("New = %v, %v; want ErrInvalidConfiguration", g, err)
			}

			if strings.Contains(err.Error(), "secret") {
				t.Errorf("New error = %q, want no configuration value", err)
			}

			if !strings.HasPrefix(err.Error(), "invalid configuration: "+tc.location) {
				t.Errorf("New error = %q, want location %q", err, tc.location)
			}
		})
	}
}

func TestMiddlewareIsCalledOnceForValidConfiguration(t *testing.T) {
	var calls atomic.Int32
	counted := func(next http.Handler) http.Handler {
		calls.Add(1)

		return next
	}

	newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:     "/api/",
				Upstream:   static(t, "http://upstream.test"),
				Middleware: []edgekit.Middleware{counted},
			},
		},
		Middleware: []edgekit.Middleware{counted},
	})

	if got := calls.Load(); got != 2 {
		t.Errorf("New called middleware functions %d times, want 2", got)
	}
}

func TestMiddlewareIsNotCalledForStructuralProblem(t *testing.T) {
	var calls atomic.Int32
	counted := func(next http.Handler) http.Handler {
		calls.Add(1)

		return next
	}

	// A missing upstream selector is a structural problem, so New must find
	// it before it calls a middleware function.
	_, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:     "/api/",
				Middleware: []edgekit.Middleware{counted},
			},
		},
		Middleware: []edgekit.Middleware{counted},
	})

	if !errors.Is(err, edgekit.ErrInvalidConfiguration) {
		t.Fatalf("New error = %v, want ErrInvalidConfiguration", err)
	}

	if got := calls.Load(); got != 0 {
		t.Errorf("New called middleware functions %d times, want 0", got)
	}
}

func TestInvalidConfigurationReportsAllProblems(t *testing.T) {
	_, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "api/",
				Upstream: static(t, "http://upstream.test"),
			},
			{
				Prefix: "/other/",
			},
		},
		ManagedHeaders: []string{"X_User"},
	})

	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		t.Fatalf("New error = %v, want a joined error", err)
	}

	problems := joined.Unwrap()
	if len(problems) != 3 {
		t.Fatalf("New reported %d problems, want 3: %v", len(problems), err)
	}

	for _, problem := range problems {
		if !errors.Is(problem, edgekit.ErrInvalidConfiguration) {
			t.Errorf("problem %v does not wrap ErrInvalidConfiguration", problem)
		}
	}
}

func TestConfigurationIsCopied(t *testing.T) {
	received := make(chan upstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- upstreamRequest{
			requestURI: r.RequestURI,
			header:     r.Header.Clone(),
		}
	}))
	defer upstream.Close()

	target := parseURL(t, upstream.URL+"/base")
	selector, err := edgekit.StaticUpstream(target)
	if err != nil {
		t.Fatal(err)
	}

	cfg := edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: selector,
			},
		},
		ManagedHeaders: []string{"X-User"},
	}
	g := newGateway(t, cfg)

	target.Path = "/changed"
	cfg.Routes[0].Prefix = "/changed/"
	cfg.ManagedHeaders[0] = "X-Other"

	r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	r.Header.Set("X-User", "spoofed")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	got := receive(t, received)
	if got.requestURI != "/base/api/x" {
		t.Errorf("upstream request URI = %q, want %q", got.requestURI, "/base/api/x")
	}

	if got.header.Get("X-User") != "" {
		t.Errorf("upstream X-User = %q, want none", got.header.Values("X-User"))
	}
}

func TestGatewayConstructionDoesNotReconfigureProcessLogging(t *testing.T) {
	previousLogger, previousWriter := slog.Default(), log.Writer()
	previousFlags, previousPrefix := log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})

	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	slog.SetDefault(logger)
	writer := log.Writer()

	newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
	})

	if slog.Default() != logger || log.Writer() != writer {
		t.Error("New changed the default slog logger or the log writer, want no change")
	}

	if output.Len() != 0 {
		t.Errorf("New wrote log output %q, want none", output.String())
	}
}

func TestInvalidHeaderOwners(t *testing.T) {
	var typedNil *headerOwner

	for _, tc := range []struct {
		name     string
		owner    edgekit.HeaderOwner
		location string
	}{
		{
			name:     "nil",
			owner:    nil,
			location: "header owners 0: is nil",
		},
		{
			name:     "typed nil",
			owner:    typedNil,
			location: "header owners 0: is nil",
		},
		{
			name:     "nil slice",
			owner:    headerOwner(nil),
			location: "header owners 0: is nil",
		},
		{
			name:     "managed header alias",
			owner:    headerOwner{"X_Customer"},
			location: "header owners 0: managed header 0:",
		},
		{
			name:     "managed hop-by-hop header",
			owner:    headerOwner{"Connection"},
			location: "header owners 0: managed header 0:",
		},
		{
			name:     "duplicate in one declaration",
			owner:    headerOwner{"X-Customer", "x-customer"},
			location: "header owners 0: managed header 1: is a duplicate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := edgekit.New(edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
					},
				},
				HeaderOwners: []edgekit.HeaderOwner{
					tc.owner,
				},
			})

			if !errors.Is(err, edgekit.ErrInvalidConfiguration) {
				t.Fatalf("New error = %v, want %v", err, edgekit.ErrInvalidConfiguration)
			}

			if !strings.Contains(err.Error(), tc.location) {
				t.Errorf("New error = %v, want location %q", err, tc.location)
			}

			if strings.Contains(strings.ToLower(err.Error()), "customer") {
				t.Errorf("New error %q contains the header name", err)
			}
		})
	}
}

func TestHeaderOwnersAccepted(t *testing.T) {
	// Declarations do not need lifecycle hooks and can overlap with each
	// other and with Config.ManagedHeaders.
	_, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		ManagedHeaders: []string{"X-Customer"},
		HeaderOwners: []edgekit.HeaderOwner{
			headerOwner{"X-Customer"},
			headerOwner{"x-customer", "X-Key-Id"},
		},
	})
	if err != nil {
		t.Errorf("New: %v", err)
	}
}
