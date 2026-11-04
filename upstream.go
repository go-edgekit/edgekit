package edgekit

import (
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// UpstreamSelector chooses the upstream URL for a request that matched a
// route. It is an extension point: the default is [StaticUpstream], and other
// selectors can do load balancing or service discovery.
//
// The gateway calls SelectUpstream after the route middleware, so route
// middleware can set request data that the selector uses.
//
// Contract:
//   - SelectUpstream must be safe for concurrent use.
//   - It must not change r or read its body.
//   - The returned URL must be an absolute http or https URL with a host. It
//     must not contain user information, a query, or a fragment. A path is a
//     base path: the gateway adds the inbound path to it.
//   - The host must be in the form that the transport sends without changes:
//     ASCII letters, digits, ".", "-", and "_", or an IPv6 literal in
//     brackets, with an optional port. Give an internationalized name in its
//     ASCII (Punycode) form. IPv6 zones, such as "[fe80::1%eth0]", are not
//     permitted. So [OutboundRequest.Host] is the Host value that the
//     upstream receives, and a hook can sign it.
//   - The gateway copies the returned URL and does not change it.
//   - If SelectUpstream returns an error, or a URL that does not obey the
//     contract, the gateway sends 500 Internal Server Error, because the
//     failure is not in the upstream. It does not send the error text to the
//     client. Completion gets the error as a [Failure] in
//     [PhaseUpstreamSelection].
type UpstreamSelector interface {
	SelectUpstream(r *http.Request) (*url.URL, error)
}

// UpstreamSelectorFunc is an adapter to use an ordinary function as an
// [UpstreamSelector], as [http.HandlerFunc] is for [http.Handler]. The
// function must obey the contract of [UpstreamSelector]. [New] rejects a nil
// UpstreamSelectorFunc.
type UpstreamSelectorFunc func(r *http.Request) (*url.URL, error)

// SelectUpstream calls f(r).
func (f UpstreamSelectorFunc) SelectUpstream(r *http.Request) (*url.URL, error) {
	return f(r)
}

// StaticUpstream returns a selector that always selects upstream. Use it for
// a route with one fixed upstream.
//
// StaticUpstream copies upstream, so later changes to it have no effect. It
// returns an error that wraps [ErrInvalidConfiguration] if upstream does not
// obey the URL rules of [UpstreamSelector]. The error does not contain the URL.
// The returned selector is safe for concurrent use.
func StaticUpstream(upstream *url.URL) (UpstreamSelector, error) {
	if err := checkUpstreamURL(upstream); err != nil {
		return nil, invalid("upstream URL: %w", err)
	}

	return staticUpstream{url: *upstream}, nil
}

// staticUpstream holds an immutable upstream URL.
type staticUpstream struct {
	url url.URL
}

// SelectUpstream returns a copy, so a caller cannot change the URL for later
// requests.
func (s staticUpstream) SelectUpstream(*http.Request) (*url.URL, error) {
	selected := s.url

	return &selected, nil
}

// checkUpstreamURL never puts parts of u in the error, because u can contain
// credentials.
func checkUpstreamURL(u *url.URL) error {
	switch {
	case u == nil:
		return errors.New("is nil")
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("scheme is not http or https")
	case u.Opaque != "":
		return errors.New("is opaque")
	case u.Host == "":
		return errors.New("host is empty")
	case !hostSentUnchanged(u.Host):
		return errors.New("host is not in the ASCII form that the transport sends")
	case u.User != nil:
		return errors.New("contains user information")
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("contains a query")
	case u.Fragment != "" || u.RawFragment != "":
		return errors.New("contains a fragment")
	case u.Path != "" && !strings.HasPrefix(u.Path, "/"):
		return errors.New("path does not start with /")
	}

	return nil
}

// hostSentUnchanged reports whether the transport sends host without changes.
// The transport converts a non-ASCII name to Punycode, removes an IPv6 zone
// (RFC 6874), and sends no Host value, or fails, for other invalid characters.
// The port separator is found as by [url.URL.Port], so a malformed host fails
// here and not later as a dial failure.
func hostSentUnchanged(host string) bool {
	name, port := host, ""
	if i := strings.LastIndexByte(host, ':'); i > strings.LastIndexByte(host, ']') {
		name, port = host[:i], host[i+1:]
	}

	if port != "" && !isDecimal(port) {
		return false
	}

	if literal, ok := strings.CutPrefix(name, "["); ok {
		literal, ok = strings.CutSuffix(literal, "]")
		addr, err := netip.ParseAddr(literal)

		return ok && err == nil && addr.Is6() && addr.Zone() == ""
	}

	if name == "" {
		return false
	}

	for i := range len(name) {
		switch c := name[i]; {
		case isHeaderNameChar(c), c == '.', c == '_':
		default:
			return false
		}
	}

	return true
}
