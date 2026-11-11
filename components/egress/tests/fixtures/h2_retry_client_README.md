# Fixed Go HTTP/2 GET observer

This fixture uses the standard Go 1.27.2 `net/http.Transport` and one `http.Client.Do` per logical GET. It never replays a request in application code and makes no non-idempotent safety claim. Build the single `h2_retry_client.go` file using the independently verified Go 1.27.2 compiler; pass its exact `go1.27.2` identity with `-require-go-version`. The program rejects any runtime mismatch. It has no non-standard dependencies or implicit toolchain fallback.

The fixed topology is `127.0.0.1:<SOCKS port>` to the actual `127.0.0.1:443` origin, with TLS SNI/authority `bound.example.com`. The synthetic CA bundle contains the MITM and origin test roots. TLS verification, standard `Transport` TLS setup, and automatic HTTP/2 negotiation remain in place. No environment proxy, redirect, alternate target, insecure TLS, or Go module dependency is used.

## Case roles

- When `--go-client` is supplied without explicit `--case` arguments, the runner selects only the two `go_early_notice_*` candidate cases; historical diagnostics run only when named explicitly.
- `go_retry` retains the earlier delayed/refusal diagnostic: new GET must hit the old connection's post-ACK fence before the fixture releases the held response. It is historical diagnostic evidence, not a candidate acceptance gate; Go Transport scheduling may legitimately change after early GOAWAY.
- `go_retry_bounded_old` retains the old bounded-release diagnostic, which releases only after the same old-connection fence. It does not prove arbitrary held-old migration and is also not a candidate gate.
- `go_early_notice_h1_origin` and `go_early_notice_h2_origin` are separate held-old interoperability cases. The source holds old's body before END_STREAM. After authenticated removal ACK, the fixture waits for Go's standard HTTP/2 Framer to parse the production GOAWAY, then starts exactly one new `Do` while old remains unfinished. The new request may use a directly fresh connection or be refused on the old connection in a natural race and then retried by the same standard Transport. It must ultimately complete on a fresh connection before old is released. H1-origin and true H2-to-H2 results remain separate.
- `pre_admission_barrier_h2` is a production-addon/LiveRuntime/Registry case driven by the manual peer. It pauses stream 3 before the real `LiveRuntime.acquire_request`; its only admitted old stream is 1, so this IPC case proves the post-ACK hook path above frozen bound 1. It commits and reads back the fence over authenticated Unix IPC, then resumes the real admission call. It is not a normal-client interoperability claim or a below-bound proof.

The socket-free lifecycle suite separately tests a lower-numbered late admission through the actual `LiveRuntime` and Registry owners: stream 3 is held before acquire, stream 5 receives a real handle, the new revision fences the connection at bound 5, then acquire for stream 3 is denied. That owner-ordering test does not simulate wire order, a `system.requestheaders` hook, or IPC.

The runtime suite's `ManualH2Peer` records GOAWAY but deliberately does not feed it to hyper-h2's protocol state machine. Its frame-order and late-frame cases are protocol stress tests, not compliant-client migration evidence. Only the standard Go cases above provide client parser/interoperability evidence.

## GOAWAY parser-event filtering

The default fixture rejects every non-empty `GODEBUG` and does not enable Go HTTP/2 diagnostics. The two `go_early_notice_*` runner cases explicitly pass `-observe-goaway-parse` and set exactly `GODEBUG=http2debug=2`. For the pinned Go 1.27.2 standard library, the fixture filters the `Framer ...: read GOAWAY` parser log into a fixed JSON allowlist: frame type, stream ID zero, last stream ID, NO_ERROR code, run-local ordinal, and a monotonic elapsed time. Unsupported or oversized lines become fixed redacted observer errors. DATA, headers, GOAWAY debug bytes, pointer IDs, raw log lines, and other diagnostics are discarded in memory; child stderr is discarded and raw logs never enter artifacts.

This is a **standard-library parser event**, not wire capture. The Framer logs after parsing and before the H2 transport processes GOAWAY; the one old H2 connection is the only successful Go connection before the new `Do` begins, so the pre-request event is attributed by that causal window. Final request success, old completion ordering, `httptrace`, and origin attempt counts are checked separately. A `GotConn` event alone is never treated as GOAWAY proof.

## Control and evidence

CLI:

```text
h2_retry_client -require-go-version go1.27.2 \
  -socks-address 127.0.0.1:<port> -ca-file <combined-ca.pem> \
  -timeout 25s -new-timeout 8s [-observe-goaway-parse]
```

Keep stdin open. Wait for `ready` and verified H2 `old_headers`; after the parent confirms authenticated removal, send one JSON line:

```json
{"command":"continue_new"}
```

The early-notice cases wait for a filtered `goaway_frame_parsed` event before sending that command. The fixture emits sanitized request/result events only: `do_calls`, `got_conn_count`, opaque connection IDs, write count, HTTP status and protocol, body hash/completeness, and bounded error classes. Neither headers, body bytes, arbitrary errors nor Go's raw HTTP/2 diagnostic lines are saved. Go 1.27.2 omits zero-valued frame stream IDs from this debug summary; the filtered event records `stream_id=0` because the standard Framer successfully parsed the frame as GOAWAY, whose parser rejects a nonzero stream ID. `done.outcome=observed` and exit 0 mean the fixture finished collecting results; only the parent runner's individual assertions can decide whether a case passed.

The H2 origin keeps the old stream open while continuing to read the same H2 connection, accept other streams, and process flow-control updates. Once the new result or its fixed deadline is observed, the parent releases old; the origin sends the remaining bytes within the stream window and then END_STREAM. The source records one attempt per logical ID and reports H1/H2 separately.

The runtime runner still requires an explicitly authorized environment with canonical loopback port 443 available. It has no high-port fallback. See the [versioned integration record](../../../../docs/architecture/network/osep-0023-h2-integration.md#observed-go-retry-limit-and-staged-delivery) for prior held-old results and outstanding acceptance limits.
