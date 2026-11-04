// Package edgekit is a library to build HTTP API gateways.
//
// The core routes inbound requests, selects an upstream, and forwards the
// requests with [net/http/httputil.ReverseProxy]. It applies secure defaults
// before any extension runs. It contains no business policy, such as
// authentication, rate limits, or quotas. Extensions add such policy through
// the public extension points: [Middleware], the lifecycle hooks
// ([RequestReceivedHook], [BeforeUpstreamHook], [UpstreamResponseHook], and
// [RequestCompletedHook]), [UpstreamSelector], and the transport
// ([Config.Transport]). Middleware and hooks of one request share typed
// request state on the [RequestContext].
//
// Use [New] to make a [Gateway] from a [Config]. The configuration is fixed
// when New returns. To change it, make a new gateway.
//
// The API is provisional until version 1.
package edgekit
