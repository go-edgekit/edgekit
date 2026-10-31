# Gateway

A framework for routing and proxying HTTP requests, with extensions for behavior outside the minimal gateway responsibilities. This glossary distinguishes request execution from observations and application-level conclusions.

## Language

**Gateway-managed handling**:
The processing of an inbound request governed by the gateway, including its registered extensions. It excludes work performed independently by callers outside that boundary.

**Request completion**:
The end of gateway-managed handling, whether it finishes normally or aborts. It does not establish that the client received the entire response or that a usage event was durably recorded.
_Avoid_: Delivery acknowledgment, billing confirmation

**Selected upstream**:
The destination chosen for a request. Selection alone does not prove that a connection was attempted or that the destination received the request.
_Avoid_: Contacted upstream, unless contact is independently established

**Execution fact**:
Information established by gateway execution, such as a matched route, a selected upstream, an explicit rejection, or an observed failure. It does not include inferred delivery or application-level meaning.

**HTTP observation**:
An observed HTTP status, accepted response-body byte count, or response-write error at a particular observation point. Upstream and downstream observations may differ and do not guarantee complete client delivery.

**Interpretation**:
A classification derived from execution facts and HTTP observations for purposes such as logging, analytics, or billing. It is not itself an authoritative fact about gateway execution.
