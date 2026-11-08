package edgekit_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// resolverFunc is an edgekit.EgressResolver from a function.
type resolverFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

// LookupNetIP calls f.
func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

// answer returns a resolver that gives addrs for each host.
func answer(addrs ...string) resolverFunc {
	return func(context.Context, string, string) ([]netip.Addr, error) {
		parsed := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			parsed = append(parsed, netip.MustParseAddr(a))
		}

		return parsed, nil
	}
}

// errRefused is the deterministic failure of a connection attempt that the
// test control function refuses.
var errRefused = errors.New("connection attempt refused by the test")

// dialRecorder records the connection attempts of a dialer and can refuse
// them before the connection.
type dialRecorder struct {
	refuse func(address string) bool

	mu        sync.Mutex
	addresses []string
	deadlines []time.Time
}

// dialer returns a dialer with timeout that records each attempt.
func (d *dialRecorder) dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout: timeout,
		ControlContext: func(ctx context.Context, _, address string, _ syscall.RawConn) error {
			deadline, _ := ctx.Deadline()
			d.mu.Lock()
			d.addresses = append(d.addresses, address)
			d.deadlines = append(d.deadlines, deadline)
			d.mu.Unlock()

			if d.refuse != nil && d.refuse(address) {
				return errRefused
			}

			return nil
		},
	}
}

// attempts returns a copy of the recorded addresses.
func (d *dialRecorder) attempts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]string(nil), d.addresses...)
}

// refuseAll refuses each attempt.
func refuseAll(string) bool {
	return true
}

// newEgress returns an egress transport and closes its idle connections when
// the test ends.
func newEgress(t *testing.T, template *http.Transport, policy edgekit.EgressPolicy) *edgekit.EgressTransport {
	t.Helper()

	transport, err := edgekit.NewEgressTransport(template, policy)
	if err != nil {
		t.Fatalf("NewEgressTransport() error = %v", err)
	}
	t.Cleanup(transport.CloseIdleConnections)

	return transport
}

// prefixes parses each prefix.
func prefixes(raw ...string) []netip.Prefix {
	parsed := make([]netip.Prefix, 0, len(raw))
	for _, p := range raw {
		parsed = append(parsed, netip.MustParsePrefix(p))
	}

	return parsed
}

// roundTrip sends a GET to rawURL with transport and closes the response.
func roundTrip(t *testing.T, transport http.RoundTripper, rawURL string) (int, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() error = %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		return 0, err
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return resp.StatusCode, nil
}

// errUnexpectedLookup is the error of noLookup.
var errUnexpectedLookup = errors.New("unexpected DNS lookup")

// noLookup fails each lookup, so a test sees errUnexpectedLookup when the
// transport resolves a host.
var noLookup resolverFunc = func(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errUnexpectedLookup
}

func TestNewEgressTransportRejectsInvalidOrigins(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin string
	}{
		{
			name:   "empty",
			origin: "",
		},
		{
			name:   "other scheme",
			origin: "ftp://upstream.test",
		},
		{
			name:   "user information",
			origin: "http://user@upstream.test",
		},
		{
			name:   "path",
			origin: "http://upstream.test/secret",
		},
		{
			name:   "query",
			origin: "http://upstream.test?token=secret",
		},
		{
			name:   "empty query",
			origin: "http://upstream.test?",
		},
		{
			name:   "empty fragment",
			origin: "http://upstream.test#",
		},
		{
			name:   "fragment",
			origin: "http://upstream.test#secret",
		},
		{
			name:   "empty port",
			origin: "http://upstream.test:",
		},
		{
			name:   "port zero",
			origin: "http://upstream.test:0",
		},
		{
			name:   "port over 65535",
			origin: "http://upstream.test:65536",
		},
		{
			name:   "non-numeric port",
			origin: "http://upstream.test:bad",
		},
		{
			name:   "trailing dot",
			origin: "http://upstream.test.",
		},
		{
			name:   "wildcard",
			origin: "http://*.test",
		},
		{
			name:   "underscore",
			origin: "http://under_score.test",
		},
		{
			name:   "label starts with a hyphen",
			origin: "http://-bad.test",
		},
		{
			name:   "label ends with a hyphen",
			origin: "http://bad-.test",
		},
		{
			name:   "IPv6 zone",
			origin: "http://[fe80::1%25eth0]",
		},
		{
			name:   "non-ASCII name",
			origin: "http://é.test",
		},
		{
			name:   "Kelvin sign",
			origin: "http://K.test",
		},
		{
			name:   "opaque",
			origin: "http:upstream.test",
		},
		{
			name:   "empty host",
			origin: "http:///missing",
		},
		{
			name:   "label longer than 63 bytes",
			origin: "http://" + strings.Repeat("a", 64) + ".test",
		},
		{
			name:   "short IPv4 form",
			origin: "http://127.1",
		},
		{
			name:   "decimal IPv4 form",
			origin: "http://2130706433",
		},
		{
			name:   "hexadecimal IPv4 part",
			origin: "http://0x7f.0.0.1",
		},
		{
			name:   "last label with a leading zero",
			origin: "http://127.0.0.010",
		},
		{
			name:   "hexadecimal last label",
			origin: "http://upstream.0x1F",
		},
		{
			name:   "empty hexadecimal last label",
			origin: "http://upstream.0x",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := edgekit.NewEgressTransport(nil, edgekit.EgressPolicy{
				AllowedOrigins:  []string{tc.origin},
				AllowedNetworks: prefixes("0.0.0.0/0"),
			})

			if !errors.Is(err, edgekit.ErrInvalidConfiguration) || !strings.Contains(err.Error(), "allowed origin 0") {
				t.Fatalf("NewEgressTransport(%q) error = %v, want an invalid origin 0", tc.origin, err)
			}

			if strings.Contains(err.Error(), "secret") || tc.origin != "" && strings.Contains(err.Error(), tc.origin) {
				t.Errorf("NewEgressTransport(%q) error = %q, want no configuration value in it", tc.origin, err)
			}
		})
	}
}

func TestNewEgressTransportRejectsInvalidConfiguration(t *testing.T) {
	valid := edgekit.EgressPolicy{
		AllowedOrigins:  []string{"https://upstream.test"},
		AllowedNetworks: prefixes("127.0.0.0/8"),
	}
	safeTemplate := edgekit.NewTransport

	var nilResolver *net.Resolver
	for _, tc := range []struct {
		name     string
		template func() *http.Transport
		policy   func(p *edgekit.EgressPolicy)
		problem  string
	}{
		{
			name: "no origin",
			policy: func(p *edgekit.EgressPolicy) {
				p.AllowedOrigins = nil
			},
			problem: "allowed origins: none is configured",
		},
		{
			name: "no network",
			policy: func(p *edgekit.EgressPolicy) {
				p.AllowedNetworks = nil
			},
			problem: "allowed networks: none is configured",
		},
		{
			name: "invalid prefix",
			policy: func(p *edgekit.EgressPolicy) {
				p.AllowedNetworks = []netip.Prefix{
					{},
				}
			},
			problem: "allowed network 0",
		},
		{
			name: "mapped prefix",
			policy: func(p *edgekit.EgressPolicy) {
				p.AllowedNetworks = prefixes("::ffff:127.0.0.0/104")
			},
			problem: "allowed network 0",
		},
		{
			name: "zero dial timeout",
			policy: func(p *edgekit.EgressPolicy) {
				p.Dialer = &net.Dialer{}
			},
			problem: "dialer: timeout is not positive",
		},
		{
			name: "resolver in the dialer",
			policy: func(p *edgekit.EgressPolicy) {
				p.Dialer = &net.Dialer{
					Timeout:  time.Second,
					Resolver: net.DefaultResolver,
				}
			},
			problem: "dialer: resolver is set",
		},
		{
			name: "typed nil resolver",
			policy: func(p *edgekit.EgressPolicy) {
				p.Resolver = nilResolver
			},
			problem: "resolver is a typed nil value",
		},
		{
			name: "proxy",
			template: func() *http.Transport {
				t := safeTemplate()
				t.Proxy = http.ProxyFromEnvironment

				return t
			},
			problem: "template: Proxy is not supported",
		},
		{
			name: "TLS dialer",
			template: func() *http.Transport {
				t := safeTemplate()
				t.DialTLSContext = func(context.Context, string, string) (net.Conn, error) {
					return nil, errors.New("custom TLS dialer")
				}

				return t
			},
			problem: "template: DialTLSContext is not supported",
		},
		{
			name: "protocol callback",
			template: func() *http.Transport {
				t := safeTemplate()
				t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{
					"h2": func(string, *tls.Conn) http.RoundTripper {
						return nil
					},
				}

				return t
			},
			problem: "template: TLSNextProto is not supported",
		},
		{
			name: "proxy-connect header",
			template: func() *http.Transport {
				t := safeTemplate()
				t.ProxyConnectHeader = http.Header{
					"X-Proxy": {"value"},
				}

				return t
			},
			problem: "template: ProxyConnectHeader is not supported",
		},
		{
			name: "proxy-connect header callback",
			template: func() *http.Transport {
				t := safeTemplate()
				t.GetProxyConnectHeader = func(context.Context, *url.URL, string) (http.Header, error) {
					return nil, nil
				}

				return t
			},
			problem: "template: GetProxyConnectHeader is not supported",
		},
		{
			name: "proxy-connect response callback",
			template: func() *http.Transport {
				t := safeTemplate()
				t.OnProxyConnectResponse = func(context.Context, *url.URL, *http.Request, *http.Response) error {
					return nil
				}

				return t
			},
			problem: "template: OnProxyConnectResponse is not supported",
		},
		{
			name: "TLS server name",
			template: func() *http.Transport {
				t := safeTemplate()
				t.TLSClientConfig = &tls.Config{
					ServerName: "other.test",
					MinVersion: tls.VersionTLS12,
				}

				return t
			},
			problem: "template: TLSClientConfig.ServerName is not supported",
		},
		{
			name: "transparent compression",
			template: func() *http.Transport {
				return &http.Transport{}
			},
			problem: "template: transparent compression is not supported; set DisableCompression",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := valid
			if tc.policy != nil {
				tc.policy(&policy)
			}

			var template *http.Transport
			if tc.template != nil {
				template = tc.template()
			}

			_, err := edgekit.NewEgressTransport(template, policy)
			if !errors.Is(err, edgekit.ErrInvalidConfiguration) || !strings.Contains(err.Error(), tc.problem) {
				t.Errorf("NewEgressTransport() error = %v, want %q", err, tc.problem)
			}
		})
	}

	disabledHTTP2 := safeTemplate()
	disabledHTTP2.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	for _, template := range []*http.Transport{nil, safeTemplate(), disabledHTTP2} {
		if _, err := edgekit.NewEgressTransport(template, valid); err != nil {
			t.Errorf("NewEgressTransport() with a safe template: error = %v", err)
		}
	}
}

func TestEgressOriginNormalization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origin  string
		request string
		dialed  string
	}{
		{
			name:    "upper case",
			origin:  "http://UPSTREAM.test",
			request: "http://upstream.test:80/path",
			dialed:  "127.0.0.1:80",
		},
		{
			name:    "default port with a leading zero",
			origin:  "https://upstream.test:0443/",
			request: "https://Upstream.Test/path",
			dialed:  "127.0.0.1:443",
		},
		{
			name:    "IPv6 with a leading zero",
			origin:  "http://[2001:0db8::1]:8080",
			request: "http://[2001:db8::1]:8080/path",
			dialed:  "[2001:db8::1]:8080",
		},
		{
			name:    "IPv4-mapped IPv6",
			origin:  "http://[::ffff:127.0.0.1]",
			request: "http://127.0.0.1/path",
			dialed:  "127.0.0.1:80",
		},
		{
			name:    "number labels before the last label",
			origin:  "http://10.0x1f.upstream.test",
			request: "http://10.0X1F.upstream.test/path",
			dialed:  "127.0.0.1:80",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &dialRecorder{
				refuse: refuseAll,
			}
			transport := newEgress(t, nil, edgekit.EgressPolicy{
				AllowedOrigins:  []string{tc.origin},
				AllowedNetworks: prefixes("127.0.0.0/8", "2001:db8::/32"),
				Dialer:          recorder.dialer(time.Second),
				Resolver:        answer("127.0.0.1"),
			})

			_, err := roundTrip(t, transport, tc.request)

			// The dialer refuses each attempt, so an allowed origin ends with
			// a refused attempt, not with a denial.
			var dialErr *edgekit.DialError
			if !errors.As(err, &dialErr) || errors.Is(err, edgekit.ErrEgressDenied) {
				t.Fatalf("RoundTrip(%s) error = %v, want a refused dial", tc.request, err)
			}

			if got := recorder.attempts(); len(got) != 1 || got[0] != tc.dialed {
				t.Errorf("dialer attempts = %v, want [%s]", got, tc.dialed)
			}
		})
	}
}

func TestDeniedOriginClosesBodyWithoutDial(t *testing.T) {
	recorder := &dialRecorder{}
	transport := newEgress(t, nil, edgekit.EgressPolicy{
		AllowedOrigins:  []string{"http://upstream.test"},
		AllowedNetworks: prefixes("127.0.0.0/8"),
		Dialer:          recorder.dialer(time.Second),
		Resolver:        noLookup,
	})

	mustParse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q) error = %v", raw, err)
		}

		return u
	}

	for _, tc := range []struct {
		name string
		url  *url.URL
	}{
		{
			name: "nil URL",
			url:  nil,
		},
		{
			name: "empty port",
			url: &url.URL{
				Scheme: "http",
				Host:   "upstream.test:",
			},
		},
		{
			name: "non-numeric port",
			url: &url.URL{
				Scheme: "http",
				Host:   "upstream.test:bad",
			},
		},
		{
			name: "opaque URL",
			url: &url.URL{
				Scheme: "http",
				Host:   "upstream.test",
				Opaque: "//other.test",
			},
		},
		{
			name: "other host",
			url:  mustParse("http://other.test"),
		},
		{
			name: "other scheme",
			url:  mustParse("https://upstream.test"),
		},
		{
			name: "other port",
			url:  mustParse("http://upstream.test:81"),
		},
		{
			name: "allowed host as a prefix",
			url:  mustParse("http://upstream.test.evil.test"),
		},
		{
			name: "user information",
			url:  mustParse("http://user:secret@upstream.test"),
		},
		{
			name: "fragment",
			url:  mustParse("http://upstream.test#secret"),
		},
		{
			name: "not HTTP",
			url:  mustParse("ftp://upstream.test"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &countedBody{}
			resp, err := transport.RoundTrip(&http.Request{
				Method: http.MethodPost,
				URL:    tc.url,
				Header: make(http.Header),
				Body:   body,
			})
			if resp != nil {
				_ = resp.Body.Close()
			}

			if resp != nil || !errors.Is(err, edgekit.ErrEgressDenied) {
				t.Fatalf("RoundTrip() = %v, %v; want ErrEgressDenied", resp, err)
			}

			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "test") {
				t.Errorf("RoundTrip() error = %q, want no part of the URL in it", err)
			}

			if body.reads.Load() != 0 || body.closes.Load() != 1 || len(recorder.attempts()) != 0 {
				t.Errorf("request body reads = %d, closes = %d, dialer attempts = %v; want 0, 1, none", body.reads.Load(), body.closes.Load(), recorder.attempts())
			}
		})
	}
}

// classifyAddress makes one request to http://upstream.test/ through an egress
// transport that allows networks and whose resolver answers addr. The dialer
// refuses each attempt. It returns the recorded attempts and the error.
func classifyAddress(t *testing.T, networks []string, addr netip.Addr) ([]string, error) {
	t.Helper()

	recorder := &dialRecorder{
		refuse: refuseAll,
	}
	transport := newEgress(t, nil, edgekit.EgressPolicy{
		AllowedOrigins:  []string{"http://upstream.test"},
		AllowedNetworks: prefixes(networks...),
		Dialer:          recorder.dialer(time.Second),
		Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{addr}, nil
		}),
	})

	_, err := roundTrip(t, transport, "http://upstream.test/")

	return recorder.attempts(), err
}

func TestEgressAllowsAddressesInNetworks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		networks []string
		addr     netip.Addr
	}{
		{
			name: "IPv4 in network",
			networks: []string{
				"10.0.0.0/8",
			},
			addr: netip.MustParseAddr("10.2.3.4"),
		},
		{
			name: "IPv4-mapped IPv6 in an IPv4 network",
			networks: []string{
				"10.0.0.0/8",
			},
			addr: netip.MustParseAddr("::ffff:10.2.3.4"),
		},
		{
			name: "IPv6 in network",
			networks: []string{
				"2001:db8::/32",
			},
			addr: netip.MustParseAddr("2001:db8::10"),
		},
		{
			name: "single loopback address",
			networks: []string{
				"127.0.0.1/32",
			},
			addr: netip.MustParseAddr("127.0.0.1"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts, err := classifyAddress(t, tc.networks, tc.addr)

			if dialErr, ok := errors.AsType[*edgekit.DialError](err); !ok || !errors.Is(dialErr, errRefused) {
				t.Errorf("RoundTrip() error = %v, want a DialError with the refusal", err)
			}

			want := netip.AddrPortFrom(tc.addr.Unmap(), 80).String()
			if len(attempts) != 1 || attempts[0] != want {
				t.Errorf("dialer attempts = %v, want [%s]", attempts, want)
			}
		})
	}
}

func TestEgressDeniesAddresses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		networks []string
		addr     netip.Addr
	}{
		{
			name: "loopback outside the network",
			networks: []string{
				"127.0.0.1/32",
			},
			addr: netip.MustParseAddr("127.0.0.2"),
		},
		{
			name: "metadata address outside the networks",
			networks: []string{
				"10.0.0.0/8",
				"127.0.0.1/32",
			},
			addr: netip.MustParseAddr("169.254.169.254"),
		},
		{
			name: "mapped metadata address",
			networks: []string{
				"10.0.0.0/8",
			},
			addr: netip.MustParseAddr("::ffff:169.254.169.254"),
		},
		{
			name: "private address outside the network",
			networks: []string{
				"10.0.0.0/8",
			},
			addr: netip.MustParseAddr("192.168.1.1"),
		},
		{
			name: "public address outside the network",
			networks: []string{
				"10.0.0.0/8",
			},
			addr: netip.MustParseAddr("8.8.8.8"),
		},
		{
			name: "unique local outside the network",
			networks: []string{
				"2001:db8::/32",
			},
			addr: netip.MustParseAddr("fc00::1"),
		},
		{
			name: "IPv6 loopback in an IPv4 network",
			networks: []string{
				"127.0.0.1/32",
			},
			addr: netip.MustParseAddr("::1"),
		},
		{
			name: "scoped address",
			networks: []string{
				"::/0",
			},
			addr: netip.MustParseAddr("fe80::1%eth0"),
		},
		{
			name: "mapped IPv4 in an IPv6 network",
			networks: []string{
				"::/0",
			},
			addr: netip.MustParseAddr("::ffff:127.0.0.1"),
		},
		{
			name: "invalid address",
			networks: []string{
				"0.0.0.0/0",
				"::/0",
			},
			addr: netip.Addr{},
		},
		{
			name: "unspecified IPv4",
			networks: []string{
				"0.0.0.0/0",
				"::/0",
			},
			addr: netip.MustParseAddr("0.0.0.0"),
		},
		{
			name: "unspecified IPv6",
			networks: []string{
				"0.0.0.0/0",
				"::/0",
			},
			addr: netip.MustParseAddr("::"),
		},
		{
			name: "IPv4 multicast",
			networks: []string{
				"0.0.0.0/0",
				"::/0",
			},
			addr: netip.MustParseAddr("224.0.0.1"),
		},
		{
			name: "IPv6 multicast",
			networks: []string{
				"0.0.0.0/0",
				"::/0",
			},
			addr: netip.MustParseAddr("ff02::1"),
		},
		{
			name: "mapped unspecified IPv4",
			networks: []string{
				"0.0.0.0/0",
				"::/0",
			},
			addr: netip.MustParseAddr("::ffff:0.0.0.0"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts, err := classifyAddress(t, tc.networks, tc.addr)

			dialErr, ok := errors.AsType[*edgekit.DialError](err)
			if !ok || len(dialErr.Attempts) != 0 || !errors.Is(err, edgekit.ErrEgressDenied) {
				t.Errorf("RoundTrip() error = %v, want a DialError that denies without attempts", err)
			}

			if len(attempts) != 0 {
				t.Errorf("dialer attempts = %v, want none", attempts)
			}
		})
	}
}

func TestLookupAnswersAreValidatedBeforeAnyDial(t *testing.T) {
	for _, addrs := range [][]string{
		{
			"10.0.0.1",
			"169.254.169.254",
		},
		{
			"169.254.169.254",
			"10.0.0.1",
		},
		{},
	} {
		t.Run(strings.Join(addrs, ","), func(t *testing.T) {
			recorder := &dialRecorder{
				refuse: refuseAll,
			}
			transport := newEgress(t, nil, edgekit.EgressPolicy{
				AllowedOrigins:  []string{"http://upstream.test"},
				AllowedNetworks: prefixes("10.0.0.0/8"),
				Dialer:          recorder.dialer(time.Second),
				Resolver:        answer(addrs...),
			})

			_, err := roundTrip(t, transport, "http://upstream.test/")

			if !errors.Is(err, edgekit.ErrEgressDenied) || len(recorder.attempts()) != 0 {
				t.Errorf("RoundTrip() error = %v, dialer attempts = %v; want a denial before any attempt", err, recorder.attempts())
			}
		})
	}
}

func TestDialUsesValidatedLiteralAndPreservesPort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		origin   string
		resolver resolverFunc
		dialed   string
		lookups  []string
	}{
		{
			name:     "name with a port",
			origin:   "http://upstream.test:8080",
			resolver: answer("10.0.0.7"),
			dialed:   "10.0.0.7:8080",
			lookups: []string{
				"ip upstream.test",
			},
		},
		{
			name:     "mapped answer on the default port",
			origin:   "https://upstream.test",
			resolver: answer("::ffff:10.0.0.7"),
			dialed:   "10.0.0.7:443",
			lookups: []string{
				"ip upstream.test",
			},
		},
		{
			name:     "IPv6 literal",
			origin:   "http://[2001:db8::1]:81",
			resolver: noLookup,
			dialed:   "[2001:db8::1]:81",
		},
		{
			name:     "IPv4 literal",
			origin:   "http://10.0.0.9:8081",
			resolver: noLookup,
			dialed:   "10.0.0.9:8081",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &dialRecorder{
				refuse: refuseAll,
			}
			var hosts []string
			resolver := tc.resolver
			transport := newEgress(t, nil, edgekit.EgressPolicy{
				AllowedOrigins:  []string{tc.origin},
				AllowedNetworks: prefixes("10.0.0.0/8", "2001:db8::/32"),
				Dialer:          recorder.dialer(time.Second),
				Resolver: resolverFunc(func(ctx context.Context, network, host string) ([]netip.Addr, error) {
					hosts = append(hosts, network+" "+host)

					return resolver(ctx, network, host)
				}),
			})

			_, err := roundTrip(t, transport, tc.origin+"/path")

			var dialErr *edgekit.DialError
			if !errors.As(err, &dialErr) || len(dialErr.Attempts) != 1 || dialErr.Attempts[0].Addr.String() != tc.dialed {
				t.Errorf("RoundTrip() error = %v, want a DialError with one attempt to %s", err, tc.dialed)
			}

			if got := recorder.attempts(); len(got) != 1 || got[0] != tc.dialed {
				t.Errorf("dialer attempts = %v, want only the literal %s", got, tc.dialed)
			}

			if !slices.Equal(hosts, tc.lookups) {
				t.Errorf("resolver lookups = %v, want %v", hosts, tc.lookups)
			}
		})
	}
}

func TestResolutionChangesCannotReuseHostnameAtDial(t *testing.T) {
	var requests atomic.Int32
	upstream := countingUpstream(t, &requests)
	_, port, err := net.SplitHostPort(parseURL(t, upstream.URL).Host)
	if err != nil {
		t.Fatalf("net.SplitHostPort() error = %v", err)
	}

	var lookups atomic.Int32
	recorder := &dialRecorder{}
	template := edgekit.NewTransport()
	template.DisableKeepAlives = true
	transport := newEgress(t, template, edgekit.EgressPolicy{
		AllowedOrigins:  []string{"http://upstream.test:" + port},
		AllowedNetworks: prefixes("127.0.0.1/32"),
		Dialer:          recorder.dialer(time.Second),
		Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			if lookups.Add(1) == 1 {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}

			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}),
	})

	if status, firstErr := roundTrip(t, transport, "http://upstream.test:"+port+"/"); firstErr != nil || status != http.StatusOK {
		t.Fatalf("first RoundTrip() = %d, %v; want 200", status, firstErr)
	}

	_, err = roundTrip(t, transport, "http://upstream.test:"+port+"/")
	if !errors.Is(err, edgekit.ErrEgressDenied) || lookups.Load() != 2 || requests.Load() != 1 {
		t.Errorf("second RoundTrip() error = %v, resolver lookups = %d, upstream requests = %d; want a denial after a new lookup", err, lookups.Load(), requests.Load())
	}

	if got := recorder.attempts(); len(got) != 1 || got[0] != "127.0.0.1:"+port {
		t.Errorf("dialer attempts = %v, want only the first checked literal", got)
	}
}

// fallbackResult is what one request saw when the resolver answered
// 127.0.0.2 and then 127.0.0.1.
type fallbackResult struct {
	recorder   *httptest.ResponseRecorder
	completion edgekit.Completion

	// attempts and deadlines are the dial attempts and their deadlines.
	attempts  []string
	deadlines []time.Time

	// lookupDeadline is the deadline of the lookup context.
	lookupDeadline time.Time

	// want are the two addresses in the order of the answer, with the port
	// of the upstream.
	want []string
}

// dialFallback sends one request through a gateway with an egress transport
// whose resolver answers two addresses, and whose dialer refuses each address
// for which refuse reports true.
func dialFallback(t *testing.T, refuse func(address string) bool) fallbackResult {
	t.Helper()

	var requests atomic.Int32
	upstream := countingUpstream(t, &requests)
	_, port, err := net.SplitHostPort(parseURL(t, upstream.URL).Host)
	if err != nil {
		t.Fatalf("net.SplitHostPort() error = %v", err)
	}

	got := fallbackResult{
		recorder: httptest.NewRecorder(),
		want: []string{
			"127.0.0.2:" + port,
			"127.0.0.1:" + port,
		},
	}
	recorder := &dialRecorder{
		refuse: refuse,
	}
	transport := newEgress(t, nil, edgekit.EgressPolicy{
		AllowedOrigins:  []string{"http://upstream.test:" + port},
		AllowedNetworks: prefixes("127.0.0.0/8"),
		Dialer:          recorder.dialer(time.Hour),
		Resolver: resolverFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
			got.lookupDeadline, _ = ctx.Deadline()

			return []netip.Addr{
				netip.MustParseAddr("127.0.0.2"),
				netip.MustParseAddr("127.0.0.1"),
			}, nil
		}),
	})

	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test:"+port),
			},
		},
		Transport: transport,
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{
				collector,
			},
		},
	})

	g.ServeHTTP(got.recorder, httptest.NewRequest(http.MethodGet, "/api/", nil))
	got.completion = receive(t, completed)
	got.attempts = recorder.attempts()
	recorder.mu.Lock()
	got.deadlines = slices.Clone(recorder.deadlines)
	recorder.mu.Unlock()

	return got
}

func TestDialFallbackConnectsToSecondAddress(t *testing.T) {
	got := dialFallback(t, func(address string) bool {
		return strings.HasPrefix(address, "127.0.0.2:")
	})
	w, c := got.recorder, got.completion

	if !slices.Equal(got.attempts, got.want) {
		t.Errorf("dialer attempts = %v, want %v", got.attempts, got.want)
	}

	for _, deadline := range got.deadlines {
		if got.lookupDeadline.IsZero() || !deadline.Equal(got.lookupDeadline) {
			t.Errorf("dialer attempt deadline = %v, want the deadline of the lookup %v", deadline, got.lookupDeadline)
		}
	}

	if w.Code != http.StatusOK || c.Failure != nil {
		t.Errorf("ServeHTTP() status = %d, RequestCompleted() = %+v; want 200 through the second address", w.Code, c)
	}
}

func TestDialFallbackFailsWithAllAttempts(t *testing.T) {
	got := dialFallback(t, refuseAll)
	w, c := got.recorder, got.completion

	if !slices.Equal(got.attempts, got.want) {
		t.Errorf("dialer attempts = %v, want %v", got.attempts, got.want)
	}

	for _, deadline := range got.deadlines {
		if got.lookupDeadline.IsZero() || !deadline.Equal(got.lookupDeadline) {
			t.Errorf("dialer attempt deadline = %v, want the deadline of the lookup %v", deadline, got.lookupDeadline)
		}
	}

	if w.Code != http.StatusBadGateway {
		t.Errorf("ServeHTTP() status = %d, want 502", w.Code)
	}

	var dialErr *edgekit.DialError
	if c.Failure == nil || !errors.As(c.Failure.Err, &dialErr) {
		t.Fatalf("RequestCompleted() = %+v, want a DialError", c)
	}

	if len(dialErr.Attempts) != 2 || dialErr.Attempts[0].Addr.String() != got.want[0] || dialErr.Attempts[1].Addr.String() != got.want[1] {
		t.Fatalf("DialError.Attempts = %+v, want %v", dialErr.Attempts, got.want)
	}

	for _, attempt := range dialErr.Attempts {
		if !errors.Is(attempt.Err, errRefused) {
			t.Errorf("DialAttempt.Err = %v, want the refusal", attempt.Err)
		}
	}

	if !errors.Is(dialErr, errRefused) {
		t.Errorf("DialError.Err = %v, want the error of the last attempt", dialErr.Err)
	}
}

func TestEgressCancellationAndDeadline(t *testing.T) {
	blockLookup := resolverFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		<-ctx.Done()

		return nil, ctx.Err()
	})
	blockDial := &net.Dialer{
		Timeout: 50 * time.Millisecond,
		ControlContext: func(ctx context.Context, _, _ string, _ syscall.RawConn) error {
			<-ctx.Done()

			return ctx.Err()
		},
	}

	for _, tc := range []struct {
		name     string
		dialer   *net.Dialer
		resolver edgekit.EgressResolver
	}{
		{
			name: "lookup deadline",
			dialer: &net.Dialer{
				Timeout: 50 * time.Millisecond,
			},
			resolver: blockLookup,
		},
		{
			name:     "dial deadline",
			dialer:   blockDial,
			resolver: answer("127.0.0.1"),
		},
		{
			name: "absolute deadline",
			dialer: &net.Dialer{
				Timeout:  time.Hour,
				Deadline: time.Now().Add(50 * time.Millisecond),
			},
			resolver: blockLookup,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := newEgress(t, nil, edgekit.EgressPolicy{
				AllowedOrigins:  []string{"http://upstream.test"},
				AllowedNetworks: prefixes("127.0.0.0/8"),
				Dialer:          tc.dialer,
				Resolver:        tc.resolver,
			})
			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
					},
				},
				Transport: transport,
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{
						collector,
					},
				},
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/", nil))
			c := receive(t, completed)

			var dialErr *edgekit.DialError
			if c.Failure == nil || !errors.As(c.Failure.Err, &dialErr) || !errors.Is(dialErr, context.DeadlineExceeded) {
				t.Errorf("RequestCompleted() = %+v, want a DialError with the deadline", c)
			}

			if w.Code != http.StatusGatewayTimeout {
				t.Errorf("ServeHTTP() status = %d, want 504", w.Code)
			}
		})
	}

	t.Run("request canceled", func(t *testing.T) {
		lookups := make(chan context.Context, 1)
		lookupEnded := make(chan error, 1)
		transport := newEgress(t, nil, edgekit.EgressPolicy{
			AllowedOrigins:  []string{"http://upstream.test"},
			AllowedNetworks: prefixes("127.0.0.0/8"),
			Dialer: &net.Dialer{
				Timeout: time.Hour,
			},
			Resolver: resolverFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
				lookups <- ctx
				<-ctx.Done()
				lookupEnded <- ctx.Err()

				return nil, ctx.Err()
			}),
		})
		collector, completed := completions()
		g := newGateway(t, edgekit.Config{
			Routes: []edgekit.Route{
				{
					Prefix:   "/api/",
					Upstream: static(t, "http://upstream.test"),
				},
			},
			Transport: transport,
			Hooks: edgekit.Hooks{
				RequestCompleted: []edgekit.RequestCompletedHook{
					collector,
				},
			},
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := serveAsync(g, httptest.NewRequest(http.MethodGet, "/api/", nil).WithContext(ctx))
		receive(t, lookups)
		cancel()

		// The transport returns the cancellation of the request and lets the
		// dial continue, so the error is no DialError.
		if w := receive(t, done); w.Code != 499 {
			t.Errorf("ServeHTTP() status = %d, want 499", w.Code)
		}

		c := receive(t, completed)
		var dialErr *edgekit.DialError
		if c.Failure == nil || !errors.Is(c.Failure.Err, context.Canceled) || errors.As(c.Failure.Err, &dialErr) {
			t.Errorf("RequestCompleted() = %+v, want the cancellation of the request", c)
		}

		// CloseIdleConnections stops a dial that no request waits for.
		transport.CloseIdleConnections()
		if err := receive(t, lookupEnded); !errors.Is(err, context.Canceled) {
			t.Errorf("resolver lookup ended with %v, want the cancellation of the dial", err)
		}
	})
}

func TestLookupErrorDoesNotDial(t *testing.T) {
	lookupErr := &net.DNSError{
		Err:        "no such host",
		Name:       "upstream.test",
		IsNotFound: true,
	}
	recorder := &dialRecorder{}
	transport := newEgress(t, nil, edgekit.EgressPolicy{
		AllowedOrigins:  []string{"http://upstream.test"},
		AllowedNetworks: prefixes("127.0.0.0/8"),
		Dialer:          recorder.dialer(time.Second),
		Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			return nil, lookupErr
		}),
	})

	_, err := roundTrip(t, transport, "http://upstream.test/")

	var dialErr *edgekit.DialError
	if !errors.As(err, &dialErr) || !errors.Is(dialErr.Err, lookupErr) || len(dialErr.Attempts) != 0 || len(recorder.attempts()) != 0 {
		t.Errorf("RoundTrip() error = %v, dialer attempts = %v; want the lookup error without attempts", err, recorder.attempts())
	}
}

func TestEgressPolicyIsCopied(t *testing.T) {
	recorder := &dialRecorder{
		refuse: refuseAll,
	}
	policy := edgekit.EgressPolicy{
		AllowedOrigins:  []string{"http://upstream.test"},
		AllowedNetworks: prefixes("127.0.0.0/8"),
		Dialer:          recorder.dialer(time.Hour),
		Resolver:        answer("127.0.0.1"),
	}
	transport := newEgress(t, nil, policy)

	policy.AllowedOrigins[0] = "http://other.test"
	policy.AllowedNetworks[0] = netip.MustParsePrefix("10.0.0.0/8")
	policy.Dialer.Timeout = time.Nanosecond

	_, err := roundTrip(t, transport, "http://upstream.test/")

	if !errors.Is(err, errRefused) {
		t.Errorf("RoundTrip() error = %v, want an attempt to the first configuration", err)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.deadlines) != 1 || time.Until(recorder.deadlines[0]) < time.Minute {
		t.Errorf("dialer deadlines = %v, want the copied timeout of 1h", recorder.deadlines)
	}
}

func TestConcurrentEgressChecks(t *testing.T) {
	transport := newEgress(t, nil, edgekit.EgressPolicy{
		AllowedOrigins:  []string{"http://upstream.test", "http://other.test"},
		AllowedNetworks: prefixes("10.0.0.0/8"),
		Dialer: (&dialRecorder{
			refuse: refuseAll,
		}).dialer(time.Second),
		Resolver: resolverFunc(func(_ context.Context, _, host string) ([]netip.Addr, error) {
			if host == "upstream.test" {
				return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
			}

			return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
		}),
	})

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			target, wantRefused := "http://upstream.test/", true
			switch i % 3 {
			case 1:
				target, wantRefused = "http://other.test/", false
			case 2:
				target, wantRefused = "http://denied.test/", false
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
			if err != nil {
				t.Errorf("http.NewRequestWithContext() error = %v", err)

				return
			}

			resp, err := transport.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}

			if errors.Is(err, errRefused) != wantRefused || !wantRefused && !errors.Is(err, edgekit.ErrEgressDenied) {
				t.Errorf("RoundTrip(%s) error = %v, want refused: %t", target, err, wantRefused)
			}
		})
	}

	wg.Wait()
}

func TestEgressPreservesHTTPAndTLSIdentityAndReusesConnections(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			var connections atomic.Int32
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.Host, "example.com:") || r.URL.RequestURI() != "/base/api/item?q=1" {
					t.Errorf("upstream got Host %q and URI %q, want example.com and /base/api/item?q=1", r.Host, r.URL.RequestURI())
				}

				if secure && (r.TLS == nil || r.TLS.ServerName != "example.com" || r.ProtoMajor != 2) {
					t.Errorf("upstream got TLS %+v and protocol %s, want SNI example.com on HTTP/2", r.TLS, r.Proto)
				}

				_, _ = io.WriteString(w, "upstream")
			}))
			upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}

			template := edgekit.NewTransport()
			if secure {
				upstream.EnableHTTP2 = true
				upstream.StartTLS()

				roots := x509.NewCertPool()
				roots.AddCert(upstream.Certificate())
				template.TLSClientConfig = &tls.Config{
					RootCAs:    roots,
					MinVersion: tls.VersionTLS12,
				}
			} else {
				upstream.Start()
			}
			t.Cleanup(upstream.Close)

			// The egress dialer replaces the dialer of the template.
			template.DialContext = func(context.Context, string, string) (net.Conn, error) {
				t.Error("template dialer was called, want only the egress dialer")

				return nil, errors.New("template dialer")
			}

			// A registered protocol does not use the egress dialer, so the
			// transport must not copy it.
			template.RegisterProtocol("http", roundTripperFunc(func(*http.Request) (*http.Response, error) {
				t.Error("registered protocol was called, want only the egress transport")

				return nil, errors.New("registered protocol")
			}))

			target := parseURL(t, upstream.URL)
			_, port, err := net.SplitHostPort(target.Host)
			if err != nil {
				t.Fatalf("net.SplitHostPort() error = %v", err)
			}

			target.Host = net.JoinHostPort("example.com", port)
			var lookups atomic.Int32
			transport := newEgress(t, template, edgekit.EgressPolicy{
				AllowedOrigins:  []string{target.String()},
				AllowedNetworks: prefixes("127.0.0.1/32"),
				Resolver: resolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
					lookups.Add(1)
					if network != "ip" || host != "example.com" {
						t.Errorf("resolver lookup %q %q, want ip example.com", network, host)
					}

					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				}),
			})

			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, target.String()+"/base"),
					},
				},
				Transport: transport,
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{
						collector,
					},
				},
			})

			for range 3 {
				w := httptest.NewRecorder()
				g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/item?q=1", nil))
				c := receive(t, completed)
				if w.Code != http.StatusOK || w.Body.String() != "upstream" || c.Failure != nil {
					t.Fatalf("ServeHTTP() response = %d %q, RequestCompleted() = %+v; want 200 upstream without a failure", w.Code, w.Body.String(), c)
				}
			}

			if connections.Load() != 1 || lookups.Load() != 1 {
				t.Errorf("upstream connections = %d, resolver lookups = %d; want 1 and 1", connections.Load(), lookups.Load())
			}

			// The origin check runs also while an allowed connection is in
			// the pool.
			if _, err := roundTrip(t, transport, "http://other.test/"); !errors.Is(err, edgekit.ErrEgressDenied) || lookups.Load() != 1 {
				t.Errorf("RoundTrip(http://other.test/) error = %v, resolver lookups = %d; want a denial before the pool", err, lookups.Load())
			}
		})
	}
}

func TestDialDeadlineDoesNotLimitResponseStreaming(t *testing.T) {
	release, finish := releaser(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		_ = http.NewResponseController(w).Flush()

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	t.Cleanup(upstream.Close)

	recorder := &dialRecorder{}
	transport := newEgress(t, nil, edgekit.EgressPolicy{
		AllowedOrigins:  []string{upstream.URL},
		AllowedNetworks: prefixes("127.0.0.1/32"),
		Dialer:          recorder.dialer(100 * time.Millisecond),
	})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() error = %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	t.Cleanup(func() {
		_ = resp.Body.Close()
	})

	reader := bufio.NewReader(resp.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("RoundTrip() response: first event = %q, %v; want data: first", line, err)
	}

	// The test shows that a deadline does not stop the stream, so it waits
	// until the deadline of the dial passed; there is no other event.
	recorder.mu.Lock()
	deadline := recorder.deadlines[0]
	recorder.mu.Unlock()
	<-time.After(time.Until(deadline) + 50*time.Millisecond)

	finish()
	if rest, err := io.ReadAll(reader); err != nil || string(rest) != "\ndata: last\n\n" {
		t.Errorf("RoundTrip() response: rest of the stream = %q, %v; want the last event", rest, err)
	}
}

func TestTLSCertificateVerificationIsNotDisabled(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("upstream with an untrusted certificate got the request")
	}))
	t.Cleanup(upstream.Close)

	transport := newEgress(t, nil, edgekit.EgressPolicy{
		AllowedOrigins:  []string{upstream.URL},
		AllowedNetworks: prefixes("127.0.0.1/32"),
	})

	_, err := roundTrip(t, transport, upstream.URL)

	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); !ok {
		t.Errorf("RoundTrip() error = %v, want a certificate verification failure", err)
	}
}

func TestEgressDenialIsAFailureAtCompletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream string
		resolver edgekit.EgressResolver
	}{
		{
			name:     "origin",
			upstream: "http://other.test",
			resolver: answer("192.0.2.1"),
		},
		{
			name:     "network",
			upstream: "http://upstream.test",
			resolver: answer("169.254.169.254"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := newEgress(t, nil, edgekit.EgressPolicy{
				AllowedOrigins:  []string{"http://upstream.test"},
				AllowedNetworks: prefixes("192.0.2.0/24"),
				Resolver:        tc.resolver,
			})

			unwound := false
			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, tc.upstream),
					},
				},
				Transport:        transport,
				SharedMiddleware: []edgekit.Middleware{unwinding(&unwound)},
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{
						edgekit.RequestCompletedHookFunc(func(ctx context.Context, rc *edgekit.RequestContext, c edgekit.Completion) {
							if !unwound {
								t.Error("RequestCompleted() ran before the middleware unwound")
							}

							collector(ctx, rc, c)
						}),
					},
				},
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/item?secret=value", nil))
			c := receive(t, completed)

			want := `{"type":"about:blank","title":"Bad Gateway","status":502}`
			if w.Code != http.StatusBadGateway || w.Body.String() != want {
				t.Errorf("ServeHTTP() response = %d %q, want %q", w.Code, w.Body.String(), want)
			}

			if c.Failure == nil || c.Failure.Phase != edgekit.PhaseRoundTrip || !errors.Is(c.Failure.Err, edgekit.ErrEgressDenied) || c.Aborted {
				t.Errorf("RequestCompleted() = %+v, want a denial in the round trip", c)
			}

			if c.SelectedUpstream == nil || c.SelectedUpstream.String() != tc.upstream {
				t.Errorf("RequestCompleted() SelectedUpstream = %v, want %s", c.SelectedUpstream, tc.upstream)
			}
		})
	}
}

func TestDialErrorMessage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{
			name:  "with a cause",
			cause: errRefused,
			want:  "dial upstream: " + errRefused.Error(),
		},
		{
			name:  "without a cause",
			cause: nil,
			want:  "dial upstream",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &edgekit.DialError{
				Err: tc.cause,
			}

			if got := err.Error(); got != tc.want {
				t.Errorf("DialError.Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDialErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		cancel bool
		status int
	}{
		{
			name:   "timeout",
			cause:  timeoutError{},
			status: http.StatusGatewayTimeout,
		},
		{
			name:   "deadline",
			cause:  context.DeadlineExceeded,
			status: http.StatusGatewayTimeout,
		},
		{
			name:   "cancellation while the client is gone",
			cause:  context.Canceled,
			cancel: true,
			status: 499,
		},
		{
			name:   "cancellation while the client is there",
			cause:  context.Canceled,
			status: http.StatusBadGateway,
		},
		{
			name:   "denial",
			cause:  edgekit.ErrEgressDenied,
			status: http.StatusBadGateway,
		},
		{
			name:   "refused",
			cause:  errRefused,
			status: http.StatusBadGateway,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialErr := &edgekit.DialError{
				Attempts: []edgekit.DialAttempt{
					{
						Addr: netip.MustParseAddrPort("192.0.2.1:80"),
						Err:  tc.cause,
					},
				},
				Err: tc.cause,
			}
			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
					},
				},
				Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return nil, dialErr
				}),
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{
						collector,
					},
				},
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/", nil).WithContext(ctx))
			c := receive(t, completed)

			var got *edgekit.DialError
			if w.Code != tc.status || c.Failure == nil || !errors.As(c.Failure.Err, &got) || got != dialErr {
				t.Errorf("ServeHTTP() status = %d, RequestCompleted() = %+v; want %d with the original DialError", w.Code, c, tc.status)
			}
		})
	}
}
