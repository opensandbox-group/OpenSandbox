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

# Optional: load an additional user-defined mitm addon (loaded after the system addon)
export OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT=/path/to/your/addon.py

# Optional: bypass decryption for selected domains (semicolon-separated regex list)
export OPENSANDBOX_EGRESS_MITMPROXY_IGNORE_HOSTS='.*\.log\.aliyuncs\.com;.*\.example\.internal'
```

## Configuration Reference

| Variable | Required | Purpose | Default |
|------|----------|------|--------|
| `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` | Yes | Enable transparent mitmproxy (`1/true/on`, etc.) | Disabled |
| `OPENSANDBOX_EGRESS_MITMPROXY_PORT` | No | mitmdump listen port; `iptables` redirects `80/443` here | `18081` |
| `OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT` | No | Additional user mitm addon script path (`-s`); loaded after the system addon | Empty |
| `OPENSANDBOX_EGRESS_MITMPROXY_IGNORE_HOSTS` | No | Host/IP regex list for TLS pass-through (`;` separated) | Empty |
| `OPENSANDBOX_EGRESS_MITMPROXY_CONFDIR` | No | mitm config and CA directory (passed as `--set confdir=`, also used as `HOME`) | Default directory under `/var/lib/mitmproxy` |
| `OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_TRUST_DIR` | No | Trust directory for upstream TLS verification (OpenSSL style) | `/etc/ssl/certs` |
| `OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE` | No | Skip upstream TLS certificate verification (`1/true/on`). Needed when clients connect by IP (no SNI → hostname mismatch). | Disabled |

Notes:

- `OPENSANDBOX_EGRESS_MITMPROXY_IGNORE_HOSTS` means **no decryption**, not “completely bypass mitm process”.
- In transparent mode, mitmproxy generally recommends matching by IP/range; verify SNI/resolve behavior if using domain regex only.
- Before mitm, `iptables`, and CA export are ready, `GET /healthz` returns `503 (mitm not ready)` to prevent premature readiness.

## Common Configuration Templates

### 1) Enable Transparent MITM Only

```bash
export OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true
```

### 2) System Addon (Always On)

The bundled system addon at `/var/egress/mitmscripts/system.py` is shipped in the egress image and loaded automatically whenever transparent mode is enabled. It stays wire-transparent (no headers added or altered) and currently provides:

- Forces streaming (`flow.response.stream = True`) for SSE (`text/event-stream`) and chunked responses, so each chunk is forwarded immediately instead of being buffered up to the `stream_large_bodies=1m` threshold (critical for LLM streaming UX).

The system addon is always loaded and cannot be disabled via configuration. To override its behavior, supply a user addon via `OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT`; user addons are loaded after the system addon and may observe or override its hooks.

### 3) Add a User Addon Alongside the System Addon

```bash
export OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true
export OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT=/path/to/your/addon.py
```

The user addon is loaded after the system addon (`-s system.py -s user.py`), so user hooks observe and may override system behavior.

### 4) Bypass Decryption for Specific Domains (e.g. log upload)

```bash
export OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true
export OPENSANDBOX_EGRESS_MITMPROXY_IGNORE_HOSTS='.*\.log\.aliyuncs\.com'
```

### 5) Use a Fixed CA (consistent fingerprint across replicas)

If CA files already exist in `confdir`, mitmproxy reuses them instead of regenerating on each startup. Typical paths:

- `/var/lib/mitmproxy/.mitmproxy/mitmproxy-ca.pem` (private key)
- `/var/lib/mitmproxy/.mitmproxy/mitmproxy-ca-cert.pem` (public cert)

Ensure correct permissions (for example `mitmproxy:mitmproxy`, private key mode `600`).

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
