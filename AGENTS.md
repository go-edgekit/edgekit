# AGENTS.md

## Current state

The code is a prototype. A rewrite will replace it, and the current tests are
the behavior specification for that rewrite. The [ADRs](docs/adr/README.md)
describe the target design, not the prototype. If the code and an accepted ADR
disagree, report the conflict before you act on it. Do not change the rationale
of an accepted ADR. Write a new ADR that supersedes it.

## Language

Use the terms in [CONTEXT-MAP.md](CONTEXT-MAP.md) and the linked `CONTEXT.md`
files. If you need a word that a `CONTEXT.md` lists under _Avoid_, fix the text
or change the glossary deliberately. Do not use the word.

## Core boundary

- The core MUST NOT contain business policy: no customers, keys, credentials,
  rate limits, quotas, billing, or storage schemas.
- Bundled extensions MUST use only the public contract. If a feature needs a
  core change or private access, the public contract has a gap. Report it.
- A new dependency needs a clear reason and approval. Do not export types
  from external packages.

## Security boundary

- Read the [threat model](docs/threat-model.md) before you change a trust
  boundary, and update it when a protection changes.
- Check input validation, resource limits, timeouts, cancellation, shutdown,
  and memory growth. Do not let attacker-controlled input amplify work.
- Keep protocol behavior RFC-compliant. Document each deliberate deviation.
- Keep error details out of responses. Send diagnostics through
  `internal/safelog`.

## Working rules

- If a requirement leaves behavior or API names unclear, ask before you
  implement.
- Do not commit, push, or merge unless asked.
- Fix the cause of a failing check. Do not delete or weaken tests, add
  `//nolint`, or skip a check to make it pass.
- A performance claim needs a benchmark, with `benchstat` output from before
  and after the change.

## Go style

- Put each field of a multi-line struct literal on its own line.
- Use blank lines to separate guard clauses, the main work, and the return.
  Do not use consecutive blank lines, or blank lines at the start or end of a
  block.
- Errors: the caller adds what it was doing. The callee describes its own
  steps, not itself. Do not put request or user content into error text that
  the code writes.

## Comments and GoDoc

- Write GoDoc, comments, and Markdown in Simplified Technical English
  (ASD-STE100).
- Comments explain why: a constraint, an invariant, or a spec reference.
  Describe the code as it is, not the change, the fixed bug, or the callers.
- The GoDoc of an exported identifier is complete: what it does, when to use
  it, and its contract. The contract includes concurrency safety, ownership
  and lifetime of values, errors, and security implications.
- Show the main entry points with testable `Example` functions.
- The GoDoc of an unexported identifier is one sentence. State what the name
  does not show: a precondition, an invariant, a side effect, or why it
  exists. Do not describe how it works. If the name already says everything,
  write one plain sentence.

## Tests

- A test MUST exercise the condition that its name states.
- A regression test MUST fail without the fix.
- Wait on a deterministic signal, not `time.Sleep`.
- Changes to header, URL, or body parsing include malformed-input and
  boundary cases.
- Use table-driven tests with named cases and `t.Run` when the cases share
  setup and assertions. Write a direct test when a table is less clear.
- Keep assertions in the test body. Use helpers for setup only.

## Report

When you finish, report:

1. The exact commands that you ran, and their results. The minimum is
   `nix develop -c just check`.
2. The trust-boundary impact, even when it is none.
3. The risks to correctness, security, and compatibility.
4. What you did not verify, and why.
