package edgekit_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-edgekit/edgekit"
)

func ExampleNew() {
	// The upstream is a test server here. In a deployment, it is an API.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream response")
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		fmt.Println(err)

		return
	}

	selector, err := edgekit.StaticUpstream(target)
	if err != nil {
		fmt.Println(err)

		return
	}

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
		}},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	// A server, such as http.Server, calls ServeHTTP for each request.
	for _, path := range []string{"/api/users", "/admin"} {
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		fmt.Printf("%s: %d %s\n", path, w.Code, w.Body.String())
	}

	// Output:
	// /api/users: 200 upstream response
	// /admin: 404 {"type":"about:blank","title":"Not Found","status":404,"reason":"no_route"}
}

func ExampleServer() {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream response")
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		fmt.Println(err)

		return
	}

	selector, err := edgekit.StaticUpstream(target)
	if err != nil {
		fmt.Println(err)

		return
	}

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: selector,
			},
		},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	// The zero ServerConfig applies all secure defaults. A disabled timeout
	// is always explicit, such as KeepAliveTimeout: edgekit.NoTimeout.
	server, err := edgekit.NewServer(gateway, edgekit.ServerConfig{
		TransferIdleTimeout: time.Minute,
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println(err)

		return
	}

	served := make(chan struct{})
	var serveErr error
	go func() {
		defer close(served)
		serveErr = server.Serve(listener)
	}()
	defer func() {
		// Close is best effort after Shutdown, and stops Serve on early returns.
		_ = server.Close()
		<-served
	}()

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+listener.Addr().String()+"/api/users", nil)
	if err != nil {
		fmt.Println(err)

		return
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	response, err := client.Do(request)
	if err != nil {
		fmt.Println(err)

		return
	}

	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		fmt.Println(readErr)

		return
	}

	if closeErr != nil {
		fmt.Println(closeErr)

		return
	}

	fmt.Println(response.StatusCode, string(body))

	// Shutdown waits for the active requests. When the time ends first,
	// Close stops them.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
	}

	<-served
	fmt.Println(errors.Is(serveErr, http.ErrServerClosed))

	// Output:
	// 200 upstream response
	// true
}

// regionSelector selects the upstream for the region that route middleware
// put in the X-Region header.
type regionSelector map[string]*url.URL

// SelectUpstream returns the upstream URL of the request's region.
func (s regionSelector) SelectUpstream(r *http.Request) (*url.URL, error) {
	upstream, ok := s[r.Header.Get("X-Region")]
	if !ok {
		return nil, errors.New("unknown region")
	}

	return upstream, nil
}

func ExampleUpstreamSelector() {
	var servers []*httptest.Server
	defer func() {
		for _, server := range servers {
			server.Close()
		}
	}()

	newUpstream := func(region string) *url.URL {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, "served in %s", region)
		}))
		servers = append(servers, server)

		return &url.URL{Scheme: "http", Host: server.Listener.Addr().String()}
	}
	selector := regionSelector{
		"eu": newUpstream("eu"),
		"us": newUpstream("us"),
	}

	// Route middleware runs before upstream selection. Here it sets the
	// region from the query. A real extension can use other request data.
	region := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.Clone(r.Context())
			r.Header.Set("X-Region", r.URL.Query().Get("region"))
			next.ServeHTTP(w, r)
		})
	}

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:     "/api/",
			Upstream:   selector,
			Middleware: []edgekit.Middleware{region},
		}},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	for _, target := range []string{"/api/x?region=eu", "/api/x?region=us", "/api/x?region=mars"} {
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		fmt.Printf("%d %s\n", w.Code, w.Body.String())
	}

	// Output:
	// 200 served in eu
	// 200 served in us
	// 500 {"type":"about:blank","title":"Internal Server Error","status":500}
}

func ExampleUpstreamSelectorFunc() {
	newUpstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, name)
		}))
	}
	stable, canary := newUpstream("stable"), newUpstream("canary")
	defer stable.Close()
	defer canary.Close()

	stableURL := &url.URL{Scheme: "http", Host: stable.Listener.Addr().String()}
	canaryURL := &url.URL{Scheme: "http", Host: canary.Listener.Addr().String()}

	// A function is enough for a selector without its own state.
	selector := edgekit.UpstreamSelectorFunc(func(r *http.Request) (*url.URL, error) {
		if r.Header.Get("X-Canary") == "1" {
			return canaryURL, nil
		}

		return stableURL, nil
	})

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
		}},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	for _, flag := range []string{"0", "1"} {
		r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		r.Header.Set("X-Canary", flag)
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, r)
		fmt.Println(w.Body.String())
	}

	// Output:
	// stable
	// canary
}

func ExampleNewTransport() {
	// This route serves long polls, so relax the response-header timeout.
	// The other secure defaults stay.
	transport := edgekit.NewTransport()
	transport.ResponseHeaderTimeout = 2 * time.Minute
	defer transport.CloseIdleConnections()

	selector, err := edgekit.StaticUpstream(&url.URL{Scheme: "https", Host: "events.example"})
	if err != nil {
		fmt.Println(err)

		return
	}

	_, err = edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/events/",
			Upstream: selector,
		}},
		Transport: transport,
	})
	fmt.Println("gateway made:", err == nil)

	// A transport with transparent compression is not valid.
	transport.DisableCompression = false
	_, err = edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/events/",
			Upstream: selector,
		}},
		Transport: transport,
	})
	fmt.Println("invalid configuration:", errors.Is(err, edgekit.ErrInvalidConfiguration))

	// Output:
	// gateway made: true
	// invalid configuration: true
}

// signer signs outbound requests. It holds only configuration, so one value
// is safe for all concurrent requests.
type signer struct {
	key []byte
}

// BeforeUpstream signs the method and the rewritten request target, and sends
// the signature in X-Signature.
func (s signer) BeforeUpstream(_ context.Context, _ *edgekit.RequestContext, out *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
	mac := hmac.New(sha256.New, s.key)
	_, _ = io.WriteString(mac, out.Method()+" "+out.Host()+out.URL().RequestURI())
	out.Header().Set("X-Signature", hex.EncodeToString(mac.Sum(nil))[:16])

	return nil, nil
}

func ExampleBeforeUpstreamHook() {
	signatures := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		signatures <- r.Header.Get("X-Signature")
	}))
	defer upstream.Close()

	// A fixed host gives a fixed signature in this example. The test server
	// gets the request through the transport.
	target := &url.URL{Scheme: "http", Host: "upstream.example"}
	selector, err := edgekit.StaticUpstream(target)
	if err != nil {
		fmt.Println(err)

		return
	}

	address := upstream.Listener.Addr().String()
	transport := edgekit.NewTransport()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer

		return dialer.DialContext(ctx, network, address)
	}
	defer transport.CloseIdleConnections()

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
		}},
		// Register a signer after the providers that change headers, so it
		// is closer to the upstream and signs their changes.
		Hooks: []any{
			signer{key: []byte("example key")},
		},
		Transport: transport,
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	gateway.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/users?page=2", nil))
	fmt.Println("upstream got X-Signature:", <-signatures)

	// Output:
	// upstream got X-Signature: 3da70cc31d851dde
}

// requestIDs gives each request an ID in route middleware and reports it at
// completion. The request state connects the two.
type requestIDs struct {
	next atomic.Int64
}

// requestIDKey is private, so only requestIDs can read or change the ID.
var requestIDKey = edgekit.NewStateKey[string]()

// Middleware stores a new request ID in the request state.
func (ids *requestIDs) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rc, ok := edgekit.FromContext(r.Context()); ok {
			edgekit.SetState(rc, requestIDKey, fmt.Sprintf("req-%d", ids.next.Add(1)))
		}

		next.ServeHTTP(w, r)
	})
}

// RequestCompleted reports the request ID together with execution facts.
func (ids *requestIDs) RequestCompleted(_ context.Context, rc *edgekit.RequestContext, c edgekit.Completion) {
	id, ok := edgekit.State(rc, requestIDKey)
	if !ok {
		id = "no ID"
	}

	fmt.Printf("%s: route %q, rejection %q, failure %v\n", id, c.RoutePrefix, c.RejectionReason, c.Failure != nil)
}

func ExampleSetState() {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		fmt.Println(err)

		return
	}

	selector, err := edgekit.StaticUpstream(target)
	if err != nil {
		fmt.Println(err)

		return
	}

	ids := &requestIDs{}
	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
		}},
		Middleware: []edgekit.Middleware{ids.Middleware},
		Hooks:      []any{ids},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	// The secure defaults reject the last request before any middleware
	// runs, so it has no ID.
	for _, path := range []string{"/api/users", "/admin", "/api/../admin"} {
		gateway.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	// Output:
	// req-1: route "/api/", rejection "", failure false
	// req-2: route "", rejection "no_route", failure false
	// no ID: route "", rejection "invalid_path", failure false
}

// apiKeys rejects requests without an API key. A real extension would also
// check the key.
type apiKeys struct{}

// missingKey is shared by all requests. Nobody changes it or its header map.
var missingKey = edgekit.Rejection{
	Status: http.StatusUnauthorized,
	Reason: "apikey.missing",
	Header: http.Header{
		"Www-Authenticate": {`ApiKey realm="api"`},
	},
}

// RequestReceived rejects a request without the X-Api-Key header. A failure,
// such as a key store that is down, would be the error result instead.
func (apiKeys) RequestReceived(_ context.Context, _ *edgekit.RequestContext, r *http.Request) (*edgekit.Rejection, error) {
	if r.Header.Get("X-Api-Key") == "" {
		return &missingKey, nil
	}

	return nil, nil
}

func ExampleRejection() {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream response")
	}))
	defer upstream.Close()

	selector, err := edgekit.StaticUpstream(&url.URL{
		Scheme: "http",
		Host:   upstream.Listener.Addr().String(),
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
		}},
		Hooks: []any{apiKeys{}},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/users", nil))
	fmt.Println(w.Code, w.Header().Get("WWW-Authenticate"))
	fmt.Println(w.Header().Get("Content-Type"))
	fmt.Println(w.Body.String())

	// Output:
	// 401 ApiKey realm="api"
	// application/problem+json
	// {"type":"about:blank","title":"Unauthorized","status":401,"reason":"apikey.missing"}
}

func ExampleRenderer() {
	// This renderer writes JSON:API error documents instead of RFC 9457
	// problem documents. It gets only the status and the rejection reason,
	// never the failure details.
	jsonAPI := func(_ context.Context, p edgekit.Problem) (string, []byte) {
		type errorObject struct {
			Status string `json:"status"`
			Code   string `json:"code,omitempty"`
			Title  string `json:"title,omitempty"`
		}

		body, err := json.Marshal(map[string][]errorObject{
			"errors": {{
				Status: strconv.Itoa(p.Status),
				Code:   string(p.Reason),
				Title:  http.StatusText(p.Status),
			}},
		})
		if err != nil {
			// The gateway then uses its default renderer.
			return "", nil
		}

		return "application/vnd.api+json", body
	}

	selector := edgekit.UpstreamSelectorFunc(func(*http.Request) (*url.URL, error) {
		return nil, fmt.Errorf("discovery: %w", edgekit.ErrNoUpstreamAvailable)
	})

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
		}},
		Renderer: jsonAPI,
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	for _, path := range []string{"/api/users", "/admin"} {
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		fmt.Println(w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}

	// Output:
	// 503 application/vnd.api+json {"errors":[{"status":"503","title":"Service Unavailable"}]}
	// 404 application/vnd.api+json {"errors":[{"status":"404","code":"no_route","title":"Not Found"}]}
}

func ExampleMaxBodyBytes() {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return
		}

		_, _ = fmt.Fprintf(w, "stored %d bytes", n)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		fmt.Println(err)

		return
	}

	selector, err := edgekit.StaticUpstream(target)
	if err != nil {
		fmt.Println(err)

		return
	}

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: selector,
			},
			{
				// An upload route sets a larger limit explicitly.
				Prefix:    "/upload/",
				Upstream:  selector,
				BodyLimit: edgekit.MaxBodyBytes(1 << 20),
			},
		},
		// The gateway limit for all other routes. Omitted, it is
		// edgekit.DefaultBodyLimit.
		BodyLimit: edgekit.MaxBodyBytes(16),
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	body := strings.Repeat("x", 32)
	for _, path := range []string{"/api/items", "/upload/files"} {
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		fmt.Printf("%s: %d %s\n", path, w.Code, w.Body.String())
	}

	// Output:
	// /api/items: 413 {"type":"about:blank","title":"Request Entity Too Large","status":413,"reason":"body_too_large"}
	// /upload/files: 200 stored 32 bytes
}

func ExampleNewAdmissionBudget() {
	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "upstream response")
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		fmt.Println(err)

		return
	}

	selector, err := edgekit.StaticUpstream(target)
	if err != nil {
		fmt.Println(err)

		return
	}

	// One request at a time on this route; there is no queue.
	budget, err := edgekit.NewAdmissionBudget(1)
	if err != nil {
		fmt.Println(err)

		return
	}

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/reports/",
				Upstream:  selector,
				Admission: budget,
			},
		},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	first := make(chan *httptest.ResponseRecorder)
	go func() {
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/reports/daily", nil))
		first <- w
	}()
	<-entered

	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/reports/weekly", nil))
	fmt.Printf("second request: %d %s\n", w.Code, w.Body.String())

	close(release)
	w = <-first
	fmt.Printf("first request: %d %s\n", w.Code, w.Body.String())

	// Output:
	// second request: 503 {"type":"about:blank","title":"Service Unavailable","status":503,"reason":"admission_saturated"}
	// first request: 200 upstream response
}

func ExampleNewEgressTransport() {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream response")
	}))
	defer upstream.Close()

	routes := []edgekit.Route{}
	for prefix, rawURL := range map[string]string{
		"/api/":      upstream.URL,
		"/metadata/": "http://169.254.169.254",
	} {
		target, err := url.Parse(rawURL)
		if err != nil {
			fmt.Println(err)

			return
		}

		selector, err := edgekit.StaticUpstream(target)
		if err != nil {
			fmt.Println(err)

			return
		}

		routes = append(routes, edgekit.Route{
			Prefix:   prefix,
			Upstream: selector,
		})
	}

	// The gateway can connect only to this origin, and only to loopback
	// addresses. A nil template uses the settings of NewTransport.
	transport, err := edgekit.NewEgressTransport(nil, edgekit.EgressPolicy{
		AllowedOrigins: []string{upstream.URL},
		AllowedNetworks: []netip.Prefix{
			netip.MustParsePrefix("127.0.0.0/8"),
		},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer transport.CloseIdleConnections()

	gateway, err := edgekit.New(edgekit.Config{
		Routes:    routes,
		Transport: transport,
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	for _, path := range []string{"/api/users", "/metadata/latest"} {
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		fmt.Printf("%s: %d %s\n", path, w.Code, w.Body.String())
	}

	// Output:
	// /api/users: 200 upstream response
	// /metadata/latest: 502 {"type":"about:blank","title":"Bad Gateway","status":502}
}
