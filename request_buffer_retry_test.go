package edgekit_test

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/go-edgekit/edgekit"
)

type refusedStreamObservation struct {
	preface      string
	maxFrameSize int
	attempts     int
	refusedSent  bool
	err          error
}

// refusedStreamUpstream returns a fixture and an idempotent stop operation
// that joins its worker and returns the observations for test assertions.
func refusedStreamUpstream(t *testing.T) (string, func() refusedStreamObservation) {
	t.Helper()

	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan refusedStreamObservation, 1)
	accepted := make(chan net.Conn, 1)
	go func() {
		var observed refusedStreamObservation
		defer func() { finished <- observed }()

		conn, err := listener.Accept()
		accepted <- conn
		if err != nil {
			observed.err = err

			return
		}
		defer func() { _ = conn.Close() }()

		observed = serveRefusedStream(conn)
	}()
	stop := sync.OnceValue(func() refusedStreamObservation {
		_ = listener.Close()
		if conn := <-accepted; conn != nil {
			_ = conn.Close()
		}

		return <-finished
	})
	t.Cleanup(func() { _ = stop() })

	return "http://" + listener.Addr().String(), stop
}

// serveRefusedStream sends REFUSED_STREAM only after the complete body.
// A replay retry gets a 204, so the test distinguishes retry from failure.
func serveRefusedStream(conn net.Conn) (observed refusedStreamObservation) {
	preface := make([]byte, 24)
	_, observed.err = io.ReadFull(conn, preface)
	observed.preface = string(preface)
	if observed.err != nil || observed.preface != "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" {
		return observed
	}

	// Empty server SETTINGS. No HPACK decoder is needed: the fixture
	// responds only to the end of a request, not its field values.
	if _, observed.err = conn.Write([]byte{0, 0, 0, 4, 0, 0, 0, 0, 0}); observed.err != nil {
		return observed
	}
	for {
		var header [9]byte
		if _, observed.err = io.ReadFull(conn, header[:]); observed.err != nil {
			return observed
		}
		length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
		observed.maxFrameSize = max(observed.maxFrameSize, length)
		if length > 1<<20 {
			return observed
		}
		if _, observed.err = io.CopyN(io.Discard, conn, int64(length)); observed.err != nil {
			return observed
		}
		if header[3] == 4 && header[4]&1 == 0 {
			if _, observed.err = conn.Write([]byte{0, 0, 0, 4, 1, 0, 0, 0, 0}); observed.err != nil {
				return observed
			}
		}
		if (header[3] == 0 || header[3] == 1) && header[4]&1 != 0 {
			stream := binary.BigEndian.Uint32(header[5:])
			observed.attempts++
			if observed.attempts == 1 {
				// RST_STREAM, REFUSED_STREAM (RFC 9113, sections 6.4, 7).
				frame := []byte{0, 0, 4, 3, 0, 0, 0, 0, 0, 0, 0, 0, 7}
				binary.BigEndian.PutUint32(frame[5:9], stream)
				if _, observed.err = conn.Write(frame); observed.err != nil {
					return observed
				}
				observed.refusedSent = true
			} else {
				// HPACK static index 9 is :status 204 (RFC 7541,
				// Appendix A). END_HEADERS and END_STREAM end it.
				frame := []byte{0, 0, 1, 1, 5, 0, 0, 0, 0, 0x89}
				binary.BigEndian.PutUint32(frame[5:9], stream)
				if _, observed.err = conn.Write(frame); observed.err != nil {
					return observed
				}
			}
		}
	}
}

func TestBufferedHTTP2RefusedStreamDoesNotReplayBody(t *testing.T) {
	upstream, stop := refusedStreamUpstream(t)
	transport := edgekit.NewTransport()
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetUnencryptedHTTP2(true)
	t.Cleanup(transport.CloseIdleConnections)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:           "/api/",
				Upstream:         static(t, upstream),
				BodyLimit:        edgekit.MaxBodyBytes(8),
				RequestBuffering: buffering(t, 8),
			},
		},
		Transport: transport,
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	req := bodyRequest("body", true).WithContext(t.Context())
	req.URL = parseURL(t, ts.url()+"/api/upload")
	req.RequestURI = ""
	resp, err := ts.client(t, false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	observed := stop()

	if want := "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"; observed.preface != want {
		t.Errorf("upstream connection preface = %q, want %q", observed.preface, want)
	}
	if observed.maxFrameSize > 1<<20 {
		t.Errorf("largest upstream frame = %d bytes, want at most %d", observed.maxFrameSize, 1<<20)
	}
	if !observed.refusedSent {
		t.Error("upstream fixture sent no REFUSED_STREAM, want one for the first attempt")
	}
	if observed.err != nil && !errors.Is(observed.err, io.EOF) && !errors.Is(observed.err, net.ErrClosed) {
		t.Errorf("upstream fixture error = %v, want nil, io.EOF, or net.ErrClosed", observed.err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("POST /api/upload after HTTP/2 REFUSED_STREAM: status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	if observed.attempts != 1 {
		t.Errorf("POST /api/upload after HTTP/2 REFUSED_STREAM: upstream attempts = %d, want 1", observed.attempts)
	}
}
