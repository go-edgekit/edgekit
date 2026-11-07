package accesslog

import (
	"bufio"
	"errors"
	"net"
	"net/http"
)

// responseWriter records the HTTP observations of one request. Its variants
// add Flush, Hijack, and Push only when the original writer has them, so
// middleware that tests for these interfaces finds the same capabilities.
// Each variant has FlushError, which returns [http.ErrNotSupported] when the
// original writer cannot flush, as [http.ResponseController] does. It has no
// ReadFrom method, so io.Copy goes through Write.
type responseWriter struct {
	http.ResponseWriter

	obs *Record
}

// newResponseWriter returns the variant with the optional interfaces of w.
func newResponseWriter(w http.ResponseWriter, obs *Record) http.ResponseWriter {
	rw := &responseWriter{
		ResponseWriter: w,
		obs:            obs,
	}

	_, flusher := w.(http.Flusher)
	_, hijacker := w.(http.Hijacker)
	pusher, isPusher := w.(http.Pusher)

	switch {
	case flusher && hijacker && isPusher:
		return struct {
			*responseWriter
			flushWriter
			hijackWriter
			http.Pusher
		}{
			responseWriter: rw,
			flushWriter:    flushWriter{w: rw},
			hijackWriter:   hijackWriter{w: rw},
			Pusher:         pusher,
		}
	case flusher && hijacker:
		return struct {
			*responseWriter
			flushWriter
			hijackWriter
		}{
			responseWriter: rw,
			flushWriter:    flushWriter{w: rw},
			hijackWriter:   hijackWriter{w: rw},
		}
	case flusher && isPusher:
		return struct {
			*responseWriter
			flushWriter
			http.Pusher
		}{
			responseWriter: rw,
			flushWriter:    flushWriter{w: rw},
			Pusher:         pusher,
		}
	case hijacker && isPusher:
		return struct {
			*responseWriter
			hijackWriter
			http.Pusher
		}{
			responseWriter: rw,
			hijackWriter:   hijackWriter{w: rw},
			Pusher:         pusher,
		}
	case flusher:
		return struct {
			*responseWriter
			flushWriter
		}{
			responseWriter: rw,
			flushWriter:    flushWriter{w: rw},
		}
	case hijacker:
		return struct {
			*responseWriter
			hijackWriter
		}{
			responseWriter: rw,
			hijackWriter:   hijackWriter{w: rw},
		}
	case isPusher:
		return struct {
			*responseWriter
			http.Pusher
		}{
			responseWriter: rw,
			Pusher:         pusher,
		}
	default:
		return rw
	}
}

// committed reports whether a final status or a hijack was observed.
func (w *responseWriter) committed() bool {
	return w.obs.Status != 0 || w.obs.Hijacked
}

// WriteHeader records the first final status. An informational status other
// than 101 does not commit the response, and it never carries the request
// ID: the ID is removed before it and set again after it, so a 1xx value of
// the upstream does not reach the client and the final response keeps the ID.
func (w *responseWriter) WriteHeader(status int) {
	final := status >= 200 || status == http.StatusSwitchingProtocols
	interim := status >= 100 && !final
	if interim && !w.committed() {
		header := w.Header()
		header.Del(RequestIDHeader)
		w.ResponseWriter.WriteHeader(status)
		header.Set(RequestIDHeader, w.obs.RequestID)

		return
	}

	w.ResponseWriter.WriteHeader(status)

	if final && !w.committed() {
		w.obs.Status = status
	}
}

// Write records the implicit 200 of the first write, the accepted bytes, and
// the first error.
func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.committed() {
		w.obs.Status = http.StatusOK
	}

	n, err := w.ResponseWriter.Write(b)
	w.obs.BytesWritten += int64(n)
	w.observeErr(err)

	return n, err
}

// FlushError flushes at once. http.ResponseController prefers it to Flush.
// A writer that cannot flush returns [http.ErrNotSupported], which commits
// nothing and is not a write error.
func (w *responseWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return err
	}

	// A flush without a status sends the implicit 200.
	if !w.committed() {
		w.obs.Status = http.StatusOK
	}

	w.observeErr(err)

	return err
}

// Unwrap gives http.ResponseController the original writer for deadlines
// and full duplex. The result still records a hijack, because the
// controller finds Hijack there when this writer has no Hijack method.
func (w *responseWriter) Unwrap() http.ResponseWriter {
	return unwrappedWriter{
		ResponseWriter: w.ResponseWriter,
		w:              w,
	}
}

// hijack takes the connection through the original writer. A failed hijack
// is not a write error: the component that hijacks owns its failure.
func (w *responseWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.obs.Hijacked = true
	}

	return conn, buffered, err
}

// observeErr keeps the first error, because later errors are consequences
// of it.
func (w *responseWriter) observeErr(err error) {
	if err != nil && w.obs.WriteErr == nil {
		w.obs.WriteErr = err
	}
}

// flushWriter adds http.Flusher.
type flushWriter struct {
	w *responseWriter
}

// Flush flushes at once. http.Flusher has no error result; the error is
// recorded.
func (f flushWriter) Flush() {
	_ = f.w.FlushError()
}

// hijackWriter adds http.Hijacker.
type hijackWriter struct {
	w *responseWriter
}

// Hijack takes the connection and records the hijack.
func (h hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.w.hijack()
}

// unwrappedWriter is the result of Unwrap. Direct writes to it are not
// recorded.
type unwrappedWriter struct {
	http.ResponseWriter

	w *responseWriter
}

// Hijack takes the connection and records the hijack. It returns
// [http.ErrNotSupported] when the original writer cannot hijack.
func (u unwrappedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return u.w.hijack()
}

// Unwrap gives http.ResponseController the original writer.
func (u unwrappedWriter) Unwrap() http.ResponseWriter {
	return u.ResponseWriter
}
