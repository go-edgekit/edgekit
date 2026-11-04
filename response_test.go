package edgekit_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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

// responseHead sends request on a new connection to server and returns the
// status line and the raw response header fields, before the client parses
// framing fields out of them.
func responseHead(t *testing.T, server *httptest.Server, request string) (string, textproto.MIMEHeader, *bufio.Reader, net.Conn) {
	t.Helper()

	var dialer net.Dialer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	conn, err := dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)

	if _, err = io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	head := textproto.NewReader(reader)
	status, err := head.ReadLine()
	if err != nil {
		t.Fatal(err)
	}

	header, err := head.ReadMIMEHeader()
	if err != nil {
		t.Fatal(err)
	}

	return status, header, reader, conn
}

// hookServer returns a started server for a gateway with one route to
// upstreamURL and the given hook providers.
func hookServer(t *testing.T, upstreamURL string, providers ...any) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstreamURL),
		}},
		Hooks: providers,
	}))
	t.Cleanup(server.Close)

	return server
}

func TestResponseHeaderRulesAfterHooks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		_, _ = io.WriteString(w, "hello")
		w.Header().Set("X-Checksum", "abc")
	}))
	defer upstream.Close()

	hooks := upstreamResponseFunc(func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) error {
		for name, value := range map[string]string{
			"Connection":          "X-Hop",
			"X-Hop":               "private",
			"Keep-Alive":          "timeout=5",
			"Proxy-Connection":    "keep-alive",
			"Proxy-Authenticate":  "Basic",
			"Proxy-Authorization": "Basic c2VjcmV0",
			"Te":                  "trailers",
			"Trailer":             "X-Fake",
			"Transfer-Encoding":   "gzip",
			"Upgrade":             "h2c",
			"Alt-Svc":             `h3=":443"`,
			"X-Added":             "kept",
		} {
			resp.Header.Set(name, value)
		}

		resp.Header["alt-svc"] = []string{`h2="evil.test:443"`}
		for _, name := range []string{"Alt-Svc", "Host", "Content-Length", "Connection", "X-Extra"} {
			resp.Trailer[name] = nil
		}

		return nil
	})
	server := hookServer(t, upstream.URL, hooks)

	status, header, _, _ := responseHead(t, server, "GET /api/x HTTP/1.1\r\nHost: gateway.test\r\nConnection: close\r\n\r\n")

	if status != "HTTP/1.1 200 OK" || header.Get("X-Added") != "kept" {
		t.Fatalf("response = %q, %v; want 200 with the header that the hook added", status, header)
	}

	for _, name := range []string{
		"X-Hop",
		"Keep-Alive",
		"Proxy-Connection",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Upgrade",
		"Alt-Svc",
	} {
		if values := header.Values(name); len(values) != 0 {
			t.Errorf("response has %s %q, want none", name, values)
		}
	}

	// The server generates Connection: close, chunked framing, and the
	// trailer announcement. The values of the hook are gone.
	if got := header.Values("Connection"); !slices.Equal(got, []string{"close"}) {
		t.Errorf("Connection = %q, want only the value of the server", got)
	}

	if got := header.Values("Transfer-Encoding"); !slices.Equal(got, []string{"chunked"}) {
		t.Errorf("Transfer-Encoding = %q, want only chunked", got)
	}

	announced := header.Values("Trailer")
	for _, name := range []string{"Alt-Svc", "Host", "Content-Length", "Connection", "X-Fake"} {
		for _, value := range announced {
			if strings.Contains(strings.ToLower(value), strings.ToLower(name)) {
				t.Errorf("Trailer announces %q, want no %s", announced, name)
			}
		}
	}

	if !slices.ContainsFunc(announced, func(v string) bool { return strings.Contains(v, "X-Checksum") }) {
		t.Errorf("Trailer = %q, want X-Checksum", announced)
	}
}

func TestResponseContentLengthAfterHooks(t *testing.T) {
	// The body is larger than the buffer of the server, so the server cannot
	// compute a Content-Length itself.
	body := strings.Repeat("b", 8<<10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	length := strconv.Itoa(len(body))
	set := func(values ...string) func(http.Header) {
		return func(h http.Header) {
			h["Content-Length"] = values
		}
	}
	for _, tc := range []struct {
		name   string
		change func(http.Header)
		want   []string
	}{
		{
			name: "upstream value",
			want: []string{length},
		},
		{
			name:   "valid value",
			change: set(length),
			want:   []string{length},
		},
		{
			name:   "largest int64",
			change: set("9223372036854775807"),
			want:   []string{"9223372036854775807"},
		},
		{
			name:   "int64 overflow by one",
			change: set("9223372036854775808"),
		},
		{
			name:   "many digits",
			change: set("999999999999999999999999999999"),
		},
		{
			name:   "not a number",
			change: set("secret-invalid-value"),
		},
		{
			name:   "negative",
			change: set("-" + length),
		},
		{
			name:   "plus sign",
			change: set("+" + length),
		},
		{
			name:   "empty",
			change: set(""),
		},
		{
			name:   "list",
			change: set(length + ", " + length),
		},
		{
			name:   "two values",
			change: set(length, length),
		},
		{
			name: "lower-case name only",
			change: func(h http.Header) {
				delete(h, "Content-Length")
				h["content-length"] = []string{length}
			},
			want: []string{length},
		},
		{
			name: "lower-case name with an invalid value",
			change: func(h http.Header) {
				h["content-length"] = []string{"secret-invalid-value"}
			},
		},
		{
			name: "two letter cases with the same value",
			change: func(h http.Header) {
				h["CONTENT-LENGTH"] = []string{length}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := upstreamResponseFunc(func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) error {
				if tc.change != nil {
					tc.change(resp.Header)
				}

				return nil
			})
			var serverLog bytes.Buffer
			server := httptest.NewUnstartedServer(newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, upstream.URL),
				}},
				Hooks: []any{hooks},
			}))
			server.Config.ErrorLog = log.New(&serverLog, "", 0)
			server.Start()
			defer server.Close()

			_, header, reader, _ := responseHead(t, server, "GET /api/x HTTP/1.1\r\nHost: gateway.test\r\nConnection: close\r\n\r\n")
			rest, _ := io.ReadAll(reader)

			if got := header.Values("Content-Length"); !slices.Equal(got, tc.want) {
				t.Errorf("Content-Length = %q, want %q", got, tc.want)
			}

			for name, values := range header {
				if slices.ContainsFunc(values, func(v string) bool { return strings.Contains(v, "secret") }) {
					t.Errorf("response has %s %q, want no value of the hook", name, values)
				}
			}

			chunked := header.Get("Transfer-Encoding") == "chunked"
			if chunked != (tc.want == nil) || !strings.Contains(string(rest), body[:64]) {
				t.Errorf("chunked = %v, body has %d bytes; want chunked = %v and the upstream body", chunked, len(rest), tc.want == nil)
			}

			// The server logs an invalid Content-Length that a handler set,
			// with its value.
			if serverLog.Len() != 0 {
				t.Errorf("server logged %q, want nothing", serverLog.String())
			}
		})
	}
}

// upgradeUpstream returns a started upstream that switches to an echo
// protocol with the given 101 header lines.
func upgradeUpstream(t *testing.T, lines string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = io.WriteString(buffered, "HTTP/1.1 101 Switching Protocols\r\n"+lines+"\r\n")
		_ = buffered.Flush()

		line, err := buffered.ReadString('\n')
		if err != nil {
			return
		}

		_, _ = io.WriteString(buffered, line)
		_ = buffered.Flush()
	}))
	t.Cleanup(server.Close)

	return server
}

// upgradeRequest asks for the echo protocol.
const upgradeRequest = "GET /api/echo HTTP/1.1\r\n" +
	"Host: gateway.test\r\n" +
	"Connection: Upgrade\r\n" +
	"Upgrade: echo\r\n" +
	"\r\n"

func TestUpgradeResponseHeaderRulesAfterHooks(t *testing.T) {
	upstream := upgradeUpstream(t, "Connection: Upgrade, X-Hop\r\n"+
		"Upgrade: echo\r\n"+
		"X-Hop: private\r\n"+
		"Keep-Alive: timeout=5\r\n")

	hooks := upstreamResponseFunc(func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) error {
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Errorf("hook got status %d, want 101", resp.StatusCode)
		}

		resp.Header.Add("Connection", "X-Hook-Hop")
		for name, value := range map[string]string{
			"X-Hook-Hop":          "private",
			"Content-Length":      "0",
			"Transfer-Encoding":   "chunked",
			"Alt-Svc":             `h3=":443"`,
			"Te":                  "trailers",
			"Trailer":             "X-Fake",
			"Proxy-Connection":    "keep-alive",
			"Proxy-Authenticate":  "Basic",
			"Proxy-Authorization": "Basic c2VjcmV0",
			"X-Added":             "kept",
		} {
			resp.Header.Set(name, value)
		}

		return nil
	})
	server := hookServer(t, upstream.URL, hooks)

	status, header, reader, conn := responseHead(t, server, upgradeRequest)

	if status != "HTTP/1.1 101 Switching Protocols" {
		t.Fatalf("status = %q, want 101", status)
	}

	if got := header.Values("Connection"); !slices.Equal(got, []string{"Upgrade"}) {
		t.Errorf("Connection = %q, want only %q", got, "Upgrade")
	}

	if header.Get("Upgrade") != "echo" || header.Get("X-Added") != "kept" {
		t.Errorf("response = %v, want Upgrade echo and the header that the hook added", header)
	}

	for _, name := range []string{
		"X-Hop",
		"X-Hook-Hop",
		"Keep-Alive",
		"Content-Length",
		"Transfer-Encoding",
		"Alt-Svc",
		"Te",
		"Trailer",
		"Proxy-Connection",
		"Proxy-Authenticate",
		"Proxy-Authorization",
	} {
		if values := header.Values(name); len(values) != 0 {
			t.Errorf("101 response has %s %q, want none", name, values)
		}
	}

	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}

	if line, err := reader.ReadString('\n'); err != nil || line != "ping\n" {
		t.Errorf("echo = %q, %v; want %q", line, err, "ping\n")
	}
}

func TestUpgradeProtocolIsRestoredAfterHooks(t *testing.T) {
	upstream := upgradeUpstream(t, "Connection: Upgrade\r\nUpgrade: echo\r\n")

	for _, tc := range []struct {
		name   string
		change func(http.Header)
	}{
		{
			name: "Upgrade token removed",
			change: func(h http.Header) {
				h.Set("Connection", "X-Other")
			},
		},
		{
			name: "Connection deleted",
			change: func(h http.Header) {
				h.Del("Connection")
			},
		},
		{
			name: "protocol changed",
			change: func(h http.Header) {
				h.Set("Upgrade", "other")
			},
		},
		{
			name: "Upgrade deleted",
			change: func(h http.Header) {
				h.Del("Upgrade")
			},
		},
		{
			name: "Upgrade in another letter case",
			change: func(h http.Header) {
				h["upgrade"] = []string{"h2c"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := upstreamResponseFunc(func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) error {
				tc.change(resp.Header)

				return nil
			})
			completionHooks, completed := completions()
			server := hookServer(t, upstream.URL, hooks, completionHooks)

			// The header map has no fixed iteration order, so more than one
			// request makes an order-dependent result visible.
			for range 10 {
				status, header, reader, conn := responseHead(t, server, upgradeRequest)

				if status != "HTTP/1.1 101 Switching Protocols" {
					t.Fatalf("status = %q, want 101", status)
				}

				if got := header.Values("Upgrade"); !slices.Equal(got, []string{"echo"}) {
					t.Errorf("Upgrade = %q, want only %q", got, "echo")
				}

				if got := header.Values("Connection"); !slices.Equal(got, []string{"Upgrade"}) {
					t.Errorf("Connection = %q, want only %q", got, "Upgrade")
				}

				if _, err := io.WriteString(conn, "ping\n"); err != nil {
					t.Fatal(err)
				}

				if line, err := reader.ReadString('\n'); err != nil || line != "ping\n" {
					t.Errorf("echo = %q, %v; want %q", line, err, "ping\n")
				}

				_ = conn.Close()
				if c := receive(t, completed); c.Failure != nil {
					t.Errorf("completion failure = %+v, want none", c.Failure)
				}
			}
		})
	}
}

func TestUpgradeMismatchFromUpstream(t *testing.T) {
	upstream := upgradeUpstream(t, "Connection: Upgrade\r\nUpgrade: other\r\n")
	completionHooks, completed := completions()
	server := hookServer(t, upstream.URL, completionHooks)

	status, _, _, _ := responseHead(t, server, upgradeRequest)

	if status != "HTTP/1.1 502 Bad Gateway" {
		t.Errorf("status = %q, want 502", status)
	}

	c := receive(t, completed)
	if c.Failure == nil || c.Failure.Phase != edgekit.PhaseUnknown {
		t.Errorf("completion failure = %+v, want a failure of the proxy without a known source", c.Failure)
	}
}

func TestResponseHooksSkipInformationalResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	var statuses []int
	hooks := upstreamResponseFunc(func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) error {
		statuses = append(statuses, resp.StatusCode)

		return nil
	})
	server := hookServer(t, upstream.URL, hooks)

	var informational atomic.Int32
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(int, textproto.MIMEHeader) error {
			informational.Add(1)

			return nil
		},
	}
	ctx := httptrace.WithClientTrace(t.Context(), trace)
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/x", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if informational.Load() != 1 || !slices.Equal(statuses, []int{http.StatusOK}) {
		t.Errorf("client got %d informational responses, hook saw %v; want 1 and only [200]", informational.Load(), statuses)
	}
}

func TestConnectionTokensDoNotDependOnMapOrder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	// One Connection entry names Connection itself. If the core removed
	// fields while it read the tokens, the order of the map would decide
	// whether it reads the other entry.
	hooks := upstreamResponseFunc(func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) error {
		resp.Header["Connection"] = []string{"Connection"}
		resp.Header["connection"] = []string{"X-Hop"}
		resp.Header["X-Hop"] = []string{"private"}

		return nil
	})
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		Hooks: []any{hooks},
	})

	// Go randomizes the iteration order of maps, so many requests examine
	// many orders.
	leaks := 0
	for range 200 {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

		if w.Header().Get("X-Hop") != "" || w.Header().Get("Connection") != "" {
			leaks++
		}
	}

	if leaks != 0 {
		t.Errorf("%d of 200 responses kept X-Hop or Connection, want 0", leaks)
	}
}

func TestUpgradeConnectionTokensDoNotDependOnMapOrder(t *testing.T) {
	upstream := upgradeUpstream(t, "Connection: Upgrade\r\nUpgrade: echo\r\n")
	hooks := upstreamResponseFunc(func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) error {
		resp.Header["Connection"] = []string{"Upgrade, Connection"}
		resp.Header["connection"] = []string{"X-Hop"}
		resp.Header["X-Hop"] = []string{"private"}

		return nil
	})
	server := hookServer(t, upstream.URL, hooks)

	for i := range 20 {
		status, header, _, _ := responseHead(t, server, upgradeRequest)

		if status != "HTTP/1.1 101 Switching Protocols" || header.Get("X-Hop") != "" || !slices.Equal(header.Values("Connection"), []string{"Upgrade"}) {
			t.Fatalf("request %d: %q, X-Hop %q, Connection %q; want 101, no X-Hop, Connection Upgrade", i, status, header.Get("X-Hop"), header.Values("Connection"))
		}
	}
}

func TestResponseHooksGetTheGatewayContext(t *testing.T) {
	type derivedKey struct{}

	for _, tc := range []struct {
		name    string
		request func(*http.Request) *http.Request
	}{
		{
			name: "transport sets no request",
			request: func(*http.Request) *http.Request {
				return nil
			},
		},
		{
			name: "transport sets another request",
			request: func(r *http.Request) *http.Request {
				return r.Clone(context.Background())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("ok")),
					Request:    tc.request(r),
				}, nil
			})
			var (
				sameContext bool
				route       string
				derived     any
			)
			hooks := upstreamResponseFunc(func(ctx context.Context, rc *edgekit.RequestContext, _ *http.Response) error {
				fromContext, ok := edgekit.FromContext(ctx)
				sameContext = ok && rc != nil && fromContext == rc
				if rc != nil {
					route, _ = rc.RoutePrefix()
				}

				derived = ctx.Value(derivedKey{})

				return nil
			})
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, "http://upstream.test"),
					Middleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), derivedKey{}, "derived")))
						})
					}},
				}},
				Hooks:     []any{hooks},
				Transport: transport,
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != http.StatusOK || !sameContext || route != "/api/" || derived != "derived" {
				t.Errorf("status = %d, same gateway context = %v, route = %q, derived value = %v; want 200, true, /api/, derived", w.Code, sameContext, route, derived)
			}
		})
	}
}

// closeRecorder records whether Close was called. It is also a writer, as
// ReverseProxy requires for the body of a 101 response.
type closeRecorder struct {
	io.Reader
	io.Writer

	closed atomic.Bool
}

// Close records the call.
func (c *closeRecorder) Close() error {
	c.closed.Store(true)

	return nil
}

func TestResponseHookCannotCrossTheUpgradeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream int
		changeTo int
		status   int
		body     string
		failure  bool
	}{
		{
			name:     "101 to 403",
			upstream: http.StatusSwitchingProtocols,
			changeTo: http.StatusForbidden,
			status:   http.StatusInternalServerError,
			body:     "Internal Server Error\n",
			failure:  true,
		},
		{
			name:     "200 to 101",
			upstream: http.StatusOK,
			changeTo: http.StatusSwitchingProtocols,
			status:   http.StatusInternalServerError,
			body:     "Internal Server Error\n",
			failure:  true,
		},
		{
			name:     "200 to 103",
			upstream: http.StatusOK,
			changeTo: http.StatusEarlyHints,
			status:   http.StatusInternalServerError,
			body:     "Internal Server Error\n",
			failure:  true,
		},
		{
			name:     "200 to 600",
			upstream: http.StatusOK,
			changeTo: 600,
			status:   http.StatusInternalServerError,
			body:     "Internal Server Error\n",
			failure:  true,
		},
		{
			name:     "200 to 199",
			upstream: http.StatusOK,
			changeTo: 199,
			status:   http.StatusInternalServerError,
			body:     "Internal Server Error\n",
			failure:  true,
		},
		{
			name:     "200 to 403",
			upstream: http.StatusOK,
			changeTo: http.StatusForbidden,
			status:   http.StatusForbidden,
			body:     "denied",
		},
		{
			name:     "200 to 599",
			upstream: http.StatusOK,
			changeTo: 599,
			status:   599,
			body:     "denied",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstreamBody := &closeRecorder{
				Reader: strings.NewReader("upstream body"),
				Writer: io.Discard,
			}
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				header := make(http.Header)
				if tc.upstream == http.StatusSwitchingProtocols {
					header.Set("Connection", "Upgrade")
					header.Set("Upgrade", "echo")
				}

				return &http.Response{
					StatusCode: tc.upstream,
					Header:     header,
					Body:       upstreamBody,
					Request:    r,
				}, nil
			})
			hooks := upstreamResponseFunc(func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) error {
				_ = resp.Body.Close()
				resp.StatusCode = tc.changeTo
				resp.Body = io.NopCloser(strings.NewReader("denied"))
				resp.Header.Del("Content-Length")

				return nil
			})
			completionHooks, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, "http://upstream.test"),
				}},
				Hooks:     []any{hooks, completionHooks},
				Transport: transport,
			})

			r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "echo")
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)

			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Errorf("response = %d %q, want %d %q", w.Code, w.Body.String(), tc.status, tc.body)
			}

			if !upstreamBody.closed.Load() {
				t.Error("upstream body is not closed")
			}

			c := receive(t, completed)
			switch {
			case tc.failure && (c.Failure == nil || c.Failure.Phase != edgekit.PhaseUpstreamResponse):
				t.Errorf("completion failure = %+v, want a failure in UpstreamResponse", c.Failure)
			case !tc.failure && c.Failure != nil:
				t.Errorf("completion failure = %+v, want none", c.Failure)
			}
		})
	}
}
