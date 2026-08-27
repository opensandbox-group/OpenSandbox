# Fleet Profile: Shared MITM Data Plane — Design

> Status: **design**. Control plane (per-subject vault stores, revision push)
> is implemented; the data plane below is not.

## Goal

In the fleet profile, N sandboxes share one Pod netns. HTTP(S) traffic of
every sandbox is transparently intercepted by a **single shared mitmdump**,
and credentials are selected from the **subject's own vault** by the client's
source IP (which REDIRECT preserves). The sidecar profile and its
single-vault addon behavior are unchanged.

## Architecture: per-sandbox netns OUTPUT REDIRECT

```
 sandbox netns                          Pod netns
 ┌─────────────────────┐               ┌────────────────────────────────────┐
 │ iptables -t nat     │               │  mitmdump --mode transparent        │
 │   OUTPUT tcp 80,443 │               │   127.0.0.1:18081  (shared)         │
 │   -> REDIRECT 18081 │               │  addon (system.py):                 │
 │ (installed from     │               │    client IP -> subject -> vault    │
 │  host via nsenter)  │               │  active socket 127.0.0.1:18082/_active
 └─────────┬───────────┘               └───────────────┬────────────────────┘
           │ HTTP(S) + source IP                       │ /_active?clientIp=X
           └───────────────────────────────────────────┘
```

- **Interception point**: one REDIRECT rule pair per sandbox netns
  (OUTPUT), installed from the host/Pod through `nsenter --net=<hostNetnsPath>`.
  REDIRECT rewrites only the destination (to `127.0.0.1:18081`) and **preserves
  the source IP** — the per-client vault selection depends on this.
- **Single interception target**: the shared mitmdump in the Pod netns.
  The Pod-netns rules are deliberately NOT installed (sidecar's
  `SetupTransparentHTTP` would also intercept the Pod's own traffic).

### Rule shape (per sandbox netns)

Derived from the sidecar's `transparentHTTPRules`, without the `--uid-owner`
exclusion (mitmdump runs outside the sandbox netns, so no self-loop):

```
iptables -t nat -A OUTPUT -p tcp -d 127.0.0.0/8 -j RETURN
iptables -t nat -A OUTPUT -p tcp -m multiport --dports 80,443 -j REDIRECT --to-ports 18081
```

### Lifecycle

| Event | Action |
|---|---|
| `OnRegistered` (after deny-first nft + resolv) | install the REDIRECT rule pair via nsenter; failure ⇒ registration fails and retries (fail closed: never register a sandbox whose HTTP(S) is not intercepted) |
| `OnUnloaded` | remove the rule pair (`-D`, order reversed) |
| egress restart | stale rules die with the sandbox netns (rules live in per-sandbox netns, destroyed with it); a restart also re-registers subjects → re-installs pairs |

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
- Sidecar compatibility: no `clientIp` param → the single-vault behavior,
  unchanged.
- Socket reuse: `pkg/credentialvault.StartActiveSocketServer` infrastructure
  with a request-aware handler variant (sidecar handler untouched).
- All subject vaults live in the same egress process, so one socket trivially
  serves every subject; per-subject sockets would only add lifecycle
  bookkeeping without any isolation benefit.

## Addon changes (`mitmscripts/system.py`)

- `_load_active_vault()` → `_load_active_vault(client_ip: str | None)`; the
  0.5s cache becomes keyed by client IP (sidecar path uses the shared cache).
- Call sites pass `flow.client.peername[0]`.
- 404 / unknown subject ⇒ no vault for the flow (credentials not injected;
  traffic still proxied).
- Env knob `OPENSANDBOX_CREDENTIAL_PROXY_SOCKET` unchanged; the fleet socket
  path defaults to the same location (per-Pod, one egress process).

## Assembly (`fleet.go`)

- Gate: `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true` (reuse the sidecar
  env) in fleet mode starts the shared mitmdump via the existing
  `mitmTransparent` machinery, **minus** Pod-netns `SetupTransparentHTTP`
  (the REDIRECT pairs are per-subject instead).
- `mitmGate` (HealthGate) wired into the fleet healthz (mitm pending ⇒ 503),
  matching sidecar semantics; vault `Ready()` gating stays sidecar-only.
- `OnRegistered`/`OnUnloaded` mount the per-sandbox rule pairs through the
  new `pkg/netnsredirect` applier (injectable runner for tests; default
  `nsenter`).

## Fail-closed semantics

| Failure | Effect |
|---|---|
| REDIRECT install fails at registration | registration retries (subject stays denying — no traffic at all) |
| mitmdump dies | gate → healthz 503; `watchMitmproxy` restarts with backoff; flows fail while down (TCP closed on 18081) |
| active API 404 / socket down | addon injects nothing; traffic still flows without credentials (matching sidecar behavior when no vault exists) |
| REDIRECT removal fails at unload | logged; rules die with the sandbox netns |

## Deployment preconditions (fast-sandbox commitment)

1. Egress container can `setns` into per-sandbox netns: shared netns mount
   (`/var/run/netns` or equivalent) + `CAP_SYS_ADMIN`/`CAP_NET_ADMIN` —
   matches the existing "per-sandbox netns OUTPUT from host" precedent
   (`linux_driver.go`).
2. **MITM CA distribution into sandboxes** (open question): the sidecar
   exports its CA to the sandbox via the shared volume + agent bootstrap;
   the fleet model needs a per-sandbox CA delivery path (slot contract field
   or mounted CA file) before TLS interception can succeed.

## Implementation units

1. `pkg/netnsredirect`: rule builder + `nsenter`-wrapped applier (fake-runner
   tests; real-netns integration test gated on Linux+CAP, skipped otherwise).
2. `fleet_server.go`: request-aware active handler (`clientIp` → subject →
   snapshot) + socket server variant.
3. `fleet.go`: shared mitmdump assembly (no Pod rules), gate wiring, rule
   mount on `OnRegistered`/`OnUnloaded`.
4. `mitmscripts/system.py`: client-IP-keyed vault loading.
5. Tests: rule shape + nsenter argv (fake runner); active API resolution
   (httptest over the unix socket); addon behavior documented/manual.
