package accesslog

import (
	"bufio"
	"errors"
	"net"
	"net/http"
)

// responseCapture counts writes without claiming unsupported HTTP capabilities.
// ReaderFrom is intentionally not exposed: io.Copy must go through Write.
// The deprecated CloseNotifier interface is not exposed; use request contexts.
type responseCapture struct {
	http.ResponseWriter
	requestID string
	status    int
	bytes     int64
	err       error
	hijacked  bool
}

// Unwrap keeps controller-based hijacks observable even when an intervening
// writer exposes only Unwrap, not Hijacker. Other controller operations continue
// through the bridge to the underlying writer.
func (c *responseCapture) Unwrap() http.ResponseWriter {
	return controllerBridge{ResponseWriter: c.ResponseWriter, capture: c}
}

type controllerBridge struct {
	http.ResponseWriter
	capture *responseCapture
}

func (b controllerBridge) Unwrap() http.ResponseWriter { return b.ResponseWriter }
func (b controllerBridge) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return b.capture.hijack()
}

func (c *responseCapture) WriteHeader(status int) {
	if c.status == 0 && !c.hijacked {
		c.setRequestID()
	}
	c.ResponseWriter.WriteHeader(status)
	// Informational responses do not commit a final status, except upgrades.
	if c.status == 0 && !c.hijacked && (status >= 200 || status == http.StatusSwitchingProtocols) {
		c.status = status
	}
}

func (c *responseCapture) Write(body []byte) (int, error) {
	if c.status == 0 && !c.hijacked {
		c.setRequestID()
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(body)
	c.bytes += int64(n)
	c.observeError(err)
	return n, err
}

// FlushError keeps ResponseController flushing observable, including an implicit
// 200. It returns ErrNotSupported when the underlying writer cannot flush.
func (c *responseCapture) FlushError() error {
	if c.status == 0 && !c.hijacked {
		c.setRequestID()
	}
	err := http.NewResponseController(c.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		if c.status == 0 && !c.hijacked {
			c.status = http.StatusOK
		}
		c.observeError(err)
	}
	return err
}

func (c *responseCapture) setRequestID() {
	if c.requestID != "" {
		c.Header().Set("X-Request-ID", c.requestID)
	}
}

func (c *responseCapture) observeError(err error) {
	if c.err == nil {
		c.err = err
	}
}

type flushObserver struct{ capture *responseCapture }

func (f flushObserver) Flush() { _ = f.capture.FlushError() }

type hijackObserver struct{ capture *responseCapture }

func (h hijackObserver) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.capture.hijack()
}

func (c *responseCapture) hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(c.ResponseWriter).Hijack()
	if err == nil {
		c.hijacked = true
	}
	if !errors.Is(err, http.ErrNotSupported) {
		c.observeError(err)
	}
	return conn, buffer, err
}

// wrap preserves the presence of Flusher, Hijacker, and Pusher independently.
// Advertising these unconditionally would change middleware capability checks.
func (c *responseCapture) wrap() http.ResponseWriter {
	const (
		canFlush = 1 << iota
		canHijack
		canPush
	)
	capabilities := 0
	if _, ok := c.ResponseWriter.(http.Flusher); ok {
		capabilities |= canFlush
	}
	if _, ok := c.ResponseWriter.(http.Hijacker); ok {
		capabilities |= canHijack
	}
	pusher, ok := c.ResponseWriter.(http.Pusher)
	if ok {
		capabilities |= canPush
	}
	flusher, hijacker := flushObserver{c}, hijackObserver{c}
	switch capabilities {
	case canFlush:
		return struct {
			*responseCapture
			http.Flusher
		}{c, flusher}
	case canHijack:
		return struct {
			*responseCapture
			http.Hijacker
		}{c, hijacker}
	case canPush:
		return struct {
			*responseCapture
			http.Pusher
		}{c, pusher}
	case canFlush | canHijack:
		return struct {
			*responseCapture
			http.Flusher
			http.Hijacker
		}{c, flusher, hijacker}
	case canFlush | canPush:
		return struct {
			*responseCapture
			http.Flusher
			http.Pusher
		}{c, flusher, pusher}
	case canHijack | canPush:
		return struct {
			*responseCapture
			http.Hijacker
			http.Pusher
		}{c, hijacker, pusher}
	case canFlush | canHijack | canPush:
		return struct {
			*responseCapture
			http.Flusher
			http.Hijacker
			http.Pusher
		}{c, flusher, hijacker, pusher}
	default:
		return c
	}
}
