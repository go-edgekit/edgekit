package edgekit

import (
	"testing"
	"time"
)

// The http.Server of a Server is not part of the public API, so this test
// checks the mapping of the configuration to net/http directly. The
// black-box tests check the behavior of each protection.
func TestServerConfigMapping(t *testing.T) {
	// NewServer does not use the gateway before Serve.
	g := &Gateway{}

	for _, tc := range []struct {
		name             string
		cfg              ServerConfig
		headerRead       time.Duration
		maxHeaderBytes   int
		keepAlive        time.Duration
		transferIdle     time.Duration
		streams          int
		streamBuffer     int
		connectionBuffer int
	}{
		{
			name:             "defaults",
			cfg:              ServerConfig{},
			headerRead:       10 * time.Second,
			maxHeaderBytes:   64 << 10,
			keepAlive:        60 * time.Second,
			transferIdle:     300 * time.Second,
			streams:          100,
			streamBuffer:     64 << 10,
			connectionBuffer: 1 << 20,
		},
		{
			name: "overrides",
			cfg: ServerConfig{
				HeaderReadTimeout:                  time.Second,
				MaxHeaderBytes:                     1 << 10,
				KeepAliveTimeout:                   2 * time.Second,
				TransferIdleTimeout:                3 * time.Second,
				HTTP2MaxConcurrentStreams:          5,
				HTTP2MaxReceiveBufferPerStream:     1 << 10,
				HTTP2MaxReceiveBufferPerConnection: 1 << 17,
			},
			headerRead:       time.Second,
			maxHeaderBytes:   1 << 10,
			keepAlive:        2 * time.Second,
			transferIdle:     3 * time.Second,
			streams:          5,
			streamBuffer:     1 << 10,
			connectionBuffer: 1 << 17,
		},
		{
			name: "NoTimeout",
			cfg: ServerConfig{
				HeaderReadTimeout:   NoTimeout,
				KeepAliveTimeout:    NoTimeout,
				TransferIdleTimeout: NoTimeout,
			},
			headerRead:       0,
			maxHeaderBytes:   64 << 10,
			keepAlive:        0,
			transferIdle:     0,
			streams:          100,
			streamBuffer:     64 << 10,
			connectionBuffer: 1 << 20,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServer(g, tc.cfg)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}

			h := s.http
			if h.ReadHeaderTimeout != tc.headerRead || h.ReadTimeout != 0 || h.WriteTimeout != 0 {
				t.Errorf("timeouts = header %v, read %v, write %v; want %v, 0, 0", h.ReadHeaderTimeout, h.ReadTimeout, h.WriteTimeout, tc.headerRead)
			}

			if h.MaxHeaderBytes != tc.maxHeaderBytes {
				t.Errorf("MaxHeaderBytes = %d, want %d", h.MaxHeaderBytes, tc.maxHeaderBytes)
			}

			if h.IdleTimeout != tc.keepAlive {
				t.Errorf("IdleTimeout = %v, want %v", h.IdleTimeout, tc.keepAlive)
			}

			if s.transferIdle != tc.transferIdle || h.HTTP2.WriteByteTimeout != tc.transferIdle {
				t.Errorf("transfer idle = %v, HTTP/2 WriteByteTimeout = %v; want %v", s.transferIdle, h.HTTP2.WriteByteTimeout, tc.transferIdle)
			}

			if h.HTTP2.MaxConcurrentStreams != tc.streams {
				t.Errorf("HTTP2.MaxConcurrentStreams = %d, want %d", h.HTTP2.MaxConcurrentStreams, tc.streams)
			}

			if h.HTTP2.MaxReceiveBufferPerStream != tc.streamBuffer {
				t.Errorf("HTTP2.MaxReceiveBufferPerStream = %d, want %d", h.HTTP2.MaxReceiveBufferPerStream, tc.streamBuffer)
			}

			if h.HTTP2.MaxReceiveBufferPerConnection != tc.connectionBuffer {
				t.Errorf("HTTP2.MaxReceiveBufferPerConnection = %d, want %d", h.HTTP2.MaxReceiveBufferPerConnection, tc.connectionBuffer)
			}

			if h.ErrorLog == nil {
				t.Error("ErrorLog is nil, want the safelog sink")
			}
		})
	}
}
