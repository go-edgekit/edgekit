// Package egress provides an optional origin and IP-network policy for direct
// HTTP upstream connections. It does not replace deployment egress controls.
package egress

import (
	"context"
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

// ErrDenied means the outbound origin or a resolved address is not allowed.
// It does not prove that a connection was attempted.
var ErrDenied = errors.New("egress: destination denied")

// Config defines an immutable allowlist. Both lists must be non-empty.
// There are no implicit exceptions for private, loopback, or public networks.
// Configure the supplied objects before construction and do not mutate them
// while requests are active. Trusted Go callbacks are not sandboxed.
type Config struct {
	// AllowedOrigins contains HTTP(S) origins, not route URLs. An optional
	// trailing slash is allowed; credentials, paths, queries, and fragments are not.
	AllowedOrigins []string
	// AllowedNetworks contains IPv4 or IPv6 prefixes. Use IPv4 prefixes for
	// IPv4-mapped addresses. Unspecified, multicast, and scoped IPs are rejected.
	AllowedNetworks []netip.Prefix
	// Dialer supplies connection settings and the resolver. Nil uses a 30s
	// connection budget and 30s TCP keepalive. A supplied dialer must have a
	// positive Timeout, which bounds resolution and all connection attempts.
	Dialer *net.Dialer
}

// Transport checks every request origin and every new connection destination.
// It supports concurrent requests and owns its connection pool. Its zero value
// is not usable. Construct it with New and reuse it across requests.
type Transport struct {
	base     *http.Transport
	origins  map[string]struct{}
	peers    map[string]struct{}
	networks []netip.Prefix
	dialer   net.Dialer
	lookup   func(context.Context, string, string) ([]netip.Addr, error)
	dial     func(context.Context, string, string) (net.Conn, error)
}

// New builds a transport using the template's timeout, pool, compression, buffer,
// TLS, and standard HTTP/2 settings. It does not reuse the template's pool.
// Proxy, dial, custom protocol, and TLSNextProto callbacks are not copied. TLS
// ServerName overrides are rejected so the request hostname remains the identity.
// A nil template is invalid. The caller owns CloseIdleConnections on the result.
func New(template *http.Transport, config Config) (*Transport, error) {
	if template == nil {
		return nil, errors.New("egress: transport template is required")
	}
	if len(config.AllowedOrigins) == 0 || len(config.AllowedNetworks) == 0 {
		return nil, errors.New("egress: origins and networks are required")
	}
	p := &Transport{origins: make(map[string]struct{}), peers: make(map[string]struct{})}
	for i, raw := range config.AllowedOrigins {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("egress: invalid origin at index %d", i)
		}

		if u.Path != "" && u.Path != "/" ||
			u.RawPath != "" ||
			u.RawQuery != "" ||
			u.ForceQuery ||
			strings.Contains(raw, "#") {
			return nil, fmt.Errorf("egress: invalid origin at index %d", i)
		}

		origin, peer, err := destination(u)
		if err != nil {
			return nil, fmt.Errorf("egress: invalid origin at index %d", i)
		}
		p.origins[origin] = struct{}{}
		p.peers[peer] = struct{}{}
	}
	for i, prefix := range config.AllowedNetworks {
		if !prefix.IsValid() || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("egress: invalid network at index %d", i)
		}
		p.networks = append(p.networks, prefix.Masked())
	}
	p.dialer = net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if config.Dialer != nil {
		p.dialer = *config.Dialer
	}
	if p.dialer.Timeout <= 0 {
		return nil, errors.New("egress: dial timeout must be positive")
	}
	resolver := p.dialer.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	p.lookup = resolver.LookupNetIP
	p.dial = p.dialer.DialContext
	// Start with a clean transport, not Clone: a clone can retain alternate
	// RoundTrippers registered for http or https and bypass DialContext.
	p.base = &http.Transport{
		DialContext:            p.dialContext,
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
	if template.TLSClientConfig != nil {
		if template.TLSClientConfig.ServerName != "" {
			return nil, errors.New("egress: TLS server name override is not supported")
		}
		p.base.TLSClientConfig = template.TLSClientConfig.Clone()
	}
	return p, nil
}

// RoundTrip checks the URL even when an existing connection can be reused.
// Denials close the request body without reading it or delegating to the pool.
// Errors enter the gateway's existing generic proxy error handling.
func (p *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	origin, _, err := destination(r.URL)
	if _, allowed := p.origins[origin]; err != nil || !allowed {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, ErrDenied
	}
	return p.base.RoundTrip(r)
}

// CloseIdleConnections closes unused upstream connections. It does not interrupt
// active requests or retire a policy while active streams are still using it.
func (p *Transport) CloseIdleConnections() { p.base.CloseIdleConnections() }

func (p *Transport) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, ErrDenied
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrDenied
	}
	host, err = canonicalHost(host)
	if err != nil {
		return nil, ErrDenied
	}
	port, err = canonicalPort(port)
	if err != nil {
		return nil, ErrDenied
	}
	if _, allowed := p.peers[net.JoinHostPort(host, port)]; !allowed {
		return nil, ErrDenied
	}
	deadline := time.Now().Add(p.dialer.Timeout)
	if !p.dialer.Deadline.IsZero() && p.dialer.Deadline.Before(deadline) {
		deadline = p.dialer.Deadline
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var addresses []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{ip}
	} else {
		addresses, err = p.lookup(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(addresses) == 0 {
		return nil, ErrDenied
	}
	// Validate the complete answer before making any connection attempt.
	for _, ip := range addresses {
		if !p.allowedIP(ip) {
			return nil, ErrDenied
		}
	}
	var lastErr error
	for _, ip := range addresses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A literal prevents the dialer from resolving the hostname again.
		conn, err := p.dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (p *Transport) allowedIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	for _, prefix := range p.networks {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// destination returns a canonical origin and the corresponding dial authority.
func destination(u *url.URL) (string, string, error) {
	if u == nil {
		return "", "", ErrDenied
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", ErrDenied
	}

	if u.User != nil || u.Opaque != "" {
		return "", "", ErrDenied
	}

	if u.Fragment != "" || u.RawFragment != "" {
		return "", "", ErrDenied
	}

	host := u.Hostname()
	port := u.Port()
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if port != "" {
		authority += ":" + port
	}
	if u.Host != authority {
		return "", "", ErrDenied
	}
	host, err := canonicalHost(host)
	if err != nil {
		return "", "", ErrDenied
	}
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	port, err = canonicalPort(port)
	if err != nil {
		return "", "", ErrDenied
	}
	peer := net.JoinHostPort(host, port)
	return u.Scheme + "://" + peer, peer, nil
}

func canonicalPort(port string) (string, error) {
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", ErrDenied
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", ErrDenied
	}
	return strconv.Itoa(n), nil
}

func canonicalHost(host string) (string, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", ErrDenied
		}
		return ip.Unmap().String(), nil
	}
	// Require ASCII DNS names. IDNs must be configured as A-labels. Reject
	// trailing dots rather than silently expanding the accepted origin aliases.
	if len(host) == 0 || len(host) > 253 {
		return "", ErrDenied
	}
	for _, c := range host {
		if c > 127 {
			return "", ErrDenied
		}
	}
	host = strings.ToLower(host)
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrDenied
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", ErrDenied
			}
		}
	}
	return host, nil
}
