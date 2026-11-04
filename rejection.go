package edgekit

import (
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
)

// Rejection is a deliberate refusal of a request, such as a denial by a rate
// limit. It is a decision, not a failure. A processing hook
// ([RequestReceivedHook], [BeforeUpstreamHook], or [UpstreamResponseHook])
// returns it as its first result:
//
//	Rejection  error    Result
//	nil        nil      Handling continues.
//	non-nil    nil      The gateway checks the rejection, then rejects.
//	nil        non-nil  A failure.
//	non-nil    non-nil  A failure. The gateway ignores the rejection and
//	                    does not check it.
//
// For a valid rejection, the gateway stops the hooks that come after the
// rejecting hook in the run order of its phase, and sends a response with
// Status, the headers in Header, and a body from the renderer (see
// [Config.Renderer]). Completion gets Reason as [Completion.RejectionReason],
// and no [Failure].
//
// A Rejection is valid when all of these are true:
//   - Status is from 400 to 599.
//   - Reason has the form "<extension>.<check>". Each part starts with a
//     lowercase ASCII letter and contains only lowercase ASCII letters,
//     digits, and "_". The complete code has at most 64 bytes.
//   - Each header name contains only ASCII letters, digits, and "-", and no
//     header value contains a control character other than horizontal tab.
//
// For a rejection that is not valid, the gateway also stops the later hooks,
// sends 500 Internal Server Error, and completion gets a [Failure] in the
// phase of the hook whose error wraps [ErrInvalidRejection]. Neither the error
// nor a diagnostic contains a value of the rejection.
//
// The gateway removes Connection and the fields that it names, the other
// hop-by-hop fields, Alt-Svc, Content-Length, Content-Type,
// Transfer-Encoding, and Trailer from the headers that it sends. Each other
// field in Header replaces the value that middleware set for the same name.
//
// Reason is a static public name for the check that refused the request. The
// client gets it in the response body. It must never be an identifier, such
// as a key ID, or a value from the request.
//
// The gateway reads a Rejection when the hook returns, copies the header map,
// and never changes the Rejection or its Header. A shared Rejection, such as
// a package variable, is safe for concurrent use only while nobody changes
// its fields or its Header.
type Rejection struct {
	// Status is the response status.
	Status int

	// Reason is the public code of the check that refused the request.
	Reason RejectionReason

	// Header holds response headers, such as Retry-After or
	// WWW-Authenticate. It can be nil.
	Header http.Header
}

// ErrInvalidRejection is the error that a [Failure] wraps when a hook
// returned a [Rejection] that is not valid. The error names the rule that the
// rejection broke, but contains no value of it. Use [errors.Is] to identify
// it.
var ErrInvalidRejection = errors.New("invalid rejection")

// The rules that an invalid rejection can break.
var (
	errRejectionStatus = fmt.Errorf("%w: status is not from 400 to 599", ErrInvalidRejection)
	errRejectionReason = fmt.Errorf("%w: reason is not of the form <extension>.<check>", ErrInvalidRejection)
	errRejectionHeader = fmt.Errorf("%w: header name or value is not valid", ErrInvalidRejection)
)

// errRejected goes to ReverseProxy for a valid rejection, so that it calls the
// error handler, which renders the recorded rejection.
var errRejected = errors.New("request rejected by a hook")

// maxReasonLength limits a reason code from an extension.
const maxReasonLength = 64

// validatedHeaders checks the complete rejection and returns a sanitized
// copy of its headers. It never changes r.
func (r *Rejection) validatedHeaders() (http.Header, error) {
	if r.Status < 400 || r.Status > 599 {
		return nil, errRejectionStatus
	}

	if !extensionReason(r.Reason) {
		return nil, errRejectionReason
	}

	nominated, _ := connectionTokens(r.Header)

	var header http.Header
	for name, values := range r.Header {
		if !headerNameAllowed(name) || slices.ContainsFunc(values, invalidFieldValue) {
			return nil, errRejectionHeader
		}

		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if rejectionFieldRemoved(canonical) || nominated[canonical] {
			continue
		}

		if header == nil {
			header = make(http.Header, len(r.Header))
		}

		header[canonical] = append(header[canonical], values...)
	}

	return header, nil
}

// rejectionFieldRemoved needs a canonical name. The renderer owns
// Content-Type, and the server owns the framing.
func rejectionFieldRemoved(canonical string) bool {
	return responseFieldRemoved(canonical) || canonical == "Content-Type"
}

// extensionReason reports whether reason has the form "<extension>.<check>".
// Core codes never contain ".", so an extension cannot use one.
func extensionReason(reason RejectionReason) bool {
	if len(reason) > maxReasonLength {
		return false
	}

	extension, check, ok := strings.Cut(string(reason), ".")

	return ok && reasonPart(extension) && reasonPart(check)
}

// reasonPart reports whether part starts with a lowercase ASCII letter and
// contains only lowercase ASCII letters, digits, and "_".
func reasonPart(part string) bool {
	if part == "" || part[0] < 'a' || part[0] > 'z' {
		return false
	}

	for i := range len(part) {
		switch c := part[i]; {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '_':
		default:
			return false
		}
	}

	return true
}
