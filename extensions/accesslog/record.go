package accesslog

import (
	"log/slog"

	"github.com/go-edgekit/edgekit"
)

// Record is the access-log record of one request. It is a snapshot: the
// recorder does not change it after it gives it to the [Sink], and a Sink
// can keep it.
//
// A Record joins three kinds of information, which have different owners
// (ADR-0009):
//   - HTTP observations of the middleware: Status, BytesWritten, WriteErr,
//     and Hijacked. They show what the response writer accepted, not what
//     the client received.
//   - The upstream status that the UpstreamResponse hook of the recorder
//     observed: UpstreamStatus.
//   - The execution facts of the gateway: Completion.
//
// Fields that are safe by default contain no request content, no
// credentials, no query, no body, and no error text: Observed, RequestID,
// Status, BytesWritten, Hijacked, UpstreamStatus, and of Completion: Start,
// Duration, RejectionReason, Failure.Phase, ConnectionObtained,
// RequestWritten, and Aborted. A RejectionReason from an extension is
// safe only when the extension obeys the rule of [edgekit.Rejection]: a
// static name, never an identifier or a request value.
//
// The other fields are sensitive. A Sink must apply its own policy before it
// writes them:
//   - Method can be any method token that the client sent.
//   - Path is the escaped request path. It can contain identifiers or
//     secrets.
//   - TraceID is the value that [Config.TraceID] returned. The recorder does
//     not check it.
//   - WriteErr and Completion.Failure.Err are errors without changes. Their
//     text can contain addresses, URLs, or request data.
//   - Completion.SelectedUpstream is the upstream URL that the upstream
//     selector returned. It can contain user information and a query from
//     the configuration.
//   - Completion.RoutePrefix is a label from the configuration.
//
// LogValue gives only the fields that are safe by default, so
// slog.Info("request", slog.Any("record", r)) does not write the sensitive
// fields.
type Record struct {
	// Observed is true when the middleware of the recorder handled the
	// request. When it is false, only Completion is set: for example for a
	// rejection by the secure defaults, which occurs before any middleware,
	// or when the middleware is not registered.
	Observed bool

	// RequestID is the request ID that the middleware made: 32 lowercase
	// hexadecimal digits. It is empty when Observed is false.
	RequestID string

	// Method is the request method.
	Method string

	// Path is the escaped path of the request, without the query.
	Path string

	// TraceID is the value that Config.TraceID returned, or "".
	TraceID string

	// Status is the final status that the response writer got, also an
	// implicit 200 of a write or a flush. It is 0 when no final status was
	// observed: for example during a panic before a write, after a hijack,
	// or when Observed is false. Informational (1xx) statuses are not
	// recorded, except an explicit 101.
	Status int

	// BytesWritten is the number of body bytes that the response writer
	// accepted. It does not include headers, framing, or the data of a
	// hijacked connection, and it does not prove that the client got them.
	// After a streaming failure, it is the count before the failure.
	BytesWritten int64

	// WriteErr is the first error of a write or a flush of the response, or
	// nil. It is an HTTP observation, separate from Completion.Failure: a
	// client that is gone gives a WriteErr, not a Failure.
	WriteErr error

	// Hijacked is true when a component took the connection through the
	// response writer, such as the gateway for a protocol switch (101).
	Hijacked bool

	// UpstreamStatus is the status of the upstream response when the
	// UpstreamResponse hook of the recorder ran, or 0 when no upstream
	// response reached it. Register the recorder last in UpstreamResponse
	// to observe changes from earlier hooks. If an earlier hook rejects or
	// fails, the recorder does not run and UpstreamStatus is 0.
	UpstreamStatus int

	// Completion holds the execution facts of the gateway, without changes.
	Completion edgekit.Completion
}

// LogValue implements [slog.LogValuer]. It gives only the fields that are
// safe by default (see [Record]). For a failure, it gives the phase, not the
// error. It gives only whether a write error, a route, and a selected
// upstream exist.
func (r Record) LogValue() slog.Value {
	c := r.Completion
	attrs := []slog.Attr{
		slog.Bool("observed", r.Observed),
		slog.String("request_id", r.RequestID),
		slog.Int("status", r.Status),
		slog.Int64("bytes", r.BytesWritten),
		slog.Bool("write_failed", r.WriteErr != nil),
		slog.Bool("hijacked", r.Hijacked),
		slog.Int("upstream_status", r.UpstreamStatus),
		slog.Time("start", c.Start),
		slog.Duration("duration", c.Duration),
		slog.Bool("route_matched", c.RoutePrefix != ""),
		slog.Bool("upstream_selected", c.SelectedUpstream != nil),
		slog.String("rejection_reason", string(c.RejectionReason)),
	}

	if c.Failure != nil {
		attrs = append(attrs, slog.String("failure_phase", c.Failure.Phase.String()))
	}

	attrs = append(attrs,
		slog.Bool("connection_obtained", c.ConnectionObtained),
		slog.Bool("request_written", c.RequestWritten),
		slog.Bool("aborted", c.Aborted),
	)

	return slog.GroupValue(attrs...)
}
