// Package gateway provides a small, extensible HTTP reverse proxy.
// Routing and reverse proxying are part of the core; authentication,
// authorization, and limits can be added as ordinary HTTP middleware.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"example.com/go-gateway-starter/internal/safelog"
)

// Route associates a URL prefix with one upstream and optional middleware.
// Prefixes must start and end with /; the original path is forwarded unchanged.
type Route struct {
	Prefix      string
	Upstream    *url.URL
	Middlewares []Middleware
}

// Config configures a gateway at startup.
type Config struct {
	Routes      []Route
	Middlewares []Middleware
	// Extensions must implement at least one lifecycle hook interface.
	// Hooks execute in registration order, including completion hooks.
	// Instances are shared across requests and must support concurrent requests.
	Extensions []any
	// Transport is shared by all routes. Nil uses http.DefaultTransport.
	// The caller owns configuration and idle-connection cleanup. Configure it
	// before serving and do not mutate it while requests are active.
	Transport http.RoundTripper
}

// Gateway implements http.Handler. It is configured once at startup.
type Gateway struct {
	handler     http.Handler
	completions []CompletionHook
}

// New assembles routes into a gateway and applies global middleware around
// the router. Each route's middleware wraps only its own reverse proxy.
func New(routes []Route, global ...Middleware) (*Gateway, error) {
	return NewWithConfig(Config{Routes: routes, Middlewares: global})
}

// NewWithConfig assembles middleware and optional lifecycle extensions.
// Request hooks run before routing. Response hooks run only for upstream
// responses. The first hook error stops the remaining hooks in that phase.
func NewWithConfig(config Config) (*Gateway, error) {
	routes := config.Routes
	if len(routes) == 0 {
		return nil, errors.New("gateway: at least one route is required")
	}

	hooks, err := collectHooks(config.Extensions)
	if err != nil {
		return nil, err
	}

	transport := config.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	transport = &outboundTransport{base: transport, hooks: hooks.outbound}
	mux := http.NewServeMux()
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		if !strings.HasPrefix(route.Prefix, "/") || !strings.HasSuffix(route.Prefix, "/") {
			return nil, fmt.Errorf("gateway: route prefix %q must begin and end with /", route.Prefix)
		}
		if _, exists := seen[route.Prefix]; exists {
			return nil, fmt.Errorf("gateway: duplicate route prefix %q", route.Prefix)
		}
		seen[route.Prefix] = struct{}{}
		if route.Upstream == nil || route.Upstream.Host == "" ||
			(route.Upstream.Scheme != "http" && route.Upstream.Scheme != "https") {
			return nil, fmt.Errorf("gateway: route %q needs an http(s) upstream", route.Prefix)
		}

		target := *route.Upstream // Avoid sharing a mutable URL with the caller.
		proxy := &httputil.ReverseProxy{
			Transport: transport,
			ErrorLog:  safelog.New("reverse_proxy"),
			Rewrite: func(pr *httputil.ProxyRequest) {
				rc, _ := FromContext(pr.In.Context())
				rc.upstream = &target
				pr.SetURL(&target)
				// ReverseProxy clears standard forwarding headers before Rewrite.
				// Do not pass a competing client-supplied identity to the upstream.
				pr.Out.Header.Del("X-Real-IP")
				pr.SetXForwarded()
			},
			ModifyResponse: func(response *http.Response) error {
				ctx := response.Request.Context()
				rc, _ := FromContext(ctx)
				for _, hook := range hooks.responses {
					if err := hook.OnUpstreamResponse(ctx, rc, response); err != nil {
						return err
					}
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				rc, _ := FromContext(r.Context())
				renderError(w, rc, err, http.StatusBadGateway)
			},
		}
		routeHandler := Chain(proxy, route.Middlewares...)
		mux.Handle(route.Prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc, _ := FromContext(r.Context())
			rc.routePrefix = route.Prefix
			routeHandler.ServeHTTP(w, r)
		}))
	}

	core := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, _ := FromContext(r.Context())
		for _, hook := range hooks.requests {
			if err := hook.OnRequest(r.Context(), rc, r); err != nil {
				renderError(w, rc, err, http.StatusInternalServerError)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
	return &Gateway{
		handler:     Chain(core, config.Middlewares...),
		completions: hooks.completions,
	}, nil
}

// ServeHTTP allows Gateway to be used directly in an http.Server.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := &RequestContext{startedAt: time.Now()}
	r = r.WithContext(context.WithValue(r.Context(), requestContextKey{}, rc))
	returned := false
	defer func() {
		outcome := Outcome{
			Duration: time.Since(rc.startedAt),
			Err:      rc.err,
			Aborted:  !returned,
		}
		for _, hook := range g.completions {
			hook.OnComplete(r.Context(), rc, outcome)
		}
	}()
	g.handler.ServeHTTP(w, r)
	returned = true
}
