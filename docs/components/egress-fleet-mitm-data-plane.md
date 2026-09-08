---
title: "Fleet Profile: Shared MITM Data Plane"
description: "Design and implementation of the fleet-profile shared mitmdump: per-subject Pod-netns DNAT interception, the authoritative INPUT enforcement chain, the subject-aware active vault API, and CA delivery."
---

# Fleet Profile: Shared MITM Data Plane — Design

> Status: **implemented** (control plane + data plane). The per-sandbox CA
> delivery into the sandbox trust store is the remaining external dependency
> (fast-sandbox bind-mount contract, see fast-sandbox issue #19); the
> interception + injection mechanism itself is complete and unit-tested.

## Goal

In the fleet profile, N sandboxes share one Pod netns. HTTP(S) traffic of
every sandbox is transparently intercepted by a **single shared mitmdump**,
and credentials are selected from the **subject's own vault** by the client's
source IP (which NAT preserves). The sidecar profile and its
single-vault addon behavior are unchanged.

## Architecture: per-subject prerouting DNAT in the Pod netns

```
 sandbox netns                          fastlet Pod netns
 ┌─────────────────────┐               ┌──────────────────────────────────┐
 │ OUTPUT filter (real │  dst=real:80,443│  prerouting nat (per subject):   │
 │ dst visible; policy │ ────────────► │   ip saddr <sandbox> tcp 80,443   │
 │ enforced on it)     │    (veth)     │     dnat to <gateway>:18081       │
 └─────────────────────┘               │         ↓ (local delivery)        │
                                       │ mitmdump 0.0.0.0:18081 (shared)   │
                                       │ addon (system.py):                │
                                       │   client IP -> subject -> vault    │
                                       │  active socket (unix: /run/opensandbox/credential-proxy/active.sock)
                                       └──────────────────────────────────┘
```

- **Interception point**: one DNAT rule pair per sandbox in the **Pod netns
  prerouting**, keyed by the sandbox's source IP (`ip saddr <sandboxIP> tcp
  dport {80,443} dnat to <gateway>:18081`), installed from the shared egress
  container (`nft`, like the gateway-DNS REDIRECT). A `return` rule keeps
  sandbox → gateway (management-plane) traffic out of the proxy.
- **Deliberate placement change vs the original design**: the original design
  proposed per-sandbox netns OUTPUT REDIRECT installed via `nsenter`. The Pod
  netns placement is strictly better:
  1. The Pod-netns forward hook (and the per-subject INPUT enforcement chain
     for intercepted traffic) sees the **real destination** — policy matching
     stays exact with no rule changes (a sandbox-side DNAT would rewrite the
     dst before every filter hook saw it).
  2. `SO_ORIGINAL_DST` on the mitmdump socket (Pod-netns conntrack original
     tuple) yields the **true target address** — transparent mode needs no
     Host/SNI fallback.
  3. The rule is invisible/tamper-proof to the sandbox (it has no NET_ADMIN
     in the Pod netns), and the egress's own traffic never matches the
     sandbox saddr key (the sidecar's uid-owner exclusion is unnecessary).

  (The earlier per-sandbox netns OUTPUT mirror layer — `pkg/sandboxnft` — was
  removed when the fleet profile moved to the fast-sandbox Sandbox Actions
  protocol: the action envelope does not carry the sandbox netns path, and
  the Pod-netns layers are authoritative for both forwarded and intercepted
  traffic.)
- **Enforcement of intercepted traffic (important)**: the DNAT delivers the
  intercepted 80/443 **locally** (INPUT path), so the Pod-netns **forward
  hook never sees it**. The authoritative enforcement for MITM traffic is a
  dedicated **Pod-netns INPUT chain** (`opensandbox-fleet` table, installed
  when MITM is enabled): it matches only `ct status dnat` packets on the
  mitmproxy port, dispatches per sandbox, and applies the same deny/allow/
  DNS-learned/DoH-443 policy to the conntrack **ORIGINAL** destination. A
  compromised sandbox that flushes its own OUTPUT table therefore cannot
  bypass the authoritative layers for intercepted traffic; non-MITM traffic
  keeps the forward hook as its authoritative layer.
- **Single interception target**: the shared mitmdump in the Pod netns,
  listening on `0.0.0.0:18081` (a 127.0.0.1 bind would never receive traffic
  DNATed to the gateway veth address — same reason the fleet DNS proxy binds
  `:15353`). The Pod-netns OUTPUT REDIRECT is deliberately NOT installed
  (sidecar's `SetupTransparentHTTP` would also intercept the Pod's own
  traffic).

### Rule shape (Pod netns, per subject)

```
table inet opensandbox_gateway_mitm
  chain gw { type nat hook prerouting priority dstnat; }
    ip saddr <sandboxIP> ip daddr <gateway> tcp dport {80,443} return
    ip saddr <sandboxIP> tcp dport {80,443} dnat to <gateway>:18081
```

The table is rebuilt wholesale from the fleet server's in-memory subject map
on every register/unload; nft batches are transactional, so a failed rebuild
leaves the previous table live (fail closed at registration: the rebuild runs
BEFORE the subject is marked registered).

### Lifecycle

| Event | Action |
|---|---|
| `OnRegistered` (after deny-first nft + resolv + gateway DNS redirect) | add the subject's DNAT entry + rebuild the Pod-netns table; failure ⇒ registration fails and retries (fail closed: never register a sandbox whose HTTP(S) is not intercepted) |
| `OnSlotUpdated` | refresh the entry (sandbox IP / gateway may have moved) + rebuild |
| `OnUnloaded` | drop the entry + rebuild (best effort; a stale rule for a dead sandbox's IP is inert) |
| egress restart | `ApplyReset`-style recovery is not needed here: the table is rebuilt from the in-memory subjects on re-registration |

## Subject-aware active vault API

**Decision: one shared unix socket; subject dispatch happens inside the
socket server.** No per-subject sockets, no UID in the socket protocol.

The addon's only identity material for a flow is the client IP, so the
request carries it and the socket handler resolves client IP → subject →
vault snapshot:

```
GET /credential-vault/_active?clientIp=10.0.0.5
```

- egress (inside the socket handler): `registry.Resolve(SubjectKey{SourceIP:
  ip})` → subject → that subject's vault `ActiveSnapshot()`; unknown IP →
  404 (addon treats as no-vault, no injection).
- Sidecar compatibility: the sidecar's request-unaware handler is unchanged;
  the fleet variant is `StartActiveSocketServerRequestAware` (the socket
  server itself is shared infrastructure).
- All subject vaults live in the same egress process, so one socket trivially
  serves every subject.

## Addon changes (`mitmscripts/system.py`)

- `_load_active_vault(client_ip)` — in fleet mode (`OPENSANDBOX_EGRESS_PROFILE
  =fleet`) the 0.5s cache is keyed by client IP; the sidecar path uses the
  single shared cache, unchanged.
- Call sites pass `flow.client_conn.peername[0]` (defensive: missing peername
  ⇒ no dispatch key ⇒ no vault).
- 404 / unknown subject ⇒ no vault for the flow (credentials not injected;
  traffic still proxied).
- Env knob `OPENSANDBOX_CREDENTIAL_PROXY_SOCKET` unchanged; the fleet socket
  path defaults to the same location (per-Pod, one egress process).

## Assembly (`fleet.go` / `fleet_mitm.go`)

- Gate: `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true` in fleet mode starts
  the shared mitmdump via the existing `mitmTransparent` machinery, **minus**
  Pod-netns `SetupTransparentHTTP` (the DNAT pairs are per-subject instead),
  with `--listen-host 0.0.0.0`.
- `mitmGate` (HealthGate) wired into the fleet healthz (mitm pending ⇒ 503),
  matching sidecar semantics; vault `Ready()` gating stays sidecar-only.
- `OnRegistered`/`OnSlotUpdated`/`OnUnloaded` mount/unmount the per-subject
  DNAT entries through the fleet server's injected installer
  (`pkg/iptables.InstallMitmRedirects`).
- The subject-aware active socket server starts with the shared mitmdump and
  shuts down with the profile context.

## CA export (fleet)

- The CA is exported to a **dedicated subdir**:
  `/opt/opensandbox/mitm-ca/mitmproxy-ca-cert.pem`
  (`mitmproxy.SyncRootCAFleet`) — the fastlet bind-mounts this directory
  read-only into every sandbox at creation (fast-sandbox issue #19). A
  directory-level mount keeps the export visible across egress's atomic
  rename-based CA rotation; the dedicated subdir (not the whole
  `/opt/opensandbox`) avoids shadowing the fastlet-placed execd binary and
  avoids a sandbox-shared-writable trust anchor.
- `PurgeStaleExportedCA` clears both the sidecar path and the fleet subdir on
  startup (stale-CA class of #1370).

## Fail-closed semantics

| Failure | Effect |
|---|---|
| DNAT install fails at registration | registration retries (subject stays denying — no traffic at all) |
| mitmdump dies | gate → healthz 503; `watchMitmproxy` restarts with backoff; flows fail while down (TCP closed on 18081) |
| active API 404 / socket down | addon injects nothing; traffic still flows without credentials (matching sidecar behavior when no vault exists) |
| DNAT removal fails at unload | logged; the stale rule is inert and overwritten by the next rebuild |

## Deployment preconditions (fast-sandbox commitment)

1. Egress container shares the fastlet Pod netns (nft access for the
   per-subject DNAT + forward-hook enforcement) — matches the gateway-DNS
   REDIRECT precedent.
2. **MITM CA distribution into sandboxes** (fast-sandbox issue #19): fastlet
   bind-mounts `/opt/opensandbox/mitm-ca` (read-only, directory-level) into
   each sandbox at creation; execd bootstrap seeds the sandbox trust stack
   from it. This is the only remaining external dependency for real TLS
   end-to-end.

## Implementation units

1. `pkg/iptables/gateway_mitm.go`: per-subject DNAT script builder + wholesale
   rebuild (fake-runner-tested via pure function tests).
2. `fleet_server.go`: subject-aware active handler (`clientIp` → subject →
   snapshot), `SetMitm` wiring, DNAT entry lifecycle in the registration
   hooks, healthz gate.
3. `fleet_mitm.go` / `fleet.go`: shared mitmdump assembly (no Pod rules),
   CA subdir export, active socket startup.
4. `pkg/credentialvault`: request-aware socket-server variant (additive;
   sidecar handler untouched).
5. `pkg/mitmproxy`: `Config.ListenHost` (fleet passes 0.0.0.0),
   `SyncRootCAFleet` (subdir export, no system-trust install).
6. `mitmscripts/system.py`: client-IP-keyed vault loading (fleet mode).
7. Tests: rule shapes, socket dispatch over unix sockets, registration
   fail-closed, healthz gate, addon cache semantics (52 Python tests).
