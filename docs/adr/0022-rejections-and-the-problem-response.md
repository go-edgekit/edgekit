# ADR-0022: Define extension rejections and the problem response

- **Status:** Accepted
- **Date:** 2026-11-04
- **Decision owners:** Project maintainers
- **Amends:** [ADR-0018](0018-completion-observes-gateway-rejections.md) (the contract of the rejection reason) and [ADR-0013](0013-typed-rejections-and-replaceable-renderer.md) (the shape of the rejection and the renderer)
- **Supersedes in part:** [ADR-0017](0017-failure-status-by-source.md) (the sentence "All failure responses have a generic body: the status text, without failure details.")

## Context

ADR-0013 decides that a rejection carries a status, response headers, and a public reason code, and that one replaceable renderer writes an RFC 9457 `application/problem+json` body. It does not define the Go shape, how a hook returns a rejection, the validation, or what the renderer can do.

Three accepted texts disagree:

- ADR-0017 says that all failure responses have a body that contains the status text. ADR-0013 says that the default body is `problem+json`.
- ADR-0018 says that only the core sets the rejection reason, and only from exported constants of the core. It also says that the typed rejection of ADR-0013 supplies a reason of the same type. Both cannot be true when an extension rejects a request with its own code.
- ADR-0009 says that the core must not replace a response after the final headers are committed. A renderer that writes to the response writer can forget the status, so that the server sends 200, or can write after a commit.

A hook can give a rejection to the core in the error that it returns, or as a separate result. In the error, the rejection and a failure share one `error` value. The core then needs rules that exist only because of this: which rejection counts when an error tree, such as one from `errors.Join`, contains more than one; what a rejection from an upstream selector or a transport means; and which error an invalid rejection gives. Wrapping alone does not cause the problem, because `fmt.Errorf("lookup: %w", err)` keeps the identity of the error for `errors.As`. The difficulty is the error chains that mix a rejection with a failure, and the precedence rules that they need.

The glossary says that a rejection is not an unexpected failure, and not to call a deliberate refusal an error. Go's `errname` linter requires that an exported error type has a name that ends in `Error`.

## Decision

### Rejection

`Rejection` is a plain struct with `Status`, `Reason`, and `Header`. It does not implement `error`. `RequestReceivedHook`, `BeforeUpstreamHook`, and `UpstreamResponseHook` (the processing hooks) return `(*Rejection, error)`:

| Rejection | Error | Result |
| --- | --- | --- |
| nil | nil | Handling continues. |
| non-nil | nil | The core checks the rejection, then rejects. |
| nil | non-nil | A failure. |
| non-nil | non-nil | A failure. The core ignores the rejection and does not check it. |

- Only the processing hooks can reject. An upstream selector and a transport have no rejection result.
- When the error is not nil, the core keeps it without changes in `Failure.Err`. It does not check the ignored rejection, so a problem of the rejection cannot replace the original failure.
- A rejection stops the hooks that come after it in the run order of its phase (ADR-0021). Middleware unwinds, and completion runs once.
- The core reads the rejection when the hook returns, copies the header map before it removes fields, and never changes the rejection or its header map. A shared rejection, such as a package variable, is safe for concurrent use only while nobody changes its fields or its header map.
- Rejections from middleware stay deferred. They need a public operation that renders the rejection and records its reason at completion; a value type alone does not give that.

### Validation

A rejection is valid when all of these are true:

- The status is from 400 to 599.
- The reason has the form `<extension>.<check>`. Each of the two parts starts with a lowercase ASCII letter and contains only lowercase ASCII letters, digits, and `_`. The complete code has at most 64 bytes.
- Each header name contains only ASCII letters, digits, and `-` (R3), and no header value contains a control character other than horizontal tab.

An invalid rejection stops the later hooks of the phase and gives 500. Completion gets a `Failure` in the phase of the hook, whose error the core makes and which wraps `ErrInvalidRejection`. The error names the broken rule (status, reason, or header), but contains no value of the rejection. The core writes no diagnostic for it. `RejectionReason` stays empty.

The core removes these header fields from a valid rejection without a failure, as it does after the response hooks (ADR-0019): `Connection` and each field that it names, the other hop-by-hop fields, `Alt-Svc`, `Content-Length`, `Content-Type`, `Transfer-Encoding`, and `Trailer`. The renderer owns `Content-Type`, and the server owns the framing. Each other rejection header replaces the value that middleware set for the same name.

### Rejection reason

The contract of ADR-0018 changes as follows. The other rules of ADR-0018 stay.

- The core sets the reason for its own rejections and for each valid rejection from a hook. Middleware responses do not set it.
- The codes of the core are exported constants of the core and never contain `.`. The codes of extensions always contain one `.`. So the two sets cannot collide, also when the core adds codes later, and an observer can tell the source of a code.
- A code is a static public name for the check that refused the request. It is never an identifier, such as a key ID or a customer, and never a request value. The syntax limits what a code can contain, but it cannot prove this rule. The extension is responsible for it.
- A valid rejection does not set `Failure`, and completion gets no cause for it. A hook that needs the cause later keeps it in request state.

### Renderer

- The renderer makes only the body: `func(ctx context.Context, p Problem) (contentType string, body []byte)`. `Problem` has the status and the rejection reason, which is empty for a failure. It has no failure details, so the renderer cannot leak them.
- The core owns the rest. It removes `Content-Length` and `Content-Type` in all letter cases, applies the rejection headers, sets `Content-Type` and `X-Content-Type-Options: nosniff`, writes the status, and writes the body. It does not set `Content-Length`, because middleware can compress the body.
- The core calls the renderer for every response that the gateway makes itself: secure-default rejections, routing misses, rejections and failures of hooks, selector failures, round-trip failures, protocol-switch failures, and a lost gateway context. Successful local responses and ordinary middleware responses stay in middleware.
- For a secure-default rejection, the context is the entry context with a gateway context that has only the start time. The core gives no additional request content to the renderer. The entry context can contain values that the caller added.
- When the content type is empty or is not a valid header value, the core uses the default renderer for that response. It does not log the value.
- The core never calls the renderer after the final response headers are committed: after a final status, a body write, a flush, or a hijack of the connection. It records the failure for completion only. The core sees a commit only when it happens in the proxy. A middleware that calls `next` must not commit the response before it does so, because the core cannot see that commit. The core does not wrap the response writer of the middleware to see it: a wrapper hides optional interfaces, such as `http.Flusher` and `http.Hijacker`, from middleware that finds them with type assertions.
- A panic in the renderer continues, as a panic in a hook does. Completion runs, and the request is aborted.

### Default body

The default renderer writes `application/problem+json` with the members `type`, `title`, `status`, and, for a rejection, `reason`:

```json
{"type":"about:blank","title":"Too Many Requests","status":429,"reason":"ratelimit.exceeded"}
{"type":"about:blank","title":"Bad Gateway","status":502}
```

The `title` is the status text of `net/http` (RFC 9457, section 4.2.1). It is absent when that text is empty, as for 499. There is no `detail` and no `instance`, so the body never contains failure details or the request path. `reason` is an extension member. RFC 9457, section 3.2, requires that clients ignore extension members that they do not know.

This replaces the plain-text status-text body of ADR-0017. The body still contains no failure details.

## Consequences

**Positive:** One response format for all gateway-owned responses. Extensions can send correct 401 and 429 responses with their headers, and access logs see their reason. A replacement renderer cannot change the status, forget a rejection header, or write after a commit. A caller cannot confuse the recognition of a rejection with the inspection of an error chain, and the decision paths of a hook are explicit at the return statement.

**Negative:** Every error response changes from plain text to `problem+json`. Completion observers now see reason codes from extensions. The renderer cannot negotiate the content type from the request, and it cannot add headers. Every processing hook returns two values, also a hook that never rejects (`return nil, nil`). The type does not guarantee the decision: a hook can still return a rejection where it should return a failure, such as an "unknown key" rejection after a key store failed. Extensions must test these paths separately.

## Alternatives considered

- **`Reject(Rejection) error`, with the rejection found by `errors.As` in the error of a hook:** One result, but it needs the error-chain rules in the context: the first match of more than one rejection, rejections from selectors and transports, and the error of an invalid rejection.
- **`*Rejection` that implements `error`:** The most common Go shape, and the value stays usable outside hooks. It needs the same error-chain rules and must handle a typed nil `*Rejection`.
- **A rejection in the error, recognized only without unwrapping:** The core accepts the rejection only when the returned error is the rejection itself, so a wrapped rejection is a failure and an error always wins. It breaks the expectation that `%w` keeps the identity of an error: `fmt.Errorf("ratelimit: %w", r)` becomes a 500 without a warning.
- **An opaque decision value, such as `Continue()` and `RejectWith(r)`:** It has no nil pointer and could later carry successful local responses. It adds a type and two names for what `nil` already says, and no requirement needs local responses now (ADR-0003). The pointer result follows the callbacks of the standard library where a nil result means "use the default": `http.Transport.Proxy`, `tls.Config.GetCertificate`, and `tls.Config.GetConfigForClient`. The `nilnil` linter stays enabled for the core and is excluded only where hooks are implemented.
- **An exported error type `RejectionError`:** It satisfies the linter, but it calls a deliberate refusal an error, against the glossary.
- **A general hook-result type:** It could also carry successful local responses, but no requirement needs that now (ADR-0003).
- **Reason codes from core constants only:** The access-log and quota extensions could not tell why an extension refused a request.
- **A reserved list of core codes instead of the `.` rule:** A core code that is added later can collide with an extension code.
- **A cause for the rejection at completion:** ADR-0018 rejects a full rejection value at completion; the hook can keep its own cause in request state.
- **A renderer that writes to the response writer:** It can send 200 when it writes nothing, and it can write after a commit.
- **A type URI for each reason:** It needs a URI namespace that the project owns for codes that extensions define.

## Validation / follow-up

Tests verify the four result combinations in each processing phase, that an ignored invalid rejection does not replace the original failure, that an invalid rejection stops the later hooks without values in the failure or in diagnostics, each validation rule, that a shared rejection does not change, header removal on HTTP/1.1 and HTTP/2, the exact default body, `HEAD`, a custom renderer for each source, the fallback for an invalid content type, and no rendering after a commit through a write, a flush, or a hijack.

The key-authentication extension must test a store failure and an unknown key separately. P2 of the threat model (equivalent rejections for missing and unknown keys) is a separate requirement of that extension.

**Run order 2026-11-07:** [ADR-0030](0030-typed-hook-phase-lists.md) supersedes ADR-0021. A rejection stops the hooks that come after it in the written order of its phase list.
