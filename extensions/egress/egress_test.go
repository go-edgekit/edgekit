package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testPolicy prevents tests from reaching external DNS or networks.
func testPolicy(t *testing.T, origins []string, networks ...string) *Transport {
	t.Helper()

	config := Config{
		AllowedOrigins: origins,
		Dialer: &net.Dialer{
			Timeout: time.Second,
		},
	}
	for _, network := range networks {
		config.AllowedNetworks = append(config.AllowedNetworks, netip.MustParsePrefix(network))
	}

	p, err := New(http.DefaultTransport.(*http.Transport), config)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(p.CloseIdleConnections)
	// Unit tests must never reach external DNS or networks.
	p.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Error("unexpected DNS lookup")

		return nil, errors.New("unexpected DNS lookup")
	}
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("unexpected connection attempt")

		return nil, errors.New("unexpected connection attempt")
	}

	return p
}

func TestInvalidConfiguration(t *testing.T) {
	for _, raw := range []string{
		"",
		"ftp://upstream.test",
		"http://user:secret@upstream.test",
		"http://upstream.test/path",
		"http://upstream.test?token=secret",
		"http://upstream.test?",
		"http://upstream.test#",
		"http://upstream.test#secret",
		"http://upstream.test:",
		"http://upstream.test:0",
		"http://upstream.test:65536",
		"http://upstream.test:bad",
		"http://upstream.test.",
		"http://*.test",
		"http://under_score.test",
		"http://-bad.test",
		"http://bad-.test",
		"http://[fe80::1%25eth0]",
		"http://é.test",
		"http://K.test",
		"http:upstream.test",
		"http:///missing",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := New(&http.Transport{}, Config{
				AllowedOrigins: []string{raw},
				AllowedNetworks: []netip.Prefix{
					netip.MustParsePrefix("0.0.0.0/0"),
				},
			})
			if err == nil {
				t.Fatal("accepted invalid origin")
			}

			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("configuration error exposed input: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		template *http.Transport
		config   Config
	}{
		{
			name: "nil template",
			config: Config{
				AllowedOrigins: []string{"http://upstream.test"},
				AllowedNetworks: []netip.Prefix{
					netip.MustParsePrefix("127.0.0.0/8"),
				},
			},
		},
		{
			name:     "empty origins",
			template: &http.Transport{},
			config: Config{
				AllowedNetworks: []netip.Prefix{
					netip.MustParsePrefix("127.0.0.0/8"),
				},
			},
		},
		{
			name:     "empty networks",
			template: &http.Transport{},
			config: Config{
				AllowedOrigins: []string{"http://upstream.test"},
			},
		},
		{
			name:     "invalid prefix",
			template: &http.Transport{},
			config: Config{
				AllowedOrigins: []string{"http://upstream.test"},
				AllowedNetworks: []netip.Prefix{
					{},
				},
			},
		},
		{
			name:     "mapped prefix",
			template: &http.Transport{},
			config: Config{
				AllowedOrigins: []string{"http://upstream.test"},
				AllowedNetworks: []netip.Prefix{
					netip.MustParsePrefix("::ffff:127.0.0.0/104"),
				},
			},
		},
		{
			name:     "zero timeout",
			template: &http.Transport{},
			config: Config{
				AllowedOrigins: []string{"http://upstream.test"},
				AllowedNetworks: []netip.Prefix{
					netip.MustParsePrefix("127.0.0.0/8"),
				},
				Dialer: &net.Dialer{},
			},
		},
		{
			name: "TLS identity override",
			template: &http.Transport{
				TLSClientConfig: &tls.Config{
					ServerName: "other.test",
				},
			},
			config: Config{
				AllowedOrigins: []string{"https://upstream.test"},
				AllowedNetworks: []netip.Prefix{
					netip.MustParsePrefix("127.0.0.0/8"),
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.template, tc.config); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestOriginNormalization(t *testing.T) {
	for _, tc := range []struct {
		input, origin, peer string
	}{
		{
			input:  "http://UPSTREAM.test",
			origin: "http://upstream.test:80",
			peer:   "upstream.test:80",
		},
		{
			input:  "https://upstream.test:0443/",
			origin: "https://upstream.test:443",
			peer:   "upstream.test:443",
		},
		{
			input:  "http://[2001:0db8::1]:8080",
			origin: "http://[2001:db8::1]:8080",
			peer:   "[2001:db8::1]:8080",
		},
		{
			input:  "http://[::ffff:127.0.0.1]",
			origin: "http://127.0.0.1:80",
			peer:   "127.0.0.1:80",
		},
	} {
		t.Run(tc.input, func(t *testing.T) {
			u, err := url.Parse(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			origin, peer, err := destination(u)
			if err != nil {
				t.Fatalf("get destination: %v", err)
			}

			if origin != tc.origin {
				t.Errorf("origin = %q, want %q", origin, tc.origin)
			}

			if peer != tc.peer {
				t.Errorf("peer = %q, want %q", peer, tc.peer)
			}
		})
	}
}

// countedBody records whether a denied request body was read or closed.
type countedBody struct {
	reads  int
	closes int
}

// Read records the call without supplying body data.
func (b *countedBody) Read([]byte) (int, error) {
	b.reads++

	return 0, io.EOF
}

// Close records the call.
func (b *countedBody) Close() error {
	b.closes++

	return nil
}

func TestDeniedRequestClosesBodyWithoutDelegation(t *testing.T) {
	p := testPolicy(t, []string{"http://upstream.test"}, "127.0.0.0/8")
	for _, raw := range []string{
		"http://other.test",
		"https://upstream.test",
		"http://upstream.test:81",
		"http://upstream.test.evil.test",
		"http://user:secret@upstream.test",
		"http://upstream.test#secret",
		"ftp://upstream.test",
	} {
		t.Run(raw, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodPost, raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			body := &countedBody{}
			r.Body = body
			response, err := p.RoundTrip(r)
			if response != nil || !errors.Is(err, ErrDenied) {
				t.Fatalf("RoundTrip = %v, %v; want nil, ErrDenied", response, err)
			}

			if body.reads != 0 || body.closes != 1 {
				t.Fatalf("body reads = %d, closes = %d; want 0, 1", body.reads, body.closes)
			}
		})
	}
	for _, tc := range []struct {
		name string
		url  *url.URL
	}{
		{
			name: "nil URL",
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := p.RoundTrip(&http.Request{URL: tc.url})
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}

			if response != nil || !errors.Is(err, ErrDenied) {
				t.Fatalf("RoundTrip = %v, %v; want nil, ErrDenied", response, err)
			}
		})
	}
}

func TestAddressClassification(t *testing.T) {
	p := testPolicy(t, []string{"http://upstream.test"}, "10.0.0.0/8", "2001:db8::/32", "127.0.0.1/32")
	for _, tc := range []struct {
		ip      string
		allowed bool
	}{
		{
			ip:      "10.2.3.4",
			allowed: true,
		},
		{
			ip:      "::ffff:10.2.3.4",
			allowed: true,
		},
		{
			ip:      "2001:db8::10",
			allowed: true,
		},
		{
			ip:      "127.0.0.1",
			allowed: true,
		},
		{
			ip: "127.0.0.2",
		},
		{
			ip: "169.254.169.254",
		},
		{
			ip: "192.168.1.1",
		},
		{
			ip: "8.8.8.8",
		},
		{
			ip: "fc00::1",
		},
		{
			ip: "::1",
		},
		{
			ip: "::ffff:169.254.169.254",
		},
		{
			ip: "fe80::1%eth0",
		},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			if got := p.allowedIP(netip.MustParseAddr(tc.ip)); got != tc.allowed {
				t.Errorf("allowedIP(%s) = %v, want %v", tc.ip, got, tc.allowed)
			}
		})
	}
	all := testPolicy(t, []string{"http://upstream.test"}, "0.0.0.0/0", "::/0")
	for _, ip := range []netip.Addr{
		{},
		netip.MustParseAddr("0.0.0.0"),
		netip.MustParseAddr("::"),
		netip.MustParseAddr("224.0.0.1"),
		netip.MustParseAddr("ff02::1"),
		netip.MustParseAddr("::ffff:0.0.0.0"),
	} {
		t.Run(ip.String(), func(t *testing.T) {
			if all.allowedIP(ip) {
				t.Errorf("accepted non-unicast address %s", ip)
			}
		})
	}
	v6 := testPolicy(t, []string{"http://upstream.test"}, "::/0")
	if v6.allowedIP(netip.MustParseAddr("::ffff:127.0.0.1")) {
		t.Fatal("mapped IPv4 bypassed IPv4 network policy")
	}
}

func TestLookupAnswersAreValidatedBeforeAnyDial(t *testing.T) {
	for _, ips := range [][]string{
		{},
		{
			"127.0.0.1",
		},
		{
			"192.0.2.1",
			"127.0.0.1",
		},
		{
			"127.0.0.1",
			"192.0.2.1",
		},
		{
			"::ffff:127.0.0.1",
		},
	} {
		t.Run(strings.Join(ips, ","), func(t *testing.T) {
			p := testPolicy(t, []string{"http://upstream.test"}, "192.0.2.0/24")
			p.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
				var answer []netip.Addr
				for _, ip := range ips {
					answer = append(answer, netip.MustParseAddr(ip))
				}
				return answer, nil
			}
			if _, err := p.dialContext(context.Background(), "tcp", "upstream.test:80"); !errors.Is(err, ErrDenied) {
				t.Fatalf("unsafe lookup was not denied: %v", err)
			}
		})
	}
}

func TestDialUsesValidatedLiteralAndPreservesPort(t *testing.T) {
	for _, tc := range []struct {
		origin  string
		address string
		ip      string
		want    string
	}{
		{
			origin:  "http://upstream.test:8080",
			address: "upstream.test:8080",
			ip:      "192.0.2.10",
			want:    "192.0.2.10:8080",
		},
		{
			origin:  "http://UPSTREAM.test:08080",
			address: "UPSTREAM.test:08080",
			ip:      "192.0.2.10",
			want:    "192.0.2.10:8080",
		},
		{
			origin:  "https://upstream.test",
			address: "upstream.test:443",
			ip:      "2001:db8::1",
			want:    "[2001:db8::1]:443",
		},
		{
			origin:  "http://[::ffff:192.0.2.10]",
			address: "[::ffff:192.0.2.10]:80",
			want:    "192.0.2.10:80",
		},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			p := testPolicy(t, []string{tc.origin}, "192.0.2.0/24", "2001:db8::/32")
			if tc.ip != "" {
				p.lookup = func(_ context.Context, network, host string) ([]netip.Addr, error) {
					if network != "ip" || host != "upstream.test" {
						t.Errorf("lookup = %q %q", network, host)
					}
					return []netip.Addr{netip.MustParseAddr(tc.ip)}, nil
				}
			}
			client, server := net.Pipe()
			defer server.Close()
			p.dial = func(_ context.Context, network, address string) (net.Conn, error) {
				if network != "tcp" || address != tc.want {
					t.Errorf("dial = %q %q, want literal %q", network, address, tc.want)
				}
				return client, nil
			}
			conn, err := p.dialContext(context.Background(), "tcp", tc.address)
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		})
	}
}

func TestResolutionChangesCannotReuseHostnameAtDial(t *testing.T) {
	p := testPolicy(t, []string{"http://upstream.test"}, "192.0.2.0/24")
	lookups, dials := 0, 0
	failure := errors.New("connection failed")
	p.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	p.dial = func(_ context.Context, _, address string) (net.Conn, error) {
		dials++
		if address != "192.0.2.10:80" {
			t.Errorf("dialed unvalidated destination %q", address)
		}
		return nil, failure
	}
	if _, err := p.dialContext(context.Background(), "tcp", "upstream.test:80"); !errors.Is(err, failure) {
		t.Fatalf("lost connection error: %v", err)
	}
	if _, err := p.dialContext(context.Background(), "tcp", "upstream.test:80"); !errors.Is(err, ErrDenied) || lookups != 2 || dials != 1 {
		t.Fatalf("new lookup bypassed restrictions: lookups=%d dials=%d err=%v", lookups, dials, err)
	}
}

func TestDialFallbackUsesOnlyValidatedAddressesAndOneBudget(t *testing.T) {
	p := testPolicy(t, []string{"http://upstream.test"}, "192.0.2.0/24")
	var deadline time.Time
	p.lookup = func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		deadline, _ = ctx.Deadline()
		return []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}, nil
	}
	var attempted []string
	client, server := net.Pipe()
	defer server.Close()
	p.dial = func(ctx context.Context, _, address string) (net.Conn, error) {
		gotDeadline, _ := ctx.Deadline()
		if !gotDeadline.Equal(deadline) {
			t.Error("connection attempt reset the resolution/dial budget")
		}
		attempted = append(attempted, address)
		if len(attempted) == 1 {
			return nil, errors.New("first address unavailable")
		}
		return client, nil
	}
	conn, err := p.dialContext(context.Background(), "tcp", "upstream.test:80")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	want := []string{
		"192.0.2.1:80",
		"192.0.2.2:80",
	}
	if !slices.Equal(attempted, want) {
		t.Errorf("connection attempts = %v, want %v", attempted, want)
	}

	if deadline.IsZero() {
		t.Error("connection budget has no deadline")
	}
}

func TestCancellationAndConnectionBudget(t *testing.T) {
	for _, phase := range []string{
		"already canceled",
		"lookup",
		"dial",
		"lookup budget",
		"dial budget",
		"absolute deadline",
	} {
		t.Run(phase, func(t *testing.T) {
			p := testPolicy(t, []string{"http://upstream.test"}, "192.0.2.0/24")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			if strings.Contains(phase, "budget") {
				p.dialer.Timeout = 50 * time.Millisecond
			}
			if phase == "absolute deadline" {
				p.dialer.Deadline = time.Now().Add(50 * time.Millisecond)
			}
			block := func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}
			if phase != "already canceled" {
				p.lookup = func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
					if strings.HasPrefix(phase, "lookup") || phase == "absolute deadline" {
						return nil, block(ctx)
					}
					return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
				}
				p.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
					return nil, block(ctx)
				}
			} else {
				cancel()
			}
			result := make(chan error, 1)
			go func() {
				_, err := p.dialContext(ctx, "tcp", "upstream.test:80")
				result <- err
			}()
			if phase == "lookup" || phase == "dial" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("phase did not start")
				}
				cancel()
			}
			select {
			case err := <-result:
				want := context.Canceled
				if strings.Contains(phase, "budget") || phase == "absolute deadline" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) {
					t.Fatalf("cancellation error = %v, want %v", err, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("resolution/dial did not terminate")
			}
		})
	}
}

func TestLookupErrorDoesNotDial(t *testing.T) {
	p := testPolicy(t, []string{"http://upstream.test"}, "192.0.2.0/24")
	failure := &net.DNSError{Err: "no such host", Name: "upstream.test", IsNotFound: true}
	p.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, failure
	}
	if _, err := p.dialContext(context.Background(), "tcp", "upstream.test:80"); !errors.Is(err, failure) {
		t.Fatalf("lookup error was lost: %v", err)
	}
}

func TestUnconfiguredDialAuthorityIsDenied(t *testing.T) {
	p := testPolicy(t, []string{"http://upstream.test"}, "192.0.2.0/24")
	for _, address := range []string{
		"other.test:80",
		"upstream.test:81",
		"127.0.0.1:80",
		"upstream.test",
		"upstream.test:bad",
	} {
		if _, err := p.dialContext(context.Background(), "tcp", address); !errors.Is(err, ErrDenied) {
			t.Errorf("accepted dial authority %q: %v", address, err)
		}
	}
	if _, err := p.dialContext(context.Background(), "udp", "upstream.test:80"); !errors.Is(err, ErrDenied) {
		t.Errorf("accepted non-TCP network: %v", err)
	}
}

func TestConfigurationIsCopied(t *testing.T) {
	origins := []string{"http://upstream.test"}
	networks := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	dialer := &net.Dialer{Timeout: time.Second}
	template := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		MaxConnsPerHost: 3,
	}
	p, err := New(template, Config{
		AllowedOrigins:  origins,
		AllowedNetworks: networks,
		Dialer:          dialer,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseIdleConnections()
	origins[0] = "http://other.test"
	networks[0] = netip.MustParsePrefix("127.0.0.0/8")
	dialer.Timeout = time.Hour
	template.MaxConnsPerHost = 100
	template.TLSClientConfig.MinVersion = tls.VersionTLS13
	if _, ok := p.origins["http://upstream.test:80"]; !ok {
		t.Error("policy did not keep the configured origin")
	}

	if _, ok := p.origins["http://other.test:80"]; ok {
		t.Error("policy retained the caller's origin list")
	}

	if !p.allowedIP(netip.MustParseAddr("192.0.2.1")) {
		t.Error("policy did not keep the configured network")
	}

	if p.allowedIP(netip.MustParseAddr("127.0.0.1")) {
		t.Error("policy retained the caller's network list")
	}

	if p.dialer.Timeout != time.Second {
		t.Errorf("dial timeout = %v, want 1s", p.dialer.Timeout)
	}

	if p.base.MaxConnsPerHost != 3 {
		t.Errorf("connection limit = %d, want 3", p.base.MaxConnsPerHost)
	}

	if p.base.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("minimum TLS version = %d, want TLS 1.2", p.base.TLSClientConfig.MinVersion)
	}
}

func TestConcurrentAddressChecks(t *testing.T) {
	p := testPolicy(t, []string{"http://upstream.test"}, "192.0.2.0/24")
	p.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
	}
	failure := errors.New("connection failed")
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, failure
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.dialContext(context.Background(), "tcp", "upstream.test:80"); !errors.Is(err, failure) {
				t.Errorf("concurrent dial error = %v", err)
			}
		}()
	}
	wg.Wait()
}
