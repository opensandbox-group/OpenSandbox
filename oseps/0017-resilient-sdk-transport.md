---
title: Resilient SDK Transport
authors:
  - "@Pangjiping"
creation-date: 2026-07-22
last-updated: 2026-07-22
status: implementing
---

# OSEP-0017: Resilient SDK Transport

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
- [Design Details](#design-details)
  - [Terminology](#terminology)
  - [Retry Policy](#retry-policy)
  - [Retry Decision](#retry-decision)
  - [Backoff and Jitter](#backoff-and-jitter)
  - [Exception Taxonomy](#exception-taxonomy)
  - [Observability](#observability)
  - [Per-Language Landing](#per-language-landing)
  - [Compatibility](#compatibility)
- [Test Plan](#test-plan)
- [Risks and Mitigations](#risks-and-mitigations)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Introduce a unified, client-only resilience layer in every OpenSandbox
SDK (Python, JavaScript/TypeScript, Kotlin, Go, C#) that absorbs the
class of transport-layer failures whose safety the SDK can locally
prove: TCP reset on pooled keep-alive connections, DNS jitter, TCP
connect failures, TLS handshake failures.

The proposal defines a single cross-language `RetryPolicy` contract, a
decision algorithm asymmetric across idempotent vs. non-idempotent
methods, decorrelated jitter, `Retry-After` handling with a bounded
cap, and an extended exception hierarchy with a machine-checkable
`is_retryable` accessor. No spec change, no wire-format change, no
server change.

For non-idempotent methods (`POST/PATCH`) the default policy retries
**only** on transport failures the SDK can prove happened before any
request byte was sent to the server. Status-code retry for
`POST/PATCH` is opt-in.

## Motivation

An audit of the five SDKs shows that, aside from Go, none has any
HTTP-level retry. Even the Go SDK retries `POST` on `429/502/503/504`
without an idempotency key, which is unsafe once the network fault
produces a "sent request but lost response" race.

Concrete symptoms today:

- A pooled connection reset by an intermediate load balancer surfaces
  as a hard failure to the caller. On Python/JS/Kotlin/C# there is no
  retry at all. On Go the retry may duplicate a sandbox creation.
- Server-supplied `Retry-After` on `429/503` responses is silently
  discarded by every SDK.
- Exception hierarchies do not carry a retryable flag, and network
  errors are either collapsed into a single generic type
  (Python/Kotlin) or leaked to the caller unwrapped (JS/C#).

Scope: absorb routine infrastructure blips whose safety the SDK can
locally prove — DNS jitter, TCP connect failures, TLS handshake
failures, keep-alive RST reaped by a middle box before the first byte
is sent. Any failure mode whose safety depends on server
implementation details (`502`, `503`, `504`, `429`) is opt-in for
`POST/PATCH`: the SDK never silently accepts duplicate-execution risk.

### Goals

- Single cross-language retry policy contract with identical
  semantics in all five SDKs.
- Absorb the class of infrastructure blips whose safety is
  locally provable, without a server-side idempotency store.
- Under the default policy, guarantee `POST/PATCH` are never retried
  on any signal whose safety depends on server implementation
  details. This is a hard contract, not a best effort.
- Explicit opt-in path for callers who accept duplicate-execution
  risk on specific `POST/PATCH` operations (e.g. business-layer
  idempotent, or availability-critical).
- Extend the exception hierarchy with a machine-checkable
  `is_retryable` accessor and three new subclasses (timeout,
  connection, rate-limit), without breaking existing classes.
- Honor server-supplied `Retry-After` (delta-seconds and HTTP-date)
  with a bounded ceiling.
- Emit metrics and logs that make retry activity observable.
- Keep public API changes additive.

### Non-Goals

- Server-side idempotency store, `Idempotency-Key` wire protocol, or
  any server-side change.
- Retrying `POST/PATCH` by default on status codes or post-send
  transport failures (read timeout, write timeout after partial
  send, unexpected EOF). Callers opt in explicitly.
- Trading availability for duplicate-execution risk on the caller's
  behalf. When the trade-off is unavoidable, it is an explicit field
  the caller sets.
- Client-side circuit breaker, hedging, adaptive rate limiting.
- SSE stream resume with `Last-Event-ID` (separate follow-up OSEP).
  This OSEP only requires that SSE parsers stop discarding SSE `id:`
  and `retry:` fields.
- WebSocket transport.
- Changing the wire format of the error body. `X-Request-ID` header
  and `{code, message}` body remain the source of truth.

## Requirements

- Cross-language behavior must be identical for the same input.
- The default policy must retry `POST/PATCH` only on conditions
  provable side-effect free (pre-send transport failures on a
  classifying runtime). No status code triggers a `POST/PATCH` retry
  by default.
- Retries must be opt-out at the caller layer via
  `RetryPolicy.disabled()`, and fully overridable at the client
  level.
- Retry must respect `Retry-After` (delta-seconds and HTTP-date),
  capped by a configurable ceiling.
- The retry loop must be bounded by both attempt count and
  wall-clock budget.
- Caller cancellation (`context.Context`, `CancellationToken`,
  `AbortSignal`, coroutine cancellation) is terminal.
- Existing exception classes and their public fields must remain
  backward-compatible.
- No spec change and no server change.

## Proposal

Two coordinated pieces:

1. **A shared `RetryPolicy` contract on `ConnectionConfig`.** The
   policy governs which requests are retried, how backoff is
   computed, and how `Retry-After` is interpreted. Identical
   defaults, status-code sets, and jitter algorithm across
   languages.

2. **An extended exception hierarchy.** `SandboxException`,
   `SandboxApiException`, `SandboxInternalException` gain three
   subclasses (`SandboxTimeoutException`,
   `SandboxConnectionException`, `SandboxRateLimitException`), and
   every exception exposes an `is_retryable` accessor reflecting
   the SDK's actual retry decision.

The retry engine is implemented once per language, injected at the
layer each SDK already uses for transport concerns (httpx transport,
`openapi-fetch` middleware plus raw-fetch wrapper, OkHttp
`Interceptor`, `net/http.RoundTripper` wrapper, `DelegatingHandler`).
Generated OpenAPI code is untouched.

## Design Details

### Terminology

- **Attempt** — one HTTP request/response cycle. The first attempt
  is the initial send.
- **Retry** — any attempt after the first. `max_retries=N` allows up
  to `N+1` total attempts.
- **Idempotent method** — `GET`, `HEAD`, `PUT`, `DELETE`, `OPTIONS`.
- **Non-idempotent method** — `POST`, `PATCH`.
- **Pre-send failure** — a transport exception raised before any
  request byte was written to the socket. DNS, TCP connect, TLS
  handshake, or RST on a reused keep-alive connection before the
  first write.
- **Opaque transport failure** — a transport exception that the
  runtime surfaces without a reliable pre-send vs. post-send
  signal. Primary case: browser or custom `fetch` returning
  `TypeError: Failed to fetch` with no `cause.code`.
- **Overall deadline** — wall-clock cap across all attempts of a
  single logical operation.
- **Per-attempt timeout** — timeout applied to one individual
  request/response cycle.

### Retry Policy

Every SDK exposes the following configuration surface, with
language-idiomatic casing:

```
RetryPolicy {
  max_retries                            : int      = 3
  initial_backoff                        : Duration = 500ms
  max_backoff                            : Duration = 30s
  backoff_multiplier                     : float    = 2.0
  jitter                                 : enum     = DECORRELATED
                                                      // { NONE, FULL, DECORRELATED }
  retryable_status_codes_idempotent      : Set<int> = { 408, 425, 429, 500, 502, 503, 504 }
  retryable_status_codes_non_idempotent  : Set<int> = { }        // empty; opt-in only
  respect_retry_after                    : bool     = true
  retry_after_cap                        : Duration = 60s
  per_attempt_timeout                    : Duration?= null   // if null, use request_timeout
  overall_deadline                       : Duration?= null   // if null, use caller cancellation
  on_retry                               : Callback?= null   // observability hook
}
```

**Why `retryable_status_codes_non_idempotent` defaults to empty.**
A `502` cannot prove upstream business logic did not run — upstream
may complete a mutation and fail while returning the response, and
the gateway surfaces this as `502`. Whether `429` is emitted before
or after business processing is server-implementation-dependent.
Making status-code retry for `POST/PATCH` opt-in preserves the
invariant that the SDK never trades availability for
duplicate-execution risk without explicit caller consent. Callers
who accept the trade-off set the field, e.g.
`retryable_status_codes_non_idempotent = {429, 502}`; see examples
below.

**`on_retry` callback.** Optional observability hook invoked
synchronously before each retry sleeps. Must be non-blocking;
exceptions from the callback are logged and swallowed. Signature is
language-idiomatic (`Callable[[RetryEvent], None]` for Python,
`(event: RetryEvent) => void` for JS, `(RetryEvent) -> Unit` for
Kotlin, `func(RetryEvent)` for Go, `Action<RetryEvent>` for C#).
`RetryEvent` carries: `attempt`, `retries_used`, `method`, `url`,
`cause` (enum: `PRE_SEND | OPAQUE_TRANSPORT | READ_TIMEOUT |
WRITE_TIMEOUT | UNEXPECTED_EOF | STATUS_408 | STATUS_425 |
STATUS_429 | STATUS_500 | STATUS_502 | STATUS_503 | STATUS_504 |
STATUS_OTHER`), `status_code`, `backoff`, `request_id`,
`exception`.

**Configuration examples (Python).** Semantics identical for
`ConnectionConfig` and `ConnectionConfigSync`.

```python
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.transport import RetryPolicy

# 1. Default policy.
sandbox = Sandbox(config=ConnectionConfig())

# 2. Fully disable retry (fast-fail).
sandbox = Sandbox(config=ConnectionConfig(retry_policy=RetryPolicy.disabled()))

# 3. Override attempts and backoff.
sandbox = Sandbox(config=ConnectionConfig(retry_policy=RetryPolicy(
    max_retries=5,
    initial_backoff=timedelta(seconds=1),
    max_backoff=timedelta(seconds=60),
)))

# 4. Opt in to POST retry on 502 and 429. Use only when the operation is
#    idempotent at the business layer or duplicate execution is acceptable.
sandbox = Sandbox(config=ConnectionConfig(retry_policy=RetryPolicy(
    retryable_status_codes_non_idempotent={429, 502},
)))

# 5. Cap total retry wall-clock budget and per-attempt timeout.
sandbox = Sandbox(config=ConnectionConfig(retry_policy=RetryPolicy(
    per_attempt_timeout=timedelta(seconds=10),
    overall_deadline=timedelta(seconds=45),
)))
```

Per-request override is out of scope for this OSEP. Callers who need
different retry behavior per call site construct separate `Sandbox`
clients.

### Retry Decision

Two invariants apply on top of `RetryPolicy`:

- **Fresh-connection recovery.** An **idempotent** request that
  fails on a reused pooled connection with a pre-send failure is
  retried exactly once on a fresh connection. Subject to
  `RetryPolicy`: `disabled()` skips this too, so fast-fail is truly
  end-to-end.
- **Cancellation is terminal.** Caller cancellation returns
  immediately regardless of `RetryPolicy`.

The `max_retries` parameter counts retries **after** the initial
request, so `max_retries=3` allows up to 4 total attempts.
`retries_used` in the pseudocode is the count already consumed.

```
decide_retry(request, retries_used, outcome, elapsed) -> Decision
  if retries_used >= policy.max_retries:                  return NO
  if policy.overall_deadline and elapsed >= deadline:     return NO
  if caller_cancelled():                                  return NO

  is_idempotent = request.method in { GET, HEAD, PUT, DELETE, OPTIONS }

  if outcome is TransportError:
    if outcome is PreSendFailure:                         return YES
    if outcome is OpaqueTransportError:
      return YES if is_idempotent else NO
    # Post-send transport failures (read/write timeout, EOF).
    return YES if is_idempotent else NO

  # outcome is HttpResponse.
  status_set = policy.retryable_status_codes_idempotent if is_idempotent
               else policy.retryable_status_codes_non_idempotent
  return YES if outcome.status in status_set else NO
```

Decision matrix under the default policy:

| Failure                             | GET/HEAD/PUT/DELETE | POST/PATCH (default) | POST/PATCH (opt-in) |
|-------------------------------------|:-------------------:|:--------------------:|:-------------------:|
| DNS / TCP connect / TLS handshake   | Retry               | Retry                | Retry               |
| Fresh-conn RST (pre-send)           | Retry               | Retry                | Retry               |
| Opaque transport error              | Retry               | No                   | No                  |
| Read/write timeout, EOF             | Retry               | No                   | No                  |
| 408 / 425 / 500 / 503 / 504         | Retry               | No                   | if in caller's set  |
| 429 / 502                           | Retry               | No                   | if in caller's set  |
| 4xx (400/401/403/404/409/422/…)     | No                  | No                   | No                  |
| 501 / 505 / 510 / 511               | No                  | No                   | No                  |

The "opt-in" column applies when the caller has extended
`retryable_status_codes_non_idempotent` beyond its empty default.
Post-send transport failures and opaque transport failures remain
non-retryable for `POST/PATCH` regardless of any status-code
opt-in, because their safety cannot be locally proven at all.

**Opaque transport classification.** Runtimes that can classify
(Node.js undici via `err.cause.code`, Python `httpx.ConnectError`,
Kotlin `ConnectException`, Go `net.OpError` with `Op == "dial"`,
C# `SocketException.ConnectionRefused/HostNotFound`) MUST classify
accurately and use the `PreSendFailure` branch. Only environments
that cannot classify (browser `fetch`, custom `fetch`) fall back to
`OpaqueTransportError`.

`Retry-After` handling (when `respect_retry_after=true`): if the
response carries `Retry-After` (delta-seconds or HTTP-date), the
next wait is `min(retry_after_value, retry_after_cap)`. Unparseable
values are ignored and the computed backoff is used.

### Backoff and Jitter

All three jitter modes share `initial_backoff`, `max_backoff`, and
`backoff_multiplier = M`. Let `n` be the retry index (0-based).

- **NONE**: `sleep_n = min(max_backoff, initial_backoff * M**n)`.
  Deterministic exponential. Provided for testing.
- **FULL**: `sleep_n = uniform(0, min(max_backoff, initial_backoff * M**n))`.
  AWS "full jitter", each sleep independent of the previous.
- **DECORRELATED** (default): `sleep_0 = initial_backoff`;
  `sleep_n = min(max_backoff, uniform(initial_backoff, sleep_{n-1} * M))`.
  Per Marc Brooker's "Exponential Backoff And Jitter"; the AWS
  example uses `M=3.0`, we default to `M=2.0` for gentler growth
  under the "LB flap" target scenario.

Decorrelated jitter is strictly better than symmetric jitter under
coordinated failure and matches AWS's canonical guidance. Callers
who want AWS-exact behavior set `backoff_multiplier=3.0`.

### Exception Taxonomy

```
SandboxException                              # unchanged base
├── SandboxApiException                       # unchanged; adds is_retryable
│   └── SandboxRateLimitException             # NEW; carries retry_after
├── SandboxInternalException                  # unchanged; adds is_retryable
│   ├── SandboxTimeoutException               # NEW; per-attempt or overall
│   └── SandboxConnectionException            # NEW; connect/dns/tls/reset
├── SandboxUnhealthyException                 # unchanged
├── SandboxReadyTimeoutException              # unchanged (health-poll timeout)
└── InvalidArgumentException                  # unchanged
```

**`is_retryable`** reflects the SDK's actual retry decision at
exception-production time. It is `true` if and only if **all** of:

1. The exception's cause is in the applicable retryable set for the
   request method.
2. `retries_used < max_retries` (budget not exhausted).
3. Overall deadline had not fired.
4. Caller cancellation was not triggered.

Consequences: under `disabled()` (`max_retries=0`), every exception
has `is_retryable=false`. On budget-exhausted exceptions,
`is_retryable=false`. Under the default policy, `POST/PATCH` API
exceptions and post-send timeouts produce `is_retryable=false`
regardless of status. Independent of `retry_after_cap`:
`Retry-After: 3600` on a `429` still produces `is_retryable=true`
because the SDK will retry (after the capped delay).

Go SDK's existing `APIError.IsTransient()` is preserved as the
Go-idiomatic accessor.

### Observability

Every SDK emits, at minimum:

- Log at `WARN` on each retry with attempt number, method, cause,
  computed backoff, and `X-Request-ID` if available.
- Counter `opensandbox.sdk.retry.attempts` with dimensions
  `{sdk_language, method, endpoint, cause, idempotent}`.
- Counter `opensandbox.sdk.retry.exhausted` when `max_retries` is
  reached without success.
- Histogram `opensandbox.sdk.retry.backoff_ms`.

Metrics route through OpenTelemetry (OSEP-0010) where available.
The `RetryPolicy.on_retry` callback lets integrators bridge to
custom telemetry without depending on any specific stack.

### Per-Language Landing

Each SDK integrates the retry engine at the transport layer it
already uses. Generated OpenAPI code is not modified.

**Python** (`sdks/sandbox/python`, `sdks/code-interpreter/python`).
Two parallel wrappers: `httpx.AsyncBaseTransport` and
`httpx.BaseTransport`. Both `ConnectionConfig` (async) and
`ConnectionConfigSync` (sync) gain `retry_policy: RetryPolicy` with
identical defaults. `internal/lifecycle_metrics.py` is updated to
reuse the shared transport rather than constructing its own.

SSE clients currently share the same transport as normal clients,
so the retry wrapper would apply to SSE bootstraps by accident. To
fix: each config exposes a transport pair — `transport`
(retry-wrapped, non-SSE) and `sse_transport` (unwrapped inner
transport, SSE). Both share the same underlying
`httpx.AsyncHTTPTransport`/`httpx.HTTPTransport` for connection
pooling; only the retry-wrapper layer differs. SSE adapters MUST
pass `sse_transport` explicitly.

**JavaScript/TypeScript** (`sdks/sandbox/javascript`,
`sdks/code-interpreter/javascript`). Two entry points:
- Generated `openapi-fetch` clients get a retry middleware via
  `client.use({ onResponse, onRequest })`.
- Hand-written raw-fetch call sites for non-streaming JSON and
  downloads (`filesystemAdapter` read paths, `egressAdapter`) go
  through a shared `retryableFetch` wrapper.

The SSE bootstrapper is deliberately excluded from `retryableFetch`
and continues to use plain `sseFetch`, matching the Python and
Kotlin exclusion of SSE clients.

Request-body replayability: unlike Kotlin's OkHttp `RequestBody`
which is re-readable by default, `fetch` request bodies of type
`ReadableStream`/`AsyncIterable` are one-shot. `retryableFetch`
inspects the body type: replayable bodies (`string`, `ArrayBuffer`,
`ArrayBufferView`, `Blob`, `FormData`, `URLSearchParams`, or
`undefined`) retry normally; one-shot bodies fall back to
single-attempt. Callers who want retry for streaming uploads supply
a body factory `() => BodyInit` invoked per attempt. Fresh-conn
recovery still applies on pre-send failures (body never consumed).

The two duplicated SSE parsers are consolidated to a shared
internal module in the same change, with the UTF-8 tail-flush
divergence fixed.

`ConnectionConfig` gains `retryPolicy`.

**Kotlin** (`sdks/sandbox/kotlin`). A new `RetryInterceptor`
implementing `okhttp3.Interceptor` is installed on `httpClient` and
`authenticatedClient` in `HttpClientProvider`; the SSE `sseClient`
is deliberately excluded. OkHttp `RequestBody` is re-readable by
default (`isOneShot() == false`), so replay is safe; one-shot
`RequestBody` subclasses (rare) fall back to single-attempt with
fresh-conn recovery on pre-send. `ConnectionConfig` gains a
`retryPolicy` builder field. Generated `sandbox-api` code is
untouched.

**Go** (`sdks/sandbox/go`). The existing `RetryConfig` in `retry.go`
is corrected against the shared contract:

- Split retryable status set into idempotent and non-idempotent.
  `POST` no longer retries on any status code by default (`429`,
  `502`, `503`, `504` all move to the idempotent-only set) — a
  significant safety change from Go's current behavior. Callers
  who need `POST` retry on `429/502` opt in explicitly.
- Add `408`, `425`, `500` to the idempotent status set.
- Switch jitter to decorrelated.
- `Retry-After` semantics change from `max(computed, header)` to
  strictly honoring the header, capped by `retry_after_cap`.
- Enable retry by default (currently opt-in). Add
  `per_attempt_timeout` and `overall_deadline`.
- Remove `withRetry` from `doStreamRequest`
  (`sdks/sandbox/go/http.go:314`), so SSE bootstraps match the
  Python/JS/Kotlin exclusion. Pre-send failures on the streaming
  path remain covered by fresh-conn recovery.

**C#** (`sdks/sandbox/csharp`, `sdks/code-interpreter/csharp`). A
new `OpenSandbox.Internal.RetryHandler : DelegatingHandler` is
inserted into the `HttpClient` pipeline. `HttpClientHandler` is
replaced with `SocketsHttpHandler` with an explicit
`PooledConnectionLifetime` (mitigating the known DNS-staleness
anti-pattern). `ConnectionConfig` gains `RetryOptions`.
`HttpClientProvider` builds the pipeline once. Duplicated error
mapping in `SseParser` and `CommandsAdapter` is consolidated onto
`HttpClientWrapper.ThrowApiException`.

**SSE Resume (follow-up).** SSE stream resume with `Last-Event-ID`
is deferred to a separate OSEP. This OSEP only requires that every
SDK's SSE parser stop discarding SSE `id:` and `retry:` fields,
exposing `id` to consumers and honoring `retry:` as a reconnect
hint. That unblocks the follow-up without changing this OSEP's
scope.

### Compatibility

- Existing exception classes and public fields unchanged. New
  subclasses inherit from existing bases, so `except
  SandboxApiException` continues to catch rate-limit and API
  errors.
- No wire-format change, no spec change, no server change.
- `RetryPolicy` defaults enable retry for idempotent methods.
  Callers wanting the previous no-retry behavior pass
  `RetryPolicy.disabled()`.
- `RetryPolicy` is a value type; adding fields in future OSEPs is
  backward-compatible as long as new fields default to current
  behavior.
- Go SDK's `RetryConfig` is preserved as a deprecated alias for one
  minor version, then removed.

## Test Plan

**Shared test vectors.** A YAML file under `sdks/testdata/retry-vectors.yaml`
enumerates scenarios (initial method, response sequence, expected
attempt count, expected outcome). Each SDK implements a harness
that consumes this file. Primary correctness gate for cross-language
parity.

**Per-SDK unit tests under the default policy:**

- Retry on 429 for `GET` with `Retry-After: 2`: attempt count = 2,
  backoff = 2s.
- Retry on 500/502/503 for `GET`: attempt count = 4, decorrelated
  jitter backoff.
- Retry on connection reset before first byte for both `GET` and
  `POST`: attempt count = 4.
- Retry on TCP connect refused for `POST`: attempt count = 4.
- Retry on read timeout for `GET`: attempt count = 4.
- **Do not** retry on any status (`429/500/502/503/504`) for `POST`
  under the default policy: attempt count = 1 each.
- **Do not** retry on read timeout for `POST`: attempt count = 1.
- **Do not** retry on 4xx (400/401/404/409) for any method:
  attempt count = 1.
- `Retry-After: 3600` on `GET` capped to `retry_after_cap=60`:
  backoff = 60s. `SandboxRateLimitException.is_retryable = true`
  even though the raw header exceeds the cap.
- Overall deadline: 10 attempts scheduled, deadline fires before
  attempt 6, attempt count = 5, terminates with
  `SandboxTimeoutException`.
- Cancellation during backoff sleep: attempt count = 2, terminates
  with cancellation error.
- Fresh-conn recovery triggers on `ECONNRESET` from a pooled
  connection for `GET` and `POST` (pre-send): attempt count = 2.
- Fresh-conn recovery is **suppressed** under
  `RetryPolicy.disabled()`: attempt count = 1 for `GET` on
  `ECONNRESET`.

**Opt-in policy tests** (caller extends
`retryable_status_codes_non_idempotent`):

- With `{429, 502}`: retry on 429 for `POST`, attempt count = 2;
  retry on 502 for `POST`, attempt count = 4, same body each
  attempt.
- With `{429, 502, 503, 504}`: retry on 503 for `POST`, attempt
  count = 4.
- With `{502}`: **do not** retry on 429 for `POST` (not in set),
  attempt count = 1.
- With `{502}`: **do not** retry on read timeout for `POST`
  (post-send failures on non-idempotent are never lifted by
  status-code opt-in), attempt count = 1.

**Per-language specific:**

- (Python) `ConnectionConfigSync` accepts `RetryPolicy` and applies
  it to `SandboxSync`; same test vectors as `ConnectionConfig`.
- (Python) SSE `POST` bootstrap uses `sse_transport`, not the
  retry-wrapped `transport`. Verified by injecting a counting
  middleware into `transport` and asserting zero calls during a
  streaming command run.
- (JS, with `{502}`) `ArrayBuffer` body on 502: attempt count = 4
  with same bytes. `ReadableStream` body without factory: attempt
  count = 1 (single-attempt fallback). `ReadableStream` with body
  factory: attempt count = 4, factory invoked per attempt.
- (Browser/opaque runtime) `TypeError: Failed to fetch` for `POST`:
  `is_retryable=false`, attempt count = 1. Same error for `GET`:
  retries.
- (Go) SSE bootstrap (`POST /commands` streaming) is not retried
  on 502 even under `{502}` opt-in: attempt count = 1. Confirms
  `withRetry` removal from `doStreamRequest`.

**`is_retryable` and `on_retry` invariants:**

- `SandboxRateLimitException.is_retryable = false` for `POST`
  under default; becomes `true` when caller sets `{429}`.
- `on_retry` invoked exactly once per retry (3 times when
  `max_retries=3` and all retries occur), with monotonic `attempt`
  and `retries_used`, and correct `cause`, `status_code`,
  `backoff`, `request_id`. An exception from the callback does not
  propagate.

**Integration:**

- Kind e2e adds a fault-injection sidecar dropping 1% of
  connections (pre-send RST); default policy succeeds within the
  retry budget for both `GET` and `POST`.
- A variant returns 502 on 5% of requests to `execd`: under
  default, `POST` fails-fast; under `{502}` opt-in, `POST`
  recovers. Both outcomes asserted.

## Risks and Mitigations

- **Coverage gap on `POST/PATCH` for gateway blips.** Under
  default, `POST/PATCH` do not retry on `502/503/504`; a gateway
  flap surfacing as `502` fails to the caller. Mitigated by:
  pre-send failures (DNS/TCP/TLS/fresh-conn RST) are still retried
  on `POST` — most keep-alive blips actually land there; callers
  with business-idempotent operations opt in via
  `retryable_status_codes_non_idempotent`; operators observing a
  material `502` failure rate have concrete signal to consider a
  future server-side idempotency-key OSEP.
- **Retry storm under correlated failure.** Mitigated by
  decorrelated jitter, bounded max backoff, bounded overall retry
  budget.
- **`Retry-After` abuse.** Mitigated by `retry_after_cap`
  (default 60s).
- **Behavior drift between languages.** Mitigated by the shared
  decision function and shared test-vector matrix.
- **Retry masks real bugs.** Mitigated by `WARN` log and metric
  on every retry so operators see the transient rate.
- **Increased latency on unrecoverable failures.** Mitigated by
  bounded `max_retries` (default 3) and `max_backoff` (default
  30s), both configurable.

## Drawbacks

- Retry-by-default for idempotent methods is a behavior change.
  Callers relying on fast-fail semantics on `GET/HEAD/PUT/DELETE`
  will see additional latency. Mitigated by `RetryPolicy.disabled()`.
- The default `POST/PATCH` retry set is smaller than industry SDKs
  (Stripe/OpenAI/AWS retry `POST` on `429/5xx` by default). This
  is deliberate: those SDKs rely on server-side idempotency-key
  stores, which OpenSandbox does not ship. Callers whose
  operations tolerate duplicate execution close the gap via
  `retryable_status_codes_non_idempotent`.
- Adds implementation complexity in every SDK. Mitigated by the
  shared decision function and shared test vectors.

## Alternatives

- **Do nothing.** Rejected. The audit shows the status quo
  produces user-visible failures under routine infrastructure blips
  in four of five SDKs.
- **Broad `POST` retry on all 5xx (Go SDK's current behavior).**
  Rejected. Retrying `POST` on `503/504` without server-side
  idempotency risks silent double-creation of sandboxes, snapshots,
  or command submissions. This proposal narrows Go's set for safety.
- **Retry `POST` on `502` by default.** Considered and rejected.
  `502` is a probabilistic signal, not a logical guarantee: a
  gateway may return `502` after upstream has already begun (or
  completed) processing. The SDK does not silently take that
  trade-off on the caller's behalf. Callers opt in via
  `retryable_status_codes_non_idempotent = {502}`.
- **Server-emitted marker header (e.g. `X-Sandbox-Handled`).**
  Rejected. Requires a server-side change, and marker semantics
  under auth middleware, exception handlers, and streaming
  responses is subtle. The empty non-idempotent default captures
  the safety intent without server involvement.
- **Delegate retry to the underlying HTTP client's built-in retry**
  (OkHttp `retryOnConnectionFailure`, undici, Polly). Rejected.
  Behavior differs across languages; none handles `Retry-After`
  correctly; none respects the idempotent vs. non-idempotent
  distinction. Cross-language parity is non-negotiable.
- **Symmetric jitter (Go's current choice).** Rejected. Decorrelated
  jitter is strictly better under coordinated failure — the failure
  mode this OSEP targets.

## Upgrade & Migration Strategy

**For SDK users:**

- Retry is enabled by default on idempotent methods
  (`GET/HEAD/PUT/DELETE`). Existing code that already tolerates
  transient errors will see fewer failures.
- For `POST/PATCH`, the default retries only on pre-send transport
  failures. Callers who need automatic recovery on gateway blips
  opt in via `retry_policy=RetryPolicy(retryable_status_codes_non_idempotent={429, 502})`.
- Callers relying on fast-fail on idempotent methods opt out via
  `ConnectionConfig(retry_policy=RetryPolicy.disabled())`.
- Go SDK users relying on the previous `POST` retry on
  `429/502/503/504` will see those requests fail-fast now. Two
  migration paths: (1) opt in explicitly for business-idempotent
  operations, or (2) handle the surfaced exception in application
  code.
- Existing exception `except` handlers continue to work; new
  subclasses are strictly narrower than existing bases.

**For server operators:** no server-side change required.

**Language-specific:**

- Go: `RetryConfig` kept as deprecated alias for one minor version.
  Default retryable status set for `POST` narrows from
  `{429, 502, 503, 504}` to `{}`.
- Python/JS/Kotlin/C#: new fields on `ConnectionConfig`; no rename
  of existing fields.
