package edgekit

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// The dialer checks its authority again, as a second line behind the origin
// check. RoundTrip checks the origin first, so no request through the public
// API reaches the dialer with an authority that is not allowed.
func TestUnconfiguredDialAuthorityIsDenied(t *testing.T) {
	transport, err := NewEgressTransport(nil, EgressPolicy{
		AllowedOrigins: []string{"http://upstream.test"},
		AllowedNetworks: []netip.Prefix{
			netip.MustParsePrefix("127.0.0.0/8"),
		},
		Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			t.Error("unexpected DNS lookup")

			return nil, errors.New("unexpected DNS lookup")
		}),
	})
	if err != nil {
		t.Fatalf("NewEgressTransport() error = %v", err)
	}
	t.Cleanup(transport.CloseIdleConnections)

	for _, tc := range []struct {
		network string
		address string
	}{
		{
			network: "tcp",
			address: "other.test:80",
		},
		{
			network: "tcp",
			address: "upstream.test:443",
		},
		{
			network: "udp",
			address: "upstream.test:80",
		},
		{
			network: "tcp",
			address: "upstream.test",
		},
		{
			network: "tcp",
			address: "127.0.0.1:80",
		},
	} {
		t.Run(tc.network+" "+tc.address, func(t *testing.T) {
			conn, err := transport.dialContext(t.Context(), tc.network, tc.address)
			if conn != nil {
				_ = conn.Close()
			}

			var dialErr *DialError
			if conn != nil || !errors.As(err, &dialErr) || !errors.Is(err, ErrEgressDenied) || len(dialErr.Attempts) != 0 {
				t.Errorf("dialContext(%q, %q) = %v, %v; want a denial without attempts", tc.network, tc.address, conn, err)
			}
		})
	}
}

// resolverFunc is an EgressResolver from a function.
type resolverFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

// LookupNetIP calls f.
func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

// The base transport of an EgressTransport has no public surface, so only a
// white-box test can compare its settings with the template.
func TestEgressTransportSettings(t *testing.T) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	template := &http.Transport{
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    time.Second,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxIdleConns:           12,
		MaxIdleConnsPerHost:    4,
		MaxConnsPerHost:        6,
		IdleConnTimeout:        2 * time.Second,
		ResponseHeaderTimeout:  3 * time.Second,
		ExpectContinueTimeout:  4 * time.Second,
		MaxResponseHeaderBytes: 4096,
		WriteBufferSize:        2048,
		ReadBufferSize:         1024,
		ForceAttemptHTTP2:      true,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: 7,
		},
		Protocols: protocols,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			t.Error("the template dialer was called")

			return nil, errors.New("template dialer")
		},
	}

	transport, err := NewEgressTransport(template, EgressPolicy{
		AllowedOrigins: []string{"http://upstream.test"},
		AllowedNetworks: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.0/24"),
		},
	})
	if err != nil {
		t.Fatalf("NewEgressTransport() error = %v", err)
	}
	t.Cleanup(transport.CloseIdleConnections)

	got := transport.base
	if got == template || got.Proxy != nil || got.DialTLSContext != nil || got.DialContext == nil {
		t.Fatalf("NewEgressTransport() base transport = %+v, want a new transport without a bypass", got)
	}

	if got.TLSClientConfig == tlsConfig || got.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Errorf("NewEgressTransport() base TLSClientConfig = %+v, want a clone of the template", got.TLSClientConfig)
	}

	if got.TLSNextProto == nil || len(got.TLSNextProto) != 0 {
		t.Errorf("NewEgressTransport() base TLSNextProto = %v, want an empty map that disables HTTP/2", got.TLSNextProto)
	}

	if got.HTTP2 == template.HTTP2 || got.HTTP2.MaxConcurrentStreams != 7 {
		t.Errorf("NewEgressTransport() base HTTP2 = %+v, want a copy of the template", got.HTTP2)
	}

	if got.Protocols == template.Protocols || !got.Protocols.HTTP1() || got.Protocols.HTTP2() {
		t.Errorf("NewEgressTransport() base Protocols = %v, want a copy of the template", got.Protocols)
	}

	type transportSettings struct {
		TLSHandshakeTimeout    time.Duration
		DisableKeepAlives      bool
		DisableCompression     bool
		MaxIdleConns           int
		MaxIdleConnsPerHost    int
		MaxConnsPerHost        int
		IdleConnTimeout        time.Duration
		ResponseHeaderTimeout  time.Duration
		ExpectContinueTimeout  time.Duration
		MaxResponseHeaderBytes int64
		WriteBufferSize        int
		ReadBufferSize         int
		ForceAttemptHTTP2      bool
	}

	snapshot := func(tr *http.Transport) transportSettings {
		return transportSettings{
			TLSHandshakeTimeout:    tr.TLSHandshakeTimeout,
			DisableKeepAlives:      tr.DisableKeepAlives,
			DisableCompression:     tr.DisableCompression,
			MaxIdleConns:           tr.MaxIdleConns,
			MaxIdleConnsPerHost:    tr.MaxIdleConnsPerHost,
			MaxConnsPerHost:        tr.MaxConnsPerHost,
			IdleConnTimeout:        tr.IdleConnTimeout,
			ResponseHeaderTimeout:  tr.ResponseHeaderTimeout,
			ExpectContinueTimeout:  tr.ExpectContinueTimeout,
			MaxResponseHeaderBytes: tr.MaxResponseHeaderBytes,
			WriteBufferSize:        tr.WriteBufferSize,
			ReadBufferSize:         tr.ReadBufferSize,
			ForceAttemptHTTP2:      tr.ForceAttemptHTTP2,
		}
	}

	gotSettings, wantSettings := snapshot(got), snapshot(template)
	if gotSettings != wantSettings {
		t.Errorf("NewEgressTransport() copied settings = %+v, want %+v", gotSettings, wantSettings)
	}
}
