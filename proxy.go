package edgekit

import (
	"net/http"
	"net/http/httputil"

	"github.com/go-edgekit/edgekit/internal/safelog"
)

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
		ErrorHandler:   proxyFailed,
	}
}

// forward selects the upstream after route middleware and records it for rewrite.
func forward(proxy *httputil.ReverseProxy, selector UpstreamSelector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, ok := requestContext(w, r)
		if !ok {
			return
		}

		selected, err := selector.SelectUpstream(r)
		if err == nil {
			err = checkUpstreamURL(selected)
		}

		if err != nil {
			rc.fail(PhaseUpstreamSelection, err)
			reject(w, http.StatusInternalServerError)

			return
		}

		upstream := *selected
		rc.upstream = &upstream
		proxy.ServeHTTP(newInformationalWriter(w), r)

		// The server sends the trailers after the handlers return, so a
		// removal here is still in time.
		removeTrailers(w.Header())
	})
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

// proxyFailed chooses the response status by the failure source (ADR-0017).
func proxyFailed(w http.ResponseWriter, r *http.Request, err error) {
	// The proxy runs only after forward found the gateway context.
	rc, _ := FromContext(r.Context())

	// A failure without a recorded source comes from ReverseProxy itself, such
	// as a protocol switch that the upstream did not accept.
	rc.fail(PhaseUnknown, err)
	reject(w, failureStatus(rc.failure.Phase))
}

// failureStatus lists each phase, so the exhaustive linter makes a new phase
// choose its status.
func failureStatus(phase Phase) int {
	switch phase {
	case PhaseRoundTrip, PhaseUnknown:
		return http.StatusBadGateway
	case PhaseRequestReceived, PhaseUpstreamSelection, PhaseBeforeUpstream, PhaseUpstreamResponse:
		return http.StatusInternalServerError
	}

	return http.StatusInternalServerError
}

// reject sends a status-text response without failure details.
func reject(w http.ResponseWriter, status int) {
	http.Error(w, http.StatusText(status), status)
}
