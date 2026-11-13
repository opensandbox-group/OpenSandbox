---
title: In-Sandbox Privilege Reduction
description: Run every process inside a sandbox below the container ceiling — execd as sandbox init plus a capability, seccomp, and Landlock floor configured through one TOML file baked into your image.
---

# In-Sandbox Privilege Reduction

Sandboxes run untrusted and AI-generated code. The container boundary ([secure container runtime](/guides/secure-container), capability drops, seccomp profiles) is set by the operator at create time — everything inside the container starts with those same privileges. Privilege reduction adds a second layer *inside* the sandbox: execd launches every user-code process with a strict subset of the container's privileges — no effective capabilities, `no_new_privs`, a syscall denylist, and optional Landlock filesystem confinement.

The feature is configured by a single TOML file that travels with your image. You decide *whether* to reduce; the built-in defaults decide *how*. Everything is fail-open: a missing kernel feature is reported and skipped, never fatal, and the capabilities endpoint publishes what is actually enforced instead of what was requested.

Privilege reduction is an operator/image-level policy, not a per-request option — it cannot be toggled from `CreateSandboxRequest`.

## How it is structured

Two independent switches work together:

| Switch | Where it lives | Default | What it gives you |
|---|---|---|---|
| Init mode (`runtime.execd_run_as_init`) | Server config, `[runtime]` section | `true` on `main`; `false` in 1.2.1 and earlier | execd becomes the sandbox init (PID 1): zombie reaping, signal forwarding, the kernel's PID 1 signal shield |
| The hardening floor (`[hardening] enabled`) | execd isolation TOML, staged via `EXECD_ISOLATION_CONFIG` | off | every user-code process runs reduced: capabilities dropped, `no_new_privs`, seccomp, optional Landlock |

**Init mode** — with `execd_run_as_init = true`, the server injects `EXECD_INIT=1` and the image's `bootstrap.sh` ends in `exec execd --init -- <entrypoint>`, so execd replaces the bootstrap shell as PID 1. It reaps orphaned children (no zombie accumulation), forwards application signals (`HUP`, `USR1`, `USR2`, `WINCH`) to your entrypoint, forwards an external `SIGTERM` (Docker stop, Kubernetes pod deletion) so your process shuts down gracefully, and propagates the entrypoint's exit code to the runtime. As PID 1 it also gains the kernel's signal shield — an in-namespace `kill 1` or `kill -9 1` from workload code is inert — and execd marks itself non-dumpable, so workload code cannot read `/proc/1/environ` to steal execd's credentials.

**The floor** — when `[hardening] enabled = true`, execd launches every user-code process through a small native helper (`opensandbox-launcher`, staged into the sandbox by the platform). Between `fork` and `exec`, the launcher applies an order-sensitive sequence and then `execve`s your program:

```
strip execd credential env → trim the capability bounding set
  → no_new_privs → set identity → raise kept capabilities (ambient)
  → Landlock ruleset → seccomp filter (last) → execve(user command)
```

The floor covers **every** user-code path: the image entrypoint, `/command` (commands and shell sessions), `/code` (Jupyter kernels), and PTY terminals. Isolated sessions ([isolation sessions](/guides/isolation-sessions)) already run inside a bwrap namespace with their own seccomp/`no_new_privs` floor and compose with this one unchanged.

On Fast Sandbox templates the server bakes `EXECD_INIT=1` into the golden image instead: execd runs as a subreaper under the guest init, so reaping, signal forwarding, and the floor all apply, but the PID 1 signal shield stays with the guest init.

::: warning Drift between the switches
If the TOML enables `[hardening]` but init mode is off, execd cannot wrap the image entrypoint (the bootstrap shell launches it, not execd). Enforcement continues for execd-spawned commands, and the capabilities endpoint honestly reports the enabled layers as `degraded` with guidance to enable `runtime.execd_run_as_init`. It never pretends to cover what it does not.
:::

## Step 1 — write the isolation config

This is the recommended configuration — the same content as [`configs/isolation.hardened.toml`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/execd/configs/isolation.hardened.toml) in the repository:

```toml
[hardening]
enabled = true   # the privilege floor for every user-code process

[landlock]
enabled = true   # filesystem allowlist on top of the floor (Linux >= 5.13)
```

Two lines, one per layer; everything else uses the built-in defaults described below. Save it in your image, for example at `/etc/opensandbox/isolation.toml`.

You do not have to write the file from scratch — the repository ships ready-made configs under [`components/execd/configs/`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/execd/configs/):

- [`isolation.hardened.toml`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/execd/configs/isolation.hardened.toml) — the recommended config above, verbatim. Start here.
- [`isolation.example.toml`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/execd/configs/isolation.example.toml) — the complete annotated reference: every key with its default value, plus the built-in seccomp denylist written out, ready to uncomment section by section. Use it as the base when you need an override.
- [`isolation.custom.toml`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/execd/configs/isolation.custom.toml) — a worked custom-policy example: a `[seccomp] deny` override (replacing the built-in list) plus `keep_capabilities = ["CAP_NET_RAW"]`.

Copy one into your image and trim what you do not need — an absent section always means the built-in default.

### What the recommended settings mean

- `[hardening] enabled = true` routes every user-code process through the launcher. Each process ends up with an empty effective capability set, a trimmed bounding set, `no_new_privs = 1`, the built-in seccomp denylist, and — because the identity is unchanged by default — whatever UID the image declares. Set `keep_capabilities` if a workload genuinely needs one capability back (see the [reference](#configuration-reference)).
- `[landlock] enabled = true` adds a filesystem allowlist on top: system paths stay readable, `/tmp`, `/run`, `/dev` device files, and the workspace writable paths stay writable, and everything else is denied. The kernel must support Landlock (Linux ≥ 5.13); on older kernels the layer reports `unsupported` and is skipped while the rest of the floor keeps working.

If you only want the privilege floor without filesystem confinement, keep the `[hardening]` section and omit `[landlock]`.

## Step 2 — stage the config into sandboxes

execd reads the config path from the `EXECD_ISOLATION_CONFIG` environment variable (or the `--isolation-config` flag). Pick one delivery route:

### Bake it into your image (recommended)

```dockerfile
# Your sandbox image
COPY isolation.toml /etc/opensandbox/isolation.toml
ENV EXECD_ISOLATION_CONFIG=/etc/opensandbox/isolation.toml
```

The platform stages execd, `bootstrap.sh`, and the launcher into your container at create time (Docker) or via the `execd-installer` init container (Kubernetes), so you do not need to build execd yourself — use a current execd image (`runtime.execd_image` in the server config) and set `runtime.execd_run_as_init = true` (default). The image ENV makes execd pick up the file on every sandbox created from this image; no server or request changes needed.

### Inject it from the server (Docker runtime)

For a policy that applies to every sandbox without rebuilding images, bind-mount the file and point execd at it via the server config:

```toml
[docker]
sandbox_env = { EXECD_ISOLATION_CONFIG = "/etc/opensandbox/isolation.toml" }
sandbox_binds = [
  "/etc/opensandbox/isolation.toml:/etc/opensandbox/isolation.toml",
]
```

([storage] `allowed_host_paths` must include the host path of the bind.)

### Mount it from a ConfigMap (Kubernetes runtime)

Add the TOML as a ConfigMap key, mount it in the BatchSandbox pod template, and set `EXECD_ISOLATION_CONFIG` to the mounted path — either in the template env or per create-request env. This is how the reference e2e delivers the policy (`opensandbox-e2e-execd-isolation` ConfigMap in `scripts/common/kubernetes-e2e.sh`).

::: tip Config errors are fatal, missing features are not
A file that does not parse, a misspelled capability name, or a `[seccomp] deny` that lists the reserved `execve` makes execd exit at startup and the sandbox fails to start — fix the file. A missing *runtime* capability (old kernel, missing `CAP_SETPCAP`, absent launcher binary) never blocks startup: the layer is reported as `degraded` or `unsupported` and the rest keeps working.
:::

## Step 3 — create a sandbox and verify

```python
from opensandbox import SandboxSync

sbx = SandboxSync.create()  # your hardened image
caps = sbx.isolation.capabilities()
h = caps.hardening
print(h.init_mode)             # "pid1"  (execd is the sandbox init)
print(h.signal_shield)         # True
print(h.cap_drop.state)        # "active"
print(h.seccomp.state)         # "active"
print(h.landlock.state)        # "active" or "unsupported" (kernel < 5.13)
```

From inside the sandbox, the floor is directly observable in `/proc/self/status`:

```text
CapEff:   0000000000000000   # no effective capabilities (default policy)
CapBnd:   0000000000000000   # bounding set trimmed
NoNewPrivs: 1
Seccomp:  2                  # seccomp filter mode
```

Each layer's `state` is `active` (enforced), `disabled` (not configured), `degraded` (configured but a prerequisite was missing — see the `message` field for the concrete reason), or `unsupported` (this kernel or build cannot provide it).

## Configuration reference

All keys are optional. A missing file or a missing key falls back to the default shown. The same surface is documented inline, with every default written out, in [`configs/isolation.example.toml`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/execd/configs/isolation.example.toml); sources of truth: [`pkg/isolation/config.go`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/execd/pkg/isolation/config.go) and [`pkg/runtime/hardening_linux.go`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/execd/pkg/runtime/hardening_linux.go).

### File-level keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `upper_root` | string | `"/var/lib/execd/isolation"` | Parent directory for per-isolated-session overlay upper directories. |
| `upper_max_bytes` | int | `8589934592` (8 GiB) | Allocation-time threshold for total overlay upper size. Existing sessions may exceed it; `0` disables the check. |
| `diff_max_bytes` | int | `4294967296` (4 GiB) | Maximum size of a tar.gz session diff. |
| `allowed_writable` | []string | `["/workspace", "/mnt", "/media", "/data"]` | Paths callers may bind writable, and the writable base for Landlock. **Setting it replaces the default — no merging.** `[]` rejects all writable binds. |

### `[seccomp]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `deny` | []string | *(absent → built-in denylist)* | Syscalls to block. When the section is absent, the [built-in denylist](#built-in-seccomp-denylist) is used. When present, it **completely replaces** the built-in list — no merging — and with `[hardening]` on it becomes the seccomp floor for every user-code process, not just isolated sessions. Names unknown to the current architecture are silently skipped. With hardening enabled, `execve` is rejected at load time (reserved for the launcher's final exec; deny `execveat` instead if you need that one). |

### `[hardening]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool | `false` | Master switch for the privilege floor. When on, every user-code process (entrypoint, `/command`, `/code`, PTY) is launched through the launcher with dropped capabilities, `no_new_privs`, and the seccomp floor. Paired with `EXECD_INIT` injected by the server. |
| `keep_capabilities` | []string | `[]` (drop all) | Capabilities the workload retains. Each named cap (`CAP_NET_RAW`, `CAP_SYS_TIME`, …) is raised in the ambient set so it survives `execve`. A name must be a valid `CAP_*` identifier or execd exits at startup. A kept cap must also exist in the container ceiling — if the operator's `drop_capabilities` removed it, the ambient raise fails and the workload simply runs without it. |

`no_new_privs` is always set when the floor is on and is not configurable; it costs nothing and is what makes the reduction irrevocable across `execve`.

### `[landlock]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool | `false` | Apply a filesystem allowlist to user-code processes on top of the floor. Requires Linux ≥ 5.13 (kernel ABI ≥ 1); otherwise the layer reports `unsupported` and is skipped. |
| `extra_writable` | []string | `[]` | Additional read-write paths beyond the built-in set (see the [default allowlist](#built-in-landlock-allowlist)). Each path **must exist** in the image: an uninstallable rule disables the whole Landlock layer (`degraded`) rather than weakening confinement. |
| `extra_readable` | []string | `[]` | Additional read-only (read + exec) paths beyond the built-in set. Same exists-check semantics. |

## Built-in defaults you inherit

These are deliberately **not** configuration keys — they are applied whenever the relevant switch is on.

### Applied when `[hardening] enabled = true`

| Behavior | Built-in value |
|---|---|
| `no_new_privs` | always set on user-code processes |
| Dropped capabilities | all (empty effective set; raise `keep_capabilities` selectively) |
| Identity | the image's own runtime user — the floor never invents a UID. A root image keeps UID 0 but without capabilities; for UID separation use a non-root image or [isolated sessions](/guides/isolation-sessions) |
| Env strip for `/command`, `/code`, PTY | `EXECD_ACCESS_TOKEN`, `JUPYTER_HOST`, `JUPYTER_TOKEN`, `EXECD_ISOLATION_CONFIG`, `EXECD_ENVS`, `OPENSANDBOX_LIFECYCLE`, `EXECD_LIFECYCLE_CONFIG`, `EXECD_RUNTIME_INIT` |
| Env strip for the entrypoint | `EXECD_ACCESS_TOKEN`, `OPENSANDBOX_LIFECYCLE`, `EXECD_LIFECYCLE_CONFIG`, `EXECD_RUNTIME_INIT` — entrypoint scripts keep `JUPYTER_TOKEN`/`EXECD_ENVS` because images like the code-interpreter entrypoint configure themselves from them |
| Signal forwarding (init mode) | `HUP`, `USR1`, `USR2`, `WINCH` to the entrypoint; external `SIGTERM` (container stop) is forwarded for graceful shutdown; the entrypoint's exit code becomes the container's |

### Applied when `[landlock] enabled = true`

Landlock is an allowlist: access under unlisted paths is denied, and rules can only grant — you cannot carve an exception out of a granted subtree. Grants are automatically duplicated onto every mount point beneath a granted path, so bind-mounted workspaces and PVCs keep working.

| Paths | Access |
|---|---|
| `/` | execute only (path traversal and `execve`, no read) |
| `/usr`, `/bin`, `/lib`, `/lib64`, `/etc`, `/opt` | read + execute |
| `/proc/self`, `/proc/sys` | read + execute — deliberately never all of `/proc`, so other processes' `/proc/<pid>` (including execd's `/proc/1/environ`) stays inaccessible even to a same-UID workload |
| `/dev/null`, `/dev/zero`, `/dev/full`, `/dev/random`, `/dev/urandom`, `/dev/tty`, `/dev/pts` | read + write |
| `/tmp`, `/run` | read, write, create, remove, rename, truncate |
| each `allowed_writable` path | read/write + execute (workloads compile and run scripts there) |
| `extra_writable` | read/write/create/remove/rename/truncate |
| `extra_readable` | read + execute |

All built-in grants are best-effort — a path that does not exist in your image (alpine has no `/lib64`, minimal images lack `/workspace`) is skipped. Only operator-declared `extra_*` paths are required, and a missing one disables the layer (fail closed for your explicit grants, fail open overall).

::: warning Known Landlock limitation with `/proc/self`
The `/proc/self` rule is inode-based: it grants the launcher's own `/proc/<pid>`, which the initial workload keeps across `execve`. A *forked descendant* resolves `/proc/self` to a different directory the inherited ruleset does not cover, so tools that inspect per-process procfs (for example `ps`, or a child reading its own `/proc/self/status`) fail with `EACCES` under Landlock. Granting all of `/proc` is not an option — it would re-expose `/proc/1` and execd's credentials.
:::

### Built-in seccomp denylist

Active for isolated sessions by default, and for every user-code process once `[hardening] enabled = true`. A custom `[seccomp] deny` replaces this list entirely.

```text
mount, umount2, chroot, pivot_root,                      filesystem manipulation
ptrace, process_vm_readv, process_vm_writev, kcmp,       process introspection
init_module, finit_module, delete_module,                kernel modules
bpf, seccomp,                                            BPF / seccomp manipulation
personality,                                             execution domain
add_key, request_key, keyctl,                            kernel key management
iopl, ioperm,                                            I/O privilege
reboot, syslog, swapon, swapoff,                         system state
setns, unshare,                                          namespace manipulation
name_to_handle_at, open_by_handle_at,                    handle-based operations
userfaultfd, kexec_load, kexec_file_load, acct           other
```

(`setuid`/`setgid` families are intentionally not denied — the launcher itself needs them to drop identity.)

## Degradation matrix

Every layer fails independently and is reported with a reason on the capabilities endpoint:

| Layer | Prerequisite | When it is missing |
|---|---|---|
| Cap drop | `CAP_SETPCAP` in execd's effective caps | `degraded` — bounding-set trim skipped (workload keeps the ceiling's bounding set), but the effective-cap drop, `no_new_privs`, and seccomp still apply |
| Seccomp | `PR_SET_SECCOMP` permitted | seccomp floor skipped; other layers unaffected |
| Landlock | kernel ABI ≥ 1 (Linux ≥ 5.13) | `unsupported` — filesystem confinement skipped |
| Launcher binary | `opensandbox-launcher` staged by the platform | all three floor layers `degraded` with the search-path message |
| Init mode | `runtime.execd_run_as_init = true` | enabled floor layers report `degraded` with `EXECD_INIT` guidance; execd-spawned commands still reduced, entrypoint not wrapped |
| Any layer, under gVisor/Kata | host kernel exposes the syscall | that feature is skipped |

The reference e2e (`tests/python/tests/test_execd_hardening_e2e.py`) pins all of these states end to end, including the missing-`CAP_SETPCAP` degradation and the drift case above.

## Choosing the container ceiling (the other half of the story)

The floor subtracts from what the container grants; it cannot grant anything back. The server's default Docker ceiling already drops the dangerous caps (`AUDIT_WRITE`, `MKNOD`, `NET_ADMIN`, `NET_RAW`, `SYS_ADMIN`, `SYS_MODULE`, `SYS_PTRACE`, `SYS_TIME`, `SYS_TTY_CONFIG`) while keeping `CAP_SETPCAP`/`CAP_SETUID`/`CAP_SETGID` — exactly what the launcher needs. If you tune `drop_capabilities` yourself:

- **Keep `CAP_SETPCAP`** — without it execd cannot trim the children's bounding set (the `degraded` state above).
- **Keep anything you list in `keep_capabilities`** — an ambient raise can only hand out caps the ceiling still grants.
- Container-level `no_new_privileges` and a Docker seccomp profile complement the floor at the ceiling; the launcher's syscalls work under Docker's default profile. Use the ceiling for what the *container* may do, and the TOML for what *user code inside it* may do.

## Compatibility notes

- **Non-Linux platforms** — the floor is Linux-only; configured layers report `unsupported` on Windows.
- **`execve` is reserved** in a custom `[seccomp] deny` when `[hardening] enabled` is on (config error); `execveat` remains allowed.
- **Override keys replace, never merge** — `[seccomp] deny` and `allowed_writable` both fully replace their defaults.
- **Landlock + forked descendants** — see the `/proc/self` warning above; test `ps`-style tooling before enabling Landlock on images whose users rely on it.
- **Rollback** — set `runtime.execd_run_as_init = false` and remove the TOML (or its `EXECD_ISOLATION_CONFIG` pointer) to return to the classic background-and-wait topology with no floor; no other migration is needed.

## Related

- [Isolation sessions](/guides/isolation-sessions) — per-execution bwrap namespaces that compose with the floor
- [Secure container runtimes](/guides/secure-container) — the outer boundary this feature nests inside
- [Execd architecture](/architecture/data-plane/execd) — how execd, the launcher, and the layers fit together
- [OSEP-0018](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0018-execd-as-sandbox-init.md) — design document behind this feature
