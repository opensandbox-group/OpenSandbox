---
title: Kernel drop telemetry mechanism
description: How egress samples dropped packets without changing nftables enforcement.
---

# Kernel drop telemetry mechanism

The sidecar nft runner instruments existing drop statements only when telemetry is enabled. With `off`, it passes the original script through byte for byte. Counter mode appends a named counter to each original drop, preserving its match, position, and verdict. No accept or policy rule changes.

## Sampling before the verdict

For an address-only or unconditional drop, sample mode inserts two non-terminal rules immediately before the original drop:

```text
add rule inet opensandbox egress ip daddr @deny_v4 meta l4proto tcp tcp flags & (syn | ack) == syn limit rate 20/second burst 40 packets log group 3085 snaplen 128 prefix "osb:deny_set"
add rule inet opensandbox egress ip daddr @deny_v4 meta l4proto udp limit rate 20/second burst 40 packets log group 3085 snaplen 128 prefix "osb:deny_set"
add rule inet opensandbox egress ip daddr @deny_v4 counter name "deny_set" drop
```

Protocol-specific drops receive only reachable sampling branches: TCP matches receive the SYN branch, UDP matches receive the UDP branch. This avoids nft's conflicting-protocol rejection. Established-flow revocation drops receive only a counter. A rate-limit miss falls through to the same unconditional verdict for that original match; sampling never accepts traffic or makes a drop conditional on the limit.

The reason mapper covers each existing rendered drop form and rejects an unmapped form when telemetry is enabled. Sidecar renderer tests verify each drop against an explicit reason map, including strict and blocklisted DoH, port-scoped deny rules, and established-flow revocations.

## Counter ownership

Sidecar counters belong to `inet opensandbox` and are recreated with every full policy replacement.

Counter reads use nft JSON and accumulate positive per-object differences into `egress.blocked.packets`. A smaller value indicates a reset. Replacement owners acquire the telemetry lock and complete the final counter read before planning any remaining DNS lease timeouts. They hold that lock through rendering and submission, so neither the final read nor a periodic poll can delay an already-planned lease transaction; a successful replacement still resets the counter baseline. A failed read does not advance the baseline; a failed transaction does not reset it. This reduces loss but does not provide lossless accounting across replacement or process restart.

## Receiver and capability checks

The Linux receiver uses `github.com/florianl/go-nflog/v2`, copies at most 128 bytes, and parses IPv4/IPv6 plus TCP/UDP headers. It rejects malformed or truncated transport headers and non-initial fragments. IPv6 extension headers are bounded by the capture length. The receiver does not export packet payloads.

The NFLOG group is fixed at 3085 per network namespace, and burst is twice the rate count clamped to [1, 1000]. Startup first binds that NFLOG group, then submits a temporary `inet opensandbox_drop_probe` table containing a regular chain, named counter, rate limit, and NFLOG statement, deleting it in the same transaction. A bind or ruleset failure selects counter mode before any production rules are rendered. Sampling is never removed by retrying a failed production ruleset. Receiver errors report the sampler as unavailable with `cause=receive`; packet drops continue to be counted and enforced. The receiver owns a child context and cancels it before closing its socket, so stopping it does not wait for the parent context or spin on closed-socket errors.

Sidecar audit IDs use the egress process sandbox ID. Metrics aggregate by reason and do not add Subject labels. No detector, quarantine state, or webhook is part of this layer.

Configuration, deployment limitations, and precision limits are described in [the guide](/guides/egress-drop-telemetry).
