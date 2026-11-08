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
// refers to, such as selectors, hooks, admission budgets, and the transport,
// are shared with the gateway.
type Config struct {
	// Routes are the routes of the gateway. At least one route is necessary.
	Routes []Route

	// SharedMiddleware processes requests after fixed route selection, body
	// protection, admission, RouteMatched hooks, and optional buffering. It
	// does not run for earlier rejections. The first middleware is outermost.
	SharedMiddleware []Middleware

	// Hooks sets the lifecycle hooks in execution order for each phase. The
	// stages of request handling have a fixed order (see [Gateway]). A hook
	// runs only in the lists that contain it, even if it implements other
	// hook interfaces. See [Hooks] for validation and concurrency rules.
	Hooks Hooks

	// HeaderOwners are the header owners that declare managed headers. Nil
	// entries, also typed nil entries, are not permitted. Declarations can
	// overlap with each other and with ManagedHeaders. See [HeaderOwner] for
	// when New calls them and what it keeps.
	HeaderOwners []HeaderOwner

	// Transport sends requests to upstreams. If it is nil, the gateway uses its
	// own transport from [NewTransport]. A typed nil value, such as a nil
	// pointer, is not permitted. A transport that you set is yours: the
	// gateway does not close its connections. Set an [EgressTransport] to
	// limit the destinations of the gateway.
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

	// Renderer makes the body of the responses that the gateway makes itself,
	// for rejections and failures. If it is nil, the gateway writes a minimal
	// RFC 9457 problem document (see [Renderer]).
	Renderer Renderer

	// BodyLimit is the inherited request-body limit (see [BodyLimit]). A route
	// can replace it with a larger, smaller, or disabled limit. It is not a
	// ceiling. The zero value is [DefaultBodyLimit].
	BodyLimit BodyLimit

	// RequestBuffering is the inherited request-buffering configuration. Nil
	// or [NoRequestBuffering] keeps streaming as the default. A route can
	// replace the complete configuration or explicitly select streaming.
	// New copies the effective settings but shares the Budget across routes
	// and requests. Each buffered route must have a finite effective body
	// limit that fits the budget. See [RequestBuffering] for deadline support,
	// timeouts, replay, and resource lifetime.
	RequestBuffering *RequestBuffering

	// Admission is the admission budget of the gateway (see
	// [AdmissionBudget]). The gateway takes a slot before route matching,
	// body reads, and processing extensions. Nil means no
	// gateway budget. The same budget must not also be the budget of a
	// route.
	Admission *AdmissionBudget

	// QueryPolicy sets how the gateway checks the query before any middleware
	// runs. The zero value, [QueryReject], is the secure default.
	QueryPolicy QueryPolicy

	// HeaderNamePolicy sets what the gateway does with header and trailer
	// names that have characters other than ASCII letters, digits, and "-".
	// The zero value, [HeaderNameReject], is the secure default.
	HeaderNamePolicy HeaderNamePolicy

	// AllowAbsoluteForm accepts a request target in the absolute form, such
	// as "GET http://example.com/api HTTP/1.1", with the scheme http or https,
	// a host, and no user information or fragment. An unescaped "#" anywhere
	// in the target is a fragment, as in the origin form; "%23" is not.
	// False, the zero value, rejects it (see [Gateway]). An accepted request
	// is handled in the origin form with the same path and query, and with
	// Host set to the authority of the target (RFC 9112, section 3.2.2).
	// Middleware, hooks, and the upstream selector get a URL without scheme
	// and host; an empty path becomes "/". The authority must be a host that
	// [UpstreamSelector] accepts. It goes to the upstream only in
	// X-Forwarded-Host, and does not select the upstream, except through a
	// selector that reads Host. [http.Request.RequestURI] keeps the value
	// that the client sent.
	AllowAbsoluteForm bool
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

	// Middleware processes matched requests after buffering and shared
	// middleware. The first middleware is the outermost one.
	Middleware []Middleware

	// BodyLimit is the request-body limit of the route (see [BodyLimit]). The
	// zero value uses [Config.BodyLimit]. Set a larger limit, or
	// [NoBodyLimit], for upload routes.
	BodyLimit BodyLimit

	// Admission is the admission budget of the route (see
	// [AdmissionBudget]). The gateway takes a slot after the route match and
	// the body check, and before the route middleware. Nil means no route
	// budget. Routes that have the same budget share it.
	Admission *AdmissionBudget

	// RequestBuffering selects complete, bounded memory buffering before
	// middleware. Nil inherits [Config.RequestBuffering]. [NoRequestBuffering]
	// explicitly selects streaming. Other values replace the complete gateway
	// setting, without field merging. See [RequestBuffering] for required
	// limits, deadline support, replay, and resource lifetime.
	RequestBuffering *RequestBuffering
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
//  4. Take gateway admission ([Config.Admission]); reject saturation with 503.
//  5. Match one route using the validated initial escaped path; send 404 on
//     a miss. The original match and its protections remain fixed.
//  6. Install the route's effective body limit ([Route.BodyLimit]).
//  7. Reject an excessive declared body length with 413 Content Too Large.
//  8. Take route admission ([Route.Admission]); reject saturation with 503.
//  9. Run [RouteMatchedHook] checks, then the optional effective request
//     buffering ([Route.RequestBuffering] or [Config.RequestBuffering]).
//  10. Run shared middleware, then route middleware.
//  11. Select the upstream URL.
//  12. Rewrite the outbound request. The path is the upstream URL path
//     followed by the inbound path. The query is the raw query of the
//     request that the route middleware passes on, without changes. The Host
//     header is the host of the upstream URL. The inbound Host goes in
//     X-Forwarded-Host.
//  13. Run the [BeforeUpstreamHook] hooks, then apply the outbound header
//     rules (see below).
//  14. Send the request with the transport.
//  15. Run the [UpstreamResponseHook] hooks, apply the response header rules,
//     and send the response.
//  16. Invalidate buffered replay handles, release storage reservations, and
//     release admission slots when protected handling returns or unwinds.
//  17. After the middleware returned or finished unwinding, run the
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
// Register hooks in the phase lists of [Config.Hooks]. Each list runs in its
// written order, including response and completion hooks (ADR-0030). The
// stages above have a fixed order: [RouteMatchedHook] checks run before
// buffering and processing middleware. The middleware and hooks of one
// request get the same [RequestContext]. Middleware runs before the hooks of
// the same stage: the route middleware can set request state that the
// [BeforeUpstreamHook] hooks use.
//
// An error from a hook stops its phase and the handling of the request. A
// panic in a hook or in middleware continues after completion ran. The
// gateway does not recover it.
//
// # Fixed routing and outbound changes
//
// Routing uses the validated initial escaped path. Middleware may pass a copy
// with changed headers, method, Host, path/query, body/trailers, or a derived
// context. Path/query changes rewrite the outbound request; they never rematch
// or change the selected route's protections. The core rechecks the applicable
// request protections before selection. Invalid middleware output is an
// internal failure in PhaseBeforeUpstream, not a client rejection. Middleware
// must bound generated or expanded bodies itself. See [Middleware].
//
// # Rejections and failures
//
// A processing hook can reject a request with a [Rejection]: the gateway
// sends its status and headers. The gateway also rejects requests itself:
// with the secure defaults (see below), with 404 Not Found when no route
// matches, with 413 Content Too Large after the match for a declared length
// over the limit ([ReasonBodyTooLarge]), and with 503 Service Unavailable
// when an admission budget is full ([ReasonAdmissionSaturated]). Explicit
// buffering can also reject a full byte budget with 503
// ([ReasonRequestBufferSaturated]). Established inbound buffering timeouts
// give 408, established truncation gives 400, and unknown buffering reader
// errors give 500, all in [PhaseRequestBuffering].
//
// The source of a failure sets the status (ADR-0017, ADR-0023). The first
// rule that applies counts:
//
//   - 499 when the error wraps [context.Canceled] and the context that the
//     gateway got with the request is canceled: the client is gone. This
//     applies to hooks, the upstream selector, the round trip, and the
//     protocol switch. A canceled context alone does not change the status
//     of another error.
//   - 413 Content Too Large when the error wraps
//     [ErrRequestBodyTooLarge] and the body limit of this request found the
//     overflow, in each stage. The error alone does not give 413.
//   - 400 Bad Request when the error wraps [ErrInvalidTrailerName] and the
//     trailer check of this request rejected a name at the end of the body,
//     in each stage (see "Request trailers"). The error alone does not give
//     400.
//   - 504 Gateway Timeout when the round trip fails with a [net.Error] whose
//     Timeout method returns true, such as a dial, TLS-handshake, or
//     response-header timeout, or [context.DeadlineExceeded].
//   - 503 Service Unavailable when the upstream selector returns an error that
//     wraps [ErrNoUpstreamAvailable].
//   - 502 Bad Gateway when an [UpstreamResponseHook] hook returns an error
//     that wraps [ErrInvalidUpstreamResponse].
//   - 500 Internal Server Error for another error from a hook or from the
//     upstream selector, for a selected URL that does not obey the contract
//     of [UpstreamSelector], for an invalid outbound header (see below), for
//     a [Rejection] that is not valid, and for a defect of the embedder in a
//     protocol switch: a 101 response body that cannot be written, or a
//     response writer that cannot hijack the connection. 500 also when
//     middleware does not derive its context from the request context, so
//     the gateway context is lost.
//   - 502 Bad Gateway for another failure of the round trip, such as a
//     connection failure, response headers that are too large, or a denial
//     by an [EgressTransport] ([ErrEgressDenied]), and for a failure without
//     a known source, such as an upstream that does not accept a protocol
//     switch.
//
// The gateway never reads the text of an error. A [Renderer] makes the body
// of these responses; it gets no failure details. The gateway does not send
// such a response after the final response headers are committed, such as
// when the response body fails while it streams, or after the hijack of the
// connection for a protocol switch. It also sends none when the connection is
// hijacked already ([net/http.ErrHijacked]), because another component owns
// it; completion still gets the failure.
//
// A read error other than io.EOF from the final response body, after the
// response hooks, is a [Failure] in [PhaseResponseBody]. It keeps the original
// error, also after the response is committed. Response-write failures are
// observations of middleware and are not recorded by this reader.
//
// Completion gets the failure, with its phase, as a [Failure], and the
// transport facts [Completion.ConnectionObtained] and
// [Completion.RequestWritten]. For a lost gateway context, completion gets no
// Failure, because the stage that finds the loss cannot get to the gateway
// context.
//
// # Secure defaults
//
// Before any middleware runs, the gateway rejects these requests:
//
//   - CONNECT requests, with 501 Not Implemented.
//   - Request targets that do not have the origin form, with 400 Bad Request.
//     This includes the absolute form ("GET http://host/path"), unless
//     [Config.AllowAbsoluteForm] relaxes it, the asterisk form
//     ("OPTIONS *"), and a target with a fragment: an unescaped "#" anywhere
//     in the target ("GET /path#y", "GET /path?x#y"), which
//     [net/url.ParseRequestURI] keeps in the path or the raw query. "%23" is
//     not a fragment.
//   - Paths with a dot-segment or a backslash, or a percent-encoded ".", "/",
//     "\", NUL, or "#", with 400 Bad Request.
//   - Queries that [net/url.ParseQuery] rejects, with 400 Bad Request: a ";",
//     a malformed percent-escape, or more than 10000 parameters. Middleware
//     reads the query with the same parser, so it sees the same parameters as
//     the upstream. [QueryUnchecked] relaxes this check.
//   - Header and trailer names with characters other than ASCII letters,
//     digits, and "-", with 400 Bad Request. [HeaderNameDrop] removes these
//     names instead.
//   - A Connection header with the token "upgrade" and an Upgrade value
//     that is not printable ASCII, with 400 Bad Request.
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
// It also removes these names from the inbound request trailers (see
// "Request trailers"). Middleware and hooks can set these headers again, and
// they go to the upstream. The gateway sets X-Forwarded-For,
// X-Forwarded-Host, and X-Forwarded-Proto from the direct peer. It does not
// trust forwarding headers from other proxies. It also removes the names of
// these headers from the inbound Connection header, so a client cannot make
// the proxy remove one of these headers after middleware set it. Other
// middleware-owned headers need a declaration in [Config.ManagedHeaders] or
// [Config.HeaderOwners] for the same protection.
//
// The gateway removes hop-by-hop headers in both directions. It removes
// Alt-Svc and hop-by-hop fields from responses, informational (1xx) responses,
// and trailers. It keeps the Connection and Upgrade headers of 101 Switching
// Protocols, so protocol upgrades work. Response headers that middleware set
// before the upstream responds are not sent in informational responses, and
// they stay in the final response.
//
// The gateway limits request bodies to [DefaultBodyLimit] when
// [Config.BodyLimit] is omitted (see [BodyLimit]).
//
// The relaxations ([Config.QueryPolicy], [Config.HeaderNamePolicy], and
// [Config.AllowAbsoluteForm]) apply to the complete gateway, because the
// checks protect the routing itself (ADR-0026). The other checks above,
// including the path policy, cannot be relaxed.
//
// The gateway does not change the request that the caller gave to ServeHTTP.
// Middleware must also not change it (see [Middleware]).
//
// # Header rules after middleware and hooks
//
// For each proxied request, after the route middleware and the
// [BeforeUpstreamHook] hooks, the gateway checks the complete outbound header
// map and the available outbound trailer names and values. The check also
// runs when no hook is configured, because middleware can set the same
// entries:
//
//   - It restores Connection, Upgrade, Te, Keep-Alive, Proxy-Connection, and
//     Proxy-Authorization to their values before the hooks.
//   - It removes Host, Content-Length, Transfer-Encoding, and Trailer. The
//     transport writes these from request fields.
//   - A header or trailer name with characters other than ASCII letters,
//     digits, and "-", or a header value with a control character other than
//     horizontal tab, including an available trailer value, is a failure in
//     [PhaseBeforeUpstream]. The gateway does not call the transport. The
//     trailer values that a client sends arrive at the end of the body; the
//     gateway checks them then (see "Request trailers").
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
// # Request trailers
//
// The request that shared middleware gets has its own trailer map, with
// the declared trailer names that the secure defaults keep (ADR-0027). When a
// read of the body returns [io.EOF], also with data in the same call, the
// gateway checks the trailers that arrived with the body, also the names that
// the request did not declare:
//
//   - A name with characters other than ASCII letters, digits, and "-": with
//     [HeaderNameReject], the read returns an error that wraps
//     [ErrInvalidTrailerName] instead of io.EOF, and no trailer is published;
//     with [HeaderNameDrop], the field is removed.
//   - Identity headers and managed headers, hop-by-hop fields (also names
//     in the inbound Connection options), Alt-Svc, and
//     the fields that a trailer must not contain (RFC 9110, section 6.5.1)
//     are removed: framing (Content-Length, Transfer-Encoding, Trailer),
//     Host, authentication (Authorization, Proxy-Authorization,
//     Www-Authenticate, Proxy-Authenticate, Realm, Cookie, Set-Cookie),
//     request modifiers (Cache-Control, Pragma, Expect, Max-Forwards, Range,
//     and each field whose name starts with "If-"), and content format
//     (Content-Type, Content-Encoding, Content-Range).
//
// The gateway then sets the other fields in the trailer map of the request
// that shared middleware got, and in the trailer map of the request that
// the transport gets when that request still has the body of the gateway and
// a trailer map. So middleware that replaces the body, or sets the trailer
// map to nil, forwards no client trailers. Each map gets its own copy of the
// values. The gateway does not replace a value that middleware set, and does
// not add back a declared name that middleware deleted. The
// writes run on the goroutine that read the end of the body: read the
// trailers only after the read that returned io.EOF, as [http.Request]
// requires. A read that fails, a truncated body, and a body over the limit
// publish no trailers, and Close does not read the body to find them. The
// transport sends the trailers that its protocol can send. HTTP/2 servers
// keep only declared trailers. A request copied before EOF also gets the
// checked values when it still has the gateway body. When the upstream
// responds before the end of the body and the transport continues to read
// it, the same check applies, but a rejected name only ends the upload: the
// gateway does not replace the response.
//
// On HTTP/1.1, net/http writes the trailers to the Trailer map of the request
// that its server made. When that map is nil, net/http makes a new map that
// only its own request has. So the gateway gets the trailers that the request
// did not declare only when ServeHTTP gets the request of the server, or a
// copy that shares a Trailer map that is not nil with it. [Server] makes such
// a map. Middleware in front of the gateway that copies a request without a
// trailer map, such as with [http.Request.WithContext], loses these
// trailers: they reach neither middleware nor the upstream.
//
// # Deviations from HTTP specifications
//
// These secure defaults deliberately differ from the HTTP specifications:
//
//   - RFC 9112, section 3.2.2, says that a server must accept the absolute
//     form. The gateway rejects it by default, because the authority in the
//     request target can disagree with the Host header that the client
//     sent. [Config.AllowAbsoluteForm] accepts it.
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
// A Gateway is a handler. Serve it with a [Server], which applies the
// server-level secure defaults: header-read timeouts, header size limits,
// HTTP/2 limits, transfer idle deadlines, and the idle deadlines and shutdown
// of upgraded connections (ADR-0016). When another server calls the gateway,
// these protections are the responsibility of that server.
type Gateway struct {
	handler        http.Handler
	hooks          compiledHooks
	managedHeaders map[string]struct{}
	ownedTransport *http.Transport
	renderer       Renderer

	// bodyLimit is the inherited default, not a deployment ceiling.
	bodyLimit int64

	admission        *AdmissionBudget
	queryPolicy      QueryPolicy
	headerNamePolicy HeaderNamePolicy
	absoluteForm     bool
}

// route is a validated route with its compiled handler.
type route struct {
	prefix  string
	handler http.Handler

	// bodyLimit is the effective limit of the route, or noBodyLimit.
	bodyLimit int64
	admission *AdmissionBudget
	buffering *RequestBuffering
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
// phase, it calls only the ManagedHeaders method of entries in
// [Config.HeaderOwners]. Then it calls each middleware function one time to
// build the handlers, and checks the handlers that they return. New does not
// change process-wide state, such as the default logger.
func New(cfg Config) (*Gateway, error) {
	g := &Gateway{
		renderer:         cfg.Renderer,
		admission:        cfg.Admission,
		queryPolicy:      cfg.QueryPolicy,
		headerNamePolicy: cfg.HeaderNamePolicy,
		absoluteForm:     cfg.AllowAbsoluteForm,
	}
	if g.renderer == nil {
		g.renderer = renderProblem
	}

	var problems []error
	g.hooks, g.managedHeaders, problems = checkConfig(cfg)
	if len(problems) != 0 {
		return nil, errors.Join(problems...)
	}

	g.bodyLimit = cfg.BodyLimit.resolve(DefaultBodyLimit)

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
		entry: r.Context(),
	}
	ctx := context.WithValue(r.Context(), requestContextKey{}, rc)

	returned := false
	defer func() {
		g.complete(ctx, rc, !returned)
	}()

	if status, reason := g.checkRequest(r); status != 0 {
		g.rejectRequest(ctx, w, rc, status, reason)
		returned = true

		return
	}

	// WithContext makes a shallow copy. toOriginForm gives it its own URL, and
	// stripHeaders its own maps.
	inner := r.WithContext(ctx)
	// A caller-provided replay could expose the unprotected source body.
	// Only explicit buffering supplies gateway-owned replay capabilities.
	inner.GetBody = nil
	toOriginForm(inner)
	g.stripHeaders(inner)
	rc.source = r
	g.handler.ServeHTTP(w, inner)
	returned = true
}

// complete runs during a panic too, and lets the panic continue.
func (g *Gateway) complete(ctx context.Context, rc *RequestContext, aborted bool) {
	// A completion hook must not panic, but the state is dropped also when
	// one does.
	defer rc.end()

	if len(g.hooks.completed) != 0 {
		snapshot := rc.completion(time.Since(rc.start), aborted)
		for _, hook := range g.hooks.completed {
			hook.RequestCompleted(ctx, rc, copyCompletion(snapshot))
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
		handler, err := chain(g.forward(proxy, r.Upstream), r.Middleware)
		if err != nil {
			problems = append(problems, invalid("route %d: %w", i, err))

			continue
		}

		var buffering *RequestBuffering
		if effective := r.RequestBuffering.resolve(cfg.RequestBuffering); effective != nil {
			copied := *effective
			buffering = &copied
		}

		routes = append(routes, route{
			prefix:    r.Prefix,
			handler:   handler,
			bodyLimit: r.BodyLimit.resolve(g.bodyLimit),
			admission: r.Admission,
			buffering: buffering,
		})
	}

	// The longest prefix is first, so the first match is the longest match.
	slices.SortFunc(routes, func(a, b route) int {
		return cmp.Compare(len(b.prefix), len(a.prefix))
	})

	shared, err := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, ok := g.requestContext(w, r)
		if ok {
			rc.matched.handler.ServeHTTP(w, r)
		}
	}), cfg.SharedMiddleware)
	if err != nil {
		problems = append(problems, invalid("shared %w", err))
	}

	return g.router(routes, shared), problems
}

// router matches once before any permitted body reader. Routes are sorted
// with the longest prefix first.
func (g *Gateway) router(routes []route, shared http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, ok := g.requestContext(w, r)
		if !ok {
			return
		}

		if g.admission != nil {
			if !g.admit(w, r, rc, g.admission) {
				return
			}

			// A deferred release also runs during a panic, which continues.
			defer g.admission.release()
		}

		path := r.URL.EscapedPath()
		for i := range routes {
			if strings.HasPrefix(path, routes[i].prefix) {
				rc.matched = &routes[i]
				rc.inboundMethod = r.Method
				g.serveRoute(w, r, rc, &routes[i], shared)

				return
			}
		}

		g.rejectRequest(r.Context(), w, rc, http.StatusNotFound, ReasonNoRoute)
	})
}

// serveRoute fixes body protection before route-aware checks or processing.
func (g *Gateway) serveRoute(w http.ResponseWriter, r *http.Request, rc *RequestContext, rt *route, shared http.Handler) {
	rc.body = g.wrapBody(rc.source, r, rt.bodyLimit)
	rc.source = nil
	if rt.bodyLimit != noBodyLimit && r.ContentLength > rt.bodyLimit {
		g.rejectRequest(r.Context(), w, rc, http.StatusRequestEntityTooLarge, ReasonBodyTooLarge)

		return
	}

	if rt.admission != nil {
		if !g.admit(w, r, rc, rt.admission) {
			return
		}

		defer rt.admission.release()
	}

	for _, hook := range g.hooks.matched {
		rejection, err := hook.RouteMatched(r.Context(), rc, r)
		if rc.hookResult(PhaseRouteMatched, rejection, err) != nil {
			g.respond(r.Context(), w, rc)

			return
		}
	}

	if rt.buffering != nil {
		store, ok := g.bufferRequest(w, r, rc, rt)
		if !ok {
			return
		}

		if store != nil {
			defer store.dispose()
		}
	}

	shared.ServeHTTP(w, r)
}

// admit takes a slot of b. When no slot is free, it rejects the request and
// reports false. The caller releases the slot.
func (g *Gateway) admit(w http.ResponseWriter, r *http.Request, rc *RequestContext, b *AdmissionBudget) bool {
	if b.tryAcquire() {
		return true
	}

	g.rejectRequest(r.Context(), w, rc, http.StatusServiceUnavailable, ReasonAdmissionSaturated)

	return false
}

// rejectRequest records a core rejection and sends its response. ctx must
// carry rc.
func (g *Gateway) rejectRequest(ctx context.Context, w http.ResponseWriter, rc *RequestContext, status int, reason RejectionReason) {
	rc.reject(status, reason, nil)
	g.respond(ctx, w, rc)
}

// wrapBody installs the fixed route limit and trailer check. source keeps
// the trailer map filled by net/http. Empty bodies remain unchanged.
func (g *Gateway) wrapBody(source, inner *http.Request, limit int64) *requestBody {
	if inner.Body == nil || inner.Body == http.NoBody {
		return nil
	}

	// The trailers that arrive with the body go to this map, also when the
	// request declared none.
	if inner.Trailer == nil {
		inner.Trailer = make(http.Header)
	}

	body := g.newRequestBody(source, inner, limit)
	inner.Body = body

	return body
}

// requestContext fails closed when middleware did not derive its context from
// the request context: it sends 500 and reports false.
func (g *Gateway) requestContext(w http.ResponseWriter, r *http.Request) (*RequestContext, bool) {
	rc, ok := FromContext(r.Context())
	if !ok {
		lostContextLog.Print("lost")
		g.writeProblem(r.Context(), w, Problem{
			Status: http.StatusInternalServerError,
		}, nil)
	}

	return rc, ok
}
