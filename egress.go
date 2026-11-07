package edgekit

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrEgressDenied is the error that an [EgressTransport] returns, or wraps,
// when the origin of a request or an address of its upstream is not allowed.
// The error contains no URL and no address. The gateway sends 502 Bad Gateway
// for it, as for other failures of the round trip. It does not prove that a
// connection was attempted. Use [errors.Is] to identify it.
var ErrEgressDenied = errors.New("egress destination denied")

// EgressResolver resolves the host of an upstream for the egress dialer.
// [*net.Resolver] implements it. An EgressResolver must be safe for concurrent
// use and must stop when ctx ends.
type EgressResolver interface {
	// LookupNetIP returns the addresses of host. network is "ip".
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// EgressPolicy is the configuration of an [EgressTransport].
// [NewEgressTransport] copies it, so later changes to it have no effect.
// The values that it refers to, such as the resolver and the callbacks of
// the dialer, are shared and must stay valid and safe for concurrent use.
type EgressPolicy struct {
	// AllowedOrigins are the HTTP(S) origins that requests can go to, such as
	// "https://orders.internal.example:8443". An origin has a scheme, a host,
	// and an optional port, and can end with "/". It must not have user
	// information, a path, a query, or a fragment. A host is an IP address or
	// an ASCII DNS name, without a trailing dot, wildcards, or "_"; give an
	// internationalized name in its ASCII (Punycode) form. A name whose last
	// label is a number, such as "127.1" or "0x7f.1", is not permitted,
	// because URL parsers and some resolvers read it as an IPv4 address; give
	// the address in its dotted-decimal form. Letter case,
	// default ports, and different spellings of the same port or IP address
	// do not make a different origin. At least one origin is necessary.
	AllowedOrigins []string

	// AllowedNetworks are the IP networks that the dialer can connect to.
	// Each prefix is an explicit permission: there are no implicit
	// permissions or prohibitions for private, loopback, or public networks,
	// and "0.0.0.0/0" permits all IPv4 addresses. Use IPv4 prefixes for
	// IPv4-mapped IPv6 addresses. Unspecified, multicast, and scoped
	// addresses are always denied. At least one network is necessary.
	AllowedNetworks []netip.Prefix

	// Dialer has the connection settings, such as the timeout and the TCP
	// keep-alive. It is copied. Its Timeout must be positive: it limits the
	// resolution and all connection attempts of one dial together. Its
	// Resolver must be nil; use the Resolver field. Nil uses a timeout of 3s
	// and a TCP keep-alive of 30s.
	Dialer *net.Dialer

	// Resolver resolves the upstream hosts. Nil uses [net.DefaultResolver].
	Resolver EgressResolver
}

// EgressTransport is an [http.RoundTripper] that sends requests only to the
// origins and IP networks of an [EgressPolicy]. It is a configurable
// protection (ADR-0025): set it in [Config.Transport] to enable it. Make it
// with [NewEgressTransport]. It is safe for concurrent use. It owns its
// connection pool: reuse it, and call [EgressTransport.CloseIdleConnections]
// when it is not necessary anymore.
//
// The transport checks the origin of each request before it uses its
// connection pool, so a request on a connection that already exists is also
// checked. For each new connection, the dialer resolves the host once,
// checks every address of the answer before any connection attempt, and
// denies an empty, invalid, or mixed allowed and denied answer. It dials only
// the checked IP literals, one after the other, and never resolves the host
// again. The Host header, the TLS server name, and the certificate
// verification use the host of the request.
//
// A failed dial returns a [*DialError]. A denial returns an error that wraps
// [ErrEgressDenied].
//
// Limits: the allowlists narrow the direct destinations of the gateway. They
// are not a complete defense against server-side request forgery. A
// connection that exists can be reused after the DNS answer changed. DNS
// queries themselves are not limited by the networks. The dialer has no
// Happy Eyeballs, so a slow first address uses the time of the dial.
// Network controls outside the process are still necessary.
type EgressTransport struct {
	base     *http.Transport
	origins  map[string]struct{}
	peers    map[string]struct{}
	networks []netip.Prefix
	dialer   net.Dialer
	resolver EgressResolver
}

// NewEgressTransport returns a new [EgressTransport] for policy, with the
// settings of template. A nil template uses the settings of [NewTransport].
//
// The transport is a new [http.Transport] with its own connection pool. It
// copies these settings of template: the TLS configuration (as a clone), the
// TLS-handshake, response-header, Expect: 100-continue, and idle timeouts,
// the pool limits, DisableKeepAlives, DisableCompression, the buffer sizes,
// the response-header size limit, ForceAttemptHTTP2, an empty TLSNextProto
// (which disables HTTP/2), HTTP2, and Protocols. The egress dialer replaces
// DialContext.
//
// NewEgressTransport rejects a template that can send requests on a path
// that the egress dialer does not control, or with another identity: a Proxy,
// a DialTLSContext, a TLSNextProto with entries, a proxy-connect
// setting (ProxyConnectHeader, GetProxyConnectHeader,
// OnProxyConnectResponse), or a TLS ServerName. It also rejects a template
// with transparent compression, as [New] does. It does not copy the
// deprecated Dial and DialTLS fields. It cannot see protocols that
// [http.Transport.RegisterProtocol] added, so it does not copy them.
//
// If the configuration is not valid, NewEgressTransport returns an error that
// wraps [ErrInvalidConfiguration], with one wrapped error for each problem.
// The errors identify the field and the index, but contain no values.
func NewEgressTransport(template *http.Transport, policy EgressPolicy) (*EgressTransport, error) {
	if template == nil {
		template = NewTransport()
	}

	t := &EgressTransport{
		origins:  make(map[string]struct{}, len(policy.AllowedOrigins)),
		peers:    make(map[string]struct{}, len(policy.AllowedOrigins)),
		resolver: policy.Resolver,
		dialer: net.Dialer{
			Timeout:   defaultDialTimeout,
			KeepAlive: defaultDialKeepAlive,
		},
	}

	problems := t.addOrigins(policy.AllowedOrigins)
	problems = append(problems, t.addNetworks(policy.AllowedNetworks)...)
	problems = append(problems, checkTemplate(template)...)

	if policy.Dialer != nil {
		t.dialer = *policy.Dialer
	}

	if t.dialer.Timeout <= 0 {
		problems = append(problems, invalid("dialer: timeout is not positive"))
	}

	if t.dialer.Resolver != nil {
		problems = append(problems, invalid("dialer: resolver is set; use the policy resolver"))
	}

	if isNilValue(t.resolver) {
		if t.resolver != nil {
			problems = append(problems, invalid("resolver is a typed nil value"))
		}

		t.resolver = net.DefaultResolver
	}

	if len(problems) != 0 {
		return nil, errors.Join(problems...)
	}

	t.base = t.newBase(template)

	return t, nil
}

// addOrigins records the canonical origins and their dial authorities.
func (t *EgressTransport) addOrigins(origins []string) []error {
	if len(origins) == 0 {
		return []error{invalid("allowed origins: none is configured")}
	}

	var problems []error
	for i, raw := range origins {
		origin, peer, err := parseOrigin(raw)
		if err != nil {
			problems = append(problems, invalid("allowed origin %d: %w", i, err))

			continue
		}

		t.origins[origin] = struct{}{}
		t.peers[peer] = struct{}{}
	}

	return problems
}

// addNetworks keeps masked copies of the prefixes.
func (t *EgressTransport) addNetworks(networks []netip.Prefix) []error {
	if len(networks) == 0 {
		return []error{invalid("allowed networks: none is configured")}
	}

	var problems []error
	for i, prefix := range networks {
		if !prefix.IsValid() || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" {
			problems = append(problems, invalid("allowed network %d: is not a valid IPv4 or IPv6 prefix", i))

			continue
		}

		t.networks = append(t.networks, prefix.Masked())
	}

	return problems
}

// parseOrigin rejects the parts that an origin must not have, so the error
// never contains them.
func parseOrigin(raw string) (origin, peer string, err error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", "", errors.New("is not a valid URL")
	case u.Path != "" && u.Path != "/", u.RawPath != "":
		return "", "", errors.New("has a path")
	case u.RawQuery != "" || u.ForceQuery:
		return "", "", errors.New("has a query")
	case strings.Contains(raw, "#"):
		return "", "", errors.New("has a fragment")
	}

	origin, peer, ok := destination(u)
	if !ok {
		return "", "", errors.New("is not an HTTP(S) origin with a valid host and port")
	}

	return origin, peer, nil
}

// checkTemplate rejects the settings that can send requests on a path that
// the egress dialer does not control.
func checkTemplate(template *http.Transport) []error {
	var problems []error
	for _, check := range []struct {
		set     bool
		problem string
	}{
		{
			set:     template.Proxy != nil,
			problem: "Proxy is not supported",
		},
		{
			set:     template.DialTLSContext != nil,
			problem: "DialTLSContext is not supported",
		},
		{
			set:     len(template.TLSNextProto) != 0,
			problem: "TLSNextProto is not supported",
		},
		{
			set:     len(template.ProxyConnectHeader) != 0,
			problem: "ProxyConnectHeader is not supported",
		},
		{
			set:     template.GetProxyConnectHeader != nil,
			problem: "GetProxyConnectHeader is not supported",
		},
		{
			set:     template.OnProxyConnectResponse != nil,
			problem: "OnProxyConnectResponse is not supported",
		},
		{
			set:     template.TLSClientConfig != nil && template.TLSClientConfig.ServerName != "",
			problem: "TLSClientConfig.ServerName is not supported",
		},
		{
			set:     !template.DisableCompression,
			problem: "transparent compression is not supported; set DisableCompression",
		},
	} {
		if check.set {
			problems = append(problems, invalid("template: %s", check.problem))
		}
	}

	return problems
}

// newBase makes a clean transport, not a Clone: a clone keeps the protocols
// that RegisterProtocol added, which do not use DialContext.
func (t *EgressTransport) newBase(template *http.Transport) *http.Transport {
	base := &http.Transport{
		DialContext:            t.dialContext,
		TLSClientConfig:        template.TLSClientConfig.Clone(),
		TLSHandshakeTimeout:    template.TLSHandshakeTimeout,
		DisableKeepAlives:      template.DisableKeepAlives,
		DisableCompression:     template.DisableCompression,
		MaxIdleConns:           template.MaxIdleConns,
		MaxIdleConnsPerHost:    template.MaxIdleConnsPerHost,
		MaxConnsPerHost:        template.MaxConnsPerHost,
		IdleConnTimeout:        template.IdleConnTimeout,
		ResponseHeaderTimeout:  template.ResponseHeaderTimeout,
		ExpectContinueTimeout:  template.ExpectContinueTimeout,
		MaxResponseHeaderBytes: template.MaxResponseHeaderBytes,
		WriteBufferSize:        template.WriteBufferSize,
		ReadBufferSize:         template.ReadBufferSize,
		ForceAttemptHTTP2:      template.ForceAttemptHTTP2,
	}

	// An empty, non-nil map disables HTTP/2; it has no callbacks to copy.
	if template.TLSNextProto != nil {
		base.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}

	if template.HTTP2 != nil {
		h2 := *template.HTTP2
		base.HTTP2 = &h2
	}

	if template.Protocols != nil {
		protocols := *template.Protocols
		base.Protocols = &protocols
	}

	return base
}

// RoundTrip sends r when its origin is allowed. Otherwise it closes the body
// of r without a read and returns an error that wraps [ErrEgressDenied].
func (t *EgressTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	origin, _, ok := destination(r.URL)
	if _, allowed := t.origins[origin]; !ok || !allowed {
		// A RoundTripper must close the body, also when it fails.
		if r.Body != nil {
			_ = r.Body.Close()
		}

		return nil, ErrEgressDenied
	}

	return t.base.RoundTrip(r)
}

// CloseIdleConnections closes the idle connections of the transport, and
// stops the dials that no request waits for. It does not stop active
// requests or upgraded connections.
func (t *EgressTransport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
}

// DialError is the evidence that a transport did not establish a
// connection, and that it gave this failure to the round trip (ADR-0025). It
// is not proof that this request started the dial: the transport can dial
// for one request and give the connection, or the failure, to the request
// that waits for it. [EgressTransport] returns it; the transport of
// [NewTransport] does not.
//
// The dialer makes a new DialError for each failed dial and does not change
// it after it returns it. The value and its Attempts slice are safe for
// concurrent reads. Do not change them.
//
// DialError does not implement [net.Error], so the status of the gateway
// comes from the error that it wraps: 504 for a timeout, and 499 when the
// cause is [context.Canceled] and the client is gone. The errors can contain
// addresses: do not send them to clients.
type DialError struct {
	// Attempts are the connection attempts of this dial, in order. It is
	// empty when no attempt was made, such as after a failed lookup or a
	// denial. It does not describe other dials of the round trip.
	Attempts []DialAttempt

	// Err is the cause: the lookup error, an error that wraps
	// [ErrEgressDenied], the context error when the time of the dial ended
	// or the dial was canceled, or the error of the last attempt.
	Err error
}

// DialAttempt is one connection attempt of a dial.
type DialAttempt struct {
	// Addr is the address that the attempt tried to connect to.
	Addr netip.AddrPort

	// Err is the error of the attempt.
	Err error
}

// Error returns a message that names the step, followed by the message of
// the cause when there is one.
func (e *DialError) Error() string {
	if e.Err == nil {
		return "dial upstream"
	}

	return "dial upstream: " + e.Err.Error()
}

// Unwrap returns the cause.
func (e *DialError) Unwrap() error {
	return e.Err
}

// dialContext checks the dial authority, resolves it once, checks all
// addresses, and dials only the checked literals.
func (t *EgressTransport) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, ok := t.dialAuthority(network, address)
	if !ok {
		return nil, &DialError{
			Err: ErrEgressDenied,
		}
	}

	// One deadline limits the resolution and all attempts.
	deadline := time.Now().Add(t.dialer.Timeout)
	if !t.dialer.Deadline.IsZero() && t.dialer.Deadline.Before(deadline) {
		deadline = t.dialer.Deadline
	}

	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return nil, &DialError{
			Err: err,
		}
	}

	addrs, err := t.resolve(ctx, host)
	if err != nil {
		return nil, &DialError{
			Err: err,
		}
	}

	var attempts []DialAttempt
	for _, addr := range addrs {
		if err := ctx.Err(); err != nil {
			return nil, &DialError{
				Attempts: attempts,
				Err:      err,
			}
		}

		target := netip.AddrPortFrom(addr, port)
		conn, err := t.dialer.DialContext(ctx, network, target.String())
		if err == nil {
			return conn, nil
		}

		attempts = append(attempts, DialAttempt{
			Addr: target,
			Err:  err,
		})
	}

	cause := attempts[len(attempts)-1].Err
	if err := ctx.Err(); err != nil {
		cause = err
	}

	return nil, &DialError{
		Attempts: attempts,
		Err:      cause,
	}
}

// dialAuthority returns the host and the port of an allowed dial authority.
// The transport dials only the authority of an allowed origin, so a denial
// here shows a path around the origin check.
func (t *EgressTransport) dialAuthority(network, address string) (string, uint16, bool) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return "", 0, false
	}

	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, false
	}

	peer, port, ok := canonicalPeer(host, rawPort)
	if !ok {
		return "", 0, false
	}

	if _, allowed := t.peers[peer]; !allowed {
		return "", 0, false
	}

	// The host of the peer is canonical; the resolver gets that form.
	host, _, err = net.SplitHostPort(peer)

	return host, port, err == nil
}

// resolve returns the addresses of host after it checked all of them, so a
// mixed answer is denied before any connection attempt.
func (t *EgressTransport) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		addrs, err = t.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: resolution gave no address", ErrEgressDenied)
	}

	checked := make([]netip.Addr, len(addrs))
	for i, addr := range addrs {
		if !t.allowedAddr(addr) {
			return nil, fmt.Errorf("%w: resolution gave an address that is not allowed", ErrEgressDenied)
		}

		checked[i] = addr.Unmap()
	}

	return checked, nil
}

// allowedAddr denies addresses that are not unicast or have a zone, also
// when a network contains them.
func (t *EgressTransport) allowedAddr(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}

	addr = addr.Unmap()
	if addr.IsUnspecified() || addr.IsMulticast() {
		return false
	}

	for _, prefix := range t.networks {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// httpTarget reports whether u is an HTTP(S) URL without user information,
// an opaque part, or a fragment. It does not check the host.
func httpTarget(u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}

	if u.User != nil || u.Opaque != "" {
		return false
	}

	return u.Fragment == "" && u.RawFragment == ""
}

// destination returns the canonical origin of u and its dial authority. It
// reports false for a URL that is not an HTTP(S) origin with a valid host.
func destination(u *url.URL) (origin, peer string, ok bool) {
	if u == nil || !httpTarget(u) {
		return "", "", false
	}

	// Hostname and Port accept spellings, such as an empty port, that the
	// authority must not have.
	host, rawPort := u.Hostname(), u.Port()
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}

	if rawPort != "" {
		authority += ":" + rawPort
	}

	if u.Host != authority {
		return "", "", false
	}

	if rawPort == "" {
		rawPort = "80"
		if u.Scheme == "https" {
			rawPort = "443"
		}
	}

	peer, _, ok = canonicalPeer(host, rawPort)
	if !ok {
		return "", "", false
	}

	return u.Scheme + "://" + peer, peer, true
}

// canonicalPeer returns the canonical dial authority of host and port, and
// the port as a number.
func canonicalPeer(host, rawPort string) (string, uint16, bool) {
	host, hostOK := canonicalHost(host)
	port, portOK := canonicalPort(rawPort)
	if !hostOK || !portOK {
		return "", 0, false
	}

	return net.JoinHostPort(host, strconv.Itoa(int(port))), port, true
}

// canonicalPort accepts only decimal digits, with leading zeros, from 1 to
// 65535.
func canonicalPort(port string) (uint16, bool) {
	if !isDecimal(port) {
		return 0, false
	}

	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return 0, false
	}

	return uint16(n), true
}

// canonicalHost returns the canonical form of an IP address, or a lowercase
// ASCII DNS name. It rejects trailing dots, wildcards, and "_", so a name has
// only one accepted spelling apart from letter case.
func canonicalHost(host string) (string, bool) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", false
		}

		return ip.Unmap().String(), true
	}

	if host == "" || len(host) > 253 {
		return "", false
	}

	// Check for ASCII before the case change: strings.ToLower changes some
	// other characters, such as the Kelvin sign, to ASCII letters.
	for i := range len(host) {
		if host[i] >= 0x80 {
			return "", false
		}
	}

	host = strings.ToLower(host)
	if endsInNumber(host) {
		return "", false
	}

	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}

		for i := range len(label) {
			if c := label[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", false
			}
		}
	}

	return host, true
}

// endsInNumber reports a lowercase name whose last label is a decimal or
// hexadecimal number. The WHATWG URL Standard ("ends in a number") and
// inet_aton read such a name, such as "127.1" or "0x7f.1", as an IPv4
// address, so it is a second spelling of an address.
func endsInNumber(host string) bool {
	last := host[strings.LastIndexByte(host, '.')+1:]
	if isDecimal(last) {
		return true
	}

	digits, hex := strings.CutPrefix(last, "0x")

	return hex && strings.Trim(digits, "0123456789abcdef") == ""
}
