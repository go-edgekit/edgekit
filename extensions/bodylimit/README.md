# Streaming request-body limit

`bodylimit.New(maxBytes)` returns ordinary gateway middleware. Register it globally or on selected routes; the example executable uses an 8 MiB limit on `/api/`. Negative limits panic at construction; zero allows only empty bodies.

- A declared `Content-Length` above the limit receives a local generic 413 before downstream handling. This is an HTTP observation, not a gateway hook error.
- Other bodies use `http.MaxBytesReader`. No body is buffered or read before calling `next`; the response writer is passed through unchanged.
- If proxy transport reading encounters `*http.MaxBytesError`, the framework records that original error in `Outcome.Err` and renders a generic 413 while final headers can still be changed. Completion runs after middleware unwinds.
- At most the configured number of body bytes can be read by the upstream transport. An unknown-length request can already have sent a partial body upstream when it exceeds the limit. This is not atomic rejection: upstreams must handle truncated uploads safely.
- The limit is on bytes read, not a promise to consume every body. If an upstream responds early without reading the excess, a 413 is not guaranteed. The limit does not bound decompressed content, headers, time, or concurrency.

Choose limits for each application. Large uploads can use a different limit or omit this middleware. Protect slow uploads separately with a streaming-aware deployment policy; do not introduce asynchronous middleware or a blanket response deadline.
