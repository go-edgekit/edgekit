package gateway

import "net/http"

// outboundTransport provides a fallible boundary after ReverseProxy.Rewrite.
// It is shared across routes and requests; hooks run serially within a request.
type outboundTransport struct {
	base  http.RoundTripper
	hooks []BeforeUpstreamHook
}

func (t *outboundTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// RoundTripper must not mutate the caller's request metadata. The clone is
	// the request passed to both hooks and the underlying transport.
	out := r.Clone(r.Context())
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
	return t.base.RoundTrip(out)
}
