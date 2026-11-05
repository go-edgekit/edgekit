package edgekit

import (
	"maps"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// identityHeaders lists canonical names; every name that starts with
// "X-Forwarded-" is also one.
var identityHeaders = map[string]struct{}{
	"Cf-Connecting-Ip":         {},
	"Fastly-Client-Ip":         {},
	"Forwarded":                {},
	"True-Client-Ip":           {},
	"X-Client-Ip":              {},
	"X-Cluster-Client-Ip":      {},
	"X-Original-Forwarded-For": {},
	"X-Original-Url":           {},
	"X-Real-Ip":                {},
	"X-Rewrite-Url":            {},
}

// reservedHeaders are the hop-by-hop headers (RFC 9110, section 7.6.1) and the
// framing and Host headers, which only the HTTP implementation can set.
var reservedHeaders = map[string]struct{}{
	"Connection":          {},
	"Content-Length":      {},
	"Host":                {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// checkRequest returns the status and the reason for a request that the
// secure defaults reject, or 0 if the request can continue.
func checkRequest(r *http.Request) (int, RejectionReason) {
	// The cases run in order and stop at the first match, so the path check
	// only gets an origin-form URL.
	switch {
	case r.Method == http.MethodConnect:
		return http.StatusNotImplemented, ReasonInvalidMethod
	case !isOriginForm(r):
		return http.StatusBadRequest, ReasonInvalidRequestTarget
	case !pathAllowed(r.URL.EscapedPath()):
		return http.StatusBadRequest, ReasonInvalidPath
	case !queryAllowed(r.URL.RawQuery):
		return http.StatusBadRequest, ReasonInvalidQuery
	case !headerNamesAllowed(r.Header), !headerNamesAllowed(r.Trailer):
		return http.StatusBadRequest, ReasonInvalidHeaderName
	case !upgradeAllowed(r.Header):
		return http.StatusBadRequest, ReasonInvalidUpgrade
	}

	return 0, ""
}

// isOriginForm examines URL, because RequestURI is empty for a request that did
// not come from a server.
func isOriginForm(r *http.Request) bool {
	// The absolute, authority, and asterisk forms (RFC 9112, section 3.2) and
	// opaque URLs set one of these fields or give a path without "/".
	u := r.URL
	if u.Opaque != "" || u.Scheme != "" || u.Host != "" || u.User != nil {
		return false
	}

	return strings.HasPrefix(u.Path, "/")
}

// pathAllowed is the path policy in reject mode, and escaped must be an escaped
// path.
func pathAllowed(escaped string) bool {
	// An upstream can decode these characters differently from the router, so
	// a request could get to a path outside its route.
	for i := 0; i < len(escaped); i++ {
		switch escaped[i] {
		case '\\', '#':
			return false
		case '%':
			if i+2 >= len(escaped) || !isHex(escaped[i+1]) || !isHex(escaped[i+2]) {
				return false
			}

			switch unhex(escaped[i+1])<<4 | unhex(escaped[i+2]) {
			case '.', '/', '\\', 0, '#':
				return false
			}

			i += 2
		}
	}

	for segment := range strings.SplitSeq(escaped, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}

	return true
}

// queryAllowed uses the parser of [http.Request.URL.Query], so middleware sees
// the same parameters as the upstream, which gets the raw query.
func queryAllowed(raw string) bool {
	_, err := url.ParseQuery(raw)

	return err == nil
}

// upgradeAllowed uses the predicate of ReverseProxy, which fails a request
// with a Connection token "upgrade" and an Upgrade value that is not
// printable ASCII, and reports that failure without a source.
func upgradeAllowed(header http.Header) bool {
	// ReverseProxy reads only the canonical names, and only the first
	// Upgrade value.
	upgrade := false
	for _, value := range header["Connection"] {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.Trim(token, " \t"), "upgrade") {
				upgrade = true
			}
		}
	}

	if !upgrade {
		return true
	}

	value := header.Get("Upgrade")
	for i := range len(value) {
		if value[i] < ' ' || value[i] > '~' {
			return false
		}
	}

	return true
}

// isHex reports whether c is a hexadecimal digit.
func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// unhex needs a hexadecimal digit, which isHex checks.
func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10
	}

	return 0
}

// headerNamesAllowed accepts a nil header, and rejects other token characters, such as "_", which make
// aliases like "X_User" for "X-User".
func headerNamesAllowed(header http.Header) bool {
	for name := range header {
		if !headerNameAllowed(name) {
			return false
		}
	}

	return true
}

// headerNameAllowed rejects an empty name.
func headerNameAllowed(name string) bool {
	if name == "" {
		return false
	}

	for i := range len(name) {
		if !isHeaderNameChar(name[i]) {
			return false
		}
	}

	return true
}

// isHeaderNameChar reports whether c is an ASCII letter, a digit, or "-".
func isHeaderNameChar(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-'
}

// removedHeader needs a canonical name.
func (g *Gateway) removedHeader(canonical string) bool {
	if strings.HasPrefix(canonical, "X-Forwarded-") {
		return true
	}

	if _, ok := identityHeaders[canonical]; ok {
		return true
	}

	_, ok := g.managedHeaders[canonical]

	return ok
}

// stripHeaders needs a shallow copy of the caller's request. It never changes
// the maps of the copy, which the caller's request shares, and gives the copy
// new header and trailer maps when it removes headers.
func (g *Gateway) stripHeaders(r *http.Request) {
	if !g.needsStrip(r.Header) && !g.trailersNeedStrip(r.Trailer) {
		return
	}

	header := r.Header.Clone()
	for name, values := range header {
		switch canonical := textproto.CanonicalMIMEHeaderKey(name); {
		case g.removedHeader(canonical):
			delete(header, name)
		case canonical == "Connection":
			// ReverseProxy removes the headers that Connection names. Without
			// this step, a client could remove a managed header that
			// middleware sets.
			header[name], _ = g.connectionValues(values)
			if len(header[name]) == 0 {
				delete(header, name)
			}
		}
	}

	r.Header = header
	r.Trailer = g.stripTrailers(r.Trailer)
}

// stripTrailers exists because ReverseProxy forwards the declared trailer
// names, so a client could send a managed or identity header as a trailer.
func (g *Gateway) stripTrailers(trailer http.Header) http.Header {
	if !g.trailersNeedStrip(trailer) {
		return trailer
	}

	kept := trailer.Clone()
	maps.DeleteFunc(kept, func(name string, _ []string) bool {
		return g.removedHeader(textproto.CanonicalMIMEHeaderKey(name))
	})

	return kept
}

// trailersNeedStrip lets stripHeaders skip the copy when no trailer name is
// removed.
func (g *Gateway) trailersNeedStrip(trailer http.Header) bool {
	for name := range trailer {
		if g.removedHeader(textproto.CanonicalMIMEHeaderKey(name)) {
			return true
		}
	}

	return false
}

// needsStrip lets stripHeaders skip the copy when it removes nothing.
func (g *Gateway) needsStrip(header http.Header) bool {
	for name, values := range header {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if g.removedHeader(canonical) {
			return true
		}

		if canonical == "Connection" {
			if _, changed := g.connectionValues(values); changed {
				return true
			}
		}
	}

	return false
}

// connectionValues returns values unchanged when it removes no token.
func (g *Gateway) connectionValues(values []string) ([]string, bool) {
	changed := false
	for token := range listTokens(values) {
		if g.removedHeader(textproto.CanonicalMIMEHeaderKey(token)) {
			changed = true

			break
		}
	}

	if !changed {
		return values, false
	}

	kept := make([]string, 0, len(values))
	for _, value := range values {
		var tokens []string
		for token := range strings.SplitSeq(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" && !g.removedHeader(textproto.CanonicalMIMEHeaderKey(token)) {
				tokens = append(tokens, token)
			}
		}

		if len(tokens) != 0 {
			kept = append(kept, strings.Join(tokens, ", "))
		}
	}

	return kept, true
}
