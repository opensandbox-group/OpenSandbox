---
title: Experimental HTTP/2 admission and drain
description: OSEP-0023 HTTP/2 admission, connection retirement, failure containment, and current verification limits.
---

# Experimental HTTP/2 admission and drain

::: warning Experimental implementation
This is an internal, default-off extension of the [OSEP-0023 revision runtime](/architecture/network/egress#transparent-tls-interception-l7). Client-migration acceptance and the full runtime verification matrix remain incomplete. It adds no supported public configuration surface or Fast Sandbox support. HTTP/3, CONNECT and cross-SNI coalescing are outside its scope.
:::

## Compatibility boundary

The adapter supports CPython 3.10 and 3.11 with the shipped direct/lazy connection strategy and exactly `mitmproxy==11.0.2`, `h2==4.1.0` and `hyperframe==6.0.1`. The image pins these dependencies and runs the adapter's version check during its build; missing or mismatched versions fail closed at runtime. Changing the pins requires compatibility verification of the adapter, rather than widening the gate alone. Other interpreters and Python versions, including Python 3.12 and later, are rejected before H2 adapter installation and request admission. The TLS slot, already acquired before ALPN selection, is reclaimed by normal disconnect. Their task scheduling and transport-registration ordering are not covered by this compatibility boundary. Configured chained upstream proxies are rejected for this H2 path because their tunnel and transport ownership is not covered. Existing H1 and legacy behavior is unchanged. Genuine untracked, non-TLS cleartext HTTP/2 remains outside credential admission; rewriting the visible scheme of tracked or decrypted HTTP/2 does not bypass raw-identity validation.

## Admission and identity

The existing `LiveReceiver` → `RevisionPublisher` → registry/receiver path remains the only publication owner, with Registry → Receiver lock order and unchanged authenticated IPC.

A request is admitted when the system `requestheaders` hook obtains its unique handle and immutable snapshot, not when HEADERS are parsed. The registry records the actual inbound client stream ID and a monotonic connection high-water mark. The first host removal freezes that bound, including zero if nothing was admitted. Finished streams, repeated publication and remove/re-add cannot reopen or raise it.

Before credential injection, the adapter validates the original pseudo-headers: HTTPS, one ASCII authority on port 443, actual destination port 443, an optional consistent single Host, and agreement with connection/token SNI. Rewritten scheme or host cannot substitute for original identity. Missing, malformed or unassociated identity fails closed.

## Retirement and drain

1. Before mutation acknowledgement, publication atomically installs the snapshot, new-request fence, frozen stream bound and first retirement deadlines. Acknowledgement does not wait for drain.
2. Valid requests refused at `requestheaders` receive REFUSED_STREAM and do not reach the origin. Previously admitted requests retain their own snapshots.
3. If old requests or their local downstream/upstream output obligations remain, H2-A5 queues one advisory NO_ERROR GOAWAY as a `GoAwayFrame` through that `BoundH2Server` instance's `BufferedH2Connection._prepare_for_sending` and its existing output buffer. It does not call `close_connection` or send h2's `SEND_GOAWAY` input for this notice, so the pinned state machine and old streams remain live. Once old obligations are terminal, ordinary h2 close emits the final GOAWAY and closes the connection. An already-drained connection uses the final GOAWAY as its only notice.
4. At the same instance serialization boundary, every GOAWAY is limited to the minimum of its proposed last-stream ID, the frozen Registry bound when available, and the smallest bound previously serialized on that connection. Public close, internal `_terminate_connection` and protocol-error GOAWAYs retain their error codes; the adapter does not alter `highest_inbound_stream_id`. The local send buffer and ACK do not prove the peer received either notice.
5. Deadline expiry force-closes the whole connection and can interrupt other streams after rotation of a still-covered host. This is abnormal closure, not graceful completion.

Maintenance polling uses the exact connection handler's registered wakeup task. A fenced connection whose notice has not yet been queued is actionable work, even while an old stream is held and no new client traffic arrives. After one notice, idle polling is filtered before dependency event dispatch, so it does not continually refresh the network inactivity watchdog or reset the first deadline. Actionable drain and cleanup work still runs without new client traffic. The integration does not replace dependency classes or globally patch task scheduling.

Response/error hooks alone do not release a handle: DATA, trailers, queued upstream requests and unfinished uploads remain obligations. The adapter observes actual upstream children and client/upstream stream mappings without rewriting their queues or forcing H1 upstreams. Reset-stranded trailers wait only for an existing first deadline, or close abnormally if none exists. An early response followed by reset during an unfinished upload can orphan upstream work; the client and verified owned upstreams then close immediately.

Only the marked kill/NO_RESPONSE path represents normal refusal. Pre-hook 400/413 responses, parser errors and missing identity must not become retryable refusals. Pending identity storage is bounded by the request budget. Exhausting that association budget closes the connection abnormally; ordinary registry capacity denial uses the `requestheaders` refusal path.

### Client retry limitation

The original `go_retry` diagnostic keeps the historical delayed/refusal shape: it requires the new GET to reach the old connection's post-ACK fence before the harness releases old. A previously recorded Go 1.27.2 run repeatedly selected that same connection after REFUSED_STREAM and exhausted an eight-second deadline without reaching the origin. This is useful historical evidence about delayed notice, but it is **not a candidate gate** once early GOAWAY is sent; standard `net/http.Transport` may have already retired the old connection before the new `Do` starts. The separate `go_retry_bounded_old` diagnostic also retains its old fence-dependent release rule and is not a candidate gate.

When a Go client fixture is supplied without explicit `--case` selection, the runtime runner defaults to the two early-notice H1/H2 candidate cases only. The historical `go_retry` and `go_retry_bounded_old` diagnostics require explicit selection and retain separate result groups, so their reuse-specific failure cannot contaminate the candidate gate.

A separate historical bounded-old case passed only when the harness released the old response early. The new GET completed once on a fresh connection with **HTTP/1.1 200**, while the old HTTP/2 response completed intact. This does not prove H2-to-H2 migration, migration while old remains unfinished, or success for arbitrary finite response durations.

The current runner adds `go_early_notice_h1_origin` and `go_early_notice_h2_origin`. Each keeps the old response unfinished, commits removal over authenticated Unix IPC, waits for Go 1.27.2's standard HTTP/2 Framer to parse the NO_ERROR GOAWAY, and only then starts the single new `Do`. The new `Do` may directly use a fresh connection or hit the old fence in the natural parser/transport scheduling race and be retried by the same standard Transport. Acceptance requires a final fresh-connection success, the old response still unfinished at that result, both origin logical IDs exactly once, and complete old body after release. The H2-origin case now holds the first body prefix while its H2 loop continues processing other streams and WINDOW_UPDATE. These new cases remain **NOT_RUN** on the current host; no interoperability result is claimed.

Their `goaway_frame_parsed` record comes from the exact Go 1.27.2 `http2debug=2` Framer read diagnostic after successful standard-library parsing. In this explicit opt-in mode the fixture filters raw diagnostics in memory to an allowlist, drops child stderr and never stores DATA, headers, debug bytes, pointers or raw log lines. This is a parser event, not packet capture, and it precedes `processGoAway`; the old request is the only successful Go connection before the new `Do`, which supplies a causal attribution window. `GotConn` alone is not treated as GOAWAY evidence.

The proxy does not replay requests or change error codes to influence client pooling. The narrower passing case does not replace the failed acceptance gate.

### H2-A5 candidate evidence

The current isolated branch adds the early notice and per-instance monotonic GOAWAY clamp. On the exact pinned libraries, socket-free regressions observed the notice before an old response completed, retained buffered DATA and its request handle, then observed DATA/END_STREAM before the final GOAWAY. Additional cases cover ordinary close, `_terminate_connection`, protocol-error GOAWAY, a previously serialized lower bound, valid post-fence refusal, fragmented CONTINUATION headers, dynamic-table decoding and same-batch post-bound DATA accounting.

The socket-free layer suite uses a manual H2 peer that records but deliberately does not submit GOAWAY to hyper-h2's receive state machine. Its late-frame ordering tests are stress coverage only, not normal-client interoperability. The existing paused-hook stub is likewise not AC-07 acceptance. The opt-in `pre_admission_barrier_h2` runtime case pauses an actual `system.requestheaders` flow before the real `LiveRuntime.acquire_request`; stream 3 is above its frozen bound 1. It commits and ACKs the Registry fence over authenticated Unix IPC, resumes the call, and checks no handle, injection or origin request while an already-admitted old response still completes. Separately, a socket-free test uses the actual `LiveRuntime` and Registry: stream 3 is held before acquire, stream 5 obtains a real handle, revision commit freezes bound 5, then the actual late acquire for stream 3 is denied even though `3 <= 5`; this owner-level test does not simulate wire order, a `system.requestheaders` hook, or IPC. Together they cover the above-bound real-hook/ACK path and below-bound owner race without claiming one test covers both. The runtime barrier and Go cases are currently **NOT_RUN** because canonical loopback port 443 is unavailable without a host privilege change that is not authorized here.

Since that five-file snapshot, the focused Python H2 suite passed 83 tests, the Go 1.27.2 filtered-observer tests passed, and the candidate Go fixture built successfully; these are socket-free/unit results. The real Go H1/H2 held-old cases and Registry admission barrier remain **NOT_RUN** because the runner binds its synthetic origin only to `127.0.0.1:443`; this UID's observed `ip_unprivileged_port_start` is 1024 and an unprivileged bind returned EACCES. No service was stopped and no port fallback was used. The independent environment report records the separate temporary socket capability probe. The GitHub-hosted Go/H2 steps are wired but not yet executed. Current held-old and early-notice runtime results are therefore **NOT_RUN**, not PASS or a newly measured FAIL; candidate green, P2/P3 and H2-to-H2 acceptance remain unclaimed.

## Failure containment

Client disconnect fences admission immediately, but cleanup retains exact ownership until related upstreams are verified terminal. A disconnected hook, a CLOSED flag or an exit code alone does not establish transport closure. Cleanup observes actual task/IO identities and writer-close confirmation; callbacks must remain effective through canceled wakeups while handler registration remains valid.

There are two distinct fatal recovery conditions. Neither releases accounting for resources whose terminal state is unproven.

### Completed-task transport defect

An exceptional once-only `os._exit(86)` of the current mitmdump is permitted only when all of these observations agree on its owner loop:

- The exact live runtime, client/token, adapter, registered handler and actual HttpLayer-owned upstream/task identities match.
- That task was captured before requesting normal close, and the ordinary close command or the same handler's synchronous close was consumed.
- The task has ended, the connection is CLOSED, and the current transport entry still identifies that task and an actual TCP StreamWriter that is not closing. An old pre-connect IO entry is insufficient.
- The runtime's escalation flag has not previously been set.

The flag is set before exit, without logging, IPC, network operations or cleanup hooks in between. The adapter does not signal the Go owner. Normal RST/refusal, pending tasks, canceled pre-connect tasks without IO, closing writers, stale identities and unknown ownership do not satisfy this predicate. Failure to satisfy it is not proof that a handle is safe to release.

### Unverifiable cleanup ownership

If lifecycle observation cannot establish required ownership or prove completed cleanup, the runtime exits the current mitmdump with code 87 instead of leaving it running indefinitely with retained admission budgets. This is a separate condition from the completed-task/current-writer defect above: it does not claim that an open writer was proven. The runtime sets its once-only exit flag before exiting, without releasing unproven resources or logging first. Ordinary stale callbacks after admission replacement or release still use their identity guards. A completed task whose admission is still exactly retained but whose handler/IO ownership no longer matches triggers code 87; an unrelated late callback does not.

### Owner recovery and availability

The existing Go generation owner reaps an unexpected owned-child exit and retains session, readiness and recovery responsibilities. An exit can interrupt other requests served by the same mitmdump process. For a current, running, non-stopping owned child, the experimental runtime latches recovery-required when its quarantine controller is configured (nft-based modes). It attempts namespace quarantine, which can stop all sandbox networking, including loopback, DNS and control/health connections. Recovery from that state requires sandbox destruction and recreation. Without the nft quarantine controller, the MITM readiness gate becomes false on exit, but ordinary child restart may succeed; the same sticky-quarantine recovery behavior does not apply. Neither path guarantees an instantaneous atomic readiness response or automatic restoration. `QuarantineUnknown`, an exit code or a 503 does not establish packet isolation. The boundary excludes arbitrary addon descendants or inherited FDs, origin business completion, replay safety and a strict total reap deadline. Changed addon connection shapes require new ownership verification. See the [revision-runtime warning](/architecture/network/egress#transparent-tls-interception-l7).

## Verification boundaries

Socket-free tests cover registry/publication, the fixed-version adapter, lifecycle, hooks and H1 regressions. They do not prove socket, TLS or IPC behavior.

The opt-in runtime runner uses the actual system addon, authenticated Unix IPC, and SOCKS5 followed by TLS to a real `127.0.0.1:443` destination. Its 15 manual-peer cases cover attachment, removal/drain, rotation, remove/re-add, reset and disconnect with H1/H2 upstreams, short/default expiry and the isolated exceptional-exit case. These passed on an earlier tested tree; they were **NOT_RUN** in the later two-case Go run. Historical results are not fresh verification of subsequent revisions.

Outstanding acceptance includes:

- H2-A5: successful library-driven migration with the old response preserved; the held-old Go case remains failed.
- P2: pre-ACK/paused-hook races, fragmented HEADERS and flow-control variants, upstream queues/unfinished uploads/stranded trailers, and path-only or policy-only rotation.
- P3: Linux transparent-443/original-destination behavior, the restricted production user, applicable IPv4/IPv6 paths, actual Go owner/quarantine readback and the complete relevant H1/policy/credential regression matrix.

Loopback testing, including loopback on Linux, does not establish these production networking properties. Process reap and origin socket closure do not establish kernel quarantine or origin business completion.

The runner reports PASS, FAIL, BLOCKED and NOT_RUN separately, with exact commit/tree and dependency identities. A selected subset passing is not full acceptance. Go observations require a separately built fixture and an explicit exact `--require-go-version`; the runner must not infer that requirement from the binary under test or silently select another toolchain. See the [runtime runner](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/egress/tests/test_credential_bound_h2_runtime.py) and [Go fixture instructions](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/egress/tests/fixtures/h2_retry_client_README.md) for execution requirements.
