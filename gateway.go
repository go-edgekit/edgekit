package edgekit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"slices"
	"strings"

	"github.com/go-edgekit/edgekit/internal/safelog"
)

// ErrInvalidConfiguration is the error that [New] and [StaticUpstream] wrap
// when the configuration is not valid. Use [errors.Is] to identify it.
var ErrInvalidConfiguration = errors.New("invalid configuration")

// Config is the configuration of a [Gateway]. [New] copies it, so later
// changes to a Config value have no effect on the gateway. Values that Config
// refers to, such as selectors and the transport, are shared with the gateway.
type Config struct {
	// Routes are the routes of the gateway. At least one route is necessary.
	Routes []Route

	// Middleware wraps the handling of all requests, before routing. The first
	// middleware is the outermost one.
	Middleware []Middleware

	// Transport sends requests to upstreams. If it is nil, the gateway uses its
	// own transport from [NewTransport]. A transport that you set is yours: the
	// gateway does not close its connections.
	//
	// The transport must not decompress responses or add Accept-Encoding. New
	// rejects an [*http.Transport] with DisableCompression set to false. It
	// cannot examine other implementations of [http.RoundTripper].
	Transport http.RoundTripper

	// ManagedHeaders are headers that the gateway or its extensions own, such
	// as a header that tells the upstream the authenticated caller. The gateway
	// removes them from inbound requests before any middleware runs, so only
	// middleware can set them. Names must contain only ASCII letters, digits,
	// and "-". Hop-by-hop, framing, and Host headers are not permitted.
	ManagedHeaders []string
}

// Route is a match on the inbound request path. It sets the upstream selector
// and the route middleware for the requests that it matches.
type Route struct {
	// Prefix is the escaped path prefix that the route matches. It must start
	// and end with "/", and it must be in escaped form. The match is
	// case-sensitive. When more than one prefix matches, the longest one wins.
	// The prefix "/api/" matches "/api/" and "/api/users", but not "/api".
	Prefix string

	// Upstream selects the upstream URL for the route. It is necessary. Use
	// [StaticUpstream] for one fixed upstream.
	Upstream UpstreamSelector

	// Middleware wraps the handling of the requests that match the route,
	// after the global middleware. The first middleware is the outermost one.
	Middleware []Middleware
}

// Gateway is an [http.Handler] that routes requests and forwards them to
// upstreams. Make it with [New]. It is safe for concurrent use.
//
// # Request handling
//
// The gateway handles each request in this order:
//
//  1. Check the request with the secure defaults (see below).
//  2. Remove identity headers and managed headers.
//  3. Run the global middleware.
//  4. Match a route. If no route matches, send 404 Not Found.
//  5. Run the route middleware.
//  6. Select the upstream URL.
//  7. Forward the request. The path is the upstream URL path followed by the
//     inbound path. The query is the raw query of the request that the
//     route middleware passes on, without changes. The Host header is the
//     host of the upstream URL. The inbound Host goes in X-Forwarded-Host.
//
// Middleware can end handling early. Then the later steps do not occur.
//
// # Secure defaults
//
// Before any middleware runs, the gateway rejects these requests:
//
//   - CONNECT requests, with 501 Not Implemented.
//   - Request targets that do not have the origin form, with 400 Bad Request.
//     This includes the absolute form ("GET http://host/path") and the
//     asterisk form ("OPTIONS *").
//   - Paths with a dot-segment, a backslash or "#", or a percent-encoded ".",
//     "/", "\", NUL, or "#", with 400 Bad Request.
//   - Queries that [net/url.ParseQuery] rejects, with 400 Bad Request: a ";",
//     a malformed percent-escape, or more than 10000 parameters. Middleware
//     reads the query with the same parser, so it sees the same parameters as
//     the upstream.
//   - Header and trailer names with characters other than ASCII letters,
//     digits, and "-", with 400 Bad Request.
//
// The gateway removes these headers from the inbound request before any
// middleware runs:
//
//   - Forwarded, X-Forwarded-*, X-Real-IP, X-Client-IP, True-Client-IP,
//     X-Cluster-Client-IP, X-Original-Forwarded-For, CF-Connecting-IP,
//     Fastly-Client-IP, X-Original-URL, and X-Rewrite-URL.
//   - The headers in [Config.ManagedHeaders].
//
// It also removes these names from the inbound request trailers. Middleware
// can set these headers again, and they go to the upstream. The gateway sets
// X-Forwarded-For, X-Forwarded-Host, and X-Forwarded-Proto from the direct
// peer. It does not trust forwarding headers from other proxies. It also
// removes the names of these headers from the inbound Connection header, so a
// client cannot make the proxy remove a header that middleware set.
//
// The gateway removes hop-by-hop headers in both directions. It removes
// Alt-Svc and hop-by-hop fields from responses, informational (1xx) responses,
// and trailers. It keeps the Connection and Upgrade headers of 101 Switching
// Protocols, so protocol upgrades work. Response headers that middleware set
// before the upstream responds are not sent in informational responses, and
// they stay in the final response.
//
// The gateway does not change the request that the caller gave to ServeHTTP.
// Middleware must also not change it (see [Middleware]).
//
// If the upstream selection or the transport fails, the gateway sends
// 502 Bad Gateway. Responses that the gateway makes contain no error details.
//
// # Deviations from HTTP specifications
//
// These secure defaults deliberately differ from the HTTP specifications:
//
//   - RFC 9112, section 3.2.2, says that a server must accept the absolute
//     form. The gateway rejects it, because the authority in the request
//     target can disagree with the Host header that the routing and the
//     upstream use.
//   - RFC 9110, section 5.1, permits other token characters in header names,
//     such as "_" and ".". The gateway rejects them, because some upstreams
//     read "X_User" and "X-User" as the same header.
//   - RFC 3986 permits ";" in a query. The gateway rejects it, because
//     [net/url.ParseQuery] ignores the parameters that contain ";", and an
//     upstream that splits the query at ";" then sees parameters that
//     middleware did not see.
//
// The GODEBUG setting urlmaxqueryparams changes the parameter limit of
// [net/url.ParseQuery], and so also the limit of the gateway.
//
// # Server-level protections
//
// A Gateway is a handler. Server-level protections, such as header-read
// timeouts and header size limits, are the responsibility of the server that
// calls it.
type Gateway struct {
	handler        http.Handler
	managedHeaders map[string]struct{}
	ownedTransport *http.Transport
}

// route is a validated route with its compiled handler.
type route struct {
	prefix  string
	handler http.Handler
}

// upstreamKey is the context key for the selected upstream URL of a request.
type upstreamKey struct{}

// New makes a gateway from cfg. It validates the complete configuration and
// prepares the handlers for all routes, so request handling does no setup.
//
// If cfg is not valid, New returns an error that wraps
// [ErrInvalidConfiguration], with one wrapped error for each problem. The
// errors identify the problem by its location, such as the route index. They
// do not contain configuration values.
//
// New works in two phases. First it checks the structure of cfg. If it finds a
// problem, it returns the errors and calls no middleware function. Then it
// calls each middleware function one time to build the handlers, and checks
// the handlers that they return. New does not change process-wide state, such
// as the default logger.
func New(cfg Config) (*Gateway, error) {
	managedHeaders, problems := checkConfig(cfg)
	if len(problems) != 0 {
		return nil, errors.Join(problems...)
	}

	g := &Gateway{
		managedHeaders: managedHeaders,
	}

	transport := cfg.Transport
	if transport == nil {
		g.ownedTransport = NewTransport()
		transport = g.ownedTransport
	}

	handler, problems := buildHandler(cfg, newProxy(transport))
	if len(problems) != 0 {
		return nil, errors.Join(problems...)
	}

	g.handler = handler

	return g, nil
}

// ServeHTTP handles a request as the [Gateway] documentation describes. It does
// not change r. The middleware can get r itself, so the middleware must also
// not change it (see [Middleware]).
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if status := checkRequest(r); status != 0 {
		reject(w, status)

		return
	}

	g.handler.ServeHTTP(w, g.stripHeaders(r))
}

// CloseIdleConnections closes the idle upstream connections of the transport
// that the gateway made with [NewTransport]. It has no effect when
// [Config.Transport] is set, because that transport belongs to the caller.
// It does not stop active requests. It is safe for concurrent use.
func (g *Gateway) CloseIdleConnections() {
	if g.ownedTransport != nil {
		g.ownedTransport.CloseIdleConnections()
	}
}

// invalid returns a configuration problem that wraps ErrInvalidConfiguration.
func invalid(format string, args ...any) error {
	problem := fmt.Errorf(format, args...)

	return fmt.Errorf("%w: %w", ErrInvalidConfiguration, problem)
}

// checkConfig checks the structure of cfg without calling a middleware
// function, and returns the canonical managed header names.
func checkConfig(cfg Config) (map[string]struct{}, []error) {
	managedHeaders, problems := checkManagedHeaders(cfg.ManagedHeaders)

	if err := checkTransport(cfg.Transport); err != nil {
		problems = append(problems, invalid("transport: %w", err))
	}

	problems = append(problems, checkRoutes(cfg.Routes)...)

	if i := slices.IndexFunc(cfg.Middleware, isNil); i >= 0 {
		problems = append(problems, invalid("middleware %d: is nil", i))
	}

	return managedHeaders, problems
}

// checkManagedHeaders returns the canonical names, which are unique.
func checkManagedHeaders(names []string) (map[string]struct{}, []error) {
	var problems []error

	canonicalNames := make(map[string]struct{}, len(names))
	for i, name := range names {
		if err := checkManagedHeader(name); err != nil {
			problems = append(problems, invalid("managed header %d: %w", i, err))

			continue
		}

		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if _, ok := canonicalNames[canonical]; ok {
			problems = append(problems, invalid("managed header %d: is a duplicate", i))
		}

		canonicalNames[canonical] = struct{}{}
	}

	return canonicalNames, problems
}

// checkTransport accepts nil, which means the gateway's own transport.
func checkTransport(transport http.RoundTripper) error {
	if t, ok := transport.(*http.Transport); ok {
		switch {
		case t == nil:
			return errors.New("is a nil *http.Transport")
		case !t.DisableCompression:
			return errors.New("does transparent compression")
		}
	}

	return nil
}

// checkRoutes checks the routes without calling a middleware function.
func checkRoutes(routes []Route) []error {
	var problems []error

	if len(routes) == 0 {
		problems = append(problems, invalid("routes: no route is configured"))
	}

	seen := make(map[string]struct{}, len(routes))
	for i, r := range routes {
		if err := checkPrefix(r.Prefix); err != nil {
			problems = append(problems, invalid("route %d: prefix: %w", i, err))
		} else if _, ok := seen[r.Prefix]; ok {
			problems = append(problems, invalid("route %d: prefix: is a duplicate", i))
		}

		seen[r.Prefix] = struct{}{}

		if isNilSelector(r.Upstream) {
			problems = append(problems, invalid("route %d: upstream selector is nil", i))
		}

		if j := slices.IndexFunc(r.Middleware, isNil); j >= 0 {
			problems = append(problems, invalid("route %d: middleware %d: is nil", i, j))
		}
	}

	return problems
}

// isNilSelector also detects a nil UpstreamSelectorFunc, which is a non-nil
// interface value that panics when the gateway calls it.
func isNilSelector(selector UpstreamSelector) bool {
	if f, ok := selector.(UpstreamSelectorFunc); ok {
		return f == nil
	}

	return selector == nil
}

// newProxy returns the proxy that all routes share.
func newProxy(transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite:        rewrite,
		Transport:      transport,
		ErrorLog:       safelog.New("reverse_proxy"),
		ModifyResponse: modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			reject(w, http.StatusBadGateway)
		},
	}
}

// buildHandler calls each middleware function one time, so cfg must have
// passed checkConfig.
func buildHandler(cfg Config, proxy *httputil.ReverseProxy) (http.Handler, []error) {
	var problems []error

	routes := make([]route, 0, len(cfg.Routes))
	for i, r := range cfg.Routes {
		handler, err := chain(forward(proxy, r.Upstream), r.Middleware)
		if err != nil {
			problems = append(problems, invalid("route %d: %w", i, err))

			continue
		}

		routes = append(routes, route{
			prefix:  r.Prefix,
			handler: handler,
		})
	}

	// The longest prefix is first, so the first match is the longest match.
	slices.SortFunc(routes, func(a, b route) int {
		return cmp.Compare(len(b.prefix), len(a.prefix))
	})

	handler, err := chain(router(routes), cfg.Middleware)
	if err != nil {
		problems = append(problems, invalid("%w", err))
	}

	return handler, problems
}

// checkManagedHeader returns an error if name cannot be a managed header.
func checkManagedHeader(name string) error {
	if !headerNameAllowed(name) {
		return errors.New("name contains characters other than letters, digits, and -")
	}

	if _, ok := reservedHeaders[textproto.CanonicalMIMEHeaderKey(name)]; ok {
		return errors.New("is a hop-by-hop, framing, or Host header")
	}

	return nil
}

// checkPrefix returns an error if prefix is not a valid route prefix.
func checkPrefix(prefix string) error {
	if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") {
		return errors.New("does not start and end with /")
	}

	// The router compares the prefix with EscapedPath, so the prefix must be
	// the escaped form that EscapedPath gives for it.
	decoded, err := url.PathUnescape(prefix)
	if err != nil || (&url.URL{Path: decoded, RawPath: prefix}).EscapedPath() != prefix {
		return errors.New("is not in escaped form")
	}

	if !pathAllowed(prefix) {
		return errors.New("does not obey the path policy")
	}

	return nil
}

// router calls the first route whose prefix matches, so routes must be sorted
// with the longest prefix first.
func router(routes []route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		for _, rt := range routes {
			if strings.HasPrefix(path, rt.prefix) {
				rt.handler.ServeHTTP(w, r)

				return
			}
		}

		reject(w, http.StatusNotFound)
	})
}

// forward selects the upstream after the route middleware ran, and gives it to
// rewrite through the request context.
func forward(proxy *httputil.ReverseProxy, selector UpstreamSelector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selected, err := selector.SelectUpstream(r)
		if err == nil {
			err = checkUpstreamURL(selected)
		}

		if err != nil {
			reject(w, http.StatusBadGateway)

			return
		}

		upstream := *selected
		r = r.WithContext(context.WithValue(r.Context(), upstreamKey{}, &upstream))
		proxy.ServeHTTP(newInformationalWriter(w), r)

		// The server sends the trailers after the handlers return, so a
		// removal here is still in time.
		removeTrailers(w.Header())
	})
}

// rewrite runs after ReverseProxy removed the hop-by-hop and inbound
// forwarding headers.
func rewrite(pr *httputil.ProxyRequest) {
	// forward always sets the upstream. Without it, the outbound URL has no
	// scheme or host, so the transport fails and the client gets 502.
	if upstream, ok := pr.In.Context().Value(upstreamKey{}).(*url.URL); ok {
		pr.SetURL(upstream)
	}

	// ReverseProxy removes the query parameters that it cannot parse before
	// Rewrite runs. The upstream URL has no query, so the inbound query is
	// the complete outbound query.
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery

	pr.SetXForwarded()
}

// reject sends a response with status and a body without details.
func reject(w http.ResponseWriter, status int) {
	http.Error(w, http.StatusText(status), status)
}
