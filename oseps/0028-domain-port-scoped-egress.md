---
title: Domain and TCP Port Egress Rules
description: Extend existing DNS-learned egress leases with TCP destination ports without changing the policy format.
authors:
  - "@cwj2001"
creation-date: 2026-11-16
last-updated: 2026-11-17
status: draft
---

# OSEP-0028: Domain and TCP Port Egress Rules

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
  - [Delivery sequencing](#delivery-sequencing)
<!-- /toc -->

## Summary

Allow existing `NetworkRule.target` domain and wildcard selectors to combine with the existing `ports` field. Extend the sidecar's DNS-learned leases to IP and TCP destination-port tuples, retaining the existing policy format, runtime owners and portless behavior.

## Motivation

IP/CIDR port rules cannot express access to a service whose addresses change. A sandbox should be able to allow `api.example.com` on TCP 443 or a database domain on TCP 5432 without granting every port on its resolved addresses.

### Goals

- Enforce domain-plus-port allow and deny rules in classic `dns+nft` and Fast Sandbox profiles.
- Preserve existing rules, platform controls, subject isolation and credential-binding checks.
- Reject unsupported combinations before provisioning, policy persistence or network effects.

### Non-Goals

Strict hostname identity, UDP port scoping, new policy fields, a new proxy, a generic policy compiler or cache framework, and permanent domain blacklists independent of DNS observation.

## Requirements

Use the existing port range, duplicate and per-rule limits. Scoped-only answers must never enter whole-address allow sets. Policy updates must remove obsolete broad grants and retain overlapping permissions from other valid sources. Failed installation must not produce a successful DNS answer.

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

Portless domain rules retain ordered first-match DNS decisions. An effective explicit name deny remains a DNS gate; a default-deny fallback can be supplemented by scoped domain allows. Matching TCP grants combine, and explicit scoped denies precede ordinary L4 allows. A matching scoped allow admits DNS even if a scoped deny blocks all its TCP ports; transport denies are enforced at L4. Existing static address denies and platform caps still constrain the resulting leases.

### Notes/Constraints/Caveats

These are learned-address permissions, not authenticated hostname identities. Domains sharing an address share the resulting endpoint permissions; a broader allow can still grant additional traffic. Under default allow, a scoped deny covers learned addresses for its lease lifetime and cannot guarantee rejection of direct-IP access. Existing conntrack survival and opt-in teardown semantics remain distinct from lease expiration.

DNS-only enforcement and chained upstream proxy configurations reject the new rule shape. Their existing rules are unchanged. Transparent direct MITM traffic is enforced on its applicable original-destination or direct upstream path.

### Risks and Mitigations

- Shared endpoints: retain source deadlines and aggregate surviving contributions rather than deleting a shared element when one rule changes.
- Stale callbacks: re-evaluate current policy and fence Fast Sandbox answers against the installed subject generation.
- Excessive elements: reuse bounded observation state and batch writes; measure wide port lists and many-subject update costs before release.
- Credential widening: prove permission at each binding scheme's actual port, including whole-selector coverage for wildcard bindings.

## Design Details

Keep the current domain matching, DNS proxy, nft manager and per-subject refresh state. Add timed IPv4/IPv6 allow and deny sets keyed by destination address and TCP port; fixed nft rules reference these sets. Preserve separate legacy whole-address sets.

Record deadlines once when accepting DNS observations. Reclassification and table rebuilds use remaining lifetime, without clamping it again or restarting the TTL. Extend TCP activity observations with destination ports so activity on one port does not renew another. Failed nft writes do not publish successful lease state.

Classic mode checks output traffic. Fast Sandbox retains prerouting marking and forward drops; intercepted input traffic uses conntrack original address and port. Rebind, removal and another subject's table rebuild preserve isolation and invalidate old callbacks.

Runtime policy changes reclassify retained observations, including full-address-to-port narrowing. Existing `dropEstablished` handling must use endpoint permissions instead of treating DNS admission as permission for every port on the address.

## Test Plan

- Policy behavior: exact and wildcard names, repeated target scopes, name gates, deny precedence, IPv4/IPv6 and credential scheme coverage.
- Lease behavior: overlapping sources, different TTLs, endpoint-specific refresh, narrowing, failed apply and stale subject callbacks.
- Real isolated-netns tests: actual allowed/denied packets, Fast Sandbox forward traffic and DNAT original-port enforcement, renewal and lifecycle rebuilds.
- API consumers: creation and runtime mutations, rejection before effects, persistence/read-back and all SDKs.
- Resource measurements: wide port lists, many addresses and many subjects; report memory and update costs rather than infer performance from element counts.

## Drawbacks

Tuple sets increase dynamic elements in proportion to addresses and allowed ports. Domain-based deny remains limited by DNS observation and lease semantics. Maintaining compatibility across two existing enforcement profiles requires lifecycle and packet-level tests, not only parser tests.

## Alternatives

Per-rule IP sets avoid port expansion but increase rule/set objects and complicate exact-port activity retention. Sharing sets by port group changes lease granularity. Fake-IP and explicit proxies change DNS or application behavior. These are not needed to extend the existing contract.

## Infrastructure Needed

No new runtime dependencies or external services. Reuse existing nftables, DNS, private network namespace fixtures and SDK generators.

## Upgrade & Migration Strategy

Existing policy JSON and portless matching semantics are unchanged. Live DNS-cache capacity is enforced before installing grants: when no expired source can be safely replaced, new observations return `SERVFAIL` rather than discarding live provenance. This is intentionally stricter exhaustion behavior, including legacy portless DNS learning, and high-churn callers may need to wait for leases to expire. Deploy supporting server and egress images together; updating an SDK does not upgrade enforcement. Old validators reject the new combination rather than silently accepting it. Document learned-address boundaries and unsupported modes. This proposal remains a draft until maintainer review.


### Delivery sequencing

SDK preparation aligns representable rule scopes and delegates deployed capability to the server; it does not promise that existing servers accept the new combination. The classic delivery enables domain-plus-port enforcement only in the single-sandbox dns+nft profile and rejects it in Fast Sandbox before effects. A dependent Fast Sandbox delivery adds subject-local tuples, lifecycle fencing and original-port enforcement before lifting that rejection. The final feature is not considered available across both profiles until those deliveries and matching images are released.
