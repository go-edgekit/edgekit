package edgekit

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/go-edgekit/edgekit/internal/safelog"
)

// errInvalidUpgrade is the failure when middleware passes on an Upgrade value
// that ReverseProxy refuses. It does not contain the value.
var errInvalidUpgrade = errors.New("upgrade value of the outbound request is not printable ASCII")

// errInvalidOutboundRequest contains no middleware or request values.
var errInvalidOutboundRequest = errors.New("outbound request does not obey request protections")

// newProxy shares one proxy whose transport records failure sources.
func (g *Gateway) newProxy(transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: rewrite,
		Transport: &outboundTransport{
			base:  transport,
			hooks: g.hooks.before,
		},
		ErrorLog:       safelog.New("reverse_proxy"),
		ModifyResponse: g.modifyResponse,
		ErrorHandler:   g.proxyFailed,
	}
}

// forward selects the upstream after route middleware and records it for rewrite.
func (g *Gateway) forward(proxy *httputil.ReverseProxy, selector UpstreamSelector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, ok := g.requestContext(w, r)
		if !ok {
			return
		}

		// A copied request can change the outbound representation, never the
		// original route or its protections. Middleware output is trusted-code
		// output, so a broken request is an internal failure.
		if !g.validOutbound(rc, r) {
			rc.failDefect(PhaseBeforeUpstream, errInvalidOutboundRequest)
			g.respond(r.Context(), w, rc)

			return
		}

		selected, err := selector.SelectUpstream(r)
		if err != nil {
			rc.fail(PhaseUpstreamSelection, err)
			g.respond(r.Context(), w, rc)

			return
		}

		if err := checkUpstreamURL(selected); err != nil {
			rc.failDefect(PhaseUpstreamSelection, err)
			g.respond(r.Context(), w, rc)

			return
		}

		upstream := *selected
		rc.upstream = &upstream

		// The secure defaults checked the client's value, so only middleware
		// can have set this one. ReverseProxy would report it without a
		// source.
		if !upgradeAllowed(r.Header) {
			rc.failDefect(PhaseBeforeUpstream, errInvalidUpgrade)
			g.respond(r.Context(), w, rc)

			return
		}

		if rc.buffer != nil {
			r = rc.bufferedOutbound(r)
		} else {
			rc.body.recordCarried(r)
		}

		proxy.ServeHTTP(newProxyWriter(w, rc), r)

		// The server sends the trailers after the handlers return, so a
		// removal here is still in time.
		removeTrailers(w.Header())
	})
}

// validOutbound reports whether middleware output obeys the request
// protections. It requires the origin form also when the inbound checks
// accept the absolute form, because only the route selector chooses the
// destination. An unchanged method keeps the inbound behavior: the inbound
// checks do not require a token either, and later stages handle that method.
func (g *Gateway) validOutbound(rc *RequestContext, r *http.Request) bool {
	if r.URL == nil || !isOriginForm(r) || invalidFieldValue(r.Host) {
		return false
	}

	methodChanged := r.Method != rc.inboundMethod
	if methodChanged && !methodToken(r.Method) {
		return false
	}

	status, _ := g.checkRequest(r)

	return status == 0
}

// methodToken reports whether method is a token (RFC 9110, sections 9.1 and
// 5.6.2).
func methodToken(method string) bool {
	if method == "" {
		return false
	}

	for i := range len(method) {
		if !isHeaderNameChar(method[i]) && !strings.ContainsRune("!#$%&'*+.^_`|~", rune(method[i])) {
			return false
		}
	}

	return true
}

// rewrite runs after ReverseProxy removes hop-by-hop and forwarding headers.
func rewrite(pr *httputil.ProxyRequest) {
	// forward always sets the upstream. The check is a fail-safe: without an
	// upstream, the outbound URL has no scheme or host, so the transport
	// fails and the client gets 502 instead of a panic in SetURL.
	if rc, ok := FromContext(pr.In.Context()); ok && rc.upstream != nil {
		pr.SetURL(rc.upstream)
	}

	// ReverseProxy removes the query parameters that it cannot parse before
	// Rewrite runs. The upstream URL has no query, so the inbound query is
	// the complete outbound query.
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery

	pr.SetXForwarded()
}

// proxyFailed renders the outcome that a stage recorded (ADR-0017, ADR-0023).
func (g *Gateway) proxyFailed(w http.ResponseWriter, r *http.Request, err error) {
	// The proxy runs only after forward found the gateway context.
	rc, _ := FromContext(r.Context())

	// ReverseProxy reports some failures without a source. The stage is
	// known when the gateway accepted a 101 response before.
	phase := PhaseUnknown
	if rc.switching {
		phase = PhaseProtocolSwitch
	}

	rc.fail(phase, err)

	// ReverseProxy leaves the upstream connection open after a protocol
	// mismatch or a writer that cannot hijack. The shared body closes the
	// underlying stream only once, also if ReverseProxy closes it.
	if rc.upgradeBody != nil {
		_ = rc.upgradeBody.Close()
		rc.upgradeBody = nil
	}

	g.respond(r.Context(), w, rc)
}
