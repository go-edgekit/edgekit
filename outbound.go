package edgekit

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"slices"
	"sync/atomic"
)

// OutboundRequest gives a [BeforeUpstreamHook] the rewritten outbound
// request. Its only mutable part is the header map, so a hook cannot change
// the destination that the upstream selector chose, the path, the query, the
// method, the Host header, or the body.
//
// Use an OutboundRequest only during the hook call that gets it. Do not keep
// the value or its header map. The gateway gives the same value to all
// BeforeUpstream hooks of a request, and the methods panic after the last of
// these hooks returns.
type OutboundRequest struct {
	r *http.Request
}

// Method returns the request method.
func (o *OutboundRequest) Method() string {
	return o.r.Method
}

// URL returns a copy of the outbound URL: the scheme and host of the selected
// upstream, the escaped path, and the raw query. Changes to the copy have no
// effect on the request.
func (o *OutboundRequest) URL() *url.URL {
	u := *o.r.URL

	return &u
}

// Host returns the value that the transport sends in the Host header. The
// contract of [UpstreamSelector] makes sure that the transport does not
// normalize it, so a hook can sign it.
func (o *OutboundRequest) Host() string {
	// The transport uses the URL host when Request.Host is empty, which
	// ProxyRequest.SetURL makes it.
	if o.r.Host != "" {
		return o.r.Host
	}

	return o.r.URL.Host
}

// ContentLength returns the length of the request body. The value -1 means
// that the length is not known.
func (o *OutboundRequest) ContentLength() int64 {
	return o.r.ContentLength
}

// Header returns the outbound header map. The hook can change it.
func (o *OutboundRequest) Header() http.Header {
	return o.r.Header
}

// Failures of the outbound header check. They do not contain the entry.
var (
	errInvalidOutboundName  = errors.New("outbound header or trailer name is invalid")
	errInvalidOutboundValue = errors.New("outbound header value is invalid")
)

// errUpgradeBodyNotWritable is the failure when the transport returns a 101
// response with a body that ReverseProxy cannot write to.
var errUpgradeBodyNotWritable = errors.New("transport returned a 101 response with a body that cannot be written")

// connectionFields are the outbound hop-by-hop fields that the core owns. The
// BeforeUpstream hooks cannot change them.
var connectionFields = [...]string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Upgrade",
}

// generatedRequestFields are fields that the transport writes from request
// fields, not from the header map.
var generatedRequestFields = map[string]struct{}{
	"Content-Length":    {},
	"Host":              {},
	"Trailer":           {},
	"Transfer-Encoding": {},
}

// outboundTransport is the fallible step after ReverseProxy.Rewrite, which
// cannot return an error. It is shared by all routes and requests.
type outboundTransport struct {
	base  http.RoundTripper
	hooks []BeforeUpstreamHook
}

// RoundTrip records failures in the gateway context, so the error handler can
// choose the status by the source of the failure.
func (t *outboundTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// forward made sure that the context has the gateway context.
	rc, _ := FromContext(r.Context())

	if err := t.prepare(rc, r); err != nil {
		// A RoundTripper must close the body, also when it fails.
		if r.Body != nil {
			_ = r.Body.Close()
		}

		return nil, err
	}

	// The trailers that arrive at the end of the body go to r, also when
	// they arrive after the proxy copied the trailer map (ADR-0027).
	rc.body.attachOutbound(r)

	// Only the transport gets the trace. The UpstreamResponse hooks get the
	// context of r, so their own requests cannot set the facts.
	facts := &transportFacts{}
	rc.facts = facts
	traced := r.WithContext(httptrace.WithClientTrace(r.Context(), facts.clientTrace()))

	resp, err := t.base.RoundTrip(traced)
	if err != nil {
		rc.fail(PhaseRoundTrip, err)

		return nil, err
	}

	// ReverseProxy would report this body without a source. The response
	// hooks do not run, because the transport broke its contract.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		if _, ok := resp.Body.(io.ReadWriteCloser); !ok {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}

			rc.failDefect(PhaseRoundTrip, errUpgradeBodyNotWritable)

			return nil, errUpgradeBodyNotWritable
		}
	}

	// The UpstreamResponse hooks get their context and the gateway context
	// from Request. A RoundTripper that is not http.Transport can leave it
	// empty or set another request, so the gateway sets it.
	resp.Request = r

	return resp, nil
}

// prepare runs the BeforeUpstream hooks and then the outbound header check,
// also without hooks, because middleware can set the same entries (ADR-0019).
// It changes the header map of r, which ReverseProxy made for this request
// only. It records a failure or a rejection in rc.
func (t *outboundTransport) prepare(rc *RequestContext, r *http.Request) error {
	var saved [len(connectionFields)][]string
	for i, name := range connectionFields {
		saved[i] = slices.Clone(r.Header[name])
	}

	rc.outbound = OutboundRequest{r: r}
	defer func() {
		rc.outbound = OutboundRequest{}
	}()

	for _, hook := range t.hooks {
		rejection, hookErr := hook.BeforeUpstream(r.Context(), rc, &rc.outbound)
		if err := rc.hookResult(PhaseBeforeUpstream, rejection, hookErr); err != nil {
			return err
		}
	}

	if err := sanitizeOutbound(r, &saved); err != nil {
		rc.failDefect(PhaseBeforeUpstream, err)

		return err
	}

	return nil
}

// transportFacts holds the trace events of the transport (ADR-0023). The
// callbacks can run on other goroutines, also after completion, so they
// change only this value.
type transportFacts struct {
	connected atomic.Bool
	written   atomic.Bool
}

// clientTrace returns the hooks that set the facts for each attempt of the
// round trip.
func (f *transportFacts) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			f.connected.Store(true)
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				f.written.Store(true)
			}
		},
	}
}

// sanitizeOutbound needs the values that connectionFields had before the
// hooks, in the same order. It checks only the trailer names, because the
// trailer values arrive while the body streams.
func sanitizeOutbound(r *http.Request, saved *[len(connectionFields)][]string) error {
	for name := range r.Trailer {
		if !headerNameAllowed(name) {
			return errInvalidOutboundName
		}
	}

	header := r.Header
	for name, values := range header {
		if !headerNameAllowed(name) {
			return errInvalidOutboundName
		}

		if slices.ContainsFunc(values, invalidFieldValue) {
			return errInvalidOutboundValue
		}

		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if _, ok := generatedRequestFields[canonical]; ok || slices.Contains(connectionFields[:], canonical) {
			delete(header, name)
		}
	}

	for i, name := range connectionFields {
		if saved[i] != nil {
			header[name] = saved[i]
		}
	}

	return nil
}

// invalidFieldValue uses the rule of the transport, which fails the round trip
// for such a value.
func invalidFieldValue(value string) bool {
	for i := range len(value) {
		if c := value[i]; c < ' ' && c != '\t' || c == 0x7f {
			return true
		}
	}

	return false
}
