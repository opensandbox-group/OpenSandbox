---
title: execd
description: The in-sandbox execution daemon providing HTTP APIs for code execution, shell commands, filesystem operations, PTY sessions, and metrics.
---

# execd - OpenSandbox Execution Daemon

`execd` is the runtime daemon used inside OpenSandbox sandboxes.

It is built on Gin and exposes HTTP APIs for code execution, shell commands, filesystem operations, PTY sessions, and metrics.

## Quick Start

### 1) Build

```bash
cd components/execd
make build
```

On Linux, `make build` uses the native C compiler and static libc to produce
`bin/opensandbox-session-gate`. The published execd image already installs
this helper. If you run execd from a source build and need isolated sessions,
install it at the fixed trusted runtime path first:

```bash
make build-session-gate
sudo make install-session-gate
# /opt/opensandbox/opensandbox-session-gate (mode 0555)
```

Compilation runs before privilege escalation; the install target only copies
the built helper. Keep `/opt/opensandbox` and the helper root-owned and not
group- or world-writable. Other execd APIs still work without it, but
isolated-session capability probing and creation fail closed.

### 2) Start Jupyter Server

execd requires a Jupyter Server that is reachable at the address passed to
`--jupyter-host`. In a source checkout, source the repository helper and invoke
its `install_jupyter` function:

```bash
. ./tests/jupyter.sh
install_jupyter
```

Alternatively, install Jupyter if necessary, then start it with the token you
will pass to execd:

```bash
python3 -m pip install jupyter
jupyter server \
  --ip=127.0.0.1 \
  --port=54321 \
  --no-browser \
  --ServerApp.token=your-jupyter-token
```

The helper sets `JUPYTER_PORT=54321` and
`JUPYTER_TOKEN=opensandboxexecdintegrationtest`, then starts a Jupyter Notebook
process in the background. Use those values for the execd connection when
following the helper path. Keep the standalone Jupyter Server process running
while using execd.

### 3) Run execd

```bash
./bin/execd \
  --jupyter-host=http://127.0.0.1:54321 \
  --jupyter-token=your-jupyter-token \
  --port=44772
```

### 4) Verify

```bash
curl -v http://localhost:44772/ping
```

### Local Smoke Test (Command Inventory Regression)

`components/execd/tests/smoke.sh` starts dedicated local Jupyter and execd instances, waits for them to become ready, and then runs API smoke tests. It verifies command-inventory default compatibility, cap eviction only after the earliest terminal summary is absent while the second and third remain visible, and TTL expiry only after the command was publicly observed before terminal retention begins. Build execd in `components/execd`, then run one of these three modes:

```bash
# Default command-inventory lifecycle compatibility
./tests/smoke.sh

# Completed-summary cap: must use exactly TTL=1h and cap=2
EXECD_COMMAND_RECOVERY_TTL=1h EXECD_COMMAND_RECOVERY_MAX_TERMINAL=2 ./tests/smoke.sh --inventory-cap

# Completed-summary TTL expiry: must use exactly TTL=2s and cap=1000
EXECD_COMMAND_RECOVERY_TTL=2s EXECD_COMMAND_RECOVERY_MAX_TERMINAL=1000 ./tests/smoke.sh --inventory-expiry
```

To deterministically validate the inventory cap/expiry observation helpers without starting an additional service, run:

```bash
python3 -m unittest discover -s tests -p 'test_smoke_api_inventory.py'
```

Prerequisites are Bash, `curl`, Python, Python `requests`, a built `./bin/execd`, and a usable `jupyter notebook`. Ports `127.0.0.1:54321` and `127.0.0.1:44772` must be free.

The script sets a fixed 30-second readiness deadline for Jupyter `/api` and execd `/ping`, and checks that the `JUPYTER_PID` and `EXECD_PID` it started remain alive before and after requests to avoid accidentally using an existing listener. On success, failure, or interruption, its exit trap automatically stops and waits for both child processes. On success, it prints `[+] smoke tests PASS` and exits with code `0`.

On failure, first check the terminal for dependency, argument, or 30-second timeout messages. Then check `startup.log` (execd startup failures) and `execd.log` (execd runtime logs) in the current directory. If a port is occupied, stop the process using it or retry in an isolated environment.

## API

- OpenAPI spec: [execd-api.yaml](/api/)
- Common capability groups:
  - Code execution (`/code`, SSE stream)
  - Session and command execution (`/session`, `/command`)
  - Filesystem operations (`/files`, `/directories`)
  - Isolated sessions (`/v1/isolated/session`, bubblewrap namespaces)
  - PTY over WebSocket (`/pty`)
  - Local metrics endpoints (`/metrics`, `/metrics/watch`)

Shell-backed sessions use Bash when it is available and fall back to `sh` on
minimal images that do not include Bash. This applies to PTY sessions, the
Bash session API (which keeps its existing name for compatibility), and
isolated sessions. Commands submitted to a fallback session must use syntax
supported by that image's `sh` implementation.

### Command Inventory `GET /command`

`GET /command` returns a **summary inventory** of commands known to the current instance, for observing running and recently completed commands. It does not return command content or logs. Optional query parameters are:

- `running=true` returns only running commands; `running=false` returns only completed commands; omit it to return both.
- `limit` is `1` to `100` per page, with a default of `50`.
- `cursor` continues to the next page.

A cursor is opaque and bound to the requested filter. It is valid only for the lifetime of the Execd controller instance that issued it. After an Execd restart, controller replacement, or request routing to another instance, omit the cursor and restart from the first page. Inventory pagination is weakly consistent; retained summaries do not replace legacy status/log APIs.

The response contains `commands` and `pagination` (`limit` and optional `nextCursor`). Each summary includes `session`, `running`, `background`, and `started_at`; completed entries also include `finished_at`, `exit_code`, and optional `error`. Ordering and pagination are weakly consistent: if commands start, complete, or disappear because of retention during a read, results across pages are not guaranteed to be an exact snapshot or a complete recoverable history.

The inventory enumerates only summaries visible in this execd instance's memory. Callers with that instance's access token can enumerate it, so the token must be protected at the instance boundary. Inventory eviction does not clear existing session status, background logs, or command content; these legacy interfaces continue to follow their respective known-session lifecycles. This feature adds no inventory metrics.

#### SDK entry points

- Python async:

  ```python
  inventory = sandbox.command_inventory
  if inventory is not None:
      await inventory.list_commands(...)
  ```

- Python sync:

  ```python
  inventory = sandbox_sync.command_inventory
  if inventory is not None:
      inventory.list_commands(...)
  ```

- JavaScript: `sandbox.commands.listCommands?.(...)`
- Kotlin: `sandbox.commands().listCommands(...)`
- C#: `sandbox.CommandInventory?.ListCommandsAsync(...)`
- Go: `sandbox.ListCommands(ctx, req)` or `ExecdClient.ListCommands(ctx, req)`

`running`, `limit`, and `cursor` are the common query inputs. Python and C# inventory access may be unavailable with legacy/custom adapters, so check the capability before calling it. JavaScript legacy/custom `ExecdCommands` implementations may omit `listCommands`, so use the optional method invocation shown above. Blank request cursors normalize to omission; present blank response cursors are rejected by SDKs.

| Method and Path | Purpose |
|---|---|
| `GET /command` | Read the command summary inventory with pagination. |
| `GET /command/status/:id` | Read the detailed status of a known session. |
| `GET /command/:id/logs` | Read background command logs; foreground commands are rejected. |
| `DELETE /command?id=:id` | Interrupt a running command; completed sessions retain the existing not-running error semantics. |

## Isolated Sessions

Isolated sessions run a shell inside a per-execution
[bubblewrap](https://github.com/containers/bubblewrap) (`bwrap`) namespace,
created via `POST /v1/isolated/session`. Bash is preferred, with `sh` used as a
fallback. Beyond the workspace, callers can expose additional host paths into
the namespace.

### UID modes and capabilities

The optional `uid_mode` request field selects how identity is established:

- `setpriv` (the default) uses the container's existing user namespace and
  drops to the requested UID/GID with `setpriv`.
- `userns` creates a new user namespace and maps the requested UID/GID inside
  it, which can work in environments where the capabilities required by
  `setpriv` mode are unavailable.

At startup, execd probes both modes independently. `GET
/v1/isolated/capabilities` reports `setpriv_available` and
`userns_available`; the overall `available` field is true when either mode is
usable. Creating a session returns `503 NOT_SUPPORTED` only when the selected
mode is unavailable. The probes exercise the same identity path used at
runtime: the public `setpriv_available` flag covers execd's default UID/GID
path (so a root session that keeps UID/GID 0 does not require the `setpriv`
binary), while `userns` applies the UID/GID mapping and the setuid-aware
`--disable-userns` policy. A setpriv request that selects IDs different from
execd's own is checked against a separate startup identity-switch probe and
returns `503 NOT_SUPPORTED` before session side effects when that switch is not
available.

### Bind mounts

Two request fields control extra host paths:

- `extra_writable`: a list of paths bind-mounted read-write at the same path
  inside the namespace (`source == destination`).
- `binds`: explicit `source` → `dest` mappings, each optionally read-only.
  - `source` (required): host path to bind. It must **already exist** and is
    resolved (symlinks followed) before use.
  - `dest`: mount destination inside the namespace; defaults to `source` when
    omitted. It must be an **existing** mount point — `bwrap` cannot create a
    destination under the read-only root, so create the directory first.
  - `readonly` (default `false`): mount read-only (`--ro-bind`) when `true`,
    read-write (`--bind`) otherwise.

Example:

```json
{
  "workspace": { "path": "/workspace", "mode": "rw" },
  "binds": [
    { "source": "/data/in",  "dest": "/mnt/in", "readonly": true },
    { "source": "/data/out", "dest": "/mnt/out" }
  ]
}
```

### Writable allowlist

The source path of every `extra_writable` entry and every `binds` entry must
fall within the `allowed_writable` allowlist (see the isolation config file
below). The allowlist is enforced against the fully symlink-resolved real
path, so a symlink cannot redirect a bind outside the allowlist. An empty
allowlist rejects all `extra_writable`/`binds` requests.

The built-in default allowlist is `/workspace`, `/mnt`, `/media`, `/data`
(subpaths included). Set `allowed_writable` in the isolation config to
override it.

## Configuration

### CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--jupyter-host` | `""` | Jupyter server URL reachable by execd. |
| `--jupyter-token` | `""` | Jupyter token for HTTP/WebSocket auth. |
| `--port` | `44772` | HTTP listen port. |
| `--log-level` | `6` | Log level (0=Emergency, 7=Debug). |
| `--access-token` | `""` | Optional shared API access token. |
| `--graceful-shutdown-timeout` | `1s` | SSE tail-drain wait window before closing. |
| `--jupyter-idle-poll-interval` | `100ms` | Poll interval after Jupyter reports idle. |
| `--isolation-config` | `""` | Path to the isolation TOML config (see below). |
| `--command-recovery-ttl` | `1h` | Retention duration for completed command summaries. |
| `--command-recovery-max-terminal` | `1000` | Maximum number of completed command summaries to retain (maximum `10000`). |

### Environment Variables

| Variable | Description |
|---|---|
| `JUPYTER_HOST` | Same as `--jupyter-host` (overridden by explicit flag). |
| `JUPYTER_TOKEN` | Same as `--jupyter-token` (overridden by explicit flag). |
| `EXECD_ACCESS_TOKEN` | Same as `--access-token` (overridden by explicit flag). |
| `EXECD_API_GRACE_SHUTDOWN` | Same as `--graceful-shutdown-timeout`. |
| `EXECD_JUPYTER_IDLE_POLL_INTERVAL` | Same as `--jupyter-idle-poll-interval`. |
| `EXECD_ISOLATION_CONFIG` | Same as `--isolation-config`. |
| `EXECD_COMMAND_RECOVERY_TTL` | Retention duration for completed command summaries; default `1h`. |
| `EXECD_COMMAND_RECOVERY_MAX_TERMINAL` | Maximum number of completed command summaries; default `1000`, maximum `10000`. |
| `EXECD_CLONE3_COMPAT` | Linux clone3 compatibility switch (see below). |
| `EXECD_LOG_FILE` | Optional log output file path; default is stdout. |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Preferred OTLP metrics endpoint. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Fallback OTLP endpoint when metrics-specific endpoint is unset. |
| `OPENSANDBOX_ID` | Optional `sandbox_id` metric/resource attribute. |
| `OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS` | Optional extra metric attrs (`k=v,k2=v2`). |

Command-summary retention configuration is read from environment variables first, then overridden by CLI flags of the same name. `EXECD_COMMAND_RECOVERY_TTL=0` or `--command-recovery-ttl=0`, and `EXECD_COMMAND_RECOVERY_MAX_TERMINAL=0` or `--command-recovery-max-terminal=0`, all mean that only running commands are listed and no completed-summary entries are retained. Empty or unparseable values, a negative TTL, a negative cap, or a cap above `10000` emit a clear invalid-configuration diagnostic at startup and prevent the process from starting; correct the configuration before starting again.

### Isolation Config File

Isolated sessions read an optional TOML file given by `--isolation-config`
(or `EXECD_ISOLATION_CONFIG`). All fields are optional; omitted fields use
built-in defaults.

```toml
# Parent directory for per-session overlay upper directories.
upper_root = "/var/lib/execd/isolation"

# Host paths callers may request via extra_writable / binds.
# Enforced against the fully symlink-resolved real path; subpaths are allowed.
# Default: ["/workspace", "/mnt", "/media", "/data"]. Empty = reject all.
allowed_writable = ["/workspace", "/mnt", "/media", "/data"]
```

## Observability

### OpenTelemetry Metrics

OTLP metrics export is enabled when either endpoint is set:

- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`
- `OTEL_EXPORTER_OTLP_ENDPOINT`

### Local Metrics Endpoints

- `GET /metrics`: point-in-time host metrics snapshot
- `GET /metrics/watch`: SSE stream (1s cadence)

## Linux clone3 Compatibility

Some sandbox environments fail on `clone3(2)`.
Set `EXECD_CLONE3_COMPAT` in sandbox env to force fallback behavior:

- `1` / `true` / `yes` / `on`: enable seccomp fallback
- `reexec`: enable fallback and re-exec binary

## License

`execd` is part of OpenSandbox. See the [LICENSE](https://github.com/opensandbox-group/OpenSandbox/blob/main/LICENSE).
