# Python mitmproxy Transparent Mode (with Egress)

Transparent mode starts `mitmdump --mode transparent` inside the sidecar and redirects local outbound `TCP 80/443` traffic to the mitmproxy listener via `iptables`. Its core benefits are:

- **No application changes**: no need to set `HTTP_PROXY`; app traffic is intercepted transparently.
- **Observability and extensibility**: use mitm scripts for header injection, auditing, and debugging.
- **Controlled bypass**: use `ignore_hosts` for pass-through TLS (forward only, no decryption).

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

To bypass decryption for selected domains, edit the baked-in
`components/egress/mitmproxy/config.yaml` and rebuild the image — see
"Static Configuration (config.yaml)" below.

## Configuration Reference

### Environment Variables (Per-Deployment Overrides)

| Variable | Required | Purpose | Default |
|------|----------|------|--------|
| `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` | Yes | Enable transparent mitmproxy (`1/true/on`, etc.) | Disabled |
| `OPENSANDBOX_EGRESS_MITMPROXY_PORT` | No | mitmdump listen port; `iptables` redirects `80/443` here | `18081` |
| `OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT` | No | User mitm addon script paths (comma-separated); each is passed as `-s` and loaded after the system addon in order | Empty |
| `OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_TRUST_DIR` | No | Trust directory for upstream TLS verification (OpenSSL style); overrides the config.yaml default | `/etc/ssl/certs` |
| `OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE` | No | Skip upstream TLS verification (`1/true/on`); use when clients connect by IP and SNI is unavailable | Disabled |
| `OPENSANDBOX_EGRESS_MITMPROXY_EXTRA_PORTS` | No | **Experimental.** Extra destination TCP ports to intercept, appended to the always-on `80,443` (comma-separated, e.g. `8080,8443`). Fails closed at startup on invalid input; total ports (including 80/443) must be ≤ 15. Note: the system addon's credential-binding matcher currently only fires on canonical 80/443 — extras are decrypted and logged but not matched against bindings. | Empty |
| `OPENSANDBOX_EGRESS_UPSTREAM_PROXY` | No | Chained upstream proxy endpoint (`http://host[:port]` or `https://host[:port]`). When set, the bundled `upstream_proxy.py` addon is loaded after the system addon and every mitmproxy-handled connection is forwarded through the proxy via `CONNECT`. Fail closed: pass-through flows that cannot be chained are refused. | Empty (disabled) |
| `OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH` | No | Complete `Proxy-Authorization` header value sent on the upstream `CONNECT` (e.g. `Basic base64(user:pass)`). Requires `OPENSANDBOX_EGRESS_UPSTREAM_PROXY`; startup fails if set alone. Never logged. | Empty |

Notes:

- In transparent mode, mitmproxy generally recommends matching by IP/range; verify SNI/resolve behavior if using domain regex only.
- Before mitm, `iptables`, and CA export are ready, `GET /healthz` returns `503 (mitm not ready)` to prevent premature readiness.

### Static Configuration (config.yaml)

Fast Sandbox-wide, rarely-changing mitm options live in
`components/egress/mitmproxy/config.yaml`, baked into the image at
`/var/lib/mitmproxy/.mitmproxy/config.yaml` and auto-loaded by mitmdump.
This is the single source of truth for:

- `mode` (`transparent`) — mitm default is `regular`
- `listen_host` (`127.0.0.1`) — mitm default is `0.0.0.0`
- `stream_large_bodies` (`1m`) — mitm default is unset (entire body buffered)
- `ssl_verify_upstream_trusted_confdir` (`/etc/ssl/certs`) — mitm default is unset; overridable per-deployment via env
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

Edit `components/egress/mitmproxy/config.yaml` and append to `ignore_hosts`,
then rebuild the egress image:

```yaml
ignore_hosts:
  - '.*\.log\.aliyuncs\.com'
```

`ignore_hosts` means **no decryption**, not "completely bypass mitm process":
mitm still proxies the TCP connection, it just forwards bytes without
breaking TLS, and addons do not see request/response content.

`ignore_hosts` (like `tcp_hosts`/`udp_hosts`) is **incompatible** with
`OPENSANDBOX_EGRESS_UPSTREAM_PROXY`: pass-through connections cannot be chained
through a CONNECT proxy, so the upstream addon refuses to load when any
pass-through list is non-empty instead of letting those destinations bypass
the proxy.

### 5) Use a Fixed CA (consistent fingerprint across replicas)

If CA files already exist in `confdir`, mitmproxy reuses them instead of regenerating on each startup. Typical paths:

- `/var/lib/mitmproxy/.mitmproxy/mitmproxy-ca.pem` (private key)
- `/var/lib/mitmproxy/.mitmproxy/mitmproxy-ca-cert.pem` (public cert)

Ensure correct permissions (for example `mitmproxy:mitmproxy`, private key mode `600`).

### 6) Chain Through an Upstream Proxy (Corporate/Forward Egress)

```bash
export OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true
export OPENSANDBOX_EGRESS_UPSTREAM_PROXY=https://proxy.example.com:8443
# Optional: complete Proxy-Authorization header value for the upstream CONNECT
export OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH="Basic $(printf 'user:pass' | base64)"
```

When set, the bundled `upstream_proxy.py` addon is loaded after the system
addon and all mitmproxy-handled egress is chained: mitmproxy dials the
configured proxy and issues `CONNECT <request.host>:<port>`. For intercepted
TLS the authority is the SNI/Host-derived FQDN (not the intercepted IP), so the
upstream proxy resolves and dials the original destination itself; flows where
only an IP is known keep `IP:port` as the authority.

Semantics and limits:

- **`https://` endpoints** get TLS to the proxy with SNI and hostname
  verification against the proxy host, using the same
  `ssl_verify_upstream_trusted_confdir`/`_trusted_ca` options that verify real
  upstreams (default `/etc/ssl/certs`, overridable via
  `OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_TRUST_DIR`).
- **Fail closed**: connections that cannot be chained — TLS pass-through
  (no-SNI or `ignore_hosts`/`tcp_hosts`/`udp_hosts` matches) and UDP/QUIC
  dials — are refused rather than silently sent direct.
- **Requires `connection_strategy: lazy`** (the shipped default): eager
  connects upstream before any request exists, so no `via` can be applied.
- **Config validation**: a malformed proxy URL, credentials in the URL, or
  `..._AUTH` without `..._PROXY` fail egress startup; the addon likewise raises
  on load, so mitmdump will not start with an inconsistent config.
- **Policy interaction (`dns+nft`)**: the proxy endpoint is treated as
  infrastructure, not sandbox egress. The egress nft chain adds a dedicated
  accept scoped to `(mitmproxy UID, proxy IP, proxy port)`; the proxy IP is
  deliberately *not* added to the sandbox allow sets, which are IP-only and
  would otherwise let sandbox code dial the proxy port directly (e.g. `CONNECT`
  on 3128) to reach denied destinations. For a hostname endpoint the DNS
  answer is exempted from sandbox policy evaluation and feeds only the
  uid-scoped set, so the proxy name stays resolvable under a deny-all policy.
- **Auth secrecy**: the auth value is sent only on the upstream `CONNECT` and
  is never logged. Prefer injecting it via the container env or a Secret over
  baking it into an image.

## Relationship with Policy/DNS

Transparent mitmproxy does not automatically consume egress `NetworkPolicy`. Domain allow/deny behavior is still determined by DNS + (optional) nft rules. If L7 policy enforcement is needed, implement it in mitm scripts.

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

- `/healthz` returns **503** until the full mitm stack is ready (process started, listener up, iptables installed, CA exported).
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
