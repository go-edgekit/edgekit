package edgekit

import (
	"context"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
)

// Problem describes a response that the gateway makes itself, for a
// rejection or a failure. It contains no failure details.
type Problem struct {
	// Status is the response status, from 400 to 599. 499 means that the
	// client is gone.
	Status int

	// Reason is the rejection reason. It is empty for a failure.
	Reason RejectionReason
}

// Renderer makes the body of a response that the gateway makes itself: for
// a rejection by the secure defaults or by routing, for a [Rejection] from a
// hook, and for each failure. Set it in [Config.Renderer]. The default writes
// a minimal RFC 9457 problem document:
//
//	Content-Type: application/problem+json
//
//	{"type":"about:blank","title":"Too Many Requests","status":429,"reason":"ratelimit.exceeded"}
//
// The title is the status text of [net/http.StatusText] and is absent when
// that text is empty. The reason is absent for a failure.
//
// A Renderer returns only the content type and the body. The gateway removes
// Content-Length and Content-Type from the response headers, adds the headers
// of the rejection, sets Content-Type and "X-Content-Type-Options: nosniff",
// and writes the status and the body. It replaces X-Content-Type-Options in
// all letter cases, so only nosniff is sent. It does not call the Renderer
// after the final response headers are sent. If contentType is empty or is
// not a valid header value, the gateway uses the default for that response.
//
// ctx is the context of the stage that made the response. For a rejection by
// the secure defaults, it is the context that the gateway got, with a
// [RequestContext] that has only the start time: the gateway gives no
// additional request content to the Renderer. When middleware did not derive
// its context from the request context, ctx has no [RequestContext], so
// [FromContext] reports false.
//
// The gateway only reads the returned body, and only until it wrote the body,
// before the request handling returns. A Renderer can return a shared body,
// such as a body that it made once, but it must not change a body that it
// returned while a request can still use it.
//
// A Renderer must be safe for concurrent use. A panic continues, as a panic
// in a hook does.
type Renderer func(ctx context.Context, p Problem) (contentType string, body []byte)

// problemContentType is the media type of RFC 9457, section 3.
const problemContentType = "application/problem+json"

// renderProblem is the default Renderer. The members need no JSON escapes:
// status texts are printable ASCII without quotes or backslashes, and reason
// codes contain only letters, digits, "_", and ".".
func renderProblem(_ context.Context, p Problem) (string, []byte) {
	var body []byte
	body = append(body, `{"type":"about:blank"`...)

	if title := http.StatusText(p.Status); title != "" {
		body = append(body, `,"title":"`...)
		body = append(body, title...)
		body = append(body, '"')
	}

	body = append(body, `,"status":`...)
	body = strconv.AppendInt(body, int64(p.Status), 10)

	if p.Reason != "" {
		body = append(body, `,"reason":"`...)
		body = append(body, p.Reason...)
		body = append(body, '"')
	}

	body = append(body, '}')

	return problemContentType, body
}

// respond writes the recorded outcome of rc, so rc must have one. It writes
// nothing after the final response is committed (ADR-0022): the outcome then
// stays a fact for completion only.
func (g *Gateway) respond(ctx context.Context, w http.ResponseWriter, rc *RequestContext) {
	if rc.committed {
		return
	}

	g.writeProblem(ctx, w, Problem{
		Status: rc.status,
		Reason: rc.rejection,
	}, rc.header)
}

// writeProblem must run before the final response headers are sent. header
// holds sanitized rejection headers, or nil.
func (g *Gateway) writeProblem(ctx context.Context, w http.ResponseWriter, p Problem, header http.Header) {
	h := w.Header()
	deleteFields(h, "Content-Length", "Content-Type", "X-Content-Type-Options")
	for name, values := range header {
		deleteFields(h, name)
		h[name] = slices.Clone(values)
	}

	contentType, body := g.renderer(ctx, p)
	if contentType == "" || invalidFieldValue(contentType) {
		contentType, body = renderProblem(ctx, p)
	}

	h["Content-Type"] = []string{contentType}
	h["X-Content-Type-Options"] = []string{"nosniff"}
	w.WriteHeader(p.Status)

	// The client can be gone; the gateway cannot do more for it.
	_, _ = w.Write(body)
}

// deleteFields removes the entries of all letter cases, so a non-canonical
// entry that middleware set does not reach the client next to the new
// value. The names must be canonical.
func deleteFields(header http.Header, canonical ...string) {
	for name := range header {
		if slices.Contains(canonical, textproto.CanonicalMIMEHeaderKey(name)) {
			delete(header, name)
		}
	}
}
