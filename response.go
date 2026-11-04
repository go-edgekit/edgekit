package edgekit

import (
	"errors"
	"iter"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
)

// altSvc advertises upstream alternative services that are not valid for the
// gateway's origin (RFC 7838).
const altSvc = "Alt-Svc"

// finalResponseFields are removed from a final response after the
// UpstreamResponse hooks (ADR-0019).
var finalResponseFields = map[string]struct{}{
	altSvc:                {},
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// upgradeResponseFields are removed from a 101 response after the
// UpstreamResponse hooks (ADR-0019). A 1xx response must not have
// Content-Length or Transfer-Encoding (RFC 9110, section 8.6; RFC 9112, section
// 6.1). Connection and Upgrade are then restored to the values of the
// upstream.
var upgradeResponseFields = map[string]struct{}{
	altSvc:                {},
	"Connection":          {},
	"Content-Length":      {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// upgradeProtocol is the protocol switch that the upstream sent in a 101
// response, before the UpstreamResponse hooks.
type upgradeProtocol struct {
	// upgrade holds the Upgrade values under the canonical name, which is
	// the only name that ReverseProxy validates.
	upgrade []string

	// token is true when Connection, in any letter case, contained the
	// Upgrade token.
	token bool
}

// errStatusChanged is the failure when an UpstreamResponse hook changes the
// status across the 101 boundary or to a status that is not final.
var errStatusChanged = errors.New("hook changed the response status across the 101 boundary or to a status that is not 200 to 599")

// modifyResponse returns a hook error, so ReverseProxy closes the body and
// calls the error handler.
func (g *Gateway) modifyResponse(resp *http.Response) error {
	// outboundTransport set resp.Request to the outbound request.
	ctx := resp.Request.Context()
	rc, _ := FromContext(ctx)
	upgrade := resp.StatusCode == http.StatusSwitchingProtocols

	var protocol upgradeProtocol
	if upgrade {
		_, token := connectionTokens(resp.Header)
		protocol = upgradeProtocol{
			upgrade: slices.Clone(resp.Header["Upgrade"]),
			token:   token,
		}
	}

	for _, hook := range g.hooks.responses {
		if err := hook.UpstreamResponse(ctx, rc, resp); err != nil {
			rc.fail(PhaseUpstreamResponse, err)

			return err
		}
	}

	// ReverseProxy chose the upgrade path before ModifyResponse, so a hook
	// cannot move the response across the 101 boundary.
	if !statusAllowed(upgrade, resp.StatusCode) {
		rc.fail(PhaseUpstreamResponse, errStatusChanged)

		return errStatusChanged
	}

	if upgrade {
		sanitizeUpgrade(resp.Header, protocol)

		return nil
	}

	sanitizeResponse(resp)

	return nil
}

// statusAllowed reports whether status is valid after the hooks: 101 for an
// upgrade, and a final status otherwise.
func statusAllowed(upgrade bool, status int) bool {
	if upgrade {
		return status == http.StatusSwitchingProtocols
	}

	return status >= 200 && status <= 599
}

// sanitizeResponse runs after the hooks, so a hook cannot add back a field
// that ReverseProxy already removed.
func sanitizeResponse(resp *http.Response) {
	header := resp.Header
	nominated, _ := connectionTokens(header)
	for name := range header {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if _, ok := finalResponseFields[canonical]; ok || nominated[canonical] {
			delete(header, name)
		}
	}

	sanitizeContentLength(header)

	// ReverseProxy announces the names in Trailer before the body. Values that
	// arrive later for these names are removed by removeTrailers.
	for name := range resp.Trailer {
		if responseFieldRemoved(textproto.CanonicalMIMEHeaderKey(name)) {
			delete(resp.Trailer, name)
		}
	}
}

// sanitizeContentLength keeps one value that the server accepts, under the
// canonical name. ReverseProxy adds the entries of all letter cases under the
// canonical name, and the server logs an invalid value.
func sanitizeContentLength(header http.Header) {
	var (
		names  []string
		values []string
	)

	for name, entries := range header {
		if textproto.CanonicalMIMEHeaderKey(name) == "Content-Length" {
			names = append(names, name)
			values = append(values, entries...)
		}
	}

	if len(names) == 0 {
		return
	}

	for _, name := range names {
		delete(header, name)
	}

	// The server accepts a value that strconv.ParseInt reads as a
	// non-negative int64. Digits only also excludes a sign.
	if len(values) == 1 && isDecimal(values[0]) {
		if _, err := strconv.ParseInt(values[0], 10, 64); err == nil {
			header["Content-Length"] = values
		}
	}
}

// sanitizeUpgrade restores the protocol switch of the upstream, so the upgrade
// validation of ReverseProxy checks what the upstream sent, not a hook change.
func sanitizeUpgrade(header http.Header, protocol upgradeProtocol) {
	nominated, _ := connectionTokens(header)
	delete(nominated, "Upgrade")

	for name := range header {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if _, ok := upgradeResponseFields[canonical]; ok || nominated[canonical] {
			delete(header, name)
		}
	}

	if protocol.upgrade != nil {
		header["Upgrade"] = protocol.upgrade
	}

	if protocol.token {
		header["Connection"] = []string{"Upgrade"}
	}
}

// connectionTokens collects the canonical field names that the Connection
// entries of all letter cases name, before any field is removed, so the result
// does not depend on the iteration order of the map. It also reports whether
// a token was Upgrade. The map is nil when no entry names a field.
func connectionTokens(header http.Header) (map[string]bool, bool) {
	var nominated map[string]bool
	for name, values := range header {
		if textproto.CanonicalMIMEHeaderKey(name) != "Connection" {
			continue
		}

		for token := range listTokens(values) {
			if nominated == nil {
				nominated = make(map[string]bool)
			}

			nominated[textproto.CanonicalMIMEHeaderKey(token)] = true
		}
	}

	return nominated, nominated["Upgrade"]
}

// listTokens yields the non-empty elements of comma-separated field values,
// without the whitespace around them.
func listTokens(values []string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for _, value := range values {
			for token := range strings.SplitSeq(value, ",") {
				if token = strings.TrimSpace(token); token != "" && !yield(token) {
					return
				}
			}
		}
	}
}

// isDecimal reports whether s is a non-empty string of ASCII digits.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}

	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}

// responseFieldRemoved needs a canonical name.
func responseFieldRemoved(canonical string) bool {
	if canonical == altSvc {
		return true
	}

	_, ok := reservedHeaders[canonical]

	return ok
}

// removeTrailers must run after ReverseProxy copied the trailers, with
// http.TrailerPrefix, into header.
func removeTrailers(header http.Header) {
	for name := range header {
		trailer, ok := strings.CutPrefix(name, http.TrailerPrefix)
		if ok && responseFieldRemoved(textproto.CanonicalMIMEHeaderKey(trailer)) {
			delete(header, name)
		}
	}
}

// informationalWriter exists because ReverseProxy forwards 1xx headers without
// removing hop-by-hop fields (golang/go#81319), and up to Go 1.26 sends and
// then clears the response headers that middleware set before the upstream
// responded (golang/go#81207).
type informationalWriter struct {
	http.ResponseWriter

	// before holds the response headers that middleware set before the proxy
	// ran. It is nil when there were none.
	before http.Header

	// cleared is true after a 1xx response, when ReverseProxy cleared the
	// header map and before is not restored yet.
	cleared bool
}

// newInformationalWriter must be called before the proxy runs, so it can save
// the headers that middleware set.
func newInformationalWriter(w http.ResponseWriter) *informationalWriter {
	iw := &informationalWriter{ResponseWriter: w}
	if header := w.Header(); len(header) != 0 {
		iw.before = header.Clone()
	}

	return iw
}

// Header restores the middleware headers that ReverseProxy cleared after a 1xx
// response.
func (w *informationalWriter) Header() http.Header {
	header := w.ResponseWriter.Header()
	if w.cleared {
		w.cleared = false
		for name, values := range w.before {
			header[name] = slices.Clone(values)
		}
	}

	return header
}

// WriteHeader changes the shared header map, which ReverseProxy clears after
// each 1xx status.
func (w *informationalWriter) WriteHeader(status int) {
	header := w.Header()

	// An upgrade needs the Connection and Upgrade headers of its 101 response.
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		// ReverseProxy appends the 1xx values to the values that middleware
		// set, so the 1xx values are the ones after them.
		for name, values := range w.before {
			if hints := header[name]; len(hints) > len(values) {
				header[name] = hints[len(values):]
			} else {
				delete(header, name)
			}
		}

		nominated, _ := connectionTokens(header)
		for name := range header {
			canonical := textproto.CanonicalMIMEHeaderKey(name)
			if responseFieldRemoved(canonical) || nominated[canonical] {
				delete(header, name)
			}
		}

		w.cleared = w.before != nil
	}

	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets ReverseProxy flush and hijack through http.ResponseController.
func (w *informationalWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
