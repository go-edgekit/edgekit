// Package gateway provides a small, extensible HTTP reverse proxy.
// Routing and reverse proxying are part of the core; authentication,
// authorization, and limits can be added as ordinary HTTP middleware.
package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Route associates a URL prefix with one upstream and optional middleware.
// Prefixes must start and end with /; the original path is forwarded unchanged.
type Route struct {
	Prefix      string
	Upstream    *url.URL
	Middlewares []Middleware
}

// Gateway implements http.Handler. It is configured once at startup.
type Gateway struct {
	handler http.Handler
}

// New assembles routes into a gateway and applies global middleware around
// the router. Each route's middleware wraps only its own reverse proxy.
func New(routes []Route, global ...Middleware) (*Gateway, error) {
	if len(routes) == 0 {
		return nil, errors.New("gateway: at least one route is required")
	}

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
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(&target)
				// Forwarded headers from untrusted clients are cleared by Rewrite.
				pr.SetXForwarded()
			},
		}
		mux.Handle(route.Prefix, Chain(proxy, route.Middlewares...))
	}

	return &Gateway{handler: Chain(mux, global...)}, nil
}

// ServeHTTP allows Gateway to be used directly in an http.Server.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.handler.ServeHTTP(w, r)
}
