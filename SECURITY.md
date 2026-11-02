# Security Policy

## Project Status

edgekit is a prototype. A rewrite will replace the current code. Do not use it
in production. There are no releases yet.

## Supported Versions

Security fixes go to the `main` branch only. When releases start, only the
latest release of each module will receive security fixes.

## Go Toolchain Security

The minimum Go version in `go.mod` describes source compatibility, not security
support. Build with the latest patch release of a supported Go version; see
https://go.dev/doc/devel/release. Updating edgekit does not update the standard
library in your binary. Rebuild with the patched toolchain.

## Scope

Report issues that can affect the safety of a gateway built with edgekit, for
example:

- Request smuggling, or a difference between how the gateway and the upstream
  read a request.
- Spoofed forwarding or managed headers that reach the upstream as trusted.
- Proxying to a destination that the configuration does not allow, such as an
  egress allowlist bypass.
- Denial of service through missing or ineffective resource limits.
- Exposure of sensitive data through responses, logs, or error values.
- An extension that gets access beyond the public contract.

Known gaps are recorded in [docs/investigations/](docs/investigations/). A
report that only repeats a known gap is not a new vulnerability, but a report
that shows a gap is worse than recorded is welcome.

## Reporting a Vulnerability

Report vulnerabilities through GitHub private vulnerability reporting:
https://github.com/go-edgekit/edgekit/security/advisories/new

Include the commit, the Go toolchain version, the gateway configuration and
extensions, the request that causes the problem, the expected result, and the
actual result. Do not publish details before triage.

## Response Timeline

We aim to acknowledge reports within 5 business days and to give a status
update within 10 business days.
