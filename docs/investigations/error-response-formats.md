# Error response formats in other gateways

**Status:** Investigation from 2026-11-04, not an architecture decision. The decision is in [ADR-0022](../adr/0022-rejections-and-the-problem-response.md).

## Question

The core sends its own responses for rejections and failures through one replaceable `Renderer`. The renderer gets the status and the rejection reason, and returns a content type and a body. This investigation asks how other gateways and frameworks answer two questions:

1. Who chooses the representation of an error response, and can it depend on the request, for example on `Accept`?
2. Can code that rejects a request add structured public data to the body, and who shapes that data?

The sources are the public documentation of each project, read on 2026-11-04. Statements marked _not confirmed_ come from knowledge of the API, not from the documents that were read.

## Findings

| Project | Who chooses the representation | Structured data from a plugin | Mechanism |
| --- | --- | --- | --- |
| Kong | The core, from `Accept`. `kong.response.error(status, message, headers)` writes JSON, XML, HTML, or plain text. A `Content-Type` in `headers` has priority, then `Accept`, then JSON. | No: a message only. `kong.response.exit(status, body, headers)` sends any body; a table body is encoded as JSON with `application/json`, outside the negotiated templates. | One template for each format for the complete gateway (`error_template_json`, `_html`, `_xml`, `_plain`), with at most two `%s` placeholders: the message and the request ID. |
| Envoy | Configured rules. The mappers of `local_reply_config` are checked in order; the first match can override the body format (`text_format` or `json_format`, with an optional `content_type`). | No: substitution placeholders such as `%RESPONSE_CODE%` only. | The filter of a mapper is an `AccessLogFilter`. The document shows only `status_code_filter`. A `header_filter` that could match `Accept` exists in the access-log filter API: _not confirmed_ for mappers. |
| Caddy | Configured routes. `handle_errors` runs ordinary routes with the placeholders `err.status_code`, `err.status_text`, `err.message`, and `err.trace`. | No. | A request matcher on `Accept` inside `handle_errors` could choose the format: _not confirmed_. |
| Traefik | An external service. The `errors` middleware sends the configured statuses to a service, with `query` such as `/{status}.html` and the variables `{status}`, `{originalStatus}`, and `{url}`. | No. | The error service owns content negotiation. Which request headers Traefik forwards to it: _not confirmed_. |
| A template-based API gateway | Templates, selected by the status and by the `Content-Type` of the request, not by `Accept`. The default is JSON. Newer versions add rules for each status that name a template and set headers. | No. | Go templates in a directory of the gateway. |
| APISIX | The plugin. `core.response.exit(code, body, headers)` takes a structured body. | Yes. The `exit-transformer` plugin passes the status, body, and headers of each response that the gateway makes through user functions. | The data and the final shape are separate, but a transformer can change everything, also the status. |
| Spring (`ProblemDetail`) | The framework, through message converters. It prefers `application/problem+json`. | Yes: extension properties on `ProblemDetail`. HTML is not supported for problem details. | The data object and the serializers are separate. |
| Huma (Go) | The application. `huma.NewError` is a replaceable function that returns a custom error model. | Yes: the custom model. An error that implements `HeadersError` adds headers. | The nearest Go equivalent to `Renderer`. |

## Patterns

**Selection of the representation.** The projects use four approaches:

- One fixed format, as the default of Envoy and of the template-based gateway.
- Configured matching rules, as in Envoy and Caddy.
- Negotiation in the core, with `Accept`, over a fixed set of formats, as in Kong and Spring.
- Delegation to another service, as in Traefik.

In a library, configured matching rules are code. A `Renderer` is that code: it can already select a format by route through the gateway context. Only the `Accept` input is missing for negotiation.

**`Vary: Accept`.** None of the documents that were read mention `Vary` for negotiated error responses, also not the documentation of Kong, which negotiates. RFC 9110, section 12.5.5, expects `Vary` when the representation depends on a request header. Without it, a shared cache can give one client the error representation that another client asked for.

**Structured data.** The projects use two models:

- **Message only:** Kong (`error`), Caddy, Envoy, Traefik, and the template-based gateway. The format stays consistent, but a plugin cannot send structured public data.
- **Data that a separate component shapes:** Spring, Huma, and APISIX. A plugin can send structured data, and one component controls the representation.

**Raw bodies from plugins.** `kong.response.exit` sends a body outside the negotiated templates. So the responses of one gateway can have different formats, which depend on the plugin that answered. In edgekit, a rejection carries data and only the renderer makes a body, so this cannot occur.

## Consequences for edgekit

The current design follows the second data model: a structured decision (`Rejection`) and a separate representation (`Renderer`). It already supports one custom format for each gateway, such as JSON:API (see `ExampleRenderer`), and a format for each route.

Two gaps remain. Close each one only when a concrete extension needs it, with an ADR:

- **Negotiation by `Accept`.** Give the renderer the `Accept` values, and make the core add `Vary: Accept` when the representation can depend on them. This covers negotiation as in Kong and rule-based selection as in Envoy and Caddy, without a fixed set of formats. The `Accept` values are request content; ADR-0022 gives the renderer no request content for a secure-default rejection, so this needs an explicit change to that rule.
- **Public data on a rejection.** An optional typed value on `Rejection` that the core passes through `Problem` to the renderer, for example for a JSON:API `meta` member or a problem extension member. It needs a strict contract, as `Reason` has: public data only, never request content or failure details. For rate limits, the `Retry-After` and `RateLimit` headers already carry the values, so the first access-policy extensions may not need it.

Do not add a raw-body escape for extensions. It would make the format of a response depend on the extension that answered.

## Sources

- Kong: [configuration reference](https://docs.konghq.com/latest/configuration), [PDK `kong.response`](https://developer.konghq.com/gateway/pdk/reference/kong.response/), [pull request 10374](https://github.com/Kong/kong/pull/10374)
- Envoy: [local reply modification](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_conn_man/local_reply)
- Caddy: [`handle_errors`](https://caddyserver.com/docs/caddyfile/directives/handle_errors)
- Traefik: [errors middleware](https://doc.traefik.io/traefik/reference/routing-configuration/http/middlewares/errorpages/)
- APISIX: [`exit-transformer`](https://apisix.apache.org/docs/apisix/plugins/exit-transformer/), [pull request 13343](https://github.com/apache/apisix/pull/13343)
- Spring: [error responses](https://docs.spring.io/spring-framework/reference/web/webflux/ann-rest-exceptions.html), [Spring Boot issue 32634](https://github.com/spring-projects/spring-boot/issues/32634)
- Huma: [response errors](https://huma.rocks/features/response-errors/)
- RFC 9110, section 12.5.5 (`Vary`), and RFC 9457 (problem details)
