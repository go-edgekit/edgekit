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

// QueryPolicy sets how the gateway checks the query of a request before any
// middleware runs (ADR-0026).
type QueryPolicy int

// The query policies.
const (
	// QueryReject is the secure default: the gateway rejects a query that
	// [net/url.ParseQuery] rejects, with 400 Bad Request and
	// [ReasonInvalidQuery].
	QueryReject QueryPolicy = iota

	// QueryUnchecked accepts each query and forwards the raw query unchanged.
	// It is a relaxation: middleware that reads [net/url.URL.Query] can then
	// see other parameters than the upstream, which can split the query at
	// ";".
	QueryUnchecked
)

// valid reports whether p is a defined query policy.
func (p QueryPolicy) valid() bool {
	return p == QueryReject || p == QueryUnchecked
}

// HeaderNamePolicy sets what the gateway does with a request header or
// trailer name that has characters other than ASCII letters, digits, and "-"
// (ADR-0026).
type HeaderNamePolicy int

// The header-name policies.
const (
	// HeaderNameReject is the secure default: the gateway rejects the request
	// with 400 Bad Request and [ReasonInvalidHeaderName].
	HeaderNameReject HeaderNamePolicy = iota

	// HeaderNameDrop removes each such header and trailer name before any
	// middleware runs, in the same step as the identity headers, and
	// continues. It is a relaxation: the headers do not reach middleware,
	// hooks, or the upstream. The gateway never forwards such a name,
	// because some upstreams read "X_User" as "X-User".
	HeaderNameDrop
)

// valid reports whether p is a defined header-name policy.
func (p HeaderNamePolicy) valid() bool {
	return p == HeaderNameReject || p == HeaderNameDrop
}

// refuses reports whether p rejects the request for a field with name.
func (p HeaderNamePolicy) refuses(name string) bool {
	return p == HeaderNameReject && !headerNameAllowed(name)
}

// refusesAny reports whether p rejects the request for a name in header,
// which can be nil.
func (p HeaderNamePolicy) refusesAny(header http.Header) bool {
	for name := range header {
		if p.refuses(name) {
			return true
		}
	}

	return false
}

// drops reports whether p removes a field with name.
func (p HeaderNamePolicy) drops(name string) bool {
	return p == HeaderNameDrop && !headerNameAllowed(name)
}

// checkRequest returns the status and the reason for a request that the
// secure defaults reject, or 0 if the request can continue.
func (g *Gateway) checkRequest(r *http.Request) (int, RejectionReason) {
	// The cases run in order and stop at the first match, so the path check
	// only gets an origin-form or accepted absolute-form URL.
	switch {
	case r.Method == http.MethodConnect:
		return http.StatusNotImplemented, ReasonInvalidMethod
	case !g.targetAccepted(r):
		return http.StatusBadRequest, ReasonInvalidRequestTarget
	case !pathAllowed(r.URL.EscapedPath()):
		return http.StatusBadRequest, ReasonInvalidPath
	case g.queryPolicy == QueryReject && !queryAllowed(r.URL.RawQuery):
		return http.StatusBadRequest, ReasonInvalidQuery
	case g.headerNamePolicy.refusesAny(r.Header), g.headerNamePolicy.refusesAny(r.Trailer):
		return http.StatusBadRequest, ReasonInvalidHeaderName
	case !upgradeAllowed(r.Header):
		return http.StatusBadRequest, ReasonInvalidUpgrade
	}

	return 0, ""
}

// targetAccepted reports whether the request target has the origin form, or
// the absolute form when the configuration accepts it.
func (g *Gateway) targetAccepted(r *http.Request) bool {
	return isOriginForm(r) || g.absoluteForm && isAbsoluteForm(r)
}

// isOriginForm examines URL, because RequestURI is empty for a request that did
// not come from a server.
func isOriginForm(r *http.Request) bool {
	// The absolute, authority, and asterisk forms (RFC 9112, section 3.2) and
	// opaque URLs set one of these fields or give a path without "/".
	u := r.URL
	if u.Opaque != "" || u.Scheme != "" || u.Host != "" || u.User != nil || hasFragment(r) {
		return false
	}

	return strings.HasPrefix(u.Path, "/")
}

// hasFragment reports a fragment, which a request target must not have (RFC
// 9112, section 3.2): an unescaped "#" anywhere in the target. A server
// request has the raw target in RequestURI; url.ParseRequestURI keeps the
// "#" in Path or RawQuery, where it cannot be told from a decoded "%23". A
// request from outside a server has no RequestURI, so the URL fields count.
// "%23" is not a fragment.
func hasFragment(r *http.Request) bool {
	u := r.URL

	return strings.Contains(r.RequestURI, "#") || u.Fragment != "" || u.RawFragment != "" || strings.Contains(u.RawQuery, "#")
}

// isAbsoluteForm accepts only an HTTP(S) target whose authority is a host
// that the upstream-selector contract accepts, because the authority becomes
// the Host of the request.
func isAbsoluteForm(r *http.Request) bool {
	u := r.URL
	if !httpTarget(u) || !hostSentUnchanged(u.Host) || hasFragment(r) {
		return false
	}

	return u.Path == "" || strings.HasPrefix(u.Path, "/")
}

// toOriginForm changes an accepted absolute-form request to the origin form,
// with Host set to the authority of the target (RFC 9112, section 3.2.2). r
// must be a shallow copy; the URL of the caller is not changed.
func toOriginForm(r *http.Request) {
	if r.URL.Scheme == "" {
		return
	}

	u := *r.URL
	r.Host = u.Host
	u.Scheme = ""
	u.Host = ""
	if u.Path == "" {
		u.Path = "/"
		u.RawPath = ""
	}

	r.URL = &u
}

// pathAllowed is the path policy in reject mode, and escaped must be an escaped
// path.
func pathAllowed(escaped string) bool {
	// An upstream can decode these characters differently from the router, so
	// a request can get to a path outside its route.
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

// headerNameAllowed rejects an empty name, and other token characters, such
// as "_", which make aliases like "X_User" for "X-User".
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

// removedName also removes a name that the header-name check rejects in
// reject mode, when the policy drops such names. name can be in any letter case.
func (g *Gateway) removedName(name string) bool {
	if g.headerNamePolicy.drops(name) {
		return true
	}

	return g.removedHeader(textproto.CanonicalMIMEHeaderKey(name))
}

// stripHeaders needs a shallow copy of the caller's request. It never changes
// the maps of the copy, which the caller's request shares. It gives the copy
// a new header map when it removes headers, and its own trailer map when the
// caller's request has one, because net/http fills the trailer map of the
// caller at the end of the body (ADR-0027).
func (g *Gateway) stripHeaders(r *http.Request) {
	r.Trailer = g.stripTrailers(r.Trailer, r.Header)
	if !g.needsStrip(r.Header) {
		return
	}

	header := r.Header.Clone()
	for name, values := range header {
		switch {
		case g.removedName(name):
			delete(header, name)
		case textproto.CanonicalMIMEHeaderKey(name) == "Connection":
			// ReverseProxy removes the headers that Connection names. Without
			// this step, a client can remove a managed header that middleware
			// sets.
			header[name], _ = g.connectionValues(values)
			if len(header[name]) == 0 {
				delete(header, name)
			}
		}
	}

	r.Header = header
}

// stripTrailers returns a copy of the declared trailers without the names
// that the gateway removes, because ReverseProxy forwards the declared
// trailer names. header supplies the inbound Connection options. It keeps nil.
func (g *Gateway) stripTrailers(trailer, header http.Header) http.Header {
	if trailer == nil {
		return nil
	}

	nominated, _ := connectionTokens(header)
	kept := trailer.Clone()
	maps.DeleteFunc(kept, func(name string, _ []string) bool {
		return g.removedTrailer(name) || nominated[textproto.CanonicalMIMEHeaderKey(name)]
	})

	return kept
}

// needsStrip lets stripHeaders skip the copy when it removes nothing.
func (g *Gateway) needsStrip(header http.Header) bool {
	for name, values := range header {
		if g.removedName(name) {
			return true
		}

		if textproto.CanonicalMIMEHeaderKey(name) == "Connection" {
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
		if g.removedName(token) {
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
			if token != "" && !g.removedName(token) {
				tokens = append(tokens, token)
			}
		}

		if len(tokens) != 0 {
			kept = append(kept, strings.Join(tokens, ", "))
		}
	}

	return kept, true
}
