package egress

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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type completionFunc func(*edgekit.RequestContext, edgekit.Outcome)

func (f completionFunc) OnComplete(_ context.Context, rc *edgekit.RequestContext, outcome edgekit.Outcome) {
	f(rc, outcome)
}

func TestGatewayPreservesHTTPAndTLSIdentityAndReusesConnections(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			var connections, requests atomic.Int32
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if !strings.HasPrefix(r.Host, "example.com:") || r.URL.RequestURI() != "/base/api/item?q=1" {
					t.Errorf("HTTP identity or URI changed: %q %q", r.Host, r.URL.RequestURI())
				}
				if secure && (r.TLS == nil || r.TLS.ServerName != "example.com" || r.ProtoMajor != 2) {
					t.Errorf("lost TLS identity or standard HTTP/2 negotiation: tls=%+v protocol=%s", r.TLS, r.Proto)
				}
				_, _ = io.WriteString(w, "upstream")
			}))
			upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			if secure {
				upstream.EnableHTTP2 = true
				upstream.StartTLS()
			} else {
				upstream.Start()
			}
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			_, port, _ := net.SplitHostPort(target.Host)
			target.Host = net.JoinHostPort("example.com", port)
			origin := target.String()
			target.Path = "/base"
			template := http.DefaultTransport.(*http.Transport).Clone()
			template.ResponseHeaderTimeout = time.Second
			if secure {
				roots := x509.NewCertPool()
				roots.AddCert(upstream.Certificate())
				template.TLSClientConfig = &tls.Config{RootCAs: roots}
			}
			// None of these executable paths may be copied into the owned transport.
			template.Proxy = func(*http.Request) (*url.URL, error) {
				t.Error("template proxy callback bypassed direct egress")
				return nil, errors.New("proxy callback ran")
			}
			badDial := func(context.Context, string, string) (net.Conn, error) {
				t.Error("template dial callback bypassed validated IP dialing")
				return nil, errors.New("custom dial callback ran")
			}
			template.DialContext, template.DialTLSContext = badDial, badDial
			template.DialTLS = func(string, string) (net.Conn, error) { return badDial(context.Background(), "", "") }
			badProtocol := roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("registered protocol bypassed egress checks")
				return nil, errors.New("custom protocol ran")
			})
			template.RegisterProtocol("http", badProtocol)
			template.RegisterProtocol("https", badProtocol)
			template.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{
				"h2": func(string, *tls.Conn) http.RoundTripper { return badProtocol },
			}
			p, err := New(template, Config{
				AllowedOrigins: []string{origin}, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
				Dialer: &net.Dialer{Timeout: time.Second},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer p.CloseIdleConnections()
			var lookups atomic.Int32
			p.lookup = func(_ context.Context, network, host string) ([]netip.Addr, error) {
				lookups.Add(1)
				if network != "ip" || host != "example.com" {
					t.Errorf("unexpected lookup %q %q", network, host)
				}
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			var outcomes []edgekit.Outcome
			app, err := edgekit.NewWithConfig(edgekit.Config{
				Routes: []edgekit.Route{{Prefix: "/api/", Upstream: target}}, Transport: p,
				Extensions: []any{completionFunc(func(_ *edgekit.RequestContext, outcome edgekit.Outcome) { outcomes = append(outcomes, outcome) })},
			})
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				w := httptest.NewRecorder()
				app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/item?q=1", nil))
				if w.Code != 200 || w.Body.String() != "upstream" {
					t.Fatalf("response=%d %q outcomes=%+v", w.Code, w.Body.String(), outcomes)
				}
			}
			if requests.Load() != 3 || connections.Load() != 1 || lookups.Load() != 1 || len(outcomes) != 3 {
				t.Fatalf("requests=%d connections=%d lookups=%d completions=%d", requests.Load(), connections.Load(), lookups.Load(), len(outcomes))
			}
			for _, outcome := range outcomes {
				if outcome.Err != nil || outcome.Aborted {
					t.Fatalf("unexpected completion: %+v", outcome)
				}
			}
			// Origin checks must still run while an allowed connection is pooled.
			r, _ := http.NewRequest(http.MethodGet, "http://other.test/", nil)
			if _, err := p.RoundTrip(r); !errors.Is(err, ErrDenied) || lookups.Load() != 1 || requests.Load() != 3 {
				t.Fatalf("pooled connection bypassed origin check: %v", err)
			}
		})
	}
}

func TestConnectionBudgetDoesNotLimitResponseStreaming(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	defer upstream.Close()
	defer finish()
	p, err := New(http.DefaultTransport.(*http.Transport), Config{
		AllowedOrigins: []string{upstream.URL}, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		Dialer: &net.Dialer{Timeout: 50 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseIdleConnections()
	client := &http.Client{Transport: p, Timeout: 5 * time.Second}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("first event was buffered: %q, %v", line, err)
	}
	time.Sleep(3 * p.dialer.Timeout)
	finish()
	if rest, err := io.ReadAll(reader); err != nil || string(rest) != "\ndata: last\n\n" {
		t.Fatalf("connection budget interrupted streaming: %q, %v", rest, err)
	}
}

func TestTLSCertificateVerificationIsNotDisabled(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached upstream with an untrusted certificate")
	}))
	defer upstream.Close()
	p := testPolicy(t, []string{upstream.URL}, "127.0.0.1/32")
	p.dial = (&net.Dialer{Timeout: time.Second}).DialContext
	client := &http.Client{Transport: p, Timeout: 5 * time.Second}
	response, err := client.Get(upstream.URL)
	if response != nil {
		_ = response.Body.Close()
	}
	var untrusted x509.UnknownAuthorityError
	if !errors.As(err, &untrusted) {
		t.Fatalf("expected certificate verification failure: %v", err)
	}
}

func TestGatewayDenialRemainsAnExecutionErrorAfterMiddlewareUnwinds(t *testing.T) {
	for _, deniedBy := range []string{"origin", "network"} {
		t.Run(deniedBy, func(t *testing.T) {
			p := testPolicy(t, []string{"http://upstream.test"}, "192.0.2.0/24")
			target, _ := url.Parse("http://upstream.test")
			if deniedBy == "origin" {
				target.Host = "other.test"
			} else {
				p.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
				}
			}
			unwound, completions := false, 0
			app, err := edgekit.NewWithConfig(edgekit.Config{
				Routes: []edgekit.Route{{Prefix: "/api/", Upstream: target}}, Transport: p,
				Middlewares: []edgekit.Middleware{func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer func() { unwound = true }()
						next.ServeHTTP(w, r)
					})
				}},
				Extensions: []any{completionFunc(func(rc *edgekit.RequestContext, outcome edgekit.Outcome) {
					completions++
					if !unwound || !errors.Is(outcome.Err, ErrDenied) || outcome.Aborted {
						t.Errorf("lost denial or completion semantics: unwound=%v outcome=%+v", unwound, outcome)
					}
					if selected, ok := rc.Upstream(); !ok || selected.Host != target.Host {
						t.Error("denial changed selected-upstream metadata")
					}
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/item?secret=value", nil))
			if w.Code != 502 || w.Body.String() != "Bad Gateway\n" || completions != 1 {
				t.Fatalf("response=%d %q completions=%d", w.Code, w.Body.String(), completions)
			}
		})
	}
}

func TestExistingPoolIsNotSharedAndSettingsArePreserved(t *testing.T) {
	template := &http.Transport{
		TLSHandshakeTimeout: time.Second, MaxIdleConns: 12, MaxIdleConnsPerHost: 4,
		MaxConnsPerHost: 6, IdleConnTimeout: 2 * time.Second,
		ResponseHeaderTimeout: 3 * time.Second, ExpectContinueTimeout: 4 * time.Second,
		MaxResponseHeaderBytes: 4096, WriteBufferSize: 2048, ReadBufferSize: 1024,
		DisableCompression: true, DisableKeepAlives: true, ForceAttemptHTTP2: true,
	}
	p, err := New(template, Config{AllowedOrigins: []string{"http://upstream.test"}, AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseIdleConnections()
	got := p.base
	if got == template || got.Proxy != nil || got.DialTLS != nil || got.DialTLSContext != nil || got.TLSNextProto != nil ||
		got.TLSHandshakeTimeout != time.Second || got.MaxIdleConns != 12 || got.MaxIdleConnsPerHost != 4 ||
		got.MaxConnsPerHost != 6 || got.IdleConnTimeout != 2*time.Second || got.ResponseHeaderTimeout != 3*time.Second ||
		got.ExpectContinueTimeout != 4*time.Second || got.MaxResponseHeaderBytes != 4096 || got.WriteBufferSize != 2048 || got.ReadBufferSize != 1024 ||
		!got.DisableCompression || !got.DisableKeepAlives || !got.ForceAttemptHTTP2 {
		t.Fatalf("unexpected transport settings: %+v", got)
	}
}
