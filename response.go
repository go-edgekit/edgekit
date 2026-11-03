package edgekit

import (
	"net/http"
	"net/textproto"
	"slices"
	"strings"
)

// altSvc advertises upstream alternative services that are not valid for the
// gateway's origin (RFC 7838).
const altSvc = "Alt-Svc"

// modifyResponse runs after ReverseProxy removed the hop-by-hop response
// headers.
func modifyResponse(response *http.Response) error {
	response.Header.Del(altSvc)

	// ReverseProxy announces the names in Trailer before the body. Values that
	// arrive later for these names are removed by removeTrailers.
	for name := range response.Trailer {
		if responseFieldRemoved(textproto.CanonicalMIMEHeaderKey(name)) {
			delete(response.Trailer, name)
		}
	}

	return nil
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

		for _, value := range header["Connection"] {
			for token := range strings.SplitSeq(value, ",") {
				header.Del(strings.TrimSpace(token))
			}
		}

		for name := range header {
			if responseFieldRemoved(textproto.CanonicalMIMEHeaderKey(name)) {
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
