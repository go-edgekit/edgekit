package edgekit_test

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-edgekit/edgekit"
)

// transportRequest is what a test transport received.
type transportRequest struct {
	host    string
	url     string
	header  http.Header
	body    string
	readErr error
}

// recordingTransport returns a transport that sends each request to the
// returned channel and answers 200 "ok".
func recordingTransport(t *testing.T) (http.RoundTripper, <-chan transportRequest) {
	t.Helper()

	ch := make(chan transportRequest, 1)
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		var (
			body    []byte
			readErr error
		)
		if r.Body != nil {
			body, readErr = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}

		ch <- transportRequest{
			host:    r.URL.Host,
			url:     r.URL.String(),
			header:  r.Header.Clone(),
			body:    string(body),
			readErr: readErr,
		}
		if readErr != nil {
			return nil, readErr
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})

	return transport, ch
}

func TestBeforeUpstreamHookChangesReachTransport(t *testing.T) {
	transport, sent := recordingTransport(t)

	type view struct {
		method        string
		url           string
		host          string
		contentLength int64
	}
	var (
		seen view
		kept *edgekit.OutboundRequest
	)
	hooks := beforeUpstreamFunc(func(_ context.Context, _ *edgekit.RequestContext, out *edgekit.OutboundRequest) error {
		seen = view{
			method:        out.Method(),
			url:           out.URL().String(),
			host:          out.Host(),
			contentLength: out.ContentLength(),
		}

		// The URL is a copy, so the hook cannot change the destination.
		changed := out.URL()
		changed.Host = "attacker.test"
		changed.Path = "/other"
		out.Header().Set("X-Signature", "signed")
		kept = out

		return nil
	})
	var inboundSignature []string
	routeMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			inboundSignature = r.Header.Values("X-Signature")
		})
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:     "/api/",
			Upstream:   static(t, "http://upstream.test/base"),
			Middleware: []edgekit.Middleware{routeMiddleware},
		}},
		Hooks:     []any{hooks},
		Transport: transport,
	})

	r := httptest.NewRequest(http.MethodPost, "/api/caf%C3%A9?q=a%20b&r=1", strings.NewReader("payload"))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	want := view{
		method:        http.MethodPost,
		url:           "http://upstream.test/base/api/caf%C3%A9?q=a%20b&r=1",
		host:          "upstream.test",
		contentLength: int64(len("payload")),
	}
	if seen != want {
		t.Errorf("hook saw %+v, want %+v", seen, want)
	}

	got := receive(t, sent)
	if got.readErr != nil {
		t.Fatalf("read outbound body: %v", got.readErr)
	}

	if got.url != want.url || got.header.Get("X-Signature") != "signed" || got.body != "payload" {
		t.Errorf("transport got %s, signature %q, body %q; want %s, the hook header, and the streamed body", got.url, got.header.Get("X-Signature"), got.body, want.url)
	}

	if len(inboundSignature) != 0 {
		t.Errorf("inbound request has X-Signature %q, want no change to the inbound request", inboundSignature)
	}

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	if !panics(func() { kept.Method() }) {
		t.Error("OutboundRequest is usable after the hook returned")
	}
}

func TestConfiguredTransportWithAndWithoutOutboundHooks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withHooks bool
		fail      bool
	}{
		{
			name: "no hooks, success",
		},
		{
			name: "no hooks, failure",
			fail: true,
		},
		{
			name:      "hooks, success",
			withHooks: true,
		},
		{
			name:      "hooks, failure",
			withHooks: true,
			fail:      true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				calls      atomic.Int32
				hookHeader atomic.Bool
			)
			failure := errors.New("private transport detail")
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				hookHeader.Store(r.Header.Get("X-Hook") == "ran")
				if tc.fail {
					return nil, failure
				}

				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("ok")),
				}, nil
			})
			completionHooks, completed := completions()
			providers := []any{completionHooks}
			if tc.withHooks {
				providers = append(providers, beforeUpstreamFunc(func(_ context.Context, _ *edgekit.RequestContext, out *edgekit.OutboundRequest) error {
					out.Header().Set("X-Hook", "ran")

					return nil
				}))
			}
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, "http://upstream.test"),
				}},
				Hooks:     providers,
				Transport: transport,
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			wantStatus, wantBody := http.StatusOK, "ok"
			if tc.fail {
				wantStatus, wantBody = http.StatusBadGateway, "Bad Gateway\n"
			}
			if calls.Load() != 1 || w.Code != wantStatus || w.Body.String() != wantBody {
				t.Errorf("calls = %d, response = %d %q; want 1, %d %q", calls.Load(), w.Code, w.Body.String(), wantStatus, wantBody)
			}

			if hookHeader.Load() != tc.withHooks {
				t.Errorf("transport got the hook header = %v, want %v", hookHeader.Load(), tc.withHooks)
			}

			c := receive(t, completed)
			switch {
			case tc.fail && (c.Failure == nil || c.Failure.Phase != edgekit.PhaseRoundTrip || !errors.Is(c.Failure.Err, failure)):
				t.Errorf("completion failure = %+v, want the transport error in the round trip", c.Failure)
			case !tc.fail && c.Failure != nil:
				t.Errorf("completion failure = %+v, want none", c.Failure)
			}
		})
	}
}

func TestBeforeUpstreamHeaderRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inbound http.Header
		want    http.Header
	}{
		{
			name: "plain request",
			want: http.Header{},
		},
		{
			name: "client accepts trailers",
			inbound: http.Header{
				"Te": {"trailers"},
			},
			want: http.Header{
				"Te": {"trailers"},
			},
		},
		{
			name: "upgrade request",
			inbound: http.Header{
				"Connection": {"Upgrade"},
				"Upgrade":    {"websocket"},
			},
			want: http.Header{
				"Connection": {"Upgrade"},
				"Upgrade":    {"websocket"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport, sent := recordingTransport(t)
			hooks := beforeUpstreamFunc(func(_ context.Context, _ *edgekit.RequestContext, out *edgekit.OutboundRequest) error {
				header := out.Header()
				header.Set("X-User", "verified")
				header.Set("Connection", "X-User")
				header["connection"] = []string{"X-Signature"}
				header.Del("Upgrade")
				header["Te"] = []string{"gzip"}
				header.Set("Keep-Alive", "timeout=5")
				header.Set("Proxy-Connection", "keep-alive")
				header.Set("Proxy-Authorization", "Basic c2VjcmV0")
				header.Set("Host", "attacker.test")
				header.Set("Content-Length", "99")
				header["Transfer-Encoding"] = []string{"chunked"}
				header.Set("Trailer", "X-Signature")
				header.Set("X-Signature", "signed")

				return nil
			})
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, "http://upstream.test"),
				}},
				ManagedHeaders: []string{"X-User"},
				Hooks:          []any{hooks},
				Transport:      transport,
			})

			r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			maps.Copy(r.Header, tc.inbound)
			g.ServeHTTP(httptest.NewRecorder(), r)

			got := receive(t, sent)
			if got.readErr != nil {
				t.Fatalf("read outbound body: %v", got.readErr)
			}

			for _, name := range []string{
				"Connection",
				"connection",
				"Upgrade",
				"Te",
				"Keep-Alive",
				"Proxy-Connection",
				"Proxy-Authorization",
				"Host",
				"Content-Length",
				"Transfer-Encoding",
				"Trailer",
			} {
				if !slices.Equal(got.header[name], tc.want[name]) {
					t.Errorf("transport got %s %q, want %q", name, got.header[name], tc.want[name])
				}
			}

			// A managed header that the hook set stays, and no Connection
			// token can remove it.
			if got.header.Get("X-User") != "verified" || got.header.Get("X-Signature") != "signed" {
				t.Errorf("transport got X-User %q, X-Signature %q; want the values of the hook", got.header.Get("X-User"), got.header.Get("X-Signature"))
			}

			if got.host != "upstream.test" {
				t.Errorf("transport host = %q, want the selected upstream", got.host)
			}
		})
	}
}

func TestBeforeUpstreamInvalidHeaderFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
		valid bool
	}{
		{
			name:  "underscore alias",
			key:   "X_User",
			value: "admin",
		},
		{
			name:  "dot in name",
			key:   "X.User",
			value: "admin",
		},
		{
			name:  "space in name",
			key:   "X User",
			value: "admin",
		},
		{
			name:  "empty name",
			key:   "",
			value: "admin",
		},
		{
			name:  "line feed in value",
			key:   "X-Signature",
			value: "a\nX-Injected: 1",
		},
		{
			name:  "carriage return in value",
			key:   "X-Signature",
			value: "a\rb",
		},
		{
			name:  "NUL in value",
			key:   "X-Signature",
			value: "a\x00b",
		},
		{
			name:  "DEL in value",
			key:   "X-Signature",
			value: "a\x7fb",
		},
		{
			name:  "other control character in value",
			key:   "X-Signature",
			value: "a\x01b",
		},
		{
			name:  "tab in value",
			key:   "X-Signature",
			value: "a\tb",
			valid: true,
		},
		{
			name:  "non-ASCII byte in value",
			key:   "X-Signature",
			value: "caf\xc3\xa9",
			valid: true,
		},
		{
			name:  "letters, digits, and hyphen",
			key:   "X-Signature-2",
			value: "",
			valid: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)

				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       http.NoBody,
					Request:    r,
				}, nil
			})
			hooks := beforeUpstreamFunc(func(_ context.Context, _ *edgekit.RequestContext, out *edgekit.OutboundRequest) error {
				out.Header()[tc.key] = []string{tc.value}

				return nil
			})
			var unwound, unwoundAtCompletion bool
			var completion edgekit.Completion
			completionHooks := requestCompletedFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
				unwoundAtCompletion = unwound
				completion = c
			})
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:     "/api/",
					Upstream:   static(t, "http://upstream.test"),
					Middleware: []edgekit.Middleware{unwinding(&unwound)},
				}},
				Hooks:     []any{hooks, completionHooks},
				Transport: transport,
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if tc.valid {
				if w.Code != http.StatusOK || calls.Load() != 1 || completion.Failure != nil {
					t.Errorf("response = %d, transport calls = %d, failure = %+v; want 200, 1, none", w.Code, calls.Load(), completion.Failure)
				}

				return
			}

			if w.Code != http.StatusInternalServerError || w.Body.String() != "Internal Server Error\n" || calls.Load() != 0 {
				t.Errorf("response = %d %q, transport calls = %d; want 500 without details and no call", w.Code, w.Body.String(), calls.Load())
			}

			failure := completion.Failure
			if failure == nil || failure.Phase != edgekit.PhaseBeforeUpstream || !unwoundAtCompletion {
				t.Fatalf("completion failure = %+v, after unwinding = %v; want a BeforeUpstream failure after unwinding", failure, unwoundAtCompletion)
			}

			if msg := failure.Err.Error(); (tc.key != "" && strings.Contains(msg, tc.key)) || strings.Contains(msg, tc.value) {
				t.Errorf("failure error %q contains the header name or value", msg)
			}
		})
	}
}

func TestOutboundHostIsTheValueThatTheUpstreamGets(t *testing.T) {
	for _, protocol := range []string{
		"HTTP/1.1",
		"HTTP/2 without TLS",
	} {
		t.Run(protocol, func(t *testing.T) {
			type got struct {
				host  string
				proto int
			}
			received := make(chan got, 1)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				received <- got{
					host:  r.Host,
					proto: r.ProtoMajor,
				}
			}))
			transport := testTransport(t)
			wantProto := 1
			if protocol != "HTTP/1.1" {
				wantProto = 2
				upstream.Config.Protocols = new(http.Protocols)
				upstream.Config.Protocols.SetUnencryptedHTTP2(true)
				transport.Protocols = new(http.Protocols)
				transport.Protocols.SetUnencryptedHTTP2(true)
			}
			upstream.Start()
			defer upstream.Close()

			// The transport connects to the test server for every host, so
			// the hosts need no DNS.
			address := upstream.Listener.Addr().String()
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				var dialer net.Dialer

				return dialer.DialContext(ctx, network, address)
			}

			for _, host := range []string{
				"upstream.example",
				"Upstream_1.internal:8080",
				"xn--bcher-kva.example",
				"[2001:db8::1]:8080",
				"[2001:db8::1]",
				"192.0.2.1:80",
			} {
				var seen string
				hooks := beforeUpstreamFunc(func(_ context.Context, _ *edgekit.RequestContext, out *edgekit.OutboundRequest) error {
					seen = out.Host()

					return nil
				})
				g := newGateway(t, edgekit.Config{
					Routes: []edgekit.Route{{
						Prefix:   "/api/",
						Upstream: static(t, "http://"+host),
					}},
					Hooks:     []any{hooks},
					Transport: transport,
				})

				g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))

				r := receive(t, received)
				if seen != host || r.host != host || r.proto != wantProto {
					t.Errorf("Host() = %q, upstream got %q over HTTP/%d; want %q over HTTP/%d", seen, r.host, r.proto, host, wantProto)
				}
			}
		})
	}
}

func TestSelectedHostThatTheTransportChangesIsRejected(t *testing.T) {
	for _, host := range []string{
		"bücher.example",
		"[fe80::1%eth0]:8080",
		"upstream.example/other",
		"upstream.example other",
		"upstream]example",
		"upstream.example:1:2",
		"upstream.example:8a",
		":8080",
		"::1",
		"[::1",
		"[::1]x",
		"[192.0.2.1]",
		"[upstream.example]:8080",
	} {
		t.Run(host, func(t *testing.T) {
			var roundTrips atomic.Int32
			transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
				roundTrips.Add(1)

				return nil, errors.New("must not run")
			})
			hooks, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix: "/api/",
					Upstream: edgekit.UpstreamSelectorFunc(func(*http.Request) (*url.URL, error) {
						return &url.URL{Scheme: "http", Host: host}, nil
					}),
				}},
				Hooks:     []any{hooks},
				Transport: transport,
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != http.StatusInternalServerError || roundTrips.Load() != 0 {
				t.Errorf("status = %d, round trips = %d; want 500, 0", w.Code, roundTrips.Load())
			}

			c := receive(t, completed)
			if c.Failure == nil || c.Failure.Phase != edgekit.PhaseUpstreamSelection || c.SelectedUpstream != nil {
				t.Errorf("completion = %+v, failure %+v; want a selection failure and no upstream", c, c.Failure)
			}
		})
	}
}

func TestKeptOutboundRequestPanicsAfterTheHooks(t *testing.T) {
	var kept *edgekit.OutboundRequest
	hooks := &testHooks{
		before: func(_ context.Context, _ *edgekit.RequestContext, out *edgekit.OutboundRequest) error {
			kept = out

			return nil
		},
	}
	transport, _ := recordingTransport(t)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, "http://upstream.test"),
		}},
		Hooks:     []any{hooks},
		Transport: transport,
	})

	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))

	if kept == nil {
		t.Fatal("the BeforeUpstream hook did not run")
	}

	if !panics(func() { kept.Header() }) {
		t.Error("Header did not panic after the BeforeUpstream hooks returned")
	}

	if !panics(func() { _ = kept.Method() }) {
		t.Error("Method did not panic after the BeforeUpstream hooks returned")
	}
}
