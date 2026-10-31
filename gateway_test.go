package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	gateway "example.com/go-gateway-starter"
	"example.com/go-gateway-starter/extensions/headers"
)

func TestProxyForwardsRequestAndAppliesRouteMiddleware(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/hello" || r.URL.RawQuery != "q=1" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("X-Test") != "value" {
			t.Errorf("request header not forwarded")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "payload" {
			t.Errorf("unexpected upstream body %q: %v", body, err)
		}
		_, _ = w.Write([]byte("upstream response"))
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	app, err := gateway.New([]gateway.Route{{
		Prefix:      "/api/",
		Upstream:    target,
		Middlewares: []gateway.Middleware{headers.AddResponseHeader("X-Extension", "active")},
	}})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/hello?q=1", strings.NewReader("payload"))
	req.Header.Set("X-Test", "value")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)

	if w.Code != http.StatusOK || w.Body.String() != "upstream response" {
		t.Fatalf("unexpected response: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Extension") != "active" {
		t.Fatal("route middleware did not run")
	}
}

func TestGlobalAndRouteMiddlewareOrder(t *testing.T) {
	var steps []string
	wrap := func(name string) gateway.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				steps = append(steps, name+":before")
				next.ServeHTTP(w, r)
				steps = append(steps, name+":after")
			})
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		steps = append(steps, "upstream")
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	app, err := gateway.New([]gateway.Route{{Prefix: "/api/", Upstream: target, Middlewares: []gateway.Middleware{wrap("route")}}}, wrap("global"))
	if err != nil {
		t.Fatal(err)
	}
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/example", nil))
	want := []string{"global:before", "route:before", "upstream", "route:after", "global:after"}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("order = %v, want %v", steps, want)
	}
}

func TestInvalidRoute(t *testing.T) {
	_, err := gateway.New([]gateway.Route{{Prefix: "api", Upstream: &url.URL{Scheme: "http", Host: "localhost"}}})
	if err == nil {
		t.Fatal("expected invalid route error")
	}
}
