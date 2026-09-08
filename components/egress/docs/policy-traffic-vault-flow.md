# Egress Policy, Traffic Flow, and Credential Vault (Fleet Profile)

This document shows how a sandbox's outbound network policy, its traffic flow,
and the credential vault work in the **fleet profile**: one egress
control plane serving N sandboxes that share one host/network domain
(fast-sandbox Fastlet Pod). Each sandbox is a **subject** with its own policy,
kernel rules, and credentials.

The sidecar profile differs (single policy, `hook output`, iptables DNS
REDIRECT on 15353); only the fleet model is drawn here.

## 1. Subject lifecycle: fastlet action protocol → deny-first → active

The Fastlet is the sole lifecycle dispatcher (Sandbox Actions Handler
protocol, `sandbox.fast.io/actions/v1`). The egress Handler implements
`GET /_fastlet/v1/actions/status` (process incarnation probe) and
`POST /_fastlet/v1/actions` (`SET_BINDING` / `LIFECYCLE_HOOK` /
`REMOVE_BINDING`). The binding input **is the policy** (declarative, carried
by the Sandbox CRD `actionBindings`); the attachment block carries the
network identity (source IP, gateway, veth, private CIDR). There is no
file-driven observation source.

A subject is fail-closed from the moment `SET_BINDING` registers it until its
`sandbox.data-plane-ready` Hook succeeds: registration installs deny-first
rules immediately, the policy is held pending, and DNS keeps denying.

```mermaid
stateDiagram-v2
    [*] --> absent
    absent --> denying: SET_BINDING (input = policy, deny-first install)
    denying --> denying: deny-first install failing (Fastlet retries)
    denying --> active: LIFECYCLE_HOOK sandbox.data-plane-ready (policy applied)
    active --> denying: SET_BINDING null input (binding removed, revert to deny-first)
    active --> denying: rebind (new runtimeInstanceId/attachmentId) - policy discarded
    active --> absent: REMOVE_BINDING (unload: chain+sets removed)
    denying --> absent: REMOVE_BINDING
```

Recovery after egress restart: `ApplyReset` wipes the table, the Handler
serves a new `instanceId`, the Fastlet detects it and replays the latest
`SET_BINDING` followed by the already-reached Hooks (every live subject
re-enters `denying` through the same registration path), and the server's
reconciliation re-pushes credential revisions.

## 2. Control plane: policy and credential push

The egress listener binds the Pod netns loopback only. It serves the action
endpoints (Fastlet) and the proxy-route policy/credential surfaces
(fastlet-proxy is the only peer and injects `X-Fast-Sandbox-Uid` to route a
push to a subject). There is no sandbox-reachable policy surface.

Policy itself rides `SET_BINDING` (the CRD binding is the complete desired
value; updates arrive as new bindings). The proxy route carries credential
vault revisions (memory-only, OSEP-0012 — the binding input is NOT a secret
transport) and runtime policy operations.

```mermaid
sequenceDiagram
    autonumber
    participant F as Fastlet
    participant E as Egress listener<br/>(127.0.0.1:18080, loopback)
    participant R as Subject registry<br/>(memory)
    participant N as nftables<br/>(table opensandbox-fleet)
    participant D as DNS proxy<br/>(gateway:53, shared)
    participant S as Server (OpenSandbox)
    participant P as fastlet-proxy

    F->>E: SET_BINDING (sandbox, revision, attachment, policy input)
    E->>R: RegisterAndEnforce (denying, deny-first)
    E->>N: deny-first install (empty sets + drop chain + dispatch rule)
    Note over E: policy stored PENDING - DNS still denies
    F->>E: LIFECYCLE_HOOK sandbox.runtime-ready (confirm)
    F->>E: LIFECYCLE_HOOK sandbox.data-plane-ready
    E->>R: ApplyPolicy (denying -> active, effective = user + always rules)
    E->>N: atomic swap (subject chain + static sets, single nft -f)
    E->>D: per-query selector now returns this subject's policy
    Note over S,P: create-then-configure (server side)
    S->>P: PUT /v1/sandboxfleets/{sid}/egress/credential-vault
    P->>E: forward (credential verified, X-Fast-Sandbox-Uid added)
    alt subject registered
        E->>E: apply vault revision (memory-only per subject)
    else binding not observed yet (race)
        E->>E: cache as pending (TTL, X-Fast-Sandbox-Generation check)
        E-->>P: 202 Accepted (push applied on registration)
    end
```

## 3. Data plane: outbound traffic flow

The authoritative enforcement layer is the Pod netns `forward` hook
(`table opensandbox-fleet`, master chain policy **accept** with an
unmarked-drop tail — the forward path never issues an explicit accept,
because on the fast-sandbox Firecracker bridge topology
(`bridge-nf-call-iptables=1`) an accept verdict returns the frame to the
bridge L2 path and drops it before postrouting). Allowed destinations are
marked in per-subject `hook prerouting` chains (`meta mark set 0x2` for
allow/dyn set members, unconditional for default-allow policies); per-subject
dispatch by `ip saddr` (the source IP is the only dispatch key — an iifname
match would never fire on the bridge topology, where the IP hooks see
skb->dev = the bridge) leads to subject chains whose deny sets drop explicitly,
and the unmarked-drop tail denies everything else (unregistered sources,
deny-first subjects). Intercepted MITM traffic is delivered locally (DNAT)
and enforced by the dedicated INPUT chain on the conntrack original
destination. DNS-learned leases are kept alive by the per-subject connection
refresh loop (Pod netns conntrack, bucketed by source IP, one batched
transaction per tick); only TCP sessions are renewed — UDP/QUIC (HTTP/3)
relies on DNS lease TTLs.

```mermaid
flowchart LR
    subgraph SANDBOX[Sandbox netns]
        APP[App] --> DNSQ[DNS query]
        APP --> TCP[TCP/UDP egress]
    end

    DNSQ -->|addressed to gateway:53| GW[gateway:53 - REDIRECT to :15353]
    GW --> DP[DNS proxy - per-query policy by source IP]
    DP -->|subject unknown / denied| NX[NXDOMAIN]
    DP -->|allowed| UP[Upstream resolver]
    UP -->|answer| DNSQ
    UP -->|resolved IPs with TTL| DYN[subject dynamic allow set - timeout lease]

    TCP -->|via host veth| DISPATCH[dispatch chain - hook forward, ACCEPT + unmarked-drop tail]
    DISPATCH -->|ct state established,related| ACC1[accept]
    DISPATCH -->|tcp/udp dport 853| DROP1[drop - DoT blocked]
    DISPATCH -->|ip saddr| JUMP[jump subj_&lt;id&gt; chain]

    JUMP -->|deny_v4/v6 sets| DROP2[drop]
    JUMP -->|dyn_v4/v6 + allow_v4/v6 sets| ACC2[accept]
    JUMP -->|default-deny policy| DROP3[drop]

    ACC1 --> MASQ[MASQUERADE - POSTROUTING]
    ACC2 --> MASQ
    MASQ --> EXT[External network]
```

## 4. Credential vault

Vault revisions are pushed over the proxy route and held **memory-only** per
subject (OSEP-0012 model — no Secret volume, nothing written to egress disk).
The shared mitmdump instance selects the subject's vault by the client's
source IP (transparent REDIRECT/DNAT preserves it); a revision push rebinds
in memory and new flows pick up the new credentials. See
[fleet-mitm-data-plane](../../../docs/components/egress-fleet-mitm-data-plane.md).

```mermaid
sequenceDiagram
    autonumber
    participant S as Server
    participant P as fastlet-proxy
    participant E as Egress listener
    participant V as Subject vault store<br/>(memory-only, per subject)
    participant M as mitmdump (shared)
    participant C as Sandbox client

    S->>P: PUT /v1/sandboxfleets/{sid}/egress/credential-vault (full revision)
    P->>E: forward (UID header -> subject)
    E->>V: replace revision (memory-only, new flows rebind)
    C->>M: HTTP(S) flow (DNAT preserves source IP)
    M->>M: script: client source IP -> subject -> subject's vault
    M->>V: resolve credential/binding for the flow
    V-->>M: credential (active snapshot)
    M-->>C: proxied flow with credential applied
```

## 5. Fail-closed invariants

| Transition / event | Guarantee |
|---|---|
| SET_BINDING, no data-plane-ready yet | deny-first: empty nft sets + drop chain, gateway DNS REDIRECT, DNS NXDOMAIN |
| Vault push before SET_BINDING | cached pending (TTL); applied on registration; generation mismatch discards it |
| data-plane-ready lands | one atomic `nft -f` transaction (chain + static sets); DNS selector switches per subject |
| Rebind (new runtimeInstanceId/attachmentId) | policy discarded in registry AND nft chain/sets/DNS leases force-reset |
| Unload (REMOVE_BINDING) | chain + all sets removed in one transaction; stale fence ignored |
| Egress restart | stale rules wiped (ApplyReset); new instanceId triggers Fastlet replay of SET_BINDING + reached Hooks |
| Unregistered source | unmarked -> master-chain tail drop — denied before the binding is ever observed |
| Malformed action envelope | rejected (never silently ignored); the subject is never activated |
| data-plane-ready without pending policy | failed (protocol violation) — the subject stays denying |

## Component map

| Concern | Implementation |
|---|---|
| Actions wire model + validation | `pkg/actionhandler` (envelope, operations, Hooks) |
| Subject state machine | `pkg/subject` (`MemoryRegistry`, lifecycle hooks) |
| Per-subject nft rules | `pkg/fleetnft` (dispatch rules, atomic swap, reset) |
| Actions endpoints + lifecycle mapping | `fleet_actions.go` (SET_BINDING / LIFECYCLE_HOOK / REMOVE_BINDING) |
| Policy/vault HTTP surface | `fleet_server.go` (UID routing, pending cache, per-subject vault) |
| DNS per-query dispatch | `pkg/dnsproxy` `SetQueryPolicySelector` |
