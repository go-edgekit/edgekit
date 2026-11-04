package edgekit

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"slices"
	"strings"
	"time"

	"github.com/go-edgekit/edgekit/internal/safelog"
)

// ErrInvalidConfiguration is the error that [New] and [StaticUpstream] wrap
// when the configuration is not valid. Use [errors.Is] to identify it.
var ErrInvalidConfiguration = errors.New("invalid configuration")

// Config is the configuration of a [Gateway]. [New] copies it, so later
// changes to a Config value have no effect on the gateway. Values that Config
// refers to, such as selectors, hook providers, and the transport, are shared
// with the gateway.
type Config struct {
	// Routes are the routes of the gateway. At least one route is necessary.
	Routes []Route

	// Middleware wraps the handling of all requests, before routing. The first
	// middleware is the outermost one.
	Middleware []Middleware

	// Hooks are the hook providers of the gateway. Each value must implement
	// one or more of [RequestReceivedHook], [BeforeUpstreamHook],
	// [UpstreamResponseHook], [RequestCompletedHook], and [HeaderOwner]. Nil
	// values, also typed nil values, are not permitted.
	//
	// The first provider is the outermost one, closest to the client, as in
	// Middleware. The hooks of each phase run serially. [RequestReceivedHook]
	// and [BeforeUpstreamHook] hooks run in the order of this list, toward the
	// upstream. [UpstreamResponseHook] and [RequestCompletedHook] hooks run in
	// the reverse order, back toward the client. So a provider that changes the
	// request and the response nests like middleware. The position orders a
	// provider only among the hook providers: the stages of request handling
	// have a fixed order (see [Gateway]). A value that occurs more than one
	// time in the list runs one time for each occurrence.
	//
	// The gateway shares each value between concurrent requests, so the value
	// must be safe for concurrent use. Keep request state in the
	// [RequestContext], not in the value.
	Hooks []any

	// Transport sends requests to upstreams. If it is nil, the gateway uses its
	// own transport from [NewTransport]. A typed nil value, such as a nil
	// pointer, is not permitted. A transport that you set is yours: the
	// gateway does not close its connections.
	//
	// The transport must not decompress responses or add Accept-Encoding. New
	// rejects an [*http.Transport] with DisableCompression set to false. It
	// cannot examine other implementations of [http.RoundTripper].
	Transport http.RoundTripper

	// ManagedHeaders are headers that the gateway or its extensions own, such
	// as a header that tells the upstream the authenticated caller. The gateway
	// removes them from inbound requests before any middleware runs, so only
	// middleware and hooks can set them. Names must contain only ASCII letters,
	// digits, and "-". Hop-by-hop, framing, and Host headers are not permitted.
	// Extensions can also declare their managed headers with [HeaderOwner].
	ManagedHeaders []string
}

// Route is a match on the inbound request path. It sets the upstream selector
// and the route middleware for the requests that it matches.
type Route struct {
	// Prefix is the escaped path prefix that the route matches. It must start
	// and end with "/", and it must be in escaped form. The match is
	// case-sensitive. When more than one prefix matches, the longest one wins.
	// The prefix "/api/" matches "/api/" and "/api/users", but not "/api". The
	// gateway does not redirect "/api" to "/api/".
	Prefix string

	// Upstream selects the upstream URL for the route. It is necessary. Nil
	// values, also typed nil values, are not permitted. Use [StaticUpstream]
	// for one fixed upstream.
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
//  1. Make the gateway context ([RequestContext]).
//  2. Check the request with the secure defaults (see below).
//  3. Remove identity headers and managed headers.
//  4. Run the global middleware.
//  5. Run the [RequestReceivedHook] hooks.
//  6. Match a route. If no route matches, send 404 Not Found.
//  7. Run the route middleware.
//  8. Select the upstream URL.
//  9. Rewrite the outbound request. The path is the upstream URL path
//     followed by the inbound path. The query is the raw query of the
//     request that the route middleware passes on, without changes. The Host
//     header is the host of the upstream URL. The inbound Host goes in
//     X-Forwarded-Host.
//  10. Run the [BeforeUpstreamHook] hooks, then apply the outbound header
//     rules (see below).
//  11. Send the request with the transport.
//  12. Run the [UpstreamResponseHook] hooks, apply the response header rules,
//     and send the response.
//  13. After the middleware returned or finished unwinding, run the
//     [RequestCompletedHook] hooks.
//
// Middleware and hooks can end handling early. Then the later steps do not
// occur, except completion. Completion also runs when the secure defaults
// reject a request: then no middleware and no other hook runs, and the gateway
// context has only the start time. [Completion] gives the execution facts,
// such as the [RejectionReason] when the core rejected the request.
//
// # Lifecycle hooks
//
// Register hook providers in [Config.Hooks], which also sets the order of the
// hooks in each phase. The stages above have a fixed order: for example, all
// global middleware runs before any [RequestReceivedHook] hook. The middleware
// and hooks of one request get the same [RequestContext].
// Middleware runs before the hooks of the same stage: the route middleware
// can set request state that the [BeforeUpstreamHook] hooks use.
//
// An error from a hook stops its phase and the handling of the request. A
// panic in a hook or in middleware continues after completion ran. The
// gateway does not recover it.
//
// # Failures
//
// The gateway sends these statuses when handling fails, with a body that
// contains only the status text:
//
//   - 500 Internal Server Error for an error from a hook, from the upstream
//     selector, or for a selected URL that does not obey the contract of
//     [UpstreamSelector]. 500 also when middleware does not derive its
//     context from the request context, so the gateway context is lost.
//   - 502 Bad Gateway when the round trip to the upstream fails, such as for
//     a connection failure, a timeout, or response headers that are too
//     large.
//
// Completion gets the failure, with its phase, as a [Failure]. For a lost
// gateway context, completion gets no Failure, because the stage that finds
// the loss cannot get to the gateway context.
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
//   - The headers in [Config.ManagedHeaders] and the headers that
//     [HeaderOwner] values declare.
//
// It also removes these names from the inbound request trailers. Middleware
// and hooks can set these headers again, and they go to the upstream. The
// gateway sets X-Forwarded-For, X-Forwarded-Host, and X-Forwarded-Proto from
// the direct peer. It does not trust forwarding headers from other proxies. It
// also removes the names of these headers from the inbound Connection header,
// so a client cannot make the proxy remove a header that middleware set.
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
// # Header rules after middleware and hooks
//
// For each proxied request, after the route middleware and the
// [BeforeUpstreamHook] hooks, the gateway checks the complete outbound header
// map and the outbound trailer names. The check also runs when no hook is
// configured, because middleware can set the same entries:
//
//   - It restores Connection, Upgrade, Te, Keep-Alive, Proxy-Connection, and
//     Proxy-Authorization to their values before the hooks.
//   - It removes Host, Content-Length, Transfer-Encoding, and Trailer. The
//     transport writes these from request fields.
//   - A header or trailer name with characters other than ASCII letters,
//     digits, and "-", or a header value with a control character other than
//     horizontal tab, is a failure in [PhaseBeforeUpstream]. The gateway
//     does not call the transport. The trailer values arrive while the body
//     streams, so the gateway cannot check them.
//
// After the [UpstreamResponseHook] hooks, the gateway applies these rules to
// a final response:
//
//   - It removes Connection and the fields that it names, Keep-Alive,
//     Proxy-Connection, Proxy-Authenticate, Proxy-Authorization, Te, Trailer,
//     Transfer-Encoding, Upgrade, and Alt-Svc. It reads all Connection
//     entries, in all letter cases, before it removes a field.
//   - It keeps Content-Length only when its entries, in all letter cases,
//     give exactly one decimal value that fits in an int64. Otherwise it
//     removes the field.
//   - It removes the same names, and Content-Length and Host, from the
//     trailer names.
//
// It applies these rules to a 101 Switching Protocols response:
//
//   - It removes the same fields as from a final response, and also
//     Content-Length.
//   - It restores the protocol switch of the upstream: the Upgrade values
//     under the canonical name, and Connection set to "Upgrade" when the
//     upstream's Connection contained the Upgrade token. Changes by the hooks
//     to these fields have no effect.
//
// A hook that changes the status of a 101 response, or changes another status
// to one outside 200 to 599, causes a failure in [PhaseUpstreamResponse].
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
	hooks          hooks
	managedHeaders map[string]struct{}
	ownedTransport *http.Transport
}

// route is a validated route with its compiled handler.
type route struct {
	prefix  string
	handler http.Handler
}

// lostContextLog records that middleware did not derive its context from the
// request context. The event has no request data.
var lostContextLog = safelog.New("gateway_context_lost")

// New makes a gateway from cfg. It validates the complete configuration and
// prepares the handlers for all routes and the hook lists, so request handling
// does no setup.
//
// If cfg is not valid, New returns an error that wraps
// [ErrInvalidConfiguration], with one wrapped error for each problem. The
// errors identify the problem by its location, such as the route index. They
// do not contain configuration values.
//
// New works in two phases. First it checks the structure of cfg. If it finds a
// problem, it returns the errors and calls no middleware function. In this
// phase, it calls only the ManagedHeaders method of [HeaderOwner] values. Then
// it calls each middleware function one time to build the handlers, and checks
// the handlers that they return. New does not change process-wide state, such
// as the default logger.
func New(cfg Config) (*Gateway, error) {
	g := &Gateway{}

	var problems []error
	g.hooks, g.managedHeaders, problems = checkConfig(cfg)
	if len(problems) != 0 {
		return nil, errors.Join(problems...)
	}

	transport := cfg.Transport
	if transport == nil {
		g.ownedTransport = NewTransport()
		transport = g.ownedTransport
	}

	handler, problems := g.buildHandler(cfg, g.newProxy(transport))
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
	rc := &RequestContext{
		start: time.Now(),
	}
	ctx := context.WithValue(r.Context(), requestContextKey{}, rc)

	returned := false
	defer func() {
		g.complete(ctx, rc, !returned)
	}()

	if status, reason := checkRequest(r); status != 0 {
		rc.rejection = reason
		reject(w, status)
		returned = true

		return
	}

	// WithContext makes a shallow copy. stripHeaders gives it its own maps
	// when it removes headers.
	inner := r.WithContext(ctx)
	g.stripHeaders(inner)
	g.handler.ServeHTTP(w, inner)
	returned = true
}

// complete runs during a panic too, and lets the panic continue.
func (g *Gateway) complete(ctx context.Context, rc *RequestContext, aborted bool) {
	// A completion hook must not panic, but the state is dropped also when
	// one does.
	defer rc.end()

	if len(g.hooks.completed) != 0 {
		duration := time.Since(rc.start)
		for _, hook := range g.hooks.completed {
			hook.RequestCompleted(ctx, rc, rc.completion(duration, aborted))
		}
	}
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

// buildHandler calls each middleware function one time, so cfg must have
// passed checkConfig.
func (g *Gateway) buildHandler(cfg Config, proxy *httputil.ReverseProxy) (http.Handler, []error) {
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

	handler, err := chain(g.router(routes), cfg.Middleware)
	if err != nil {
		problems = append(problems, invalid("%w", err))
	}

	return handler, problems
}

// router runs the RequestReceived hooks and then calls the first route whose
// prefix matches, so routes must be sorted with the longest prefix first.
func (g *Gateway) router(routes []route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, ok := requestContext(w, r)
		if !ok {
			return
		}

		for _, hook := range g.hooks.received {
			if err := hook.RequestReceived(r.Context(), rc, r); err != nil {
				rc.fail(PhaseRequestReceived, err)
				reject(w, http.StatusInternalServerError)

				return
			}
		}

		path := r.URL.EscapedPath()
		for _, rt := range routes {
			if strings.HasPrefix(path, rt.prefix) {
				rc.route = rt.prefix
				rt.handler.ServeHTTP(w, r)

				return
			}
		}

		rc.rejection = ReasonNoRoute
		reject(w, http.StatusNotFound)
	})
}

// requestContext fails closed when middleware did not derive its context from
// the request context: it sends 500 and reports false.
func requestContext(w http.ResponseWriter, r *http.Request) (*RequestContext, bool) {
	rc, ok := FromContext(r.Context())
	if !ok {
		lostContextLog.Print("lost")
		reject(w, http.StatusInternalServerError)
	}

	return rc, ok
}
