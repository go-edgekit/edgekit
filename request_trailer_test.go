package edgekit_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-edgekit/edgekit"
)

func TestConnectionOptionsRemoveRequestTrailers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared string
		headers  []string
	}{
		{
			name:     "declared hop-by-hop trailer",
			declared: "Kept, X-Hop",
			headers: []string{
				"Connection: X-Hop",
			},
		},
		{
			name:     "undeclared hop-by-hop trailer",
			declared: "Kept",
			headers: []string{
				"Connection: X-Hop",
			},
		},
		{
			name:     "mixed-case Connection options",
			declared: "Kept, X-Hop, X-Other",
			headers: []string{
				"Connection: x-hOP",
				"Connection: X-oTHER",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, transport, received := trailerUpstream(t, false)
			type observation struct {
				declared string
				values   string
			}

			observed := make(chan observation, 1)
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, upstream.URL),
					},
				},
				Transport: transport,
				SharedMiddleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							names := make([]string, 0, len(r.Trailer))
							for name := range r.Trailer {
								names = append(names, name)
							}

							sort.Strings(names)
							next.ServeHTTP(w, r)
							observed <- observation{
								declared: strings.Join(names, " "),
								values:   trailerValues(r.Trailer),
							}
						})
					},
				},
			})
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})

			trailers := "Kept: v\r\nLate: w\r\nX-Hop: hop-only\r\n"
			if len(tc.headers) > 1 {
				trailers += "X-Other: other-hop-only\r\n"
			}

			status := sendRaw(t, ts, chunkedRequest(tc.declared, trailers, tc.headers...))
			if status != http.StatusOK {
				t.Fatalf("POST /api/upload status = %d, want 200", status)
			}

			if got := receive(t, received); got != "Kept=v Late=w" {
				t.Errorf("upstream got trailers %q, want Kept=v Late=w", got)
			}

			got := receive(t, observed)
			if got.declared != "Kept" || got.values != "Kept=v Late=w" {
				t.Errorf("middleware saw %+v, want only Kept declared and Kept=v Late=w", got)
			}
		})
	}
}
