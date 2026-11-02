// Package requesttag demonstrates middleware and hooks sharing typed state.
package requesttag

import (
	"context"
	"net/http"

	"github.com/go-edgekit/edgekit"
)

// Record describes one completed request, including locally handled requests.
type Record struct {
	Tag         string
	RoutePrefix string
	Proxied     bool
	Outcome     edgekit.Outcome
}

// Extension puts a configured tag into request state, adds it to upstream
// response headers, and reports completion. It is safe to share across requests
// if the supplied report function is safe for concurrent calls.
type Extension struct {
	tag    string
	key    *edgekit.StateKey[string]
	report func(Record)
}

// New creates an example extension. A nil report function disables reporting.
// The tag must be a valid HTTP header value and must not contain secrets.
func New(tag string, report func(Record)) *Extension {
	return &Extension{tag: tag, key: edgekit.NewStateKey[string](), report: report}
}

// Middleware initializes state before lifecycle hooks run.
func (e *Extension) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rc, ok := edgekit.FromContext(r.Context()); ok {
			edgekit.SetState(rc, e.key, e.tag)
		}
		next.ServeHTTP(w, r)
	})
}

// OnUpstreamResponse adds the middleware's tag before headers are forwarded.
func (e *Extension) OnUpstreamResponse(_ context.Context, rc *edgekit.RequestContext, response *http.Response) error {
	if tag, ok := edgekit.GetState(rc, e.key); ok {
		response.Header.Set("X-Request-Tag", tag)
	}
	return nil
}

// OnComplete reports the shared state and gateway outcome.
func (e *Extension) OnComplete(_ context.Context, rc *edgekit.RequestContext, outcome edgekit.Outcome) {
	if e.report == nil {
		return
	}
	tag, _ := edgekit.GetState(rc, e.key)
	prefix, _ := rc.RoutePrefix()
	_, proxied := rc.Upstream()
	e.report(Record{Tag: tag, RoutePrefix: prefix, Proxied: proxied, Outcome: outcome})
}
