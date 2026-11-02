package edgekit

import "net/http"

// outboundTransport provides a fallible boundary after ReverseProxy.Rewrite.
// It is shared across routes and requests; hooks run serially within a request.
type outboundTransport struct {
	base  http.RoundTripper
	hooks []BeforeUpstreamHook
}

func (t *outboundTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Isolate hook mutations from the caller's request metadata.
	out := r
	if len(t.hooks) != 0 {
		out = r.Clone(r.Context())
	}
	rc, _ := FromContext(out.Context())
	for _, hook := range t.hooks {
		if err := hook.OnBeforeUpstream(out.Context(), rc, out); err != nil {
			// No transport will take ownership of the body on this path.
			if out.Body != nil {
				_ = out.Body.Close()
			}
			return nil, err
		}
	}
	response, err := t.base.RoundTrip(out)
	if response != nil && response.Request == nil {
		// Custom RoundTrippers may omit Request. Response hooks still need
		// the execution context that was passed to the transport.
		response.Request = out
	}
	return response, err
}
