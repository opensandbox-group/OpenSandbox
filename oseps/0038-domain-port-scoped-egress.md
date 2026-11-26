---
title: Domain and TCP Port Egress Rules
description: Scope DNS-learned egress grants to TCP destination ports.
authors:
  - "@cwj2001"
creation-date: 2026-11-16
last-updated: 2026-11-26
status: draft
---

# OSEP-0038: Domain and TCP Port Egress Rules

Tracking issue: [#3123](https://github.com/opensandbox-group/OpenSandbox/issues/3123)

## Summary

Combine domain and wildcard `target` values with the existing `ports` field. The proposed scope is `allow` rules with `defaultAction: deny` on Classic `dns+nft` without a chained upstream proxy.

## Motivation

An IP/CIDR port rule is not useful when a service changes addresses. A sandbox should be able to permit a named HTTPS or database server without granting every port on its resolved addresses.

### Goals and Non-Goals

The feature limits DNS-learned grants to specific TCP destinations. It does not provide authenticated hostname identity on shared IPs, support UDP ports, change existing portless behavior, or add configuration fields or a new proxy. Scoped denies, default-allow scoped policies, and per-subject Fast enforcement are not supported by the first implementation.

## Proposal

```json
{
  "defaultAction": "deny",
  "egress": [
    {"action": "allow", "target": "api.example.com", "ports": [443]},
    {"action": "allow", "target": "*.database.example.com", "ports": [5432]}
  ]
}
```

A matched scoped allow admits a DNS lookup, not the entire address. Portless domain rules retain their ordered DNS decisions. Explicit IP and port denies, as well as the platform port floor, still take precedence. Fast Sandbox, DNS-only and chained proxy deployments reject the new shape before applying it.

## Design Details

Classic uses timed IPv4 and IPv6 nftables sets keyed by IP and TCP destination port. Different domains that resolve to the same IP contribute their own expiration times. Updating or removing a source keeps the remaining permissions without restarting its lease. Active TCP flows extend only the observed endpoint; unresolved writes are repaired from the last confirmed lease state. Do not return positive DNS answers when an install fails.

Shared IPs intentionally share the resulting L4 permissions. A separate whole-address allow still permits other ports. Without `dropEstablished`, policy changes do not forcibly disconnect existing flows. Credential Vault coverage uses the port of the binding's scheme rather than DNS admission alone.

An exact DNS endpoint withdrawn by `dropEstablished` remains withdrawn across later policy applies, including subsequent opt-ins, until it is allowed again or a rebuild observes its original conntrack deadline has elapsed. Repeated opt-ins do not restart that deadline. Classic bounds both the carried withdrawal set and the established-only exception set; overflow rejects the replacement rather than dropping confirmed state. An ambiguous poll that renewed both whole-address and scoped leases fences further grant writes until a successful static rebuild restores the confirmed deadlines.

## Test Plan

Exercise exact and wildcard rules through creation and the runtime policy API, and their SDK wire representation. Use isolated Linux namespaces to prove actual allowed and blocked traffic, shared IP narrowing and established connection teardown. Check distinct expiry, activity renewal, ambiguous installs and profile rejection before durable effects. Future Fast support needs independent subject-lifecycle and forwarding proof.

## Drawbacks and Alternatives

An address answering multiple ports creates multiple timed elements. Static CIDRs avoid that cost but cannot track DNS changes; a new proxy or fake IP scheme would change the network path. Reuse the existing DNS and nftables runtime instead.

## Infrastructure Needed

Use the existing nftables runtime, isolated network namespace tests and SDK generators. No new service or runtime dependency is required.

## Upgrade & Migration Strategy

Land the Classic policy and kernel enforcement first, without changing what the public API accepts. Next, enable scoped rules at the sidecar's policy API together with the SDK models that write and read those rules. Finally, enable the same shape at sandbox creation. The first stage alone does not enable the feature for users.

Deploy matching lifecycle server and egress versions before using the rule shape. Upgrading an SDK alone does not enable enforcement. Unsupported runtimes keep rejecting domain-port rules. Leave existing policy JSON and limits in place.
