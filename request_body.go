package edgekit

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"sync/atomic"
)

// ErrInvalidTrailerName is the error that a reader of a request body gets at
// the end of the body, instead of [io.EOF], when a trailer that arrived with
// the body has a name with characters other than ASCII letters, digits, and
// "-", and [Config.HeaderNamePolicy] is [HeaderNameReject]. The error does
// not contain the name. When the transport fails with it, the gateway sends
// 400 Bad Request if it can still send a response (see [Gateway]). Use
// [errors.Is] to identify it.
var ErrInvalidTrailerName = errors.New("request trailer name is invalid")

// errLateTrailerName wraps ErrInvalidTrailerName for a name that arrived at
// the end of the body.
var errLateTrailerName = fmt.Errorf("check trailers at the end of the body: %w", ErrInvalidTrailerName)

// requestBody is the body of each request that has one. It applies the body
// limit, and at the end of the body it checks the trailers and publishes
// them (ADR-0027). A transport can read it on another goroutine, also after
// the handler returned, so it never changes the gateway context.
//
// Concurrency: limit, bytesRead, overflowed, and trailerRejected are atomic,
// because the handler reads them while a transport can read the body. The
// other fields belong to the goroutine that reads the body: the handler sets
// carried and outbound before the transport starts, and the reader of the
// body uses them and sets ended and endErr after that, as net/http requires
// for trailers. The optional-body methods setLimit, recordCarried,
// attachOutbound, overflowFound, and trailerRejectionFound accept a nil
// receiver. Read and Close require a body.
type requestBody struct {
	io.ReadCloser

	limit      atomic.Int64
	bytesRead  atomic.Int64
	overflowed atomic.Bool

	// trailerRejected is the evidence for the 400 rule: the check at the end
	// of the body found a name that the reject mode refuses.
	trailerRejected atomic.Bool

	gateway *Gateway

	// declared are the trailer names that the request declared and that the
	// secure defaults kept. A declared name that middleware deleted from
	// the outbound request stays deleted.
	declared map[string]struct{}

	// checked keeps the allowed values after EOF, also when middleware
	// copied its request before it read the body.
	checked http.Header

	// source is the request whose Trailer map net/http fills at the end of
	// the body. inner and outbound get the checked trailers. Only the reader
	// of the body uses them after the transport started, as net/http
	// requires for trailers.
	source   *http.Request
	inner    http.Header
	outbound http.Header

	// carried is true when the request that the proxy got still has this
	// body and a trailer map, so the outbound request gets the trailers.
	carried bool

	// ended is true after the end of the body was handled; endErr is then
	// the result for later reads.
	ended  bool
	endErr error
}

// newRequestBody wraps the body of inner, a shallow copy of source, with the
// limit, which can be noBodyLimit. inner must have its own trailer map.
func (g *Gateway) newRequestBody(source, inner *http.Request, limit int64) *requestBody {
	b := &requestBody{
		ReadCloser: inner.Body,
		gateway:    g,
		declared:   canonicalTrailerNames(inner.Trailer),
		source:     source,
		inner:      inner.Trailer,
	}
	b.limit.Store(limit)

	return b
}

// Read applies the limit and, at a successful end of the body, the trailer
// check. A read that fails, also for the limit, publishes no trailers.
func (b *requestBody) Read(p []byte) (int, error) {
	n, eof, err := b.readLimited(p)
	if eof {
		if endErr := b.end(); endErr != nil {
			return n, endErr
		}
	}

	return n, err
}

// readLimited reads at most one byte more than the remaining limit, so it
// finds an overflow at the exact limit without reading ahead. An overflow is
// final, also when the limit is disabled after it. eof is true only for
// io.EOF itself: the transport copies the body until that value, so a
// wrapped io.EOF is a failure and publishes no trailers.
func (b *requestBody) readLimited(p []byte) (n int, eof bool, err error) {
	limit := b.limit.Load()
	if b.overflowed.Load() {
		return 0, false, tooLarge(limit)
	}

	if limit == noBodyLimit {
		n, err = b.ReadCloser.Read(p)
		b.bytesRead.Add(int64(n))

		return n, err == io.EOF, err
	}

	// A limit below the bytes that were read is an overflow that setLimit
	// did not see, because another goroutine read the body while it ran.
	// This check prevents a slice index below zero.
	remaining := limit - b.bytesRead.Load()
	if remaining < 0 {
		b.overflowed.Store(true)

		return 0, false, tooLarge(limit)
	}

	// The comparison without remaining+1 cannot overflow for a limit of
	// math.MaxInt64.
	if int64(len(p)) > remaining {
		p = p[:remaining+1]
	}

	n, err = b.ReadCloser.Read(p)
	if int64(n) > remaining {
		b.bytesRead.Add(remaining)
		b.overflowed.Store(true)

		return int(remaining), false, tooLarge(limit)
	}

	b.bytesRead.Add(int64(n))

	return n, err == io.EOF, err
}

// setLimit sets the route limit and reports false when the body already
// went over it. Then the overflow is final, and an earlier overflow keeps
// the limit that it went over.
func (b *requestBody) setLimit(limit int64) bool {
	if b == nil {
		return true
	}

	if b.overflowed.Load() {
		return false
	}

	if limit != noBodyLimit && b.bytesRead.Load() > limit {
		b.overflowed.Store(true)
		b.limit.Store(limit)

		return false
	}

	b.limit.Store(limit)

	return true
}

// recordCarried records whether in, the request that the route middleware
// passes to the proxy, still has this body and a trailer map. Middleware owns
// the outbound request: when it replaced the body or removed the trailer
// map, the outbound request gets no client trailers. The proxy wraps the body
// of the outbound request, so only in shows whether the body is this one.
func (b *requestBody) recordCarried(in *http.Request) {
	if b == nil {
		return
	}

	b.carried = in.Body == b && in.Trailer != nil
}

// attachOutbound registers the outbound trailer map before the transport
// reads the body. After EOF, it publishes the checked values to a request
// that middleware copied before it read the body.
func (b *requestBody) attachOutbound(out *http.Request) {
	if b == nil || !b.carried || out.Trailer == nil {
		return
	}

	if b.ended {
		publishTrailers(out.Trailer, b.checked, b.declared)

		return
	}

	b.outbound = out.Trailer
}

// end runs once, at the first successful end of the body. It reads the
// trailers that net/http published to source on this goroutine.
func (b *requestBody) end() error {
	if b.ended {
		return b.endErr
	}

	b.ended = true
	checked, err := b.gateway.checkLateTrailers(b.source.Trailer, b.source.Header)
	if err != nil {
		b.trailerRejected.Store(true)
		b.endErr = err

		return err
	}

	b.checked = checked
	publishTrailers(b.inner, checked, b.declared)
	if b.outbound != nil {
		publishTrailers(b.outbound, checked, b.declared)
	}

	return nil
}

// publishTrailers copies values without replacing middleware values or
// restoring declared names that middleware deleted. Names are compared in
// all letter cases, with one index to avoid repeated scans of client maps.
func publishTrailers(dst, checked http.Header, declared map[string]struct{}) {
	fields := indexTrailerFields(dst)
	for name, values := range checked {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		field, present := fields[canonical]
		if field.hasValue {
			continue
		}

		if _, wasDeclared := declared[canonical]; !present && wasDeclared {
			continue
		}

		key := name
		if present {
			key = field.key
		}

		dst[key] = slices.Clone(values)
		fields[canonical] = trailerField{
			key:      key,
			hasValue: len(values) != 0,
		}
	}
}

// trailerField combines the entries for one canonical trailer name.
type trailerField struct {
	// key is the smallest key of the entries in all letter cases, so the
	// choice does not depend on the order of the map.
	key string

	// hasValue is true when an entry has a value.
	hasValue bool
}

// indexTrailerFields permits a lookup in all letter cases without a scan
// of the trailer map.
func indexTrailerFields(header http.Header) map[string]trailerField {
	fields := make(map[string]trailerField, len(header))
	for name, values := range header {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		f, ok := fields[canonical]
		if !ok || name < f.key {
			f.key = name
		}

		f.hasValue = f.hasValue || len(values) != 0
		fields[canonical] = f
	}

	return fields
}

// canonicalTrailerNames makes a set for comparisons in all letter cases.
func canonicalTrailerNames(header http.Header) map[string]struct{} {
	if len(header) == 0 {
		return nil
	}

	names := make(map[string]struct{}, len(header))
	for name := range header {
		names[textproto.CanonicalMIMEHeaderKey(name)] = struct{}{}
	}

	return names
}

// checkLateTrailers applies the trailer rules to the trailers that arrived
// with the body, also to names that the request did not declare. It returns
// a new map that shares the value slices with trailer; publishTrailers copies
// them. header is the original inbound header map, before the proxy removes
// Connection and its options.
func (g *Gateway) checkLateTrailers(trailer, header http.Header) (http.Header, error) {
	nominated, _ := connectionTokens(header)
	checked := make(http.Header, len(trailer))
	for name, values := range trailer {
		if g.headerNamePolicy.refuses(name) {
			return nil, errLateTrailerName
		}

		if g.removedTrailer(name) || nominated[textproto.CanonicalMIMEHeaderKey(name)] {
			continue
		}

		checked[name] = values
	}

	return checked, nil
}

// removedTrailer adds the fields that a trailer must not contain (RFC 9110,
// section 6.5.1) to the names that the gateway removes from requests. name
// can be in any letter case.
func (g *Gateway) removedTrailer(name string) bool {
	if g.removedName(name) {
		return true
	}

	canonical := textproto.CanonicalMIMEHeaderKey(name)
	if _, ok := forbiddenTrailers[canonical]; ok {
		return true
	}

	return strings.HasPrefix(canonical, "If-") || responseFieldRemoved(canonical)
}

// forbiddenTrailers are the fields of RFC 9110, section 6.5.1, that a
// recipient must evaluate before the content: framing, routing,
// authentication, request modifiers, response controls, and content format.
// The list is the list that net/http refuses in HTTP/2 trailers, with the
// cookies; the conditional fields ("If-") and the hop-by-hop fields are
// checked apart from it. An upstream that merges trailers into the header
// section must not get these values from a client after the middleware ran.
var forbiddenTrailers = map[string]struct{}{
	"Authorization":       {},
	"Cache-Control":       {},
	"Content-Encoding":    {},
	"Content-Length":      {},
	"Content-Range":       {},
	"Content-Type":        {},
	"Cookie":              {},
	"Expect":              {},
	"Host":                {},
	"Max-Forwards":        {},
	"Pragma":              {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Range":               {},
	"Realm":               {},
	"Set-Cookie":          {},
	"Www-Authenticate":    {},
}

// overflowFound reports whether the body limit found an overflow. It is the
// evidence that the 413 rule needs together with the error.
func (b *requestBody) overflowFound() bool {
	return b != nil && b.overflowed.Load()
}

// trailerRejectionFound reports whether the trailer check at the end of the
// body rejected a name. It is the evidence that the 400 rule needs together
// with the error.
func (b *requestBody) trailerRejectionFound() bool {
	return b != nil && b.trailerRejected.Load()
}
