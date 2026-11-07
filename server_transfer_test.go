package edgekit_test

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// bodyUpstream reads the request body and returns its length.
func bodyUpstream(t *testing.T) *httptest.Server {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		_, _ = io.WriteString(w, strings.Repeat("r", int(n)))
	}))
	t.Cleanup(upstream.Close)

	return upstream
}

// stalledUpload sends a request with a body of which it sends only a part,
// and returns when the test ends.
func stalledUpload(t *testing.T, ts *testServer, http2 bool) {
	t.Helper()

	if !http2 {
		conn := ts.dial(t)
		writeClientData(t, conn, "POST /api/ HTTP/1.1\r\nHost: gateway.test\r\nContent-Length: 100\r\n\r\npartial")

		return
	}

	body, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/", body)
	if err != nil {
		t.Fatal(err)
	}

	client := ts.client(t, true)
	go func() {
		response, doErr := client.Do(request)
		if doErr == nil {
			_ = response.Body.Close()
		}
	}()

	if _, err = io.WriteString(writer, "partial"); err != nil {
		t.Fatal(err)
	}
}

func TestStalledUploadTimesOut(t *testing.T) {
	upstream := bodyUpstream(t)

	for _, protocol := range protocols {
		t.Run(protocol.name, func(t *testing.T) {
			completionHook, completed := completions()
			ts := startServer(t, apiGateway(t, upstream.URL, nil, completionHook), edgekit.ServerConfig{
				TransferIdleTimeout: testIdle,
			}, serverOptions{tls: protocol.http2})

			stalledUpload(t, ts, protocol.http2)

			// Without the deadline, the request waits for the body until
			// the test ends.
			c := receive(t, completed)
			if c.Failure == nil && !c.Aborted {
				t.Errorf("completion = %+v, want a failed or aborted request", c)
			}
		})
	}
}

func TestProgressingUploadContinues(t *testing.T) {
	upstream := bodyUpstream(t)

	for _, protocol := range protocols {
		t.Run(protocol.name, func(t *testing.T) {
			ts := startServer(t, apiGateway(t, upstream.URL, nil), edgekit.ServerConfig{
				TransferIdleTimeout: testIdle,
			}, serverOptions{tls: protocol.http2})

			// Send one byte in each quarter of the timeout, for three times
			// the timeout in total. A total deadline would stop the upload.
			const parts = 12
			body, writer := io.Pipe()
			go func() {
				ticker := time.NewTicker(testIdle / 4)
				defer ticker.Stop()

				for range parts {
					<-ticker.C
					if _, err := io.WriteString(writer, "u"); err != nil {
						return
					}
				}

				_ = writer.Close()
			}()

			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/", body)
			if err != nil {
				t.Fatal(err)
			}

			response, err := ts.client(t, protocol.http2).Do(request)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}

			got, err := io.ReadAll(response.Body)
			_ = response.Body.Close()

			if response.StatusCode != http.StatusOK || err != nil || len(got) != parts {
				t.Errorf("response = %d, %d bytes, %v; want 200, %d bytes", response.StatusCode, len(got), err, parts)
			}
		})
	}
}

// endlessUpstream writes data until the request ends.
func endlessUpstream(t *testing.T) *httptest.Server {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		block := []byte(strings.Repeat("d", 32<<10))
		for r.Context().Err() == nil {
			if _, err := w.Write(block); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)

	return upstream
}

func TestStalledDownloadTimesOut(t *testing.T) {
	upstream := endlessUpstream(t)

	for _, protocol := range protocols {
		t.Run(protocol.name, func(t *testing.T) {
			completionHook, completed := completions()
			ts := startServer(t, apiGateway(t, upstream.URL, nil, completionHook), edgekit.ServerConfig{
				TransferIdleTimeout: testIdle,
			}, serverOptions{tls: protocol.http2})

			// The client reads nothing after the response headers.
			if protocol.http2 {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.url()+"/api/", nil)
				if err != nil {
					t.Fatal(err)
				}

				response, err := ts.client(t, true).Do(request)
				if err != nil {
					t.Fatalf("Do() error = %v", err)
				}
				t.Cleanup(func() { _ = response.Body.Close() })
			} else {
				conn := ts.dial(t)
				writeClientData(t, conn, "GET /api/ HTTP/1.1\r\nHost: gateway.test\r\n\r\n")
			}

			// Without the deadline, the write waits for the client until
			// the test ends.
			c := receive(t, completed)
			if c.Failure == nil && !c.Aborted {
				t.Errorf("completion = %+v, want a failed or aborted request", c)
			}
		})
	}
}

func TestProgressingLargeWriteContinues(t *testing.T) {
	const (
		size   = 2 << 20
		buffer = 64 << 10
	)

	g := apiGateway(t, "http://upstream.test", []edgekit.Middleware{
		func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// One write that takes longer than the timeout in total.
				_, _ = w.Write([]byte(strings.Repeat("w", size)))
			})
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{
		TransferIdleTimeout: testIdle,
	}, serverOptions{writeBuffer: buffer})

	conn := ts.dial(t)
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetReadBuffer(buffer); err != nil {
			t.Fatalf("set client receive buffer: %v", err)
		}
	}

	writeClientData(t, conn, "GET /api/ HTTP/1.1\r\nHost: gateway.test\r\n\r\n")

	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()

	// Read 64 KiB in each quarter of the timeout: about 1.6s in total, much
	// more than the socket buffers hold, with progress in each 16 KiB part.
	ticker := time.NewTicker(testIdle / 4)
	defer ticker.Stop()

	received := 0
	for received < size {
		<-ticker.C

		n, readErr := io.CopyN(io.Discard, response.Body, buffer)
		received += int(n)
		if readErr != nil {
			t.Fatalf("read after %d bytes: %v", received, readErr)
		}
	}
}

// quietUpstream sends one event, waits for release, and sends a second
// event.
func quietUpstream(t *testing.T, release <-chan struct{}) *httptest.Server {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "first\n")
		_ = http.NewResponseController(w).Flush()

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		_, _ = io.WriteString(w, "last\n")
	}))
	t.Cleanup(upstream.Close)

	return upstream
}

// quietGet reads the first line of a quiet response and returns the reader
// of the rest. It closes the response when the test ends.
func quietGet(t *testing.T, client *http.Client, url string) *bufio.Reader {
	t.Helper()

	// A body that the gateway reads completely before the quiet time, so
	// an HTTP/1.1 read deadline that stays set would cancel the request.
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })

	reader := bufio.NewReader(response.Body)
	if line, readErr := reader.ReadString('\n'); line != "first\n" || readErr != nil {
		t.Fatalf("first line = %q, %v", line, readErr)
	}

	return reader
}

func TestQuietStreamIsNotLimited(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol.name, func(t *testing.T) {
			release := make(chan struct{})
			upstream := quietUpstream(t, release)
			ts := startServer(t, apiGateway(t, upstream.URL, nil), edgekit.ServerConfig{
				TransferIdleTimeout: testIdle,
			}, serverOptions{tls: protocol.http2})

			reader := quietGet(t, ts.client(t, protocol.http2), ts.url()+"/api/")

			// The stream is quiet for longer than the timeout, but it does
			// not stall: no transfer waits.
			passIdleTimeouts()
			close(release)

			rest, err := io.ReadAll(reader)
			if string(rest) != "last\n" || err != nil {
				t.Errorf("rest = %q, %v; want %q", rest, err, "last\n")
			}
		})
	}
}

func TestStalledStreamDoesNotStopOtherStreams(t *testing.T) {
	release := make(chan struct{})
	quiet := quietUpstream(t, release)
	completionHook, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/upload/",
				Upstream: static(t, bodyUpstream(t).URL),
			},
			{
				Prefix:   "/quiet/",
				Upstream: static(t, quiet.URL),
			},
		},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{
				completionHook,
			},
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{
		TransferIdleTimeout: testIdle,
	}, serverOptions{tls: true})
	client := ts.client(t, true)

	reader := quietGet(t, client, ts.url()+"/quiet/")

	// A stalled upload on the same connection.
	body, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/upload/", body)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		response, doErr := client.Do(request)
		if doErr == nil {
			_ = response.Body.Close()
		}
	}()

	if _, err = io.WriteString(writer, "partial"); err != nil {
		t.Fatal(err)
	}

	if c := receive(t, completed); c.RoutePrefix != "/upload/" {
		t.Fatalf("completion route = %q, want the stalled upload", c.RoutePrefix)
	}

	close(release)

	rest, err := io.ReadAll(reader)
	if string(rest) != "last\n" || err != nil {
		t.Errorf("quiet stream rest = %q, %v; want %q", rest, err, "last\n")
	}

	if got := ts.listener.accepted.Load(); got != 1 {
		t.Errorf("connections = %d, want 1", got)
	}
}

func TestEarlyRejectionWithStalledBody(t *testing.T) {
	completionHook, completed := completions()
	ts := startServer(t, apiGateway(t, "http://upstream.test", nil, completionHook), edgekit.ServerConfig{
		TransferIdleTimeout: testIdle,
	}, serverOptions{})

	// No route matches, so the gateway rejects the request without reading
	// the body. The client sends only a part of the body.
	conn := ts.dial(t)
	writeClientData(t, conn, "POST /none HTTP/1.1\r\nHost: gateway.test\r\nContent-Length: 1000\r\n\r\npartial")

	// net/http reads the unread body before it sends the response, so
	// without a read deadline the response never arrives.
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}

	if c := receive(t, completed); c.RejectionReason != edgekit.ReasonNoRoute {
		t.Errorf("completion reason = %q, want %q", c.RejectionReason, edgekit.ReasonNoRoute)
	}
}

func TestDeadlineTakeover(t *testing.T) {
	takeover := func(write bool) edgekit.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				controller := http.NewResponseController(w)

				var err error
				if write {
					err = controller.SetWriteDeadline(time.Time{})
				} else {
					err = controller.SetReadDeadline(time.Time{})
				}

				if err != nil {
					t.Errorf("set deadline: %v", err)
				}

				next.ServeHTTP(w, r)
			})
		}
	}

	t.Run("read", func(t *testing.T) {
		ts := startServer(t, apiGateway(t, bodyUpstream(t).URL, []edgekit.Middleware{
			takeover(false),
		}), edgekit.ServerConfig{
			TransferIdleTimeout: testIdle,
		}, serverOptions{})

		// The body stalls for longer than the timeout, then completes.
		conn := ts.dial(t)
		writeClientData(t, conn, "POST /api/ HTTP/1.1\r\nHost: gateway.test\r\nContent-Length: 8\r\n\r\nfirst")
		passIdleTimeouts()
		writeClientData(t, conn, "end")

		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()

		if response.StatusCode != http.StatusOK || len(body) != 8 || err != nil {
			t.Errorf("response = %d, %d bytes, %v; want 200, 8 bytes", response.StatusCode, len(body), err)
		}
	})

	t.Run("write", func(t *testing.T) {
		const size = 1 << 20

		ts := startServer(t, apiGateway(t, "http://upstream.test", []edgekit.Middleware{
			takeover(true),
			func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(strings.Repeat("w", size)))
				})
			},
		}), edgekit.ServerConfig{
			TransferIdleTimeout: testIdle,
		}, serverOptions{writeBuffer: 4 << 10})

		conn := ts.dial(t)
		writeClientData(t, conn, "GET /api/ HTTP/1.1\r\nHost: gateway.test\r\n\r\n")

		// The client reads nothing for longer than the timeout.
		passIdleTimeouts()

		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		n, err := io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()

		if n != size || err != nil {
			t.Errorf("body = %d bytes, %v; want %d bytes", n, err, size)
		}
	})
}

func TestServerKeepsWriterCapabilities(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol.name, func(t *testing.T) {
			type capabilities struct {
				flusher, hijacker, pusher bool
				fullDuplex, deadline      error
			}

			got := make(chan capabilities, 1)
			g := apiGateway(t, "http://upstream.test", []edgekit.Middleware{
				func(http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						var c capabilities
						_, c.flusher = w.(http.Flusher)
						_, c.hijacker = w.(http.Hijacker)
						_, c.pusher = w.(http.Pusher)

						controller := http.NewResponseController(w)
						c.fullDuplex = controller.EnableFullDuplex()
						c.deadline = controller.SetWriteDeadline(time.Now().Add(time.Minute))
						got <- c

						_, _ = io.WriteString(w, "ok")
						_ = controller.Flush()
					})
				},
			})
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{tls: true})

			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.url()+"/api/", nil)
			if err != nil {
				t.Fatal(err)
			}

			response, err := ts.client(t, protocol.http2).Do(request)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil {
				t.Errorf("read response body: %v", readErr)
			}

			if closeErr != nil {
				t.Errorf("close response body: %v", closeErr)
			}

			c := receive(t, got)
			want := capabilities{
				flusher:  true,
				hijacker: !protocol.http2,
				pusher:   protocol.http2,
			}
			if c.flusher != want.flusher || c.hijacker != want.hijacker || c.pusher != want.pusher {
				t.Errorf("capabilities = %+v, want %+v", c, want)
			}

			if c.fullDuplex != nil {
				t.Errorf("EnableFullDuplex() error = %v, want nil", c.fullDuplex)
			}

			if c.deadline != nil {
				t.Errorf("SetWriteDeadline() error = %v, want nil", c.deadline)
			}

			if string(body) != "ok" {
				t.Errorf("body = %q, want %q", body, "ok")
			}
		})
	}
}

func TestFullDuplexStalledUploadTimesOut(t *testing.T) {
	result := make(chan error, 1)
	g := apiGateway(t, "http://upstream.test", []edgekit.Middleware{
		func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				controller := http.NewResponseController(w)
				if err := controller.EnableFullDuplex(); err != nil {
					t.Errorf("EnableFullDuplex() error = %v", err)
				}

				read := make(chan error, 1)
				go func() {
					_, err := io.Copy(io.Discard, r.Body)
					read <- err
				}()

				// Response writes continue while the upload stalls.
				ticker := time.NewTicker(testIdle / 4)
				defer ticker.Stop()

				for {
					select {
					case err := <-read:
						result <- err

						return
					case <-ticker.C:
						_, _ = io.WriteString(w, "x")
						_ = controller.Flush()
					}
				}
			})
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{
		TransferIdleTimeout: testIdle,
	}, serverOptions{})

	conn := ts.dial(t)
	writeClientData(t, conn, "POST /api/ HTTP/1.1\r\nHost: gateway.test\r\nContent-Length: 100\r\n\r\npartial")

	// Without its own deadline, the read waits as long as the writes go on.
	if err := receive(t, result); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("body read error = %v, want os.ErrDeadlineExceeded", err)
	}
}
