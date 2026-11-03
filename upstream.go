package edgekit

import (
	"errors"
	"net/http"
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
//   - The gateway copies the returned URL and does not change it.
//   - If SelectUpstream returns an error, or a URL that does not obey the
//     contract, the gateway sends 502 Bad Gateway. It does not send the error
//     text to the client.
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
