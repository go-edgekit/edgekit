package accesslog

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type plainWriter struct {
	header   http.Header
	statuses []int
	body     strings.Builder
}

func (w *plainWriter) Header() http.Header { return w.header }
func (w *plainWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}
func (w *plainWriter) Write(body []byte) (int, error) { return w.body.Write(body) }

type optionalCapabilities struct {
	flushes int
	hijacks int
	pushes  int
}

func (o *optionalCapabilities) Flush() { o.flushes++ }
func (o *optionalCapabilities) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	o.hijacks++
	return nil, nil, nil
}
func (o *optionalCapabilities) Push(string, *http.PushOptions) error {
	o.pushes++
	return http.ErrNotSupported
}

func TestResponseWriterCapabilityCombinations(t *testing.T) {
	factories := []func(http.ResponseWriter, *optionalCapabilities) http.ResponseWriter{
		func(w http.ResponseWriter, o *optionalCapabilities) http.ResponseWriter { return w },
		func(w http.ResponseWriter, o *optionalCapabilities) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				http.Flusher
			}{w, o}
		},
		func(w http.ResponseWriter, o *optionalCapabilities) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				http.Hijacker
			}{w, o}
		},
		func(w http.ResponseWriter, o *optionalCapabilities) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				http.Flusher
				http.Hijacker
			}{w, o, o}
		},
		func(w http.ResponseWriter, o *optionalCapabilities) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				http.Pusher
			}{w, o}
		},
		func(w http.ResponseWriter, o *optionalCapabilities) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				http.Flusher
				http.Pusher
			}{w, o, o}
		},
		func(w http.ResponseWriter, o *optionalCapabilities) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				http.Hijacker
				http.Pusher
			}{w, o, o}
		},
		func(w http.ResponseWriter, o *optionalCapabilities) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				http.Flusher
				http.Hijacker
				http.Pusher
			}{w, o, o, o}
		},
	}
	for mask, factory := range factories {
		t.Run(string(rune('0'+mask)), func(t *testing.T) {
			base := &plainWriter{header: make(http.Header)}
			optional := &optionalCapabilities{}
			capture := &responseCapture{ResponseWriter: factory(base, optional)}
			wrapped := capture.wrap()
			_, flushes := wrapped.(http.Flusher)
			_, hijacks := wrapped.(http.Hijacker)
			_, pushes := wrapped.(http.Pusher)
			if flushes != (mask&1 != 0) || hijacks != (mask&2 != 0) || pushes != (mask&4 != 0) {
				t.Fatalf("capabilities changed: flush=%v hijack=%v push=%v", flushes, hijacks, pushes)
			}
			if _, ok := wrapped.(io.ReaderFrom); ok {
				t.Fatal("ReaderFrom could bypass byte accounting")
			}
			if _, ok := wrapped.(http.CloseNotifier); ok {
				t.Fatal("deprecated CloseNotifier should not be advertised")
			}
			wrapped.WriteHeader(http.StatusCreated)
			_, _ = io.Copy(wrapped, strings.NewReader("body"))
			if capture.status != 201 || capture.bytes != 4 || base.body.String() != "body" {
				t.Fatal("base HTTP behavior or accounting changed")
			}
			if flushes {
				wrapped.(http.Flusher).Flush()
				if optional.flushes != 1 {
					t.Fatal("Flush did not reach underlying writer")
				}
			} else if err := http.NewResponseController(wrapped).Flush(); !errors.Is(err, http.ErrNotSupported) {
				t.Fatalf("unsupported flush returned %v", err)
			}
			if pushes {
				if err := wrapped.(http.Pusher).Push("/asset", nil); err != http.ErrNotSupported || optional.pushes != 1 {
					t.Fatal("Push result did not pass through")
				}
			}
			if hijacks {
				if _, _, err := http.NewResponseController(wrapped).Hijack(); err != nil || optional.hijacks != 1 || !capture.hijacked {
					t.Fatal("Hijack was not observed and forwarded")
				}
			} else if _, _, err := http.NewResponseController(wrapped).Hijack(); !errors.Is(err, http.ErrNotSupported) {
				t.Fatalf("unsupported hijack returned %v", err)
			}
		})
	}
}

type unwrapWriter struct{ http.ResponseWriter }

func (w unwrapWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestControllerHijackThroughUnwrapRemainsObservable(t *testing.T) {
	optional := &optionalCapabilities{}
	underlying := struct {
		http.ResponseWriter
		http.Hijacker
	}{&plainWriter{header: make(http.Header)}, optional}
	capture := &responseCapture{ResponseWriter: unwrapWriter{underlying}}
	wrapped := capture.wrap()
	if _, ok := wrapped.(http.Hijacker); ok {
		t.Fatal("wrapper changed the presence of the direct Hijacker interface")
	}
	if _, _, err := http.NewResponseController(wrapped).Hijack(); err != nil || !capture.hijacked || optional.hijacks != 1 {
		t.Fatal("controller hijack bypassed observation while unwrapping")
	}
}

func TestResponseStatusesAndRequestID(t *testing.T) {
	base := &plainWriter{header: make(http.Header)}
	capture := &responseCapture{ResponseWriter: base, requestID: "generated"}
	capture.WriteHeader(http.StatusEarlyHints)
	if capture.status != 0 {
		t.Fatal("informational response committed a final status")
	}
	// ReverseProxy clears response headers after forwarding an informational response.
	clear(base.header)
	base.header.Add("X-Request-ID", "upstream")
	base.header.Add("X-Request-ID", "another-upstream")
	capture.WriteHeader(http.StatusCreated)
	capture.WriteHeader(http.StatusInternalServerError)
	if capture.status != 201 || !reflect.DeepEqual(base.statuses, []int{103, 201, 500}) {
		t.Fatal("final status was not recorded once or headers were not forwarded")
	}
	if values := base.header.Values("X-Request-ID"); !reflect.DeepEqual(values, []string{"generated"}) {
		t.Fatalf("request ID not restored at final commit: %v", values)
	}
	upgrade := &responseCapture{ResponseWriter: &plainWriter{header: make(http.Header)}}
	upgrade.WriteHeader(http.StatusSwitchingProtocols)
	if upgrade.status != 101 {
		t.Fatal("explicit upgrade status not recorded")
	}
}

type controllerWriter struct {
	*plainWriter
	deadline time.Time
	flushErr error
	flushes  int
	duplex   bool
}

func (w *controllerWriter) FlushError() error { w.flushes++; return w.flushErr }
func (w *controllerWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}
func (w *controllerWriter) EnableFullDuplex() error { w.duplex = true; return nil }

func TestResponseControllerOperationsAndFlushError(t *testing.T) {
	base := &controllerWriter{
		plainWriter: &plainWriter{header: make(http.Header)},
		flushErr:    io.ErrClosedPipe,
	}
	capture := &responseCapture{ResponseWriter: base}
	wrapped := capture.wrap()
	if _, ok := wrapped.(http.Flusher); ok {
		t.Fatal("FlushError support should not advertise the Flusher interface")
	}
	controller := http.NewResponseController(wrapped)
	deadline := time.Now().Add(time.Second)
	if err := controller.SetWriteDeadline(deadline); err != nil || base.deadline != deadline {
		t.Fatal("deadline did not pass through Unwrap")
	}
	if err := controller.EnableFullDuplex(); err != nil || !base.duplex {
		t.Fatal("full-duplex operation did not pass through Unwrap")
	}
	if err := controller.Flush(); err != io.ErrClosedPipe || capture.err != io.ErrClosedPipe || capture.status != 200 || base.flushes != 1 {
		t.Fatal("flush failure or implicit status not observed")
	}
	base.flushErr = io.ErrUnexpectedEOF
	_ = controller.Flush()
	if capture.err != io.ErrClosedPipe {
		t.Fatal("first response error overwritten")
	}
	unsupported := &responseCapture{ResponseWriter: &plainWriter{header: make(http.Header)}}
	if err := http.NewResponseController(unsupported.wrap()).Flush(); !errors.Is(err, http.ErrNotSupported) || unsupported.status != 0 || unsupported.err != nil {
		t.Fatal("unsupported operation should not commit a status or record an I/O error")
	}
}
