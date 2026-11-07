// Package accesslog is a bundled extension that makes one access-log record
// for each request that a gateway handles. A record joins the HTTP
// observations of its middleware, the trace ID of the request, and the
// execution facts of [edgekit.Completion] (ADR-0009, ADR-0029).
//
// The extension uses only the public contract of edgekit and the standard
// library. It has no logging backend and no tracing SDK: a [Sink] owns the
// output, and [Config.TraceID] reads the trace ID from the context of a
// tracing SDK.
//
// # Registration
//
// Register the same [Recorder] as the first global middleware and as the
// first hook provider:
//
//	recorder, err := accesslog.New(accesslog.Config{Sink: sink})
//	// ...
//	gateway, err := edgekit.New(edgekit.Config{
//		Middleware: []edgekit.Middleware{tracing, recorder.Middleware},
//		Hooks:      []any{recorder, signer},
//		Routes:     routes,
//	})
//
// The middleware must be outside each middleware whose responses it must
// observe, such as recovery or authorization middleware. Middleware that
// gives the trace context, such as tracing middleware, must be outside the
// recorder, so [Config.TraceID] can read it. As the first hook provider, the
// recorder sets the request ID before the other BeforeUpstream hooks, sees
// the upstream response after the other UpstreamResponse hooks, and reports
// after the other completion hooks (ADR-0021).
//
// # Records
//
// The recorder reports one [Record] for each request, at completion, also
// for a request that its middleware did not handle, such as a rejection by
// the secure defaults (ADR-0018). Such a record has only the execution facts
// (see [Record.Observed]).
//
// A Record contains sensitive fields. Log a Record as a value of
// [log/slog]: its LogValue method gives only the fields that are safe by
// default. See [Record] for the classification of each field.
//
// # Request ID
//
// The middleware makes a random request ID for each request. The recorder
// declares [RequestIDHeader] as a managed header, so the gateway removes the
// values that the client sent. The recorder sends the ID to the upstream and
// in the final response headers to the client. It removes the values of the
// upstream from the final response, from informational (1xx) responses, and
// from response trailers, so the client gets only the ID of the recorder.
// Get the ID with [Recorder.RequestID].
package accesslog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/textproto"
	"reflect"
	"strings"

	"github.com/go-edgekit/edgekit"
)

// RequestIDHeader is the header that carries the request ID to the upstream
// and to the client.
const RequestIDHeader = "X-Request-ID"

// Config is the configuration of a [Recorder].
type Config struct {
	// Sink gets the records. It is required. Nil values, also typed nil
	// values, are not permitted, as for the extension points of the gateway:
	// a nil pointer, function, map, slice, or channel is rejected, also when
	// its Report method does not use the receiver.
	Sink Sink

	// TraceID returns the trace ID of the request context ctx, or "" when
	// ctx has no trace. It is optional.
	//
	// The middleware calls it once for each request, with the context that
	// the outer middleware passed on, so the trace of a tracing middleware
	// outside the recorder is still active. The recorder keeps only the
	// returned string. It does not keep, end, or change a span. TraceID must
	// be safe for concurrent use.
	TraceID func(ctx context.Context) string
}

// Sink gets the access-log records of a [Recorder].
//
// The recorder calls Report once for each request, at completion, on the
// goroutine that handles the request, before the request handling returns.
// So a slow Report delays the end of the request: net/http can hold back the
// part of the response that is not flushed yet, the connection stays busy,
// and [edgekit.Server.Shutdown] waits. The admission budgets of the gateway
// do not limit this work, because the gateway releases their slots before
// completion. A Sink that does I/O must not block: for example, it sends the
// record to a bounded queue and drops the record when the queue is full.
//
// Report gets no context. The completion context can be canceled already,
// for example when the client is gone, so it must not cancel the export of
// a record, and the record already holds the trace ID. A Sink that needs a
// context for its own work makes one.
//
// Report runs concurrently for different requests, so it must be safe for
// concurrent use. It can keep the record. Report must not panic. When it
// panics, the recorder recovers the panic and drops the record, so the other
// completion hooks still run. The recorder cannot report the drop.
type Sink interface {
	Report(r Record)
}

// SinkFunc is a function that is a [Sink].
type SinkFunc func(r Record)

// Report calls f(r).
func (f SinkFunc) Report(r Record) {
	f(r)
}

// ErrNoSink is the error of [New] for a [Config] without a [Sink], also when
// [Config.Sink] holds a typed nil value.
var ErrNoSink = errors.New("accesslog: config has no sink")

// Recorder makes the access-log records. It is a middleware and a hook
// provider of the gateway. Make it with [New], and register it as the
// package documentation describes.
//
// A Recorder is safe for concurrent use. It keeps its request state in the
// gateway context of each request, so the state of one request is not
// visible to other requests and ends at completion.
type Recorder struct {
	sink    Sink
	traceID func(ctx context.Context) string
	key     *edgekit.StateKey[*Record]
}

// New returns a Recorder for cfg. It returns [ErrNoSink] when cfg.Sink is
// nil, also when the interface holds a typed nil value. It copies cfg, so later
// changes to cfg have no effect.
func New(cfg Config) (*Recorder, error) {
	if isNilSink(cfg.Sink) {
		return nil, ErrNoSink
	}

	return &Recorder{
		sink:    cfg.Sink,
		traceID: cfg.TraceID,
		key:     edgekit.NewStateKey[*Record](),
	}, nil
}

// isNilSink also detects a typed nil, with the same rule as the
// configuration check of the gateway.
func isNilSink(sink Sink) bool {
	if sink == nil {
		return true
	}

	value := reflect.ValueOf(sink)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Middleware observes the request and the response. It is an
// [edgekit.Middleware]. Register it as the first global middleware.
//
// In a gateway, it makes a request ID, sets [RequestIDHeader] in the
// response headers, calls [Config.TraceID], and gives the next handler a
// response writer that records the final status, the body bytes that the
// writer accepted, the first write or flush error, and a hijack. The writer
// has the optional interfaces [http.Flusher], [http.Hijacker], and
// [http.Pusher] only when the original writer has them, and an Unwrap
// method for [http.ResponseController]. It writes and flushes at once.
//
// The writer removes [RequestIDHeader] from informational (1xx) responses
// other than 101 and sets the ID again for the final response. When the next
// handler returns, the middleware removes [RequestIDHeader] values from the
// trailers in the header map. So the client gets only the generated ID.
//
// When the next handler returns without a write, the record gets the
// status 200 that net/http then sends. During a panic, the record keeps the
// status that it observed, or 0, never an invented status.
//
// Outside a gateway, and when the request already passed through this
// middleware of the same Recorder, the middleware calls next without
// changes: it sets no header and records nothing.
//
// Writes to the writer that Unwrap returns are not recorded. The writer has
// no io.ReaderFrom method, so a copy goes through Write.
func (r *Recorder) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rc, ok := edgekit.FromContext(req.Context())
		if !ok {
			next.ServeHTTP(w, req)

			return
		}

		if _, ok := edgekit.State(rc, r.key); ok {
			next.ServeHTTP(w, req)

			return
		}

		// The request state holds the observations. Only the goroutine that
		// handles the request changes them. Completion copies them, so the
		// Sink gets a snapshot.
		obs := &Record{
			RequestID: newRequestID(),
			Method:    req.Method,
			Path:      req.URL.EscapedPath(),
		}
		if r.traceID != nil {
			obs.TraceID = r.traceID(req.Context())
		}

		edgekit.SetState(rc, r.key, obs)
		w.Header().Set(RequestIDHeader, obs.RequestID)

		next.ServeHTTP(newResponseWriter(w, obs), req)

		// ReverseProxy copies upstream trailers into the header map after
		// the body, and net/http sends them after the handler returns.
		removeRequestIDTrailers(w.Header())

		// net/http sends 200 when a handler returns without a write.
		if obs.Status == 0 && !obs.Hijacked {
			obs.Status = http.StatusOK
		}
	})
}

// RequestID returns the request ID that the middleware made for the request
// of rc. It reports false when the middleware did not handle the request,
// and after completion.
func (r *Recorder) RequestID(rc *edgekit.RequestContext) (string, bool) {
	obs, ok := edgekit.State(rc, r.key)
	if !ok {
		return "", false
	}

	return obs.RequestID, true
}

// ManagedHeaders declares [RequestIDHeader], so the gateway removes the
// values that the client sent. It implements [edgekit.HeaderOwner].
func (r *Recorder) ManagedHeaders() []string {
	return []string{RequestIDHeader}
}

// BeforeUpstream sets [RequestIDHeader] in the outbound request. It
// implements [edgekit.BeforeUpstreamHook] and never rejects or fails.
func (r *Recorder) BeforeUpstream(_ context.Context, rc *edgekit.RequestContext, out *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
	if obs, ok := edgekit.State(rc, r.key); ok {
		out.Header().Set(RequestIDHeader, obs.RequestID)
	}

	return nil, nil
}

// UpstreamResponse records the upstream status, also of a 101 response, and
// removes the [RequestIDHeader] values of the upstream from the response
// headers and the declared trailers, so the client gets only the ID of the
// recorder. It implements [edgekit.UpstreamResponseHook] and never rejects
// or fails.
func (r *Recorder) UpstreamResponse(_ context.Context, rc *edgekit.RequestContext, resp *http.Response) (*edgekit.Rejection, error) {
	if obs, ok := edgekit.State(rc, r.key); ok {
		obs.UpstreamStatus = resp.StatusCode
		resp.Header.Del(RequestIDHeader)

		// A declared trailer is announced before the body. A value that
		// arrives later is removed by the middleware.
		resp.Trailer.Del(RequestIDHeader)
	}

	return nil, nil
}

// RequestCompleted makes the record and gives it to the [Sink]. It
// implements [edgekit.RequestCompletedHook].
func (r *Recorder) RequestCompleted(_ context.Context, rc *edgekit.RequestContext, c edgekit.Completion) {
	var record Record
	if obs, ok := edgekit.State(rc, r.key); ok {
		record = *obs
		record.Observed = true
	}

	record.Completion = c

	r.report(record)
}

// report drops the record when the sink panics, because a completion hook
// must not panic: the gateway does not recover it, so the later completion
// hooks would not run.
func (r *Recorder) report(record Record) {
	defer func() {
		// The panic value can contain anything, so it is not kept.
		_ = recover()
	}()

	r.sink.Report(record)
}

// requestIDCanonical is the canonical form of RequestIDHeader, the form of
// the keys in a header map.
var requestIDCanonical = textproto.CanonicalMIMEHeaderKey(RequestIDHeader)

// removeRequestIDTrailers deletes the trailer entries of RequestIDHeader in
// all letter cases. ReverseProxy writes an undeclared trailer as a key with
// http.TrailerPrefix.
func removeRequestIDTrailers(header http.Header) {
	for name := range header {
		trailer, ok := strings.CutPrefix(name, http.TrailerPrefix)
		if ok && textproto.CanonicalMIMEHeaderKey(trailer) == requestIDCanonical {
			delete(header, name)
		}
	}
}

// newRequestID returns 128 random bits as 32 lowercase hexadecimal digits.
func newRequestID() string {
	var id [16]byte

	// crypto/rand.Read never returns an error; it ends the program instead.
	_, _ = rand.Read(id[:])

	return hex.EncodeToString(id[:])
}
