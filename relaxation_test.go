package edgekit_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/go-edgekit/edgekit"
)

// receivedRequest is what an upstream received.
type receivedRequest struct {
	host          string
	uri           string
	rawQuery      string
	header        http.Header
	trailer       http.Header
	forwardedHost string
}

// recordingUpstream returns a started upstream that sends what it received.
func recordingUpstream(t *testing.T) (*httptest.Server, <-chan receivedRequest) {
	t.Helper()

	received := make(chan receivedRequest, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- receivedRequest{
			host:          r.Host,
			uri:           r.RequestURI,
			rawQuery:      r.URL.RawQuery,
			header:        r.Header.Clone(),
			trailer:       r.Trailer.Clone(),
			forwardedHost: r.Header.Get("X-Forwarded-Host"),
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	return upstream, received
}

// queryGateway returns a gateway with policy in front of an upstream that
// records each request. middleware gets the raw query that the shared
// middleware saw.
func queryGateway(t *testing.T, policy edgekit.QueryPolicy) (g *edgekit.Gateway, received <-chan receivedRequest, middleware <-chan string) {
	t.Helper()

	upstream, received := recordingUpstream(t)
	seen := make(chan string, 1)
	g = newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		QueryPolicy: policy,
		SharedMiddleware: []edgekit.Middleware{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen <- r.URL.RawQuery
					next.ServeHTTP(w, r)
				})
			},
		},
	})

	return g, received, seen
}

func TestQueryRejectRejectsInvalidQueries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		{
			name:  "semicolon",
			query: "a=1;b=2",
		},
		{
			name:  "malformed escape",
			query: "q=%zz",
		},
		{
			name:  "truncated escape",
			query: "k=%",
		},
		{
			name:  "semicolon after a repeated parameter",
			query: "a=1&a=2;x",
		},
		{
			name:  "more than 10000 parameters",
			query: strings.Repeat("k=v&", 10000) + "k=v",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _, _ := queryGateway(t, edgekit.QueryReject)
			r := httptest.NewRequest(http.MethodGet, "/api/items", nil)
			r.URL.RawQuery = tc.query
			w := httptest.NewRecorder()

			g.ServeHTTP(w, r)

			if w.Code != http.StatusBadRequest {
				t.Errorf("ServeHTTP() status = %d, want 400", w.Code)
			}

			if !strings.Contains(w.Body.String(), `"reason":"invalid_query"`) {
				t.Errorf("ServeHTTP() body = %q, want the reason invalid_query", w.Body.String())
			}
		})
	}
}

func TestAllowedQueriesAreForwardedUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy edgekit.QueryPolicy
		query  string
	}{
		{
			name:   "semicolon with QueryUnchecked",
			policy: edgekit.QueryUnchecked,
			query:  "a=1;b=2",
		},
		{
			name:   "malformed escape with QueryUnchecked",
			policy: edgekit.QueryUnchecked,
			query:  "q=%zz",
		},
		{
			name:   "truncated escape with QueryUnchecked",
			policy: edgekit.QueryUnchecked,
			query:  "k=%",
		},
		{
			name:   "repeated parameter semicolon with QueryUnchecked",
			policy: edgekit.QueryUnchecked,
			query:  "a=1&a=2;x",
		},
		{
			name:   "too many parameters with QueryUnchecked",
			policy: edgekit.QueryUnchecked,
			query:  strings.Repeat("k=v&", 10000) + "k=v",
		},
		{
			name:   "valid query with QueryReject",
			policy: edgekit.QueryReject,
			query:  "plain=1",
		},
		{
			name:   "valid query with QueryUnchecked",
			policy: edgekit.QueryUnchecked,
			query:  "plain=1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, received, middleware := queryGateway(t, tc.policy)
			r := httptest.NewRequest(http.MethodGet, "/api/items", nil)
			r.URL.RawQuery = tc.query
			w := httptest.NewRecorder()

			g.ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("ServeHTTP() status = %d, want 200", w.Code)
			}

			if got := receive(t, middleware); got != tc.query {
				t.Errorf("middleware saw the query %q, want %q", got, tc.query)
			}

			// ReverseProxy removes parameters that it cannot parse; the
			// upstream must still get the raw query unchanged.
			if got := receive(t, received); got.rawQuery != tc.query {
				t.Errorf("upstream got the query %q, want %q", got.rawQuery, tc.query)
			}
		})
	}
}

// invalidNames returns the sorted names in header that have characters other
// than letters, digits, and "-".
func invalidNames(header http.Header) []string {
	var names []string
	for name := range header {
		if strings.ContainsAny(name, "_. ") {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	return names
}

// spoofedHeaders are request headers with invalid names, identity headers,
// and a managed header.
func spoofedHeaders() http.Header {
	return http.Header{
		"X_User":          {"spoofed"},
		"X_forwarded_for": {"203.0.113.9"},
		"X.Dot":           {"spoofed"},
		"Kept-Header":     {"kept"},
		"X-Customer":      {"spoofed"},
		"X-Forwarded-For": {"203.0.113.9"},
		"Connection":      {"X_User, Kept-Header"},
	}
}

func TestHeaderNameDropRemovesInvalidNames(t *testing.T) {
	upstream, received := recordingUpstream(t)
	type observation struct {
		headers  []string
		trailers []string
	}

	var atMiddleware, atHook observation
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		HeaderNamePolicy: edgekit.HeaderNameDrop,
		ManagedHeaders:   []string{"X-Customer"},
		SharedMiddleware: []edgekit.Middleware{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					atMiddleware = observation{
						headers:  invalidNames(r.Header),
						trailers: invalidNames(r.Trailer),
					}
					next.ServeHTTP(w, r)
				})
			},
		},
		Hooks: edgekit.Hooks{
			RouteMatched: []edgekit.RouteMatchedHook{
				edgekit.RouteMatchedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, r *http.Request) (*edgekit.Rejection, error) {
					atHook = observation{
						headers:  invalidNames(r.Header),
						trailers: invalidNames(r.Trailer),
					}

					return nil, nil
				}),
			},
		},
	})

	// The transport sends trailers only with a chunked body.
	r := bodyRequest("body", false)
	r.URL.Path = "/api/items"
	r.Header = spoofedHeaders()
	r.Trailer = http.Header{
		"X_Trailer":    nil,
		"Kept-Trailer": nil,
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("ServeHTTP() status = %d, want 200", w.Code)
	}

	for stage, got := range map[string]observation{
		"middleware":        atMiddleware,
		"RouteMatched hook": atHook,
	} {
		if len(got.headers) != 0 || len(got.trailers) != 0 {
			t.Errorf("%s saw invalid headers %v and trailers %v, want none", stage, got.headers, got.trailers)
		}
	}

	got := receive(t, received)
	if names := append(invalidNames(got.header), invalidNames(got.trailer)...); len(names) != 0 {
		t.Errorf("upstream got the invalid names %v, want none", names)
	}

	if _, ok := got.trailer["Kept-Trailer"]; !ok {
		t.Errorf("upstream got the trailers %v, want Kept-Trailer", got.trailer)
	}

	if got.header.Get("Kept-Header") != "" {
		t.Errorf("upstream got Kept-Header %q, want it removed by the Connection token of the client", got.header.Get("Kept-Header"))
	}

	if got.header.Get("X-Customer") != "" || strings.Contains(got.header.Get("X-Forwarded-For"), "203.0.113.9") {
		t.Errorf("upstream got the header %v, want no managed or identity header from the client", got.header)
	}

	if !headersEqual(r.Header, spoofedHeaders()) {
		t.Errorf("ServeHTTP() changed the header of the caller to %v", r.Header)
	}
}

func TestHeaderNameDropKeepsOutboundCheck(t *testing.T) {
	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		HeaderNamePolicy: edgekit.HeaderNameDrop,
		SharedMiddleware: []edgekit.Middleware{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r = r.Clone(r.Context())
					r.Header["X_Middleware"] = []string{"value"}
					next.ServeHTTP(w, r)
				})
			},
		},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{
				collector,
			},
		},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/items", nil))
	c := receive(t, completed)

	if w.Code != http.StatusInternalServerError || c.Failure == nil || c.Failure.Phase != edgekit.PhaseBeforeUpstream {
		t.Errorf("ServeHTTP() status = %d, RequestCompleted() = %+v; want 500 in the BeforeUpstream phase", w.Code, c)
	}
}

func TestHeaderNameRejectIsTheDefault(t *testing.T) {
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
	})

	r := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	r.Header = spoofedHeaders()
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"reason":"invalid_header_name"`) {
		t.Errorf("ServeHTTP() response = %d %q, want 400 invalid_header_name", w.Code, w.Body.String())
	}
}

// headersEqual compares two header maps.
func headersEqual(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}

	for name, values := range a {
		if strings.Join(values, "\x00") != strings.Join(b[name], "\x00") {
			return false
		}
	}

	return true
}

func TestInvalidRelaxationValues(t *testing.T) {
	selector := static(t, "http://upstream.test")
	for _, tc := range []struct {
		name    string
		cfg     edgekit.Config
		problem string
	}{
		{
			name: "query policy",
			cfg: edgekit.Config{
				QueryPolicy: edgekit.QueryUnchecked + 1,
			},
			problem: "query policy: is not a defined value",
		},
		{
			name: "header-name policy",
			cfg: edgekit.Config{
				HeaderNamePolicy: -1,
			},
			problem: "header-name policy: is not a defined value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Routes = []edgekit.Route{
				{
					Prefix:   "/",
					Upstream: selector,
				},
			}

			_, err := edgekit.New(tc.cfg)
			if !errors.Is(err, edgekit.ErrInvalidConfiguration) || !strings.Contains(err.Error(), tc.problem) {
				t.Errorf("New() error = %v, want %q", err, tc.problem)
			}
		})
	}
}

// targetObservation is what middleware and hooks saw of an accepted request.
type targetObservation struct {
	middlewareURL  string
	middlewareHost string
	hookURL        string
	hookHost       string
}

// absoluteFormServer is a Server with AllowAbsoluteForm in front of an
// upstream that records each request. The shared middleware and a
// RouteMatched hook record the target that they see.
type absoluteFormServer struct {
	ts           *testServer
	upstreamHost string
	received     <-chan receivedRequest
	observed     <-chan targetObservation
	completed    <-chan edgekit.Completion
}

func newAbsoluteFormServer(t *testing.T) *absoluteFormServer {
	t.Helper()

	upstream, received := recordingUpstream(t)

	// The egress transport allows only the configured upstream, so an
	// authority from the client cannot change the destination.
	transport := newEgress(t, nil, edgekit.EgressPolicy{
		AllowedOrigins:  []string{upstream.URL},
		AllowedNetworks: prefixes("127.0.0.1/32"),
	})

	observed := make(chan targetObservation, 1)
	var observation targetObservation
	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
			{
				Prefix:   "/",
				Upstream: static(t, upstream.URL),
			},
		},
		Transport:         transport,
		AllowAbsoluteForm: true,
		SharedMiddleware: []edgekit.Middleware{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					observation.middlewareURL = r.URL.String()
					observation.middlewareHost = r.Host
					observed <- observation
					next.ServeHTTP(w, r)
				})
			},
		},
		Hooks: edgekit.Hooks{
			RouteMatched: []edgekit.RouteMatchedHook{
				edgekit.RouteMatchedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, r *http.Request) (*edgekit.Rejection, error) {
					observation.hookURL = r.URL.String()
					observation.hookHost = r.Host

					return nil, nil
				}),
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				collector,
			},
		},
	})

	return &absoluteFormServer{
		ts:           startServer(t, g, edgekit.ServerConfig{}, serverOptions{}),
		upstreamHost: parseURL(t, upstream.URL).Host,
		received:     received,
		observed:     observed,
		completed:    completed,
	}
}

// send writes a request with line and the header "Host: host.example", and
// returns the status of the response.
func (s *absoluteFormServer) send(t *testing.T, line string) int {
	t.Helper()

	conn := s.ts.dial(t)
	writeClientData(t, conn, line+"\r\nHost: host.example\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("http.ReadResponse() error = %v", err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return resp.StatusCode
}

func TestAbsoluteFormThroughServerAcceptsTargets(t *testing.T) {
	s := newAbsoluteFormServer(t)

	for _, tc := range []struct {
		name  string
		line  string
		url   string
		host  string
		route string
	}{
		{
			name:  "absolute form",
			line:  "GET http://client.example/api/item?q=1 HTTP/1.1",
			url:   "/api/item?q=1",
			host:  "client.example",
			route: "/api/",
		},
		{
			name:  "absolute form with https and port",
			line:  "GET https://client.example:8443/api/item HTTP/1.1",
			url:   "/api/item",
			host:  "client.example:8443",
			route: "/api/",
		},
		{
			name:  "metadata address as authority",
			line:  "GET http://169.254.169.254/api/item HTTP/1.1",
			url:   "/api/item",
			host:  "169.254.169.254",
			route: "/api/",
		},
		{
			name:  "empty path",
			line:  "GET http://client.example HTTP/1.1",
			url:   "/",
			host:  "client.example",
			route: "/",
		},
		{
			name:  "origin form",
			line:  "GET /api/item HTTP/1.1",
			url:   "/api/item",
			host:  "host.example",
			route: "/api/",
		},
		{
			name:  "escaped number sign in the query",
			line:  "GET http://client.example/api/item?x=%23 HTTP/1.1",
			url:   "/api/item?x=%23",
			host:  "client.example",
			route: "/api/",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := s.send(t, tc.line)

			if status != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", tc.line, status)
			}

			c := receive(t, s.completed)
			got := receive(t, s.observed)
			want := targetObservation{
				middlewareURL:  tc.url,
				middlewareHost: tc.host,
				hookURL:        tc.url,
				hookHost:       tc.host,
			}
			if got != want {
				t.Errorf("middleware and RouteMatched hook saw %+v, want %+v", got, want)
			}

			sent := receive(t, s.received)
			if sent.host != s.upstreamHost || sent.uri != tc.url || sent.forwardedHost != tc.host || c.RoutePrefix != tc.route {
				t.Errorf("upstream got Host %q, URI %q, X-Forwarded-Host %q, route %q; want %q, %q, %q, %q",
					sent.host, sent.uri, sent.forwardedHost, c.RoutePrefix, s.upstreamHost, tc.url, tc.host, tc.route)
			}
		})
	}
}

func TestAbsoluteFormThroughServerRejectsTargets(t *testing.T) {
	s := newAbsoluteFormServer(t)

	for _, tc := range []struct {
		name   string
		line   string
		status int
		reason edgekit.RejectionReason
	}{
		{
			name:   "fragment after the query",
			line:   "GET http://client.example/api/item?x#fragment HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidRequestTarget,
		},
		{
			name:   "fragment after an empty query",
			line:   "GET http://client.example/api/item?#fragment HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidRequestTarget,
		},
		{
			name:   "fragment in the path",
			line:   "GET http://client.example/api/item#fragment HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidRequestTarget,
		},
		{
			name:   "origin fragment in the path",
			line:   "GET /api/item#fragment HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidRequestTarget,
		},
		{
			name:   "origin encoded number sign in the path",
			line:   "GET /api/a%23b HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidPath,
		},
		{
			name:   "origin fragment after the query",
			line:   "GET /api/item?x#fragment HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidRequestTarget,
		},
		{
			name:   "user information",
			line:   "GET http://user@client.example/api/item HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidRequestTarget,
		},
		{
			name:   "other scheme",
			line:   "GET ftp://client.example/api/item HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidRequestTarget,
		},
		{
			name:   "empty authority",
			line:   "GET http:///api/item HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidRequestTarget,
		},
		{
			name:   "authority that the transport changes",
			line:   "GET http://client_example!/api/item HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidRequestTarget,
		},
		{
			name:   "path policy still applies",
			line:   "GET http://client.example/api/%2e%2e/admin HTTP/1.1",
			status: http.StatusBadRequest,
			reason: edgekit.ReasonInvalidPath,
		},
		{
			name:   "CONNECT stays rejected",
			line:   "CONNECT client.example:443 HTTP/1.1",
			status: http.StatusNotImplemented,
			reason: edgekit.ReasonInvalidMethod,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := s.send(t, tc.line)

			if status != tc.status {
				t.Errorf("%s: status = %d, want %d", tc.line, status, tc.status)
			}

			// net/http rejects some targets before the gateway runs, without
			// a completion. All targets in this table reach the gateway.
			if c := receive(t, s.completed); c.RejectionReason != tc.reason {
				t.Errorf("RequestCompleted() RejectionReason = %q, want %q", c.RejectionReason, tc.reason)
			}
		})
	}
}

func TestAbsoluteFormOutsideServer(t *testing.T) {
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
				Middleware: []edgekit.Middleware{
					func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Scheme != "" || r.URL.Host != "" || r.Host != "client.example" {
								t.Errorf("route middleware saw URL %v and Host %q, want the origin form with Host client.example", r.URL, r.Host)
							}

							w.WriteHeader(http.StatusNoContent)
						})
					},
				},
			},
		},
		AllowAbsoluteForm: true,
	})

	for _, tc := range []struct {
		name   string
		url    *url.URL
		status int
	}{
		{
			name: "accepted",
			url: &url.URL{
				Scheme: "http",
				Host:   "client.example",
				Path:   "/api/item",
			},
			status: http.StatusNoContent,
		},
		{
			name: "fragment",
			url: &url.URL{
				Scheme:   "http",
				Host:     "client.example",
				Path:     "/api/item",
				Fragment: "part",
			},
			status: http.StatusBadRequest,
		},
		{
			name: "IPv6 zone",
			url: &url.URL{
				Scheme: "http",
				Host:   "[fe80::1%eth0]",
				Path:   "/api/item",
			},
			status: http.StatusBadRequest,
		},
		{
			name: "opaque",
			url: &url.URL{
				Scheme: "http",
				Opaque: "client.example/api/item",
			},
			status: http.StatusBadRequest,
		},
		{
			name: "non-numeric port",
			url: &url.URL{
				Scheme: "http",
				Host:   "client.example:http",
				Path:   "/api/item",
			},
			status: http.StatusBadRequest,
		},
		{
			name: "asterisk",
			url: &url.URL{
				Path: "*",
			},
			status: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.URL = tc.url
			before := *tc.url
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)

			if w.Code != tc.status {
				t.Errorf("ServeHTTP() status = %d, want %d", w.Code, tc.status)
			}

			if *r.URL != before {
				t.Errorf("ServeHTTP() changed the URL of the caller to %v", r.URL)
			}
		})
	}

	// The relaxation is off by default.
	reject := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
	})
	r := httptest.NewRequest(http.MethodGet, "http://client.example/api/item", nil)
	w := httptest.NewRecorder()
	reject.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("ServeHTTP() without AllowAbsoluteForm: status = %d, want 400", w.Code)
	}
}
