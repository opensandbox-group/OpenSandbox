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

```bash
./tests/jupyter.sh
```

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

## PTY WebSocket access

The first WebSocket attached to `/pty/{session_id}/ws` is the exclusive
read/write holder. A second read/write connection receives `409 Conflict`
unless it uses `?takeover=1` to replace that holder.

After the read/write holder has started the shell, any number of read-only
clients can attach with:

```text
ws://localhost:44772/pty/{session_id}/ws?mode=viewer&since=0
```

Viewer connections receive the retained replay followed by live output, but
they never acquire or evict the read/write holder. The JSON `connected` frame
sets `role` to `viewer` (holders receive `role: "holder"`) so clients can use
the appropriate binary-frame decoder. Holders receive `0x01` stdout and, in
pipe mode, `0x02` stderr frames. Viewers receive `0x03` replay frames for both
retained and live output: an 8-byte big-endian offset followed by raw bytes.

Binary stdin and JSON `stdin`, `signal`, or `resize` frames are rejected with
a `READ_ONLY` error; `ping` remains available. The server closes a viewer after
five rejected mutating frames to bound error traffic from a misbehaving client.
WebSocket backpressure from a slow or disconnected viewer does not block the
interactive holder's live output pipe. In pipe mode, viewer output uses the
combined replay stream because replay does not preserve stdout/stderr channel
boundaries.

A viewer can attach only while the shell is running. When the shell exits,
viewers receive the `exit` frame and close. If a holder later starts the same
session again, its bounded replay buffer is retained, so a viewer reconnecting
with `since=0` can receive retained output from the preceding shell lifetime.

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

For a private-network Session (`share_net: false`), execd fixes the
authenticated network namespace and its owning user namespace before the
native workload gate is released. The two namespace bind mounts use an
execd-owned, unpredictable directory below `/run/execd/namespaces` and stay
owned by the Session until synchronous teardown. This applies to both UID
modes; shared-network Sessions do not create namespace pins.

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
| `--init` | `false` | Run as the sandbox init (OSEP-0018): reap children, forward signals, own the container lifecycle. Set together with `EXECD_INIT`; see [Init mode](#init-mode). |

### Environment Variables

| Variable | Description |
|---|---|
| `JUPYTER_HOST` | Same as `--jupyter-host` (overridden by explicit flag). |
| `JUPYTER_TOKEN` | Same as `--jupyter-token` (overridden by explicit flag). |
| `EXECD_ACCESS_TOKEN` | Same as `--access-token` (overridden by explicit flag). |
| `EXECD_API_GRACE_SHUTDOWN` | Same as `--graceful-shutdown-timeout`. |
| `EXECD_JUPYTER_IDLE_POLL_INTERVAL` | Same as `--jupyter-idle-poll-interval`. |
| `EXECD_ISOLATION_CONFIG` | Same as `--isolation-config`. |
| `EXECD_INIT` | Init-mode switch read by `bootstrap.sh`: when truthy (`1`/`true`/`yes`/`on`), the script `exec`s `execd --init -- <user command>` so execd becomes PID 1; see [Init mode](#init-mode). Unset preserves the classic background-and-wait topology. |
| `EXECD_CLONE3_COMPAT` | Linux clone3 compatibility switch (see below). |
| `EXECD_LOG_FILE` | Optional log output file path; default is stdout. |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Preferred OTLP metrics endpoint. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Fallback OTLP endpoint when metrics-specific endpoint is unset. |
| `OPENSANDBOX_ID` | Authoritative sandbox id stamped into eBPF audit records (`sandbox_id`) and metrics; the server injects it on Docker/Kubernetes task-template paths. Kubernetes pool allocations that skip the task template (default entrypoint, no env, no init mode) cannot inject it, and the eBPF layer reports `unsupported` attribution on that path. |
| `OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS` | Optional extra metric attrs (`k=v,k2=v2`). |

### Lifecycle hook trust boundary

`execd` runs configured `preStart` and `periodic` commands directly as its own
OS user in the sandbox's existing container namespaces. Lifecycle hooks do not
use isolated-session confinement.

Lifecycle hooks are trusted setup and maintenance code, not a security or
policy boundary. The default persisted config at
`$HOME/.execd/lifecycle.toml` is writable by execd's user, and a root sandbox
workload can modify or remove it. Do not use hooks for tamper-resistant
auditing or mandatory controls against sandbox workloads.

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

### Hardening Floor

The pre-exec privilege floor (OSEP-0018 §4) is off by default. Enable it in
the same isolation TOML:

```toml
[hardening]
enabled = true

# Capabilities the workload keeps (raised in the ambient set).
# Default: drop all. Names use the CAP_ prefix.
keep_capabilities = []

# Optional: replace the built-in syscall denylist. With hardening enabled,
# "execve" is reserved for the launcher's final exec and is rejected at
# startup ("execveat" stays allowed).
[seccomp]
deny = ["mount", "ptrace", "bpf", "seccomp"]

# Optional: Landlock filesystem confinement on top of the floor.
[landlock]
enabled = true
extra_writable = []   # writable paths beyond the built-in set
extra_readable = []   # read-only paths beyond the built-in set
```

When enabled, every user-code process (entrypoint, `/command`, `/code`,
PTY) is launched through the `opensandbox-launcher` native helper, which
applies the floor between fork and exec: execd credential env vars are
stripped, the bounding set is trimmed to `keep_capabilities` (none by
default), `no_new_privs` is set, the identity is dropped to the image's
user, kept caps are raised in the ambient set, and the seccomp filter is
installed last. Isolated-session workloads are already reduced inside the
bwrap namespace and are not additionally wrapped.

Everything is fail-open and reported on `GET /v1/isolated/capabilities`
under `hardening.cap_drop` / `hardening.seccomp` / `hardening.landlock`
(`active` | `degraded` | `unsupported` | `disabled` with a reason message).
Missing `CAP_SETPCAP` degrades the cap drop but keeps seccomp; a missing
launcher binary disables the floor; a kernel without Landlock (ABI < 1)
reports `unsupported` and skips FS confinement.

With `[landlock] enabled`, user-code processes are allowlisted to: system
paths (`/usr`, `/bin`, `/lib`, `/lib64`, `/etc`) read+exec, `/proc/self`
and `/proc/sys` read+exec (never all of `/proc`, which would re-expose
`/proc/1` and execd's credentials), the needed `/dev` device files and the
controlling tty, `/tmp`, `/run`, `allowed_writable`, plus
`extra_writable`/`extra_readable`. Everything else is denied. Note that
only the initial workload process keeps `/proc/self` access (a Landlock
rule is inode-based); forked descendants lose their own `/proc/self` —
tooling that needs it should be run as the entrypoint process.

Two Landlock kernel behaviors shape the policy:

- `path_beneath` rules are scoped to the mount the path belongs to, so at
  startup execd expands every rule onto each mount point beneath it —
  bind-mounted workspaces (a separate mount) get the same access as their
  parent path.
- rules only accept directory parents, so per-file grants are impossible;
  well-known proc files (`/proc/cpuinfo`, `/proc/meminfo`, …) are not
  individually readable under Landlock.

Recommended container ceiling (operator side): keep `CAP_SETPCAP`,
`CAP_SETUID`, `CAP_SETGID` so execd can reduce children; drop the rest
(`NET_RAW`, `SYS_MODULE`, `SYS_TIME`, `SYS_TTY_CONFIG`, `AUDIT_WRITE`,
`MKNOD`).

### eBPF Observation

Opt-in exec/connect/privilege audit (OSEP-0018 §5), off by default:

```toml
[ebpf]
enabled = true
observe = ["exec", "connect", "privilege"]   # default: all three
audit_file = "/var/log/opensandbox/ebpf-audit.jsonl"  # rotated JSONL
```

Requires the `execd-ebpf` build variant (CGO + `cilium/ebpf`), a
container with `CAP_BPF` + `CAP_PERFMON`, and a BTF-capable kernel —
Linux ≥ 5.10 with `CONFIG_DEBUG_INFO_BTF` (5.10–5.15 kernels use the
inline-`filename` trace event layout, which the BPF program detects via
CO-RE; 5.16+ use the `__data_loc` layout). Events are scoped to the
sandbox cgroup, so only this sandbox's processes are observed; they are
written as JSONL (one object per line) with a stable common envelope
(`ts`, `event`, `sandbox_id`, `pid`, `comm`) plus per-kind fields
(`filename`/`ppid` for `exec`, `dst_ip`/`dst_port`/`proto` for
`connect`, uid/gid deltas and `cap_added` for `privilege`). Under
gVisor/Kata the host kernel is not attachable, and the layer reports
`unsupported`. Missing prerequisites never block startup.

The default image ships both binaries: `execd` (the static default variant,
without eBPF code) and `execd-ebpf` (the observation variant with CGO +
cilium/ebpf, built in the Dockerfile's `ebpf-builder` stage and copied
alongside `execd`). `make build-ebpf` produces the standalone
`bin/execd-ebpf` variant. Server-side selection of the observation binary
based on `[ebpf] enabled` is not wired up yet, so run the `execd-ebpf`
binary explicitly when observation is required.

## Observability

### OpenTelemetry Metrics

OTLP metrics export is enabled when either endpoint is set:

- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`
- `OTEL_EXPORTER_OTLP_ENDPOINT`

### Local Metrics Endpoints

- `GET /metrics`: point-in-time host metrics snapshot
- `GET /metrics/watch`: SSE stream (1s cadence)

## Init mode

[OSEP-0018](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0018-execd-as-sandbox-init.md) makes execd the sandbox
init: it becomes the parent of the user entrypoint, reaps every child through
a single reaper, forwards application signals, and propagates the entrypoint
exit code to the container runtime.

Init mode is **off by default** and gated by two settings set in lockstep:

- `EXECD_INIT` (read by `bootstrap.sh`): decides the process topology — the
  script `exec`s into `execd --init -- <user command>` so execd inherits PID 1
  (Docker / K8s Batch paths), instead of backgrounding execd and the user
  command as siblings.
- `--init` (read by execd): activates the init duties (reaper, signal
  forwarding, lifecycle). If execd is not PID 1 (e.g. the K8s Pool task path,
  or a stray `&`), it degrades to subreaper mode: orphan reaping works, but
  the kernel PID 1 signal shield does not.

Behavioral contract in init mode:

- The user entrypoint owns the container lifecycle: when it exits, execd
  stops the remaining children (`SIGTERM` → grace → `SIGKILL`) and exits with
  the entrypoint's status.
- `HUP`/`USR1`/`USR2`/`WINCH` are forwarded to the entrypoint process group.
- `SIGTERM` (runtime-initiated container stop) is forwarded to the workload
  and starts the graceful shutdown sequence.
- In-namespace `kill -9 1` is inert (kernel signal shield). A workload
  `kill 1` (SIGTERM) is treated like a runtime stop; the trusted out-of-band
  stop channel is a follow-up (see the OSEP, §3).
- The actual mode is reported on `GET /v1/isolated/capabilities` under
  `hardening.init_mode` (`pid1` | `subreaper` | `none`).

### Pool (pre-warmed) sandboxes

Pool tasks are executed by the task-executor with `bootstrap.sh <entrypoint>`.
With `execd_run_as_init` enabled, the generated task no longer backgrounds
bootstrap: the task-executor's shim shell execs bootstrap, which execs
`execd --init`, so execd becomes the root of the task process tree —
orphaned task children are reaped (subreaper mode, since the task process is
not the container's PID 1) and the entrypoint exit code propagates back to
the shim and the task status.

To make execd the *container's* PID 1 in pooled pods, the operator's Pool pod
template should start the main container with `bootstrap.sh` plus a
keep-alive entrypoint and `EXECD_INIT=1`:

```yaml
spec:
  template:
    spec:
      containers:
        - name: sandbox
          image: opensandbox/execd:latest
          command: ["/bootstrap.sh", "/bin/sh", "-c", "while :; do sleep 3600; done"]
          env:
            - name: EXECD_INIT
              value: "1"
            - name: EXECD
              value: /execd
```

execd then stays alive as PID 1 (reaping + kernel signal shield) while the
task-executor keeps running user tasks in the pod. The K8s Restart recycle
strategy (`kill 1` via pod exec) keeps working against init-mode execd: the
signal is forwarded and execd exits with the workload's status, so the
kubelet restarts the container.

## Linux clone3 Compatibility
Some sandbox environments fail on `clone3(2)`.
Set `EXECD_CLONE3_COMPAT` in sandbox env to force fallback behavior:

- `1` / `true` / `yes` / `on`: enable seccomp fallback
- `reexec`: enable fallback and re-exec binary

## License

`execd` is part of OpenSandbox. See the [LICENSE](https://github.com/opensandbox-group/OpenSandbox/blob/main/LICENSE).
