# Access policy

Bundled extensions that decide who a caller is and how much they may use. This is business policy: it is optional and never part of the core.

## Language

### Identity

**API key**:
A credential a caller presents to identify itself. One customer may hold many API keys.
_Avoid_: Token, session

**Key ID**:
The public identifier of an API key, separate from its secret. Subjects and analytics use the key ID, never the secret.
_Avoid_: Key hash, session ID

**Customer**:
The party that owns one or more API keys and to whom shared limits apply.
_Avoid_: Account, user, tenant

**Subject**:
A label for something a limit applies to, such as one API key, one customer, or a customer on one API. Identity extensions publish the subjects of a request; limits never interpret them.
_Avoid_: Identity, principal

### Limits

**Limit**:
A rule that allows or denies a request for one subject. Rate limits and quotas are both limits.
_Avoid_: Policy (as API-management products use it: a bundle of limits and access rights)

**Rate limit**:
A limit that protects capacity over a short rolling window. It may be approximate and does not reset on a calendar.
_Avoid_: Throttle

**Quota**:
A limit that grants an entitlement over a calendar period, such as a month. It requires exact counting and resets at period boundaries.
_Avoid_: Allowance, rate limit

**Limit evaluation**:
The check of every applicable limit for a request, each independently. A request passes only when all limits allow it; limits are never merged into one effective limit.
_Avoid_: Policy merging, "lowest wins"

**Limit source**:
The provider that answers which limits apply to a request's subjects on a route. Its answers may come from configuration or from policy data.

**Policy data**:
Keys, customers, and limit assignments that change while the gateway runs, without a configuration change.
_Avoid_: Configuration

**Consumption**:
The use of a limit's capacity by a request. A request consumes from its limits only when limit evaluation allows it, at admission before proxying.

**Refund**:
The return of consumed quota after completion, based on an interpretation of the request's outcome.
