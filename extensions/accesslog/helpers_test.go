package accesslog_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/accesslog"
)

// newGateway returns a gateway for cfg and closes its idle connections when
// the test ends.
func newGateway(t *testing.T, cfg edgekit.Config) *edgekit.Gateway {
	t.Helper()

	g, err := edgekit.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(g.CloseIdleConnections)

	return g
}

// static returns a static selector for rawURL.
func static(tb testing.TB, rawURL string) edgekit.UpstreamSelector {
	tb.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		tb.Fatalf("parse URL: %v", err)
	}

	selector, err := edgekit.StaticUpstream(u)
	if err != nil {
		tb.Fatalf("StaticUpstream: %v", err)
	}

	return selector
}

// apiRoute returns the route "/api/" to upstreamURL.
func apiRoute(tb testing.TB, upstreamURL string) edgekit.Route {
	tb.Helper()

	return edgekit.Route{
		Prefix:   "/api/",
		Upstream: static(tb, upstreamURL),
	}
}

// collector is a Sink that keeps the records and signals each one.
type collector struct {
	t       *testing.T
	mu      sync.Mutex
	records []accesslog.Record
	signal  chan accesslog.Record
}

// newCollector returns a collector with room for n signals. A record more
// fails t, so a wrong record count does not block the request until the
// test times out.
func newCollector(t *testing.T, n int) *collector {
	t.Helper()

	return &collector{
		t:      t,
		signal: make(chan accesslog.Record, n),
	}
}

// Report keeps r and signals it.
func (c *collector) Report(r accesslog.Record) {
	c.mu.Lock()
	c.records = append(c.records, r)
	c.mu.Unlock()

	select {
	case c.signal <- r:
	default:
		c.t.Errorf("collector got more than %d records", cap(c.signal))
	}
}

// all returns a copy of the records.
func (c *collector) all() []accesslog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]accesslog.Record(nil), c.records...)
}

// newRecorder returns a Recorder for cfg, or stops the test.
func newRecorder(t *testing.T, cfg accesslog.Config) *accesslog.Recorder {
	t.Helper()

	recorder, err := accesslog.New(cfg)
	if err != nil {
		t.Fatalf("accesslog.New: %v", err)
	}

	return recorder
}

// receive waits at most 5s for a value from ch, or returns the context error.
func receive[T any](ctx context.Context, ch <-chan T) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	select {
	case value := <-ch:
		return value, nil
	case <-ctx.Done():
		var zero T

		return zero, ctx.Err()
	}
}

// validRequestID reports whether id has the form of a generated request ID.
func validRequestID(id string) bool {
	decoded, err := hex.DecodeString(id)

	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}

// testCertificate is a self-signed certificate for 127.0.0.1, made once.
var testCertificate = sync.OnceValues(func() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{
			net.IPv4(127, 0, 0, 1),
		},
		KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.Certificate{
		Certificate: [][]byte{
			der,
		},
		PrivateKey: key,
		Leaf:       leaf,
	}, nil
})

// testServer is a started edgekit.Server with TLS, so clients can use
// HTTP/1.1 or HTTP/2.
type testServer struct {
	server *edgekit.Server
	addr   string
	cert   tls.Certificate
}

// startServer serves g with TLS on a new local listener and closes the
// server when the test ends.
func startServer(t *testing.T, g *edgekit.Gateway) *testServer {
	t.Helper()

	cert, err := testCertificate()
	if err != nil {
		t.Fatalf("make certificate: %v", err)
	}

	server, err := edgekit.NewServer(g, edgekit.ServerConfig{
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{
				cert,
			},
			MinVersion: tls.VersionTLS12,
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	served := make(chan struct{})
	go func() {
		defer close(served)

		_ = server.Serve(l)
	}()

	t.Cleanup(func() {
		_ = server.Close()
		<-served
	})

	return &testServer{
		server: server,
		addr:   l.Addr().String(),
		cert:   cert,
	}
}

// url returns the URL of path on the server.
func (ts *testServer) url(path string) string {
	return "https://" + ts.addr + path
}

// clientTLS returns a client TLS configuration that trusts the test
// certificate and offers nextProtos.
func (ts *testServer) clientTLS(nextProtos ...string) *tls.Config {
	roots := x509.NewCertPool()
	roots.AddCert(ts.cert.Leaf)

	return &tls.Config{
		RootCAs:    roots,
		NextProtos: nextProtos,
		MinVersion: tls.VersionTLS12,
		ServerName: "127.0.0.1",
	}
}

// client returns a client that uses HTTP/2 when http2 is true, and HTTP/1.1
// otherwise.
func (ts *testServer) client(t *testing.T, http2 bool) *http.Client {
	t.Helper()

	transport := &http.Transport{
		DisableCompression: true,
		TLSClientConfig:    ts.clientTLS(),
		Protocols:          new(http.Protocols),
	}
	if http2 {
		transport.Protocols.SetHTTP2(true)
	} else {
		transport.Protocols.SetHTTP1(true)
	}

	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}
}

// dial opens an HTTP/1.1 TLS connection to the server with a 5s deadline.
func (ts *testServer) dial(t *testing.T) net.Conn {
	t.Helper()

	dialer := &tls.Dialer{
		Config: ts.clientTLS("http/1.1"),
	}

	conn, err := dialer.DialContext(t.Context(), "tcp", ts.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}

	return conn
}

// shutdown calls Shutdown with a 5s limit.
func (ts *testServer) shutdown(t *testing.T) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	return ts.server.Shutdown(ctx)
}

// protocols are the protocol cases of tests that run for both protocols.
var protocols = []struct {
	name  string
	http2 bool
}{
	{
		name:  "http1",
		http2: false,
	},
	{
		name:  "http2",
		http2: true,
	},
}
