package edgekit_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

func TestSecureDefaultsRejectBeforeMiddleware(t *testing.T) {
	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded.Add(1)
	}))
	defer upstream.Close()

	var entered atomic.Int32
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		Middleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered.Add(1)
				next.ServeHTTP(w, r)
			})
		}},
	})

	for _, tc := range []struct {
		name    string
		method  string
		target  string
		header  http.Header
		trailer http.Header
		status  int
	}{
		// Request targets.
		{
			name:   "origin form",
			target: "/api/x",
			status: http.StatusOK,
		},
		{
			name:   "absolute form",
			target: "http://upstream.test/api/x",
			status: http.StatusBadRequest,
		},
		{
			name:   "absolute form with user information",
			target: "http://user@upstream.test/api/x",
			status: http.StatusBadRequest,
		},
		{
			name:   "asterisk form",
			method: http.MethodOptions,
			target: "*",
			status: http.StatusBadRequest,
		},
		{
			name:   "opaque",
			target: "urn:api:x",
			status: http.StatusBadRequest,
		},
		{
			name:   "CONNECT",
			method: http.MethodConnect,
			target: "upstream.test:443",
			status: http.StatusNotImplemented,
		},
		{
			name:   "CONNECT with path",
			method: http.MethodConnect,
			target: "/api/x",
			status: http.StatusNotImplemented,
		},
		// Path policy.
		{
			name:   "dot-dot segment",
			target: "/api/../admin",
			status: http.StatusBadRequest,
		},
		{
			name:   "dot segment",
			target: "/api/./x",
			status: http.StatusBadRequest,
		},
		{
			name:   "trailing dot-dot segment",
			target: "/api/..",
			status: http.StatusBadRequest,
		},
		{
			name:   "encoded dot-dot segment",
			target: "/api/%2e%2e/admin",
			status: http.StatusBadRequest,
		},
		{
			name:   "upper-case encoded dot",
			target: "/api/%2E/x",
			status: http.StatusBadRequest,
		},
		{
			name:   "half-encoded dot-dot segment",
			target: "/api/.%2e/admin",
			status: http.StatusBadRequest,
		},
		{
			name:   "encoded dot in a name",
			target: "/api/file%2ejson",
			status: http.StatusBadRequest,
		},
		{
			name:   "encoded slash",
			target: "/api/a%2fb",
			status: http.StatusBadRequest,
		},
		{
			name:   "upper-case encoded slash",
			target: "/api/a%2Fb",
			status: http.StatusBadRequest,
		},
		{
			name:   "encoded backslash",
			target: "/api/a%5cb",
			status: http.StatusBadRequest,
		},
		{
			name:   "backslash",
			target: "/api/a\\b",
			status: http.StatusBadRequest,
		},
		{
			name:   "encoded NUL",
			target: "/api/a%00b",
			status: http.StatusBadRequest,
		},
		{
			name:   "encoded number sign",
			target: "/api/a%23b",
			status: http.StatusBadRequest,
		},
		{
			name:   "number sign",
			target: "/api/a#b",
			status: http.StatusBadRequest,
		},
		{
			name:   "dots in a name",
			target: "/api/x..y/.hidden/file.json",
			status: http.StatusOK,
		},
		{
			name:   "encoded percent",
			target: "/api/%25",
			status: http.StatusOK,
		},
		{
			name:   "encoded double-encoded dot",
			target: "/api/%252e%252e/x",
			status: http.StatusOK,
		},
		{
			name:   "encoded space",
			target: "/api/a%20b",
			status: http.StatusOK,
		},
		// Query.
		{
			name:   "query semicolon",
			target: "/api/x?a=1;b=2",
			status: http.StatusBadRequest,
		},
		{
			name:   "query with only a semicolon",
			target: "/api/x?;",
			status: http.StatusBadRequest,
		},
		{
			name:   "encoded query semicolon",
			target: "/api/x?a=1%3Bb=2",
			status: http.StatusOK,
		},
		{
			name:   "empty query",
			target: "/api/x?",
			status: http.StatusOK,
		},
		{
			name:   "query percent at end",
			target: "/api/x?a=%",
			status: http.StatusBadRequest,
		},
		{
			name:   "query percent with one digit",
			target: "/api/x?a=%2",
			status: http.StatusBadRequest,
		},
		{
			name:   "query percent with non-hex digits",
			target: "/api/x?b=hello%20world&bad=%zz&a=1",
			status: http.StatusBadRequest,
		},
		{
			name:   "query percent with one non-hex digit",
			target: "/api/x?a=%2G",
			status: http.StatusBadRequest,
		},
		{
			name:   "malformed escape in query key",
			target: "/api/x?%zz=1",
			status: http.StatusBadRequest,
		},
		{
			name:   "query with too many parameters",
			target: "/api/x?" + strings.Repeat("a=1&", 10000) + "a=1",
			status: http.StatusBadRequest,
		},
		{
			name:   "query with most parameters",
			target: "/api/x?" + strings.Repeat("a=1&", 9999) + "a=1",
			status: http.StatusOK,
		},
		// Header names.
		{
			name:   "underscore alias",
			target: "/api/x",
			header: http.Header{"X_Forwarded_For": {"value"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "underscore name",
			target: "/api/x",
			header: http.Header{"X_Custom": {"value"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "dot in name",
			target: "/api/x",
			header: http.Header{"X.Custom": {"value"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "other token character",
			target: "/api/x",
			header: http.Header{"X-Custom~": {"value"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "empty name",
			target: "/api/x",
			header: http.Header{"": {"value"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "letters digits and dashes",
			target: "/api/x",
			header: http.Header{"X-Custom-2": {"value"}},
			status: http.StatusOK,
		},
		{
			name:    "underscore trailer name",
			target:  "/api/x",
			trailer: http.Header{"X_User": nil},
			status:  http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}

			r := httptest.NewRequest(method, tc.target, nil)
			maps.Copy(r.Header, tc.header)
			r.Trailer = tc.trailer

			beforeEntry, beforeForward := entered.Load(), forwarded.Load()
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)

			if w.Code != tc.status {
				t.Fatalf("%s %s status = %d, want %d", method, tc.target, w.Code, tc.status)
			}

			if tc.status == http.StatusOK {
				if got := forwarded.Load() - beforeForward; got != 1 {
					t.Errorf("upstream got %d requests, want 1", got)
				}

				return
			}

			if w.Body.String() != http.StatusText(tc.status)+"\n" {
				t.Errorf("body = %q, want only the status text", w.Body.String())
			}

			entries, forwards := entered.Load()-beforeEntry, forwarded.Load()-beforeForward
			if entries != 0 || forwards != 0 {
				t.Errorf("middleware ran %d times and upstream got %d requests, want 0 and 0", entries, forwards)
			}
		})
	}
}

func TestRequestFromOutsideServerNeedsOriginForm(t *testing.T) {
	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded.Add(1)
	}))
	defer upstream.Close()

	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
	})

	// Requests that an embedder makes have no RequestURI.
	for _, tc := range []struct {
		name   string
		url    *url.URL
		status int
	}{
		{
			name:   "path only",
			url:    &url.URL{Path: "/api/x"},
			status: http.StatusOK,
		},
		{
			name:   "absolute URL",
			url:    &url.URL{Scheme: "http", Host: "upstream.test", Path: "/api/x"},
			status: http.StatusBadRequest,
		},
		{
			name:   "host only",
			url:    &url.URL{Host: "upstream.test", Path: "/api/x"},
			status: http.StatusBadRequest,
		},
		{
			name:   "relative path",
			url:    &url.URL{Path: "api/x"},
			status: http.StatusBadRequest,
		},
		{
			name:   "empty path",
			url:    &url.URL{},
			status: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{
				Method: http.MethodGet,
				URL:    tc.url,
				Header: make(http.Header),
				Host:   "gateway.test",
			}
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)

			if w.Code != tc.status {
				t.Errorf("status = %d, want %d", w.Code, tc.status)
			}
		})
	}

	if forwarded.Load() != 1 {
		t.Errorf("upstream got %d requests, want 1", forwarded.Load())
	}
}

func TestForwardingTrustAndHopByHopHeaders(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name, proto := "http", "http"
		if secure {
			name, proto = "https", "https"
		}

		t.Run(name, func(t *testing.T) {
			seen := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()
				w.Header().Add("Connection", "X-Response-Hop")
				w.Header().Add("Connection", "Keep-Alive")
				w.Header().Set("X-Response-Hop", "private")
				w.Header().Set("Keep-Alive", "timeout=5")
				w.Header().Set("Proxy-Authenticate", "private")
				w.Header().Set("Alt-Svc", `h3=":443"`)
				w.Header().Set("X-End-To-End", "response")
				_, _ = io.WriteString(w, "ok")
			}))
			defer upstream.Close()

			var middlewareSaw http.Header
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, upstream.URL),
				}},
				ManagedHeaders: []string{"x-user"},
				Middleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						middlewareSaw = r.Header.Clone()
						r = r.Clone(r.Context())
						r.Header.Set("X-User", "authenticated")
						next.ServeHTTP(w, r)
					})
				}},
			})

			r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			r.Host = "client.example"
			r.RemoteAddr = "192.0.2.10:12345"
			if secure {
				r.TLS = &tls.ConnectionState{}
			}

			removed := []string{
				"Forwarded",
				"X-Forwarded-For",
				"X-Forwarded-Host",
				"X-Forwarded-Proto",
				"X-Forwarded-Port",
				"X-Forwarded-Prefix",
				"X-Forwarded-Uri",
				"X-Forwarded-Tls-Client-Cert",
				"X-Real-Ip",
				"X-Client-Ip",
				"True-Client-Ip",
				"X-Cluster-Client-Ip",
				"X-Original-Forwarded-For",
				"Cf-Connecting-Ip",
				"Fastly-Client-Ip",
				"X-Original-Url",
				"X-Rewrite-Url",
				"X-User",
			}
			for _, name := range removed {
				r.Header.Add(name, "spoof-one")
				r.Header.Add(name, "spoof-two")
			}

			// A client must not be able to remove a managed header with
			// Connection after middleware set it.
			r.Header.Add("Connection", "X-Request-Hop, X-Forwarded-For, x-user")
			r.Header.Add("Connection", "Keep-Alive")
			r.Header.Set("X-Request-Hop", "private")
			r.Header.Set("Keep-Alive", "timeout=5")
			r.Header.Set("Proxy-Authorization", "private")
			r.Header.Set("Proxy-Connection", "keep-alive")
			r.Header.Set("TE", "gzip, trailers")
			r.Header.Set("X-End-To-End", "request")
			original := r.Header.Clone()

			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			got := receive(t, seen)

			for _, name := range removed {
				if middlewareSaw.Get(name) != "" {
					t.Errorf("middleware saw %s %q, want none", name, middlewareSaw.Values(name))
				}
			}

			if !slices.Equal(middlewareSaw.Values("Connection"), []string{"X-Request-Hop", "Keep-Alive"}) {
				t.Errorf("middleware saw Connection %q, want %q", middlewareSaw.Values("Connection"), []string{"X-Request-Hop", "Keep-Alive"})
			}

			for _, name := range removed {
				switch name {
				case "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-User":
					continue
				}

				if got.Get(name) != "" {
					t.Errorf("upstream got %s %q, want none", name, got.Values(name))
				}
			}

			hopByHop := []string{
				"Connection",
				"X-Request-Hop",
				"Keep-Alive",
				"Proxy-Authorization",
				"Proxy-Connection",
			}
			for _, name := range hopByHop {
				if got.Get(name) != "" {
					t.Errorf("upstream got hop-by-hop header %s %q, want none", name, got.Values(name))
				}
			}

			for name, want := range map[string]string{
				"X-Forwarded-For":   "192.0.2.10",
				"X-Forwarded-Host":  "client.example",
				"X-Forwarded-Proto": proto,
				"X-User":            "authenticated",
				"TE":                "trailers",
				"X-End-To-End":      "request",
			} {
				if !slices.Equal(got.Values(name), []string{want}) {
					t.Errorf("upstream got %s %q, want only %q", name, got.Values(name), want)
				}
			}

			responseRemoved := []string{
				"Connection",
				"X-Response-Hop",
				"Keep-Alive",
				"Proxy-Authenticate",
				"Alt-Svc",
			}
			for _, name := range responseRemoved {
				if w.Header().Get(name) != "" {
					t.Errorf("client got %s %q, want none", name, w.Header().Values(name))
				}
			}

			if w.Code != http.StatusOK || w.Body.String() != "ok" || w.Header().Get("X-End-To-End") != "response" {
				t.Errorf("response = %d %q, X-End-To-End %q; want 200 %q, %q", w.Code, w.Body.String(), w.Header().Get("X-End-To-End"), "ok", "response")
			}

			for name, values := range original {
				if !slices.Equal(r.Header[name], values) {
					t.Errorf("inbound header %s = %q after ServeHTTP, want %q", name, r.Header[name], values)
				}
			}

			if len(r.Header) != len(original) {
				t.Errorf("inbound header count = %d after ServeHTTP, want %d", len(r.Header), len(original))
			}
		})
	}
}

func TestConnectionCannotRemoveManagedHeader(t *testing.T) {
	received := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
	}))
	defer upstream.Close()

	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		ManagedHeaders: []string{"X-User"},
		Middleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r = r.Clone(r.Context())
				r.Header.Set("X-User", "authenticated")
				next.ServeHTTP(w, r)
			})
		}},
	})

	// No case sends a removed header, so only the Connection token can make
	// the gateway copy the headers.
	for _, tc := range []struct {
		name       string
		connection []string
	}{
		{
			name:       "token after another token",
			connection: []string{"close, X-User"},
		},
		{
			name:       "only token",
			connection: []string{"x-user"},
		},
		{
			name:       "token in second value",
			connection: []string{"keep-alive", "X-User"},
		},
		{
			name:       "token with spaces",
			connection: []string{" X-User ,keep-alive"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			r.Header["Connection"] = tc.connection
			g.ServeHTTP(httptest.NewRecorder(), r)

			got := receive(t, received)
			if !slices.Equal(got.Values("X-User"), []string{"authenticated"}) {
				t.Errorf("upstream got X-User %q, want only %q", got.Values("X-User"), "authenticated")
			}
		})
	}
}

func TestRequestTrailersCannotCarryRemovedHeaders(t *testing.T) {
	received := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- r.Trailer.Clone()
	}))
	defer upstream.Close()

	server := httptest.NewServer(newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		ManagedHeaders: []string{"X-User"},
	}))
	defer server.Close()

	var dialer net.Dialer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context has no deadline")
	}

	if deadlineErr := conn.SetDeadline(deadline); deadlineErr != nil {
		t.Fatal(deadlineErr)
	}

	// The client declares the trailers in the Trailer header, so the server
	// knows their names before the handler runs.
	request := "POST /api/x HTTP/1.1\r\n" +
		"Host: gateway.test\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Trailer: X-User, X-Forwarded-For, X-Checksum\r\n" +
		"\r\n" +
		"3\r\n" +
		"abc\r\n" +
		"0\r\n" +
		"X-User: spoofed\r\n" +
		"X-Forwarded-For: 192.0.2.1\r\n" +
		"X-Checksum: abc\r\n" +
		"\r\n"
	if _, err = io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}

	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	got := receive(t, received)
	for _, name := range []string{"X-User", "X-Forwarded-For"} {
		if _, ok := got[name]; ok {
			t.Errorf("upstream got trailer %s %q, want none", name, got.Values(name))
		}
	}

	if _, ok := got["X-Checksum"]; !ok {
		t.Errorf("upstream trailers = %v, want X-Checksum", got)
	}
}

func TestGatewayDoesNotChangeInboundRequest(t *testing.T) {
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("X-User")
	}))
	defer upstream.Close()

	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		ManagedHeaders: []string{"X-User"},
		Middleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r = r.Clone(r.Context())
				r.Header.Set("X-User", "authenticated")
				r.URL.Path += "/changed"
				next.ServeHTTP(w, r)
			})
		}},
	})

	// The cases cover the requests with and without headers that the gateway
	// removes.
	for _, tc := range []struct {
		name    string
		header  http.Header
		trailer http.Header
	}{
		{
			name:   "no removed headers",
			header: http.Header{"X-End-To-End": {"value"}},
		},
		{
			name:   "identity header",
			header: http.Header{"X-Forwarded-For": {"192.0.2.1"}},
		},
		{
			name:   "managed header",
			header: http.Header{"X-User": {"spoofed"}},
		},
		{
			name:   "Connection token",
			header: http.Header{"Connection": {"close, X-User"}},
		},
		{
			name:    "managed trailer",
			header:  http.Header{},
			trailer: http.Header{"X-User": nil, "X-Checksum": nil},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/x?q=1", nil)
			r.Header = tc.header.Clone()
			r.Trailer = tc.trailer.Clone()
			original := r.Header.Clone()
			originalTrailer := r.Trailer.Clone()
			originalURL := *r.URL

			g.ServeHTTP(httptest.NewRecorder(), r)

			if got := receive(t, received); got != "authenticated" {
				t.Errorf("upstream got X-User %q, want %q", got, "authenticated")
			}

			if len(r.Header) != len(original) {
				t.Errorf("inbound header = %v, want %v", r.Header, original)
			}

			for name, values := range original {
				if !slices.Equal(r.Header[name], values) {
					t.Errorf("inbound header %s = %q, want %q", name, r.Header[name], values)
				}
			}

			if !maps.EqualFunc(r.Trailer, originalTrailer, slices.Equal) {
				t.Errorf("inbound trailer = %v, want %v", r.Trailer, originalTrailer)
			}

			if *r.URL != originalURL {
				t.Errorf("inbound URL = %v, want %v", r.URL, &originalURL)
			}
		})
	}
}

func TestQueryIsForwardedUnchanged(t *testing.T) {
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- r.URL.RawQuery
	}))
	defer upstream.Close()

	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
	})

	for _, query := range []string{
		"b=hello%20world&a=1",
		"b=hello+world&a=%2B",
		"a=1&a=2&b",
		"q=caf%C3%A9&Z=%7e",
		"=&&x=",
		"a=%25zz",
		"list=1,2,3&path=/x/y?z",
	} {
		t.Run(query, func(t *testing.T) {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x?"+query, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}

			if got := receive(t, received); got != query {
				t.Errorf("upstream query = %q, want %q", got, query)
			}
		})
	}
}

func TestMiddlewareQueryIsForwardedUnchanged(t *testing.T) {
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- r.URL.RawQuery
	}))
	defer upstream.Close()

	// Middleware is trusted, so its query reaches the upstream also when the
	// secure defaults reject the same query from a client.
	const query = "a=1;b=2&c=%zz"
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		Middleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r = r.Clone(r.Context())
				r.URL.RawQuery = query
				next.ServeHTTP(w, r)
			})
		}},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	if got := receive(t, received); got != query {
		t.Errorf("upstream query = %q, want %q", got, query)
	}
}
