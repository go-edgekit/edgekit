package edgekit

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// fail records a failure with the status that the evidence gives
// (ADR-0023).
func (rc *RequestContext) fail(phase Phase, err error) {
	rc.record(phase, err, rc.failureStatus(phase, err))
}

// failDefect records a broken contract of the embedder, an extension, or the
// transport. It is always 500, because the evidence of err does not apply.
func (rc *RequestContext) failDefect(phase Phase, err error) {
	rc.record(phase, err, http.StatusInternalServerError)
}

// record keeps the first outcome, because later failures are consequences of
// it.
func (rc *RequestContext) record(phase Phase, err error, status int) {
	if rc.recorded() {
		return
	}

	rc.failure = &Failure{
		Phase: phase,
		Err:   err,
	}
	rc.status = status
}

// reject keeps the first outcome. header must be sanitized and owned by rc.
func (rc *RequestContext) reject(status int, reason RejectionReason, header http.Header) {
	if rc.recorded() {
		return
	}

	rc.rejection = reason
	rc.status = status
	rc.header = header
}

// recorded reports whether a rejection or a failure is recorded. A rejection
// always has a reason.
func (rc *RequestContext) recorded() bool {
	return rc.failure != nil || rc.rejection != ""
}

// hookResult applies the result table of [Rejection] and returns nil when
// handling continues. Otherwise it returns the error for ReverseProxy.
func (rc *RequestContext) hookResult(phase Phase, rejection *Rejection, err error) error {
	switch {
	case err != nil:
		// The rejection is not checked, so its problems cannot replace the
		// original failure.
		rc.fail(phase, err)

		return err
	case rejection != nil:
		header, invalid := rejection.validatedHeaders()
		if invalid != nil {
			rc.failDefect(phase, invalid)

			return invalid
		}

		rc.reject(rejection.Status, rejection.Reason, header)

		return errRejected
	}

	return nil
}

// statusClientClosedRequest is the non-standard status for a client that is
// gone, as nginx uses it.
const statusClientClosedRequest = 499

// failureStatus applies the precedence of ADR-0023 to an error from a
// processing stage. It never reads the error text.
func (rc *RequestContext) failureStatus(phase Phase, err error) int {
	// A canceled entry context alone is no evidence: the error must report
	// the cancellation too.
	if errors.Is(err, context.Canceled) && rc.entry != nil && errors.Is(rc.entry.Err(), context.Canceled) {
		return statusClientClosedRequest
	}

	// The sentinel alone is no evidence: the body limit of this request must
	// have found the overflow (ADR-0025).
	if errors.Is(err, ErrRequestBodyTooLarge) && rc.body.overflowFound() {
		return http.StatusRequestEntityTooLarge
	}

	// The same evidence rule applies to a trailer name that the check at the
	// end of the body rejected (ADR-0027).
	if errors.Is(err, ErrInvalidTrailerName) && rc.body.trailerRejectionFound() {
		return http.StatusBadRequest
	}

	switch phase {
	case PhaseRoundTrip:
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return http.StatusGatewayTimeout
		}

		return http.StatusBadGateway
	case PhaseRequestBuffering:
		return bufferedFailureStatus(err)
	case PhaseUpstreamSelection:
		if errors.Is(err, ErrNoUpstreamAvailable) {
			return http.StatusServiceUnavailable
		}

		return http.StatusInternalServerError
	case PhaseUpstreamResponse:
		if errors.Is(err, ErrInvalidUpstreamResponse) {
			return http.StatusBadGateway
		}

		return http.StatusInternalServerError
	case PhaseRouteMatched, PhaseBeforeUpstream:
		return http.StatusInternalServerError
	case PhaseUnknown, PhaseProtocolSwitch, PhaseResponseBody:
		return http.StatusBadGateway
	}

	return http.StatusInternalServerError
}
