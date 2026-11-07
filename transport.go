package edgekit

import (
	"net"
	"net/http"
	"time"
)

// The connection settings of the dialer of [NewTransport], and of the egress
// dialer when [EgressPolicy.Dialer] is nil.
const (
	defaultDialTimeout   = 3 * time.Second
	defaultDialKeepAlive = 30 * time.Second
)

// NewTransport returns a new transport with the secure defaults of the core.
// A gateway uses it when [Config.Transport] is nil.
//
// The defaults:
//   - Dial timeout 3s, TCP keep-alive 30s.
//   - TLS handshake timeout 5s.
//   - Response-header timeout 10s. It does not limit the response body, so
//     streamed responses can continue after it.
//   - Response-header size limit 64 KiB.
//   - Expect: 100-continue timeout 1s.
//   - Idle connections: 90s timeout, 100 in total, 16 for each host.
//   - No limit on active connections for each host. An [AdmissionBudget]
//     limits concurrency at the inbound side.
//   - No proxy from the environment, so ambient settings cannot redirect
//     upstream traffic.
//   - HTTP/2 when the upstream supports it.
//   - No transparent compression. The client's Accept-Encoding goes to the
//     upstream unchanged, and the gateway does not decompress responses.
//
// The transport has no egress allowlists, and a failed dial does not return
// a [DialError]; use [NewEgressTransport] for both.
//
// To relax a default, change the field on the returned transport and set it as
// [Config.Transport]. Do not change the transport after it is in use. Each call
// returns a new transport with its own connection pool. NewTransport does not
// copy [http.DefaultTransport], so changes to that value have no effect.
func NewTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   defaultDialTimeout,
		KeepAlive: defaultDialKeepAlive,
	}

	return &http.Transport{
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		ExpectContinueTimeout:  time.Second,
		IdleConnTimeout:        90 * time.Second,
		MaxIdleConns:           100,
		MaxIdleConnsPerHost:    16,
		DisableCompression:     true,
	}
}
