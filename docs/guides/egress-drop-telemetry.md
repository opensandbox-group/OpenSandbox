---
title: Kernel drop telemetry
description: Enable opt-in nftables drop counters and sampled IP audit events in egress.
---

# Kernel drop telemetry

Egress can count kernel-enforced drops and sample blocked TCP connection attempts and UDP packets. This feature is disabled by default. It does not detect scans, quarantine workloads, or send webhooks. DNS deny events and their existing webhook are unchanged.

## Configuration

Set these variables on the **egress process**, then restart that process:

| Variable | Default | Meaning |
| --- | --- | --- |
| `OPENSANDBOX_EGRESS_DROP_TELEMETRY` | `off` | `off`: existing rules; `counter`: named drop counters; `sample`: counters plus NFLOG samples |
| `OPENSANDBOX_EGRESS_DROP_LOG_RATE` | `20/second` | Positive integer followed by `/second`, `/minute`, `/hour`, or `/day`; limit per sampling rule |
| `OPENSANDBOX_EGRESS_DROP_COUNTER_POLL` | `15s` | Positive Go duration between counter reads |

The NFLOG group is fixed at **3085** within each network namespace; the receiver filters messages by `osb:` prefix and known reason. For rate `N/unit`, the per-rule burst is `clamp(2 × N, 1, 1000)`: `20/second` gives 40 and `1/day` gives 2. Both the rate limit and burst apply separately to each rule, not as a process-wide budget.

Invalid active configuration fails startup. With `off`, the other drop telemetry variables are ignored. Both active modes require Linux, `NET_ADMIN`, and nftables **1.0.6 or newer** with JSON output (`nft -j list counters table inet <table>`). This minimum supported user-space version follows the Dockerfile's Debian Bookworm base, whose [nftables package](https://packages.debian.org/bookworm/nftables) is `1.0.6-2+deb12u2`; the Dockerfile installs that distribution package without pinning its Debian revision. Sampling additionally requires rate limits and NFLOG (`nfnetlink_log`). The process binds the NFLOG group before testing the rule features in a temporary regular chain. If binding or rule probing fails, it continues in counter mode and reports `sampler_up=0` with `cause=bind` or `cause=ruleset`. Errors after a successful start report `cause=receive`; stopping the receiver closes it without cancelling the parent process context.

::: warning Sampling is partial

Only new TCP SYN packets and UDP packets are sampled. ICMP, other protocols, established-flow revocations, and TCP packets without a connection-opening SYN are counted without samples. Rate limits apply separately to each rule; they are not a process-wide audit-log cap. Samples are best effort and must not be used as an exact packet total.

:::

### Deployment paths

For Kubernetes and Docker sidecars, supply the three variables in the sandbox create request's `env` map. The lifecycle server's egress allowlist routes them to the egress container, keeping them out of the workload environment. Select `dns+nft` or `forward` enforcement to observe kernel drops; DNS-only mode has no nft drop rules to instrument. The default remains `off` when the mode variable is absent.

```json
{
  "env": {
    "OPENSANDBOX_EGRESS_DROP_TELEMETRY": "sample",
    "OPENSANDBOX_EGRESS_DROP_LOG_RATE": "20/second",
    "OPENSANDBOX_EGRESS_DROP_COUNTER_POLL": "15s"
  }
}
```

The fragment above shows only the environment portion of a create request. The server does not provide an operator `[egress.env]` map; its Kubernetes `egress_settings.env` and Docker `extra_env` maps come from these request variables.

## Observe blocked traffic

Enable the existing [OpenTelemetry export](/guides/telemetry) to collect:

| Metric | Attributes | Meaning |
| --- | --- | --- |
| `egress.blocked.packets` | `reason` | Accumulated differences from named kernel drop counters |
| `egress.blocked.samples` | `reason` | Parsed TCP/UDP NFLOG samples emitted to the audit logger |
| `egress.drop_telemetry.sampler_up` | `cause` | `1` when sampling starts; `0` when disabled or unavailable |

Sampled audit logs use `opensandbox.event="egress.blocked_ip"`, with `sandbox_id`, `src_ip`, `dst_ip`, `dst_port`, `proto`, and `reason`. The tuple is the packet header at the drop hook; an INPUT drop after MITM DNAT can show the translated gateway/listener destination.

Reasons are `deny_set`, `port_deny`, `dot`, `doh`, `default_deny`, and `established_revoke`.

Counters are not durable. Egress reads them before planning a table replacement, then resets its difference baseline after a successful replacement. Counter collection finishes before remaining DNS lease timeouts are calculated. Packets arriving between the last read and replacement, including the delete/add transition, can be missing from the exported total. Process restarts, polling failures, and counter resets also limit accuracy. Production kernel compatibility and sample loss under high packet rates require environment-specific validation.

See [the implementation mechanism](/architecture/network/egress-drop-telemetry) for rule ordering and lifecycle details.
