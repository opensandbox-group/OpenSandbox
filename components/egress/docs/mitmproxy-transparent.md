# Python mitmproxy Transparent Mode (with Egress)

Transparent mode starts `mitmdump --mode transparent` inside the sidecar and redirects local outbound `TCP 80/443` traffic to the mitmproxy listener via `iptables`. Its core benefits are:

- **No application changes**: no need to set `HTTP_PROXY`; app traffic is intercepted transparently.
- **Observability and extensibility**: use mitm scripts for header injection, auditing, and debugging.
- **Controlled bypass**: legacy mode uses `ignore_hosts`; the experimental live runtime uses typed static TLS/443 selectors (forward only, no decryption).

Typical use case: add L7 visibility/processing at the egress boundary without changing the application networking stack.

## Quick Setup (Minimum Working Config)

### Prerequisites

- Linux network namespace with `CAP_NET_ADMIN` in the container.
- `mitmdump` installed and `mitmproxy` user present in the image (included in official egress image).
- Client/system trusts the mitm root CA; otherwise HTTPS handshakes will fail.

### Enable Transparent MITM

```bash
export OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true
```

By default, mitmproxy listens on `18081` and transparent redirect rules are set automatically.

### Common Optional Settings

```bash
# Optional: change listening port (default: 18081)
export OPENSANDBOX_EGRESS_MITMPROXY_PORT=18081

# Optional: load user-defined mitm addons (loaded after the system addon, comma-separated)
export OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT=/path/to/your/addon.py,/path/to/another.py
```

To bypass decryption for selected domains, edit the baked-in `components/egress/mitmproxy/config.yaml` and rebuild the image — see "Static Configuration (config.yaml)" below.

## Experimental live static TLS bypass

The default-off live revision runtime uses typed static TLS/443 selectors; legacy mode continues to use `ignore_hosts`. For configuration, migration, limits and verification boundaries, see the [credential vault guide](../../../docs/guides/credential-vault.md#experimental-live-credential-bound-admission-osep-0023).

## Configuration Reference

### Environment Variables (Per-Deployment Overrides)

| Variable | Required | Purpose | Default |
|------|----------|------|--------|
| `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` | Yes | Enable transparent mitmproxy (`1/true/on`, etc.) | Disabled |
| `OPENSANDBOX_EGRESS_MITMPROXY_PORT` | No | mitmdump listen port; `iptables` redirects `80/443` here | `18081` |
| `OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT` | No | User mitm addon script paths (comma-separated); each is passed as `-s` and loaded after the system addon in order | Empty |
| `OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_TRUST_DIR` | No | Trust directory for upstream TLS verification (OpenSSL style); overrides the config.yaml default | `/etc/ssl/certs` |
| `OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA` | No | Path to a PEM file with one or more extra CA certificates; passed to mitmproxy `ssl_verify_upstream_trusted_ca`. **Additive** with the system/confdir trust — it does not replace `/etc/ssl/certs` — and applies to **every** mitmproxy upstream TLS connection (HTTPS proxy and intercepted origins), not only the proxy hop | Empty |
| `OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE` | No | Skip upstream TLS verification (`1/true/on`); use when clients connect by IP and SNI is unavailable | Disabled |
| `OPENSANDBOX_EGRESS_MITMPROXY_EXTRA_PORTS` | No | **Experimental.** Extra destination TCP ports to intercept, appended to the always-on `80,443` (comma-separated, e.g. `8080,8443`). Fails closed at startup on invalid input; total ports (including 80/443) must be ≤ 15. Note: the system addon's credential-binding matcher currently only fires on canonical 80/443 — extras are decrypted and logged but not matched against bindings. | Empty |
| `OPENSANDBOX_EGRESS_UPSTREAM_PROXY` | No | Chained upstream proxy endpoint (`http://host[:port]` or `https://host[:port]`), with no credentials, query, fragment, or non-root path. The host must be a literal IP or a dotted domain name: a dotless name expands differently through the Pod resolver's DNS search list than through the egress's direct query, so the containment sets could miss the address actually dialed (startup fails otherwise). Requires `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true` and — outside the fast-sandbox profile — `OPENSANDBOX_EGRESS_MODE=dns+nft`; egress startup fails otherwise. Under the fast-sandbox profile the endpoint is contained profile-wide (see the fast-sandbox bullet under "Chain Through an Upstream Proxy"). When set, the bundled `upstream_proxy.py` addon is loaded after the system addon and every mitmproxy-handled connection is forwarded through the proxy via `CONNECT`. Fail closed: pass-through flows that cannot be chained are refused and logged with the `credential proxy:` prefix. | Empty (disabled) |
| `OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH` | No | Complete `Proxy-Authorization` header value sent on the upstream `CONNECT` (e.g. `Basic base64(user:pass)`). Requires `OPENSANDBOX_EGRESS_UPSTREAM_PROXY`; startup fails if set alone. Never logged. | Empty |

Notes:

- In transparent mode, mitmproxy generally recommends matching by IP/range; verify SNI/resolve behavior if using domain regex only.
- Before mitm, `iptables`, and CA export are ready, `GET /healthz` returns `503 (mitm not ready)` to prevent premature readiness.
- Under the fast-sandbox profile the CA is not generated at all: it is seeded from the pool-pinned trust anchor — see "Pinned CA seeding (fast-sandbox profile)" below.

### Static Configuration (config.yaml)

Fast Sandbox-wide, rarely-changing mitm options live in `components/egress/mitmproxy/config.yaml`, baked into the image at `/var/lib/mitmproxy/.mitmproxy/config.yaml` and auto-loaded by mitmdump. This is the single source of truth for:

- `mode` (`transparent`) — mitm default is `regular`
- `listen_host` (`127.0.0.1`) — mitm default is `0.0.0.0`
- `stream_large_bodies` (`1m`) — mitm default is unset (entire body buffered)
- `ssl_verify_upstream_trusted_confdir` (`/etc/ssl/certs`) — mitm default is unset; overridable per-deployment via env
- `ssl_verify_upstream_trusted_ca` — unset by default; per-deployment env `OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA` points it at a PEM bundle that **augments** the confdir trust for all upstream TLS verification
- `connection_strategy` (`lazy`) — mitmproxy 10+ changed the default from `lazy` to `eager`; pinned explicitly to preserve the historical behavior of deferring upstream connections until the full request arrives
- `ignore_hosts` (`[]`) — matches the mitm default; kept in the file as a discoverable extension point for operators adding TLS pass-through entries

Only deviations from the mitm built-in defaults are declared in `config.yaml` (the `ignore_hosts` entry is a discoverability exception; `connection_strategy` is a compatibility pin against the upstream default change). Other options that happen to match the default (`http2=true`, etc.) are omitted — the file is the diff against upstream defaults, not a full enumeration.

Precedence: command-line `--set` (from env overrides) > `config.yaml` > mitmproxy built-in defaults.

#### Overriding the built-in config.yaml

There is no env var to point mitm at an alternate config file. Operators who need different static defaults (e.g. a different `ignore_hosts` list, `connection_strategy`, or `stream_large_bodies`) should pick one of the following:

1. **Build a downstream image** that derives from the official egress image and replaces the file:

   ```dockerfile
   FROM <opensandbox-egress-image>:<tag>
   COPY my-config.yaml /var/lib/mitmproxy/.mitmproxy/config.yaml
   RUN chown mitmproxy:mitmproxy /var/lib/mitmproxy/.mitmproxy/config.yaml \
       && chmod 0644 /var/lib/mitmproxy/.mitmproxy/config.yaml
   ```

   This is the recommended path because the override is version-controlled, reviewable, and reproducible.

2. **Mount an override file at runtime** over the baked-in path. For Kubernetes, mount a `ConfigMap` as a file at `/var/lib/mitmproxy/.mitmproxy/config.yaml` (be aware that a `ConfigMap` file mount typically lands as read-only with the original UID, so verify the mitmproxy user can read it):

   ```yaml
   volumeMounts:
     - name: mitm-config
       mountPath: /var/lib/mitmproxy/.mitmproxy/config.yaml
       subPath: config.yaml
       readOnly: true
   volumes:
     - name: mitm-config
       configMap:
         name: egress-mitm-config
         defaultMode: 0644
   ```

   Useful for staged rollouts or per-environment overrides without rebuilding the image.

3. **Single-option escape hatch via env-driven `--set`** (already supported for the documented env variables above). This only works for options exposed via env and only for the single specific override; it cannot replace the whole file.

Do not edit `config.yaml` inside a running container — the file lives in the container layer, edits are lost on restart, and the mitmproxy user has read-only access by design.

## Common Configuration Templates

### 1) Enable Transparent MITM Only

```bash
export OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true
```

### 2) System Addon (Always On)

The bundled system addon at `/var/egress/mitmscripts/system.py` is shipped in the egress image and loaded automatically whenever transparent mode is enabled. It stays wire-transparent (no headers added or altered) and currently provides:

- Forces streaming (`flow.response.stream = True`) for SSE (`text/event-stream`) and chunked responses, so each chunk is forwarded immediately instead of being buffered up to the `stream_large_bodies=1m` threshold (critical for LLM streaming UX).
- Credential Proxy: binding match, path/query/header rewrites, and auth header injection run in the `requestheaders` hook, so they apply regardless of request body size — including bodies above the 1 MiB threshold that `stream_large_bodies` streams upstream before the `request` hook fires. Body placeholder substitutions run in the `request` hook and are skipped for such streamed bodies. Requests rejected before injection (ambiguous or binding-escaping paths) get a `403` when the body size is fully known and below the streaming threshold; for streamed or unknown-length bodies the connection is instead dropped without forwarding, because mitmproxy cannot serve a local response while streaming a request body.
- Path ambiguity guard (credential-scoped requests only): dot-segments and backslashes are rejected at any percent-decoding depth; encoded slashes (`%2f`, including nested forms) are allowed only when every decoding depth of the path matches the same credential binding. The authoritative policy — what is rejected, what is tolerated, and why — lives in the Credential Vault guide's [Path Ambiguity Guard section](../../../docs/guides/credential-vault.md#path-ambiguity-guard).
- Redacts credential values from response headers. Response bodies are not rewritten by default.

The system addon is always loaded and cannot be disabled via configuration. To override its behavior, supply user addons via `OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT` (comma-separated for multiple scripts); user addons are loaded after the system addon in the order given and may observe or override its hooks.

### 3) Add User Addons Alongside the System Addon

```bash
export OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true

# Single addon
export OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT=/path/to/your/addon.py

# Multiple addons (comma-separated)
export OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT=/path/to/auth.py,/path/to/logging.py
```

User addons are loaded after the system addon (`-s system.py -s auth.py -s logging.py`), in the order given. Later addons observe and may override hooks from earlier ones.

### 4) Bypass Decryption for Specific Domains (e.g. log upload)

Edit `components/egress/mitmproxy/config.yaml` and append to `ignore_hosts`, then rebuild the egress image:

```yaml
ignore_hosts:
  - '.*\.log\.aliyuncs\.com'
```

`ignore_hosts` means **no decryption**, not "completely bypass mitm process": mitm still proxies the TCP connection, it just forwards bytes without breaking TLS, and addons do not see request/response content.

`ignore_hosts` (like `tcp_hosts`/`udp_hosts`) is **incompatible** with `OPENSANDBOX_EGRESS_UPSTREAM_PROXY`: pass-through connections cannot be chained through a CONNECT proxy, so the upstream addon refuses to load when any pass-through list is non-empty instead of letting those destinations bypass the proxy.

### 5) Use a Fixed CA (consistent fingerprint across replicas)

If CA files already exist in `confdir`, mitmproxy reuses them instead of regenerating on each startup. Typical paths:

- `/var/lib/mitmproxy/.mitmproxy/mitmproxy-ca.pem` (private key)
- `/var/lib/mitmproxy/.mitmproxy/mitmproxy-ca-cert.pem` (public cert)

Ensure correct permissions (for example `mitmproxy:mitmproxy`, private key mode `600`).

### 5b) Pinned CA seeding (fast-sandbox profile)

The `fast-sandbox` profile (`OPENSANDBOX_EGRESS_PROFILE=fast-sandbox`) replaces generate-on-start with a **pinned, cluster-global, versioned CA** (OSEP-0022 "MITM Trust Anchor"): one egress restart no longer rotates the CA, so live sandboxes keep passing TLS verification across restarts, Hook replays, pause/resume, and snapshot restore.

| Aspect | Behavior |
|--------|----------|
| Provisioning | The pool's `FastletTemplate` mounts one pinned `opensandbox-mitm-ca-<version>` Secret read-only as the seed volume; point egress at it with `OPENSANDBOX_EGRESS_MITM_CA_SEED_DIR`. The mount holds `tls.crt` (certificate) + `tls.key` (private key). When transparent MITM is enabled the seed is **required**: there is no fallback to generate-on-start in this profile. |
| Seeding | At startup (before mitmdump launches) egress validates the keypair — including that the certificate is a CA (`IsCA` + `keyCertSign`) inside its validity window — and copies it **idempotently** into the writable confdir under the names mitmdump loads: `mitmproxy-ca.pem`, a single file holding the certificate AND the private key (mitmproxy's `CertStore` loads both from this one file — a key-only file makes mitmdump exit at startup), plus `mitmproxy-ca-cert.pem` (public certificate copy). mitmdump loads a confdir CA when present and only generates when missing; in this profile it must **never** generate. A confdir already holding the exact pinned pair is left untouched (restart-stable). |
| Fail-closed, scoped to the addon | A missing, unloadable, or invalid pinned CA fails the **MITM addon only**: it refuses to start and the down state is latched until process restart, surfaced on `/healthz` (503, `"status":"mitm-addon-down"`) and the actions status payload (`"mitmCaDown":true`). The rest of egress keeps serving policy actions. Pools that never enable interception need no CA and are unaffected; the sidecar profile keeps today's generate-on-start behavior unchanged. |
| Consistency check | After the listener is up, egress re-verifies that the confdir cert it just loaded equals the pinned cert; a mismatch (a CA generated or swapped behind egress's back) fails the addon closed. |
| Observability | The pinned CA's SHA-256 fingerprint (lowercase hex over the DER) is exposed on `/healthz` (`"mitmCaFingerprint"`) and mirrored in `GET /_fastlet/v1/actions/status`, so a template↔pool Secret mispairing (baked version ≠ pinned version) is visible at a glance instead of surfacing as opaque guest TLS failures. |

```bash
# Example: seed mount provisioned at /var/egress/mitm-ca-seed (read-only Secret mount)
export OPENSANDBOX_EGRESS_MITM_CA_SEED_DIR=/var/egress/mitm-ca-seed
```

The superseded fast-sandbox mechanisms are removed: the `mitm-ca` export directory egress used to publish for the fastlet to mount into sandboxes, guest trust-store seeding at bootstrap, and the stale-CA purge on restart. The export path (`SyncRootCA` + purge) remains **sidecar-only**.

### 6) Chain Through an Upstream Proxy (Corporate/Forward Egress)

```bash
export OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true
export OPENSANDBOX_EGRESS_UPSTREAM_PROXY=https://proxy.example.com:8443
# Optional: complete Proxy-Authorization header value for the upstream CONNECT
export OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH="Basic $(printf 'user:pass' | base64)"
```

When set, the bundled `upstream_proxy.py` addon is loaded after the system addon and all mitmproxy-handled egress is chained: mitmproxy dials the configured proxy and issues `CONNECT <request.host>:<port>`. For intercepted TLS the authority is the SNI/Host-derived FQDN (not the intercepted IP), so the upstream proxy resolves and dials the original destination itself; flows where only an IP is known keep `IP:port` as the authority.

Semantics and limits:

- **`https://` endpoints** get TLS to the proxy with SNI and hostname verification against the proxy host, using the same `ssl_verify_upstream_trusted_confdir`/`_trusted_ca` options that verify real upstreams (default `/etc/ssl/certs`, overridable via `OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_TRUST_DIR`). To trust a private CA for the proxy connection, deliver the PEM via `OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA` — it is additive to the system roots and also applies to intercepted origin TLS.
- **Fail closed**: connections that cannot be chained — TLS pass-through (no-SNI or `ignore_hosts`/`tcp_hosts`/`udp_hosts` matches) and UDP/QUIC dials — are refused rather than silently sent direct.
- **Requires `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true`**: the chain only exists inside the transparent mitmproxy path, so egress startup fails if the proxy is configured without transparent mode instead of silently ignoring it.
- **Supported under the fast-sandbox profile** (`OPENSANDBOX_EGRESS_PROFILE=fast-sandbox`), with containment adapted to its source-IP enforcement model: the proxy endpoint is dropped profile-wide in the shared dispatch chain (forward path, and the input path for intercepted traffic), ahead of the established accept and every per-subject rule, so no subject policy — default-allow included — can CONNECT the proxy directly. The mitmdump dial itself is locally generated Pod traffic and is never policed by the fast-sandbox table (the profile deliberately installs no OUTPUT enforcement), so no accept-side exception is needed. For a hostname endpoint the name is registered as an infrastructure domain on the shared dnsproxy: sandbox lookups resolve without per-subject policy and never feed the dynamic allow sets; DNS-learned addresses seed the drop sets as PERMANENT elements (no kernel timeout): every other rule in the table persists while the egress daemon is down (fail closed), and a kernel timeout would silently lapse the containment during a restart — expiry is owned by the egress instead (the self-resolution loop prunes addresses absent from two successful full-authority refreshes; sandbox DNS learning renews retention, and table rebuilds re-seed from the in-memory mirror). The shared mitmdump resolves the hostname through the fastlet Pod's own resolver (cluster DNS), so the name must be resolvable there — the egress component does not redirect the Pod's own DNS. Because the dnsproxy's forward upstreams (`OPENSANDBOX_EGRESS_DNS_UPSTREAM` or `/etc/resolv.conf`) and the Pod resolver can return different address sets (split-horizon DNS, an operator-configured DNS upstream, or plain rotation), the self-resolution loop queries **both** authorities concurrently and seeds the drop sets with the union: an address only the Pod resolver returns is exactly one a sandbox could CONNECT directly, so containment must cover it. Partial failures are logged and returned addresses are added without pruning. The first seed resolves with bounded retries **before resetting the nft table**; both authorities must complete successfully with a nonempty union. Failure leaves the previous kernel table intact, while success installs the seeded addresses atomically with the reset. DNS and nft application have separate timeouts. Literal proxy IPs are seeded permanently.
- **Requires `connection_strategy: lazy`** (the shipped default): eager connects upstream before any request exists, so no `via` can be applied.
- **Config validation**: a malformed proxy URL, credentials in the URL, or `..._AUTH` without `..._PROXY` fail egress startup; the addon likewise raises on load, so mitmdump will not start with an inconsistent config.
- **Policy interaction (`dns+nft`)**: the proxy endpoint is treated as infrastructure, not sandbox egress. In the sidecar profile the egress nft chain adds a dedicated accept scoped to `(mitmproxy UID, proxy IP, proxy port)`; the proxy IP is deliberately *not* added to the sandbox allow sets, which are IP-only and would otherwise let sandbox code dial the proxy port directly (e.g. `CONNECT` on 3128) to reach denied destinations. For a hostname endpoint the DNS answer is exempted from sandbox policy evaluation and feeds only the uid-scoped set, so the proxy name stays resolvable under a deny-all policy. The fast-sandbox profile enforces the same properties through its profile-wide endpoint drop and the infra-domain exemption (see the fast-sandbox bullet above).
- **Auth secrecy**: the auth value is sent only on the upstream `CONNECT` and is never logged. Prefer injecting it via the container env or a Secret over baking it into an image.

## Relationship with Policy/DNS

Transparent mitmproxy does not automatically consume egress `NetworkPolicy`. Domain allow/deny behavior is still determined by DNS + (optional) nft rules. If L7 policy enforcement is needed, implement it in mitm scripts.

Under the `dns+nft` sidecar profile, the REDIRECT rewrites an intercepted flow's destination to the mitmproxy listener port (default `18081`) before the nft egress chain evaluates it, so the chain accepts that rewritten destination (`127.0.0.1:<port>`) ahead of every port-scoped rule — the `reachable.ports.cap` floor, user `ports` rules, and the default-deny tail never blackhole interception itself, even when the original ports (80/443) are exactly what the policy allows. Enforcement is preserved end to end: mitmproxy's own re-originated connections are not exempt, so a port outside the cap or a denied destination still fails, surfaced as a proxy error rather than a silent timeout.

## Implementation Notes and Limits

Startup flow (high level):

1. Start mitmdump as user `mitmproxy`, listening on `127.0.0.1:<port>`.
2. Wait until the local listener is reachable.
3. Apply IPv4 `iptables` redirect rules: except loopback and mitmproxy-owned traffic, redirect outbound `80/443` to mitm port.

Limits:

- Currently IPv4 `iptables` only; IPv6 is not automatically handled.
- Non-Linux environments (for example local macOS runtime) are not supported for transparent mode.
- Full HTTPS decryption introduces CPU/memory and certificate trust overhead; benchmark before production rollout.

## Process Supervisor (Crash Recovery)

The egress sidecar includes a built-in supervisor that monitors the `mitmdump` child process and automatically restarts it on unexpected exits.

### Restart behavior

When `mitmdump` exits unexpectedly, the supervisor restarts it with **exponential backoff**: 1 s, 2 s, 4 s, ..., capped at **30 s**. Retries continue indefinitely until the process starts successfully or the egress sidecar itself shuts down.

A successful restart requires two conditions:

1. `mitmdump` process starts without error.
2. The listen port (`127.0.0.1:<port>`) accepts TCP connections within 15 seconds.

If the listener does not come up in time, the half-started process is gracefully terminated (SIGTERM → wait → SIGKILL) before the next attempt, so the port is released cleanly.

### Generation tagging

Each `mitmdump` launch is assigned a monotonically increasing **generation number**. When a process exits, the exit event carries the generation it was launched with. The supervisor compares this against the currently-live generation:

- **Match**: the live process just died — trigger restart.
- **Mismatch**: a stale process from a previous failed attempt was reaped — ignore.

This prevents restart storms where multiple rapid failures queue up cascading restart attempts.

### Health gate integration

When transparent mitmproxy is enabled:

- `/healthz` returns **503** until the full mitm stack is ready (process started, listener up, iptables installed, CA exported — sidecar; pinned CA seeded — fast-sandbox).
- Under the fast-sandbox profile, a missing or unloadable pinned CA latches the **addon-down** state: `/healthz` keeps returning 503 with `"status":"mitm-addon-down"` and the reason, while policy actions keep serving (the down state clears only on process restart).
- On crash, the health gate is set back to not-ready (503) immediately.
- After a successful restart and listener readiness, the health gate is restored.

Kubernetes readiness probes that hit `/healthz` will stop routing traffic to the sandbox during the restart window.

### Graceful shutdown

When the egress sidecar receives `SIGTERM` or `SIGINT`:

1. The supervisor watcher goroutine exits (context cancelled).
2. `iptables` transparent redirect rules are removed.
3. `mitmdump` receives `SIGTERM`; if it does not exit within 5 seconds, `SIGKILL` is sent.

Any `OnExit` callbacks still blocked on the restart channel are unblocked via a dedicated shutdown channel, preventing goroutine leaks.

### Observability

All supervisor activity is logged with the `[mitmproxy]` prefix:

| Log pattern | Meaning |
|-------------|---------|
| `mitmdump exited (gen=N): <error>; restarting...` | Live process crashed; restart initiated |
| `ignoring stale exit event (gen=N, current=M)` | Old generation reaped; no action needed |
| `restart attempt N failed; retrying in Xs` | Launch or listener wait failed; backing off |
| `mitmdump restarted (pid P, gen N, attempt M)` | Successful restart |
| `dropping exit event during shutdown` | Exit event discarded because egress is shutting down |
