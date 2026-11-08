package edgekit

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The cases follow hasToken in net/http/header.go.
func TestHasContinueExpectation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{
			name:  "empty",
			value: "",
		},
		{
			name:  "exact",
			value: "100-continue",
			want:  true,
		},
		{
			name:  "mixed case",
			value: "100-CoNtInUe",
			want:  true,
		},
		{
			name:  "first comma token",
			value: "100-continue, foo",
			want:  true,
		},
		{
			name:  "last comma token",
			value: "foo,100-CoNtInUe",
			want:  true,
		},
		{
			name:  "empty list elements",
			value: ",,100-continue,,",
			want:  true,
		},
		{
			name:  "space and tab boundaries",
			value: "foo 100-continue\tbar",
			want:  true,
		},
		{
			name:  "outer whitespace",
			value: " \t100-continue\t ",
			want:  true,
		},
		{
			name:  "token prefix",
			value: "x100-continue",
		},
		{
			name:  "token suffix",
			value: "100-continue-more",
		},
		{
			name:  "parameter boundary",
			value: "100-continue=foo",
		},
		{
			name:  "semicolon boundary",
			value: "100-continue;foo",
		},
		{
			name:  "non-ASCII leading whitespace",
			value: " 100-continue",
		},
		{
			name:  "non-ASCII trailing whitespace",
			value: "100-continue ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasContinueExpectation(tc.value); got != tc.want {
				t.Errorf("hasContinueExpectation(%q) = %t, want %t", tc.value, got, tc.want)
			}
		})
	}
}

// newContinueServer returns a server whose only route reads the complete body
// of at most 4 bytes and answers 204, with request buffering when buffering
// is not nil. The transfer idle timeout is 3 minutes.
func newContinueServer(t *testing.T, buffering *RequestBuffering) *Server {
	t.Helper()

	g, err := New(Config{
		Routes: []Route{
			{
				Prefix: "/",
				Upstream: staticUpstream{
					url: url.URL{
						Scheme: "http",
						Host:   "unused.test",
					},
				},
				BodyLimit:        MaxBodyBytes(4),
				RequestBuffering: buffering,
			},
		},
		SharedMiddleware: []Middleware{
			func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						t.Errorf("read body: %v", err)
					}

					w.WriteHeader(http.StatusNoContent)
				})
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.CloseIdleConnections)

	srv, err := NewServer(g, ServerConfig{TransferIdleTimeout: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	return srv
}

// The source read observes the deadline before net/http can write its
// automatic 100 Continue response. No socket buffers or clock waits are needed.
// The token rules are in TestHasContinueExpectation; these cases check that
// the server uses only the first header value and applies the deadline of each
// mode.
func TestServerContinueWriteDeadlineBeforeFirstRead(t *testing.T) {
	for _, mode := range []struct {
		name     string
		buffered bool
	}{
		{
			name: "streaming",
		},
		{
			name:     "buffered",
			buffered: true,
		},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				values   []string
				deadline bool
			}{
				{
					name: "absent",
				},
				{
					name:     "comma token",
					values:   []string{"foo, 100-continue"},
					deadline: true,
				},
				{
					name:     "first header value matches",
					values:   []string{"100-continue", "foo"},
					deadline: true,
				},
				{
					name:   "only second header value matches",
					values: []string{"foo", "100-continue"},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					writer := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
					var atRead time.Time
					reader := strings.NewReader("body")
					source := &bufferInput{}
					source.read = func(p []byte) (int, error) {
						if source.reads == 1 && len(writer.writeDeadlines) != 0 {
							atRead = writer.writeDeadlines[len(writer.writeDeadlines)-1]
						}

						return reader.Read(p)
					}

					var buffering *RequestBuffering
					bound := 3 * time.Minute
					if mode.buffered {
						budget, err := NewRequestBufferBudget(4)
						if err != nil {
							t.Fatal(err)
						}
						buffering = &RequestBuffering{
							Budget:       budget,
							IdleTimeout:  time.Minute,
							TotalTimeout: 2 * time.Minute,
						}
						bound = time.Minute
					}
					srv := newContinueServer(t, buffering)

					r := httptest.NewRequest(http.MethodPost, "/", nil)
					r.Body = source
					r.ContentLength = 4
					r.Header["Expect"] = tc.values
					before := time.Now()
					srv.serveHTTP(writer, r)
					after := time.Now()

					if writer.Code != http.StatusNoContent || source.reads == 0 {
						t.Fatalf("status = %d, source reads = %d; want 204 and at least one read", writer.Code, source.reads)
					}
					if got := !atRead.IsZero(); got != tc.deadline {
						t.Errorf("write deadline before first source read = %t, want %t", got, tc.deadline)
					}
					if !atRead.IsZero() && (atRead.Before(before.Add(bound)) || atRead.After(after.Add(bound))) {
						t.Errorf("write deadline = %v, want between %v and %v", atRead, before.Add(bound), after.Add(bound))
					}
				})
			}
		})
	}
}
