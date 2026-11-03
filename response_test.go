package edgekit_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// gatewayServer returns a started server for a gateway with one route to
// upstreamURL.
func gatewayServer(t *testing.T, upstreamURL string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstreamURL),
		}},
	}))
	t.Cleanup(server.Close)

	return server
}

func TestInformationalResponseHeadersAreSanitized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hints := map[string]string{
			"Link":       "</style.css>; rel=preload",
			"Alt-Svc":    `h3=":443"`,
			"Connection": "X-Hint-Hop",
			"X-Hint-Hop": "private",
			"Keep-Alive": "timeout=5",
		}
		for name, value := range hints {
			w.Header().Set(name, value)
		}
		w.WriteHeader(http.StatusEarlyHints)

		for name := range hints {
			w.Header().Del(name)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	server := gatewayServer(t, upstream.URL)

	hints := make(chan textproto.MIMEHeader, 1)
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(status int, header textproto.MIMEHeader) error {
			if status == http.StatusEarlyHints {
				hints <- header
			}

			return nil
		},
	}
	ctx, cancel := context.WithTimeout(httptrace.WithClientTrace(t.Context(), trace), 5*time.Second)
	defer cancel()

	r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/x", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()

	if err != nil || response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("final response = %d %q, %v; want 200 %q, <nil>", response.StatusCode, body, err, "ok")
	}

	got := http.Header(receive(t, hints))
	if want := "</style.css>; rel=preload"; got.Get("Link") != want {
		t.Errorf("103 response Link = %q, want %q", got.Values("Link"), want)
	}

	for _, name := range []string{"Alt-Svc", "Connection", "X-Hint-Hop", "Keep-Alive"} {
		if got.Get(name) != "" {
			t.Errorf("103 response %s = %q, want none", name, got.Values(name))
		}
	}
}

func TestInformationalResponseKeepsMiddlewareHeaders(t *testing.T) {
	upstreamHints := []string{"</a.css>; rel=preload", "</b.js>; rel=preload"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, hint := range upstreamHints {
			w.Header().Set("Link", hint)
			w.WriteHeader(http.StatusEarlyHints)
		}

		w.Header().Del("Link")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	middlewareLink := "</middleware.css>; rel=preload"
	server := httptest.NewServer(newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		Middleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Middleware", "set")
				w.Header().Set("Link", middlewareLink)
				next.ServeHTTP(w, r)
			})
		}},
	}))
	defer server.Close()

	hints := make(chan textproto.MIMEHeader, len(upstreamHints))
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(status int, header textproto.MIMEHeader) error {
			if status == http.StatusEarlyHints {
				hints <- header
			}

			return nil
		},
	}
	ctx, cancel := context.WithTimeout(httptrace.WithClientTrace(t.Context(), trace), 5*time.Second)
	defer cancel()

	r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/x", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	// Each 103 response has only the upstream hint, also after the gateway
	// restored the middleware headers for the first time.
	for _, want := range upstreamHints {
		got := http.Header(receive(t, hints))
		if got.Get("X-Middleware") != "" || !slices.Equal(got.Values("Link"), []string{want}) {
			t.Errorf("103 response = %v, want only Link %q", got, want)
		}
	}

	if response.StatusCode != http.StatusOK || response.Header.Get("X-Middleware") != "set" {
		t.Errorf("final response = %d, X-Middleware %q; want 200, %q", response.StatusCode, response.Header.Get("X-Middleware"), "set")
	}

	if !slices.Equal(response.Header.Values("Link"), []string{middlewareLink}) {
		t.Errorf("final response Link = %q, want only %q", response.Header.Values("Link"), middlewareLink)
	}
}

func TestTrailersAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		name      string
		announced bool
	}{
		{
			name:      "announced",
			announced: true,
		},
		{
			name:      "not announced",
			announced: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				prefix := http.TrailerPrefix
				if tc.announced {
					w.Header().Set("Trailer", "X-Checksum, Alt-Svc")
					prefix = ""
				}

				// A flush makes the response chunked, so it can have
				// trailers that it did not announce.
				_, _ = io.WriteString(w, "body")
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("flush: %v", err)

					return
				}

				w.Header().Set(prefix+"X-Checksum", "abc")
				w.Header().Set(prefix+"Alt-Svc", `h3=":443"`)
			}))
			defer upstream.Close()

			server := gatewayServer(t, upstream.URL)
			response, err := server.Client().Get(server.URL + "/api/x")
			if err != nil {
				t.Fatal(err)
			}

			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()

			if err != nil || string(body) != "body" {
				t.Errorf("body = %q, %v; want %q, <nil>", body, err, "body")
			}

			if response.Trailer.Get("X-Checksum") != "abc" {
				t.Errorf("trailer X-Checksum = %q, want %q", response.Trailer.Get("X-Checksum"), "abc")
			}

			if response.Trailer.Get("Alt-Svc") != "" || response.Header.Get("Alt-Svc") != "" {
				t.Errorf("client Alt-Svc trailer %q, header %q; want none", response.Trailer.Values("Alt-Svc"), response.Header.Values("Alt-Svc"))
			}
		})
	}
}

func TestProtocolUpgradeIsForwarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "echo" {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)

			return
		}

		conn, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		switching := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Connection: Upgrade\r\n" +
			"Upgrade: echo\r\n" +
			"\r\n"
		_, _ = io.WriteString(buffered, switching)
		_ = buffered.Flush()

		line, err := buffered.ReadString('\n')
		if err != nil {
			return
		}

		_, _ = io.WriteString(buffered, line)
		_ = buffered.Flush()
	}))
	defer upstream.Close()

	server := gatewayServer(t, upstream.URL)

	var dialer net.Dialer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)

	request := "GET /api/echo HTTP/1.1\r\n" +
		"Host: gateway.test\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: echo\r\n" +
		"\r\n"
	if _, err = io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Upgrade") != "echo" {
		t.Fatalf("response = %d, Upgrade %q; want 101, %q", response.StatusCode, response.Header.Get("Upgrade"), "echo")
	}

	if _, writeErr := io.WriteString(conn, "ping\n"); writeErr != nil {
		t.Fatal(writeErr)
	}

	line, err := reader.ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Errorf("echo = %q, %v; want %q", line, err, "ping\n")
	}
}
