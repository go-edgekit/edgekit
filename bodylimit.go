package edgekit

import (
	"errors"
	"net/http"
)

// DefaultBodyLimit is the request-body limit in bytes of a gateway whose
// [Config.BodyLimit] is omitted: 10 MiB.
//
// It is the initial default for ordinary API requests. It is not a value that
// is safe for all workloads. Set an explicit limit for upload routes and other
// routes with large bodies. A later release can change the default, with a
// stated reason and the effect on compatibility. A caller that needs an exact
// limit sets it explicitly.
const DefaultBodyLimit = 10 << 20

// ErrRequestBodyTooLarge is the error that a request-body reader gets when
// the body is larger than the body limit (see [BodyLimit]). The error also
// wraps an [*http.MaxBytesError] with the limit. Use [errors.Is] to identify
// it.
var ErrRequestBodyTooLarge = errors.New("request body too large")

// bodyLimitKind is the state of a BodyLimit.
type bodyLimitKind uint8

// The states of a BodyLimit. The zero value is omitted.
const (
	bodyLimitOmitted bodyLimitKind = iota
	bodyLimitBytes
	bodyLimitDisabled
)

// noBodyLimit is the internal limit for a disabled body limit.
const noBodyLimit int64 = -1

// BodyLimit is the request-body limit of a gateway or a route. It is a secure
// default (ADR-0025, ADR-0031). The zero value means that the limit is
// omitted: for [Config.BodyLimit], the gateway uses [DefaultBodyLimit]; for
// [Route.BodyLimit], the route uses the limit of the gateway. Make other
// values with [MaxBodyBytes] and [NoBodyLimit].
//
// The gateway selects one effective limit after matching and before any body
// reader. It rejects an excessive declared length with 413 and
// [ReasonBodyTooLarge] (RFC 9110, section 15.5.14). The limit never changes.
// It counts bytes exposed by the inbound HTTP body reader, excluding transfer
// framing. It does not decompress, bound server cleanup reads or network
// bytes, or bound middleware-generated bodies. Generated or decompressed
// output needs its own bounds. Streaming is the default, without read-ahead;
// [Route.RequestBuffering] explicitly enables complete bounded buffering.
//
// A reader that reads more than the limit gets an error that wraps
// [ErrRequestBodyTooLarge]. When the transport fails with this error, the
// gateway sends 413 if it can still send a response (see [Gateway]). The
// streaming limit is not an atomic rejection: the upstream can receive a part
// of the body before the overflow, and an upstream that responds before the
// overflow can finish the exchange without a failure.
type BodyLimit struct {
	kind bodyLimitKind
	max  int64
}

// MaxBodyBytes returns a limit of n bytes. MaxBodyBytes(0) permits only empty
// bodies. A negative n gives a limit that [New] rejects.
func MaxBodyBytes(n int64) BodyLimit {
	return BodyLimit{
		kind: bodyLimitBytes,
		max:  n,
	}
}

// NoBodyLimit returns the explicit relaxation that disables the body limit.
// Use it only for routes whose upstream limits the body itself.
func NoBodyLimit() BodyLimit {
	return BodyLimit{
		kind: bodyLimitDisabled,
	}
}

// check reports a negative byte count.
func (l BodyLimit) check() error {
	if l.kind == bodyLimitBytes && l.max < 0 {
		return errors.New("is negative")
	}

	return nil
}

// resolve returns the limit in bytes, or noBodyLimit, with fallback for an
// omitted limit. l must have passed check.
func (l BodyLimit) resolve(fallback int64) int64 {
	switch l.kind {
	case bodyLimitBytes:
		return l.max
	case bodyLimitDisabled:
		return noBodyLimit
	default:
		return fallback
	}
}

// tooLarge wraps ErrRequestBodyTooLarge and the error of
// http.MaxBytesReader, so code that knows either one finds it.
func tooLarge(limit int64) error {
	return &bodyTooLargeError{
		maxBytes: &http.MaxBytesError{
			Limit: limit,
		},
	}
}

// bodyTooLargeError has the message of ErrRequestBodyTooLarge only, because
// the message of http.MaxBytesError says the same.
type bodyTooLargeError struct {
	maxBytes *http.MaxBytesError
}

// Error returns the message of ErrRequestBodyTooLarge.
func (e *bodyTooLargeError) Error() string {
	return ErrRequestBodyTooLarge.Error()
}

// Unwrap returns ErrRequestBodyTooLarge and the http.MaxBytesError.
func (e *bodyTooLargeError) Unwrap() []error {
	return []error{
		ErrRequestBodyTooLarge,
		e.maxBytes,
	}
}
