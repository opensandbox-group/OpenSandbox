---
title: Isolated Execution API
authors:
  - "@pjp"
creation-date: 2026-06-06
last-updated: 2026-06-23
status: implementing
---

# OSEP-0013: Isolated Execution API

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [API Overview](#api-overview)
  - [Isolation Model](#isolation-model)
  - [Workspace Modes and Artifact Recovery](#workspace-modes-and-artifact-recovery)
  - [Filesystem Proxy](#filesystem-proxy)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [1. Endpoint Summary](#1-endpoint-summary)
  - [2. Request Schema](#2-request-schema)
  - [3. Profile Defaults](#3-profile-defaults)
  - [4. Response Schema](#4-response-schema)
  - [5. Capabilities Endpoint](#5-capabilities-endpoint)
  - [6. Isolator Interface](#6-isolator-interface)
  - [7. bwrap argv Fixed Segment Order](#7-bwrap-argv-fixed-segment-order)
  - [8. Startup Probing](#8-startup-probing)
  - [9. Upper Directory Management](#9-upper-directory-management)
  - [10. Commit Implementation](#10-commit-implementation)
  - [11. Filesystem Proxy Implementation](#11-filesystem-proxy-implementation)
  - [12. Concurrency Model](#12-concurrency-model)
  - [13. Session Idle GC](#13-session-idle-gc)
  - [14. Static bwrap Distribution](#14-static-bwrap-distribution)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Introduce [bubblewrap](https://github.com/containers/bubblewrap) into execd to
provide per-execution namespace isolation within a running sandbox Pod. A new
HTTP API prefix (`/v1/isolated/*`) exposes isolated sessions with independent
PID/mount/tmpfs/env namespaces, workspace overlay with artifact recovery, and a
full filesystem proxy — all with < 1ms cold-start overhead. Existing execd
endpoints remain unchanged.

## Motivation

Today, execd forks child processes in the sandbox container's main namespace.
Multiple executions within the same Pod share `/tmp`, PID namespace, network,
and environment variables. This creates two classes of problems:

**Security**: a prior execution can poison the filesystem for subsequent ones,
ptrace sibling processes, or read sensitive tokens from `/proc/<pid>/environ`.
The attack surface grows linearly with execution count.

**Throughput**: in RL training and evaluation, one sandbox = one task. Switching
between adjacent steps or shards requires a full Pod creation cycle (runc
startup 40–100ms+, consuming one Pool buffer slot). Per-execution cold-start
inside a single Pod can be compressed to < 1ms with namespace isolation,
reducing control plane QPS and Pool pressure by an order of magnitude.

bubblewrap (~200KB static C binary, the Flatpak sandbox core) uses Linux
mount/pid namespaces to provide process-level isolation with sub-millisecond
startup and automatic namespace cleanup on process exit. This design avoids
user namespaces (which require CRI-level nesting support) and instead uses real
UID setuid for per-session privilege separation.

### Goals

- New API (`/v1/isolated/*`) — callers explicitly opt into isolated execution;
  existing API behavior is unchanged.
- Independent PID/mount/tmpfs/env namespace per isolated execution within a
  single sandbox Pod.
- Workspace overlay mounting with artifact recovery: tar download (diff, option
  A) or write-back to workspace (commit, option B), composable in any order.
- Full filesystem operations (info/download/upload/delete/mv/chmod/search/
  replace/mkdir/rmdir) within isolated sessions, proxied by execd outside the
  namespace.
- Abstract `Isolator` interface — bwrap is the first implementation, extensible
  to future backends.
- Static bwrap distribution — execd embeds a musl-compiled static bwrap binary
  (~800KB), no base image pre-installation required.

### Non-Goals

- Modifying existing endpoints (`/command`, `/session`, `/code`, `/pty`,
  `/files`, `/metrics`).
- Jupyter kernel isolation (`/v1/isolated/code`).
- PTY isolation (`/v1/isolated/pty`).
- Replacing the outer container runtime (runc/gVisor/Firecracker).
- Introducing cgroup resource limits (deferred to outer Pod cgroup).
- Blocking data exfiltration via stdout encoding (DLP scope).

## Requirements

- Isolated sessions must create independent PID, mount, and tmpfs namespaces per
  session.
- Namespace startup overhead must be < 1ms.
- Namespace destruction must be automatic on session close (process exit →
  kernel cleanup).
- Workspace overlay mode must support copy-on-write with optional artifact
  persistence.
- Both diff (tar export) and commit (merge back) must be available on the same
  upper directory, composable in any order.
- The filesystem proxy must handle overlay semantics (whiteout, opaque markers)
  transparently outside the namespace.
- Capabilities endpoint must reflect runtime probe results so callers can make
  explicit fallback decisions.
- SDK must not silently fall back to non-isolated APIs — callers decide.
- Authentication reuses the existing `ServerAccessToken` mechanism.
- bwrap binary must be embedded in execd and extracted at startup — no external
  dependency on base image packaging.
- Must not require nested user namespace support from the CRI/container runtime.
- execd must run as root or with `CAP_SYS_ADMIN` to create mount/PID namespaces.
- UID/GID isolation must use real setuid, not user namespace mapping — callers
  declare uid/gid at session creation time.

## Proposal

### API Overview

```text
POST   /v1/isolated/session              Create isolated bash session
GET    /v1/isolated/session/<id>         Query session state
POST   /v1/isolated/session/<id>/run     Execute within session (SSE streaming)
DELETE /v1/isolated/session/<id>         Destroy session
GET    /v1/isolated/session/<id>/diff    Download upper directory as tar.gz
POST   /v1/isolated/session/<id>/commit  Merge upper back into workspace

       /v1/isolated/session/<id>/files/* Filesystem proxy (same schema as /files/*)
       /v1/isolated/session/<id>/directories  Directory operations

GET    /v1/isolated/capabilities         Probe runtime capabilities
```

### Isolation Model

Session wrapper: execd creates a bwrap + bash long-lived process. Multiple `run`
calls execute within the same namespace. Session deletion terminates the bwrap
process group, destroying the namespace and all child processes.

```text
execd (root)
 └── exec.Command("bwrap", <profile-args>, "--", setpriv, --reuid=N, --regid=N, bash)
      └── bash (long-lived in namespace, running as uid N)
           └── run: sh -c <code> (forked per run)
```

bwrap runs as root to create mount/PID namespaces (requires `CAP_SYS_ADMIN`),
then drops privileges to the requested uid/gid via `setpriv` before exec'ing
the user shell. No user namespace is created (`--unshare-user` is not used),
avoiding CRI nesting requirements. Different sessions use different real UIDs,
providing file permission isolation at the host level.

Filesystem inheritance follows a "read-only root + selective overlay" strategy:

```text
--ro-bind / /                      # Entire container / read-only
--bind <workspace> <workspace>     # Workspace per mode
<extra_writable>                   # Additional rw paths (allowlisted)
--tmpfs /tmp                       # Private (strict) or shared (balanced)
--tmpfs /run
--dev /dev
--proc /proc                       # With --unshare-pid for isolated PID view
```

Two profiles (`strict` and `balanced`) provide preset defaults. All fields can
be overridden per-session.

### Workspace Modes and Artifact Recovery

| Mode | Implementation | Write-through | Rollback | Use Case |
|------|---------------|---------------|----------|----------|
| `rw` | `--bind <ws> <ws>` | Yes | No | Persistent artifacts (RL rollout data) |
| `overlay` | `--overlay-src <ws> --overlay <upper> <work> <ws>` | Upper only | Yes | Prevent workspace corruption; optional recovery |
| `ro` | `--ro-bind <ws> <ws>` | No | No | Static analysis, read-only scanning |

Artifact recovery (overlay mode with `persist.enabled = true`):

| Operation | Meaning | Implementation |
|-----------|---------|----------------|
| A: `GET .../diff` | Export upper as tar.gz | Streaming tar output |
| B: `POST .../commit` | Merge upper into workspace via overlayfs semantics | Re-mount overlayfs + rsync |

A and B operate on the same upper directory. B does not consume the upper (reads
upper, writes lower). They compose in any order. Concurrency is controlled by a
per-session `sync.RWMutex`: B holds a write lock; A and run hold read locks.

### Filesystem Proxy

execd proxies filesystem operations outside the bwrap namespace, simulating the
overlay merged view without mounting:

- **Read** (info/download): check upper first; whiteout → 404; miss → fall
  through to lower.
- **Search**: walk upper + lower, merge and deduplicate, skip whiteout paths.
- **Write** (upload/replace): write to upper; `os.Chown` with session uid/gid.
- **Delete**: create whiteout in upper (character device 0,0); directory deletion
  creates opaque xattr.
- **Move**: create whiteout at source + write at destination in upper.
- **Permissions**: upper has file → operate directly; lower only → copy-up to
  upper first.
- **ro mode**: write operations return `403 Forbidden`.

### Notes/Constraints/Caveats

- Session isolation parameters are immutable after creation — `run` cannot
  override isolation fields (namespace immutability).
- `run.envs` has the highest priority and overrides `env_passthrough` for
  conflicting keys.
- Workspace path is auto-created (`mkdir -p`) if it does not exist.
- `idle_timeout_seconds = 0` disables idle GC; the caller must explicitly DELETE,
  otherwise the bwrap process lives until Pod destruction.
- In multi-user scenarios, workspace isolation is write-only — other users'
  workspaces are read-only visible via `--ro-bind / /`. Full read isolation
  requires `--tmpfs <parent>` masking (deferred to v2).

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Shared kernel — namespace isolation does not protect against kernel exploits | Position as RL performance accelerator and intra-Pod execution isolation, not a security boundary replacement. Untrusted code still requires gVisor/Firecracker |
| bwrap historical CVEs (e.g. CVE-2024-42472 symlink race) | Track upstream releases; bwrap version pinned to execd release cycle via static embedding |
| Mount order sensitivity — argv builder is a bug-prone area | Fixed segment order with unit test coverage for every segment |
| seccomp blocklist only — does not prevent 0-day syscalls | Defense-in-depth; primary boundary remains namespace isolation |
| No per-execution cgroup — CPU/memory/pids quota falls to outer Pod | Accept for v1; cgroup wrapper planned for v2 |
| gVisor mount restrictions — commit unavailable | Capabilities endpoint marks `commit_supported = false`; diff and isolated execution still work |
| Overlay upper not persistent across Pods | Upper lives on Pod filesystem; cross-Pod persistence requires caller-side upload to object storage |

## Design Details

### 1. Endpoint Summary

```text
POST   /v1/isolated/session              Create session
GET    /v1/isolated/session/<id>         Get session state
POST   /v1/isolated/session/<id>/run     Execute (SSE streaming)
DELETE /v1/isolated/session/<id>         Destroy session
GET    /v1/isolated/session/<id>/diff    Download upper tar.gz (option A)
POST   /v1/isolated/session/<id>/commit  Merge upper to workspace (option B)

GET    /v1/isolated/session/<id>/files/info        Stat (batch paths)
GET    /v1/isolated/session/<id>/files/download    Read file (streaming)
POST   /v1/isolated/session/<id>/files/upload      Write file (multipart)
DELETE /v1/isolated/session/<id>/files              Delete file
POST   /v1/isolated/session/<id>/files/mv           Rename/move
POST   /v1/isolated/session/<id>/files/permissions  chmod/chown
POST   /v1/isolated/session/<id>/files/replace      Text replace
GET    /v1/isolated/session/<id>/files/search       Glob/walk search
POST   /v1/isolated/session/<id>/directories        Create directory
DELETE /v1/isolated/session/<id>/directories         Delete directory

GET    /v1/isolated/capabilities         Probe capabilities and defaults
```

SSE/state-machine/hook protocol is identical to existing APIs. SDKs can reuse
parsing logic.

Filesystem endpoint request/response schemas are identical to existing
`/files/*` and `/directories/*`. The only difference is path resolution strategy
(see [§11](#11-filesystem-proxy-implementation)).

### 2. Request Schema

#### CreateIsolatedSessionRequest

```yaml
isolation:
  profile: strict | balanced
  workspace:
    path: string                       # Required
    mode: rw | overlay | ro
    persist:
      enabled: bool                    # Overlay: persist upper for diff/commit
      retain_seconds: int              # Upper GC window
      max_size_bytes: int              # Upper size limit
  extra_writable: [string]             # Additional bind-rw paths (allowlisted)
  share_net: bool                      # Default per profile
  env_passthrough:
    mode: deny | allow
    keys: [string]
  uid: int
  gid: int
  idle_timeout_seconds: int            # Auto-destroy after idle (default 1800)
```

#### RunInSessionRequest

```yaml
code: string                           # Command to execute
cwd: string                            # Working directory
envs: map<string,string>               # Additional env vars (highest priority)
hooks: { ... }                         # Same as existing API hooks
timeout_seconds: int                   # Per-run timeout (default 300)
```

#### Field Semantics

| Field | Description |
|-------|-------------|
| `isolation.profile` | Preset defaults (`strict` or `balanced`); individual fields can override. Default `strict` |
| `workspace.path` | Required. Exposed as writable/overlay/read-only in namespace. Auto `mkdir -p` if absent |
| `workspace.mode` | `rw` direct bind; `overlay` CoW isolation; `ro` read-only |
| `workspace.persist.enabled` | Overlay only. `false` (default) → upper on tmpfs, destroyed on exit. `true` → upper persisted for recovery. Requires `--isolation-upper-root` emptyDir, else returns 400 |
| `workspace.persist.retain_seconds` | Upper retention after session close. Default 3600 |
| `workspace.persist.max_size_bytes` | Upper size limit. Default 2 GiB, hard limit 8 GiB (execd flag) |
| `extra_writable` | Additional bind-rw paths. Constrained by execd `--isolation-allowed-writable` allowlist (default empty = reject all). Out-of-bounds returns 400 |
| `share_net` | `true` shares container network (default for both profiles). `false` → `--unshare-net` (loopback only) |
| `env_passthrough.mode` | `deny` → pass through caller env minus `keys` blacklist. `allow` → `--clearenv` then inject only listed `keys` |
| `uid` / `gid` | Real setuid via `setpriv --reuid=N --regid=N` (no user namespace). Default: execd process uid/gid. Linux setuid does not require `/etc/passwd` entries; callers manage UID allocation |
| `idle_timeout_seconds` | Auto-destroy after last `run` completion. Default 1800 (30 min). 0 disables |
| `timeout_seconds` (run) | Per-run timeout. SIGKILL on expiry. Default 300 |

### 3. Profile Defaults

| Profile | workspace.mode | /tmp | share_net | env_passthrough | seccomp | uid |
|---------|---------------|------|-----------|-----------------|---------|-----|
| `strict` | overlay | tmpfs (private) | true | deny + blacklist | blocklist on | real setuid |
| `balanced` | rw | bind container `/tmp` | true | allow (pass-through) | blocklist on | same |

Default profile = `strict`.

`strict` env blacklist (glob, case-insensitive):

```text
*_API_KEY, *_TOKEN, *_SECRET, *_PASSWORD,
AWS_*, ALI_*, ALIYUN_*, K8S_*, KUBE_*
```

`strict` defaults to `share_net = true` because execd runs inside a Pod where
egress policy is enforced by the egress sidecar. Cutting network would break
`pip install`, API calls, and other routine operations. Callers explicitly set
`share_net: false` when full network isolation is needed.

### 4. Response Schema

#### Create Session Response

```json
{
  "session_id": "abc123",
  "created_at": "...",
  "isolation": {
    "profile": "strict",
    "workspace": { "path": "/workspace", "mode": "overlay" }
  },
  "artifacts": {
    "diff_url": "/v1/isolated/session/abc123/diff",
    "commit_url": "/v1/isolated/session/abc123/commit"
  }
}
```

`artifacts` is null when `persist.enabled = false`.

#### Run Response (SSE streaming, final frame)

```json
{
  "run_id": "run-001",
  "exit_code": 0,
  "started_at": "...",
  "finished_at": "..."
}
```

### 5. Capabilities Endpoint

```json
{
  "available": true,
  "isolator": "bwrap",
  "version": "0.8.0",
  "profiles": ["strict", "balanced"],
  "allowed_workspaces": ["/workspace", "/data"],
  "allowed_extra_writable_prefixes": ["/data/"],
  "share_net_overridable": true,
  "commit_supported": true,
  "seccomp_profile_sha256": "...",
  "persist": {
    "available": true,
    "max_size_bytes_default": 2147483648,
    "max_size_bytes_limit": 8589934592,
    "retain_seconds_default": 3600
  }
}
```

- `available = false`: all `/v1/isolated/*` write operations return
  `503 Service Unavailable`. Callers should fall back to the existing API or
  error. **SDK does not silently fall back.**
- `persist.available = false`: `--isolation-upper-root` emptyDir not mounted.
  diff/commit/`persist.enabled=true` unavailable; isolated execution still works
  (upper on tmpfs).
- `commit_supported = false`: no mount permission (typical in gVisor). diff
  still works; commit returns 503.

### 6. Isolator Interface

```go
package isolation

type Isolator interface {
    Name() string
    Available() bool
    Capabilities() Capabilities
    Wrap(cmd *exec.Cmd, opts WrapOptions) error
}

type WrapOptions struct {
    Profile        Profile
    Workspace      WorkspaceSpec
    ExtraWritable  []string
    ShareNet       bool
    EnvPassthrough EnvSpec
    Uid, Gid       *uint32
    UpperDir       string   // Allocated by upper.go when persist.enabled=true; empty for tmpfs
    WorkDir        string
}

type WorkspaceSpec struct {
    Path string
    Mode WorkspaceMode  // RW / Overlay / RO
}
```

Call site (`isolated_session.go`) constructs `*exec.Cmd` then calls
`isolator.Wrap(cmd, opts)`. Wrap rewrites `cmd.Path` and `cmd.Args` to inject
the bwrap wrapper.

### 7. bwrap argv Fixed Segment Order

Mount order is fixed by segment to prevent ordering bugs (e.g., Gemini CLI's
`--tmpfs /tmp` erasing `--bind /tmp/sub`):

```text
1.  Namespace flags: --unshare-pid --unshare-uts --unshare-ipc etc. (no --unshare-user)
2.  --ro-bind / /
3.  --tmpfs /tmp (strict) or --bind /tmp /tmp (balanced)
4.  --tmpfs /run
5.  --dev /dev
6.  --proc /proc
7.  Workspace segment: --bind / --overlay-src+--overlay / --ro-bind per mode
8.  extra_writable segment: --bind per item
9.  Env segment: --clearenv (deny mode) then --setenv allowlist / passthrough minus blacklist
10. --seccomp <fd>
11. -- setpriv --reuid=<n> --regid=<n> --init-groups <user cmd>
```

The builder outputs the complete argv. Unit tests cover segment order and mutual
exclusion.

### 8. Startup Probing

execd runs probes at startup in order:

1. `bwrap --version` — binary check
2. `bwrap --ro-bind / / --unshare-pid -- true` — smoke test
3. Load `/etc/execd/seccomp.bpf` — failure: seccomp field returns empty (non-fatal)
4. Probe commit capability (mount overlayfs) — failure: commit marked unavailable; diff still works

Results are reflected in `/v1/isolated/capabilities`.

### 9. Upper Directory Management

- Root path: execd flag `--isolation-upper-root` (default
  `/var/lib/execd/isolation`).
- **Required**: Pod spec must mount emptyDir at this path. execd checks at
  startup via `/proc/self/mountinfo`. Failure → `persist.available = false`.
- Per-session subdirectory: `<id>/upper`, `<id>/work`.
- Size limit: default 2 GiB, hard limit 8 GiB (execd flag
  `--isolation-upper-max-bytes`). Periodic `du` check; exceeding limit →
  SIGKILL wrapper process and mark execution failed.
- GC: background goroutine scans expired directories every 60s. Startup scan
  cleans orphaned directories.
- Abnormal exits (non-zero, SIGKILL, timeout) are treated identically to
  success — upper is retained until `retain_seconds` expires, enabling diff
  for crash artifact inspection.

### 10. Commit Implementation

Server-side commit (option B):

```text
mkdir -p /tmp/merged-<id>
mount -t overlay overlay \
  -o lowerdir=<workspace>,upperdir=<upper>,workdir=<work> \
  /tmp/merged-<id>
rsync -aHAX --delete /tmp/merged-<id>/ <workspace>/
umount /tmp/merged-<id>
rmdir  /tmp/merged-<id>
if reset_upper_after:
  rm -rf <upper>/* <work>/*
```

Handles whiteout (character device 0,0 → delete corresponding workspace path)
and opaque (`trusted.overlay.opaque=y` xattr → delete subtree then copy upper).

v1 supports only `strategy = overwrite`. `skip-existing`, `fail-on-conflict`,
and selective commit (path whitelist) are deferred to v2.

gVisor typically cannot mount overlayfs — commit is marked
`commit_supported = false` in capabilities; diff and isolated execution remain
available.

Diff output: `GET .../diff` streams tar.gz via `Transfer-Encoding: chunked`.
execd flag `--isolation-diff-max-bytes` (default 4 GiB) rejects oversized
downloads with 413.

### 11. Filesystem Proxy Implementation

execd proxies filesystem operations outside the bwrap namespace. Path resolution
varies by workspace mode:

#### rw mode

Direct operations on `workspace.path`. Equivalent to existing `/files/*` with
path prefix validation (workspace escape → 400). Write operations `os.Chown`
with session uid/gid.

#### overlay mode

Simulated merged view without mounting:

| Operation | Resolution Strategy |
|-----------|-------------------|
| Read (info/download) | Check upper first; whiteout → 404; upper miss → fall through lower |
| Search | Walk upper + lower, merge/deduplicate, skip whiteout paths |
| Write (upload/replace) | Write to upper path; `os.Chown(path, session.uid, session.gid)` |
| Delete | Create whiteout in upper (char device 0,0); directory: opaque xattr |
| Move | Source: create whiteout; destination: write to upper |
| Permissions (chmod/chown) | Upper has file → direct; lower only → copy-up to upper first |
| mkdir | Create in upper; existing lower directory → create opaque marker |

Whiteout handling reuses `commit.go` logic, extracted to `merged_view.go`.

#### ro mode

Read operations work normally. Write operations (upload/replace/mv/delete/mkdir/
rmdir/chmod/chown) return `403 Forbidden`.

#### Path Safety

All path parameters go through `filepath.Clean` + prefix validation. Only
`workspace.path` subtree is accessible. `..` escape returns 400. Upload checks
upper size against `persist.max_size_bytes`.

### 12. Concurrency Model

Per-session `sync.RWMutex`:

| Operation | Lock |
|-----------|------|
| run (within session) | Read |
| diff (option A) | Read |
| Filesystem read (info/download/search) | Read |
| Filesystem write (upload/replace/mv/delete/mkdir/rmdir/chmod) | Read |
| commit (option B) | Write |
| reset upper | Write |

`reset_upper_after = true`: commit clears upper/work after completion. No active
run can be in progress (enforced by lock).

### 13. Session Idle GC

- Sessions record `lastRunAt` timestamp, updated on each `run` completion.
- Background goroutine scans every 60s. `now - lastRunAt > idle_timeout_seconds`
  triggers automatic destruction (equivalent to DELETE).
- Destruction order: kill bwrap process group → wait for bash exit → namespace
  auto-reclaim → upper enters `retain_seconds` GC queue.
- `idle_timeout_seconds = 0` disables idle GC for that session.
- Session GET endpoint returns `created_at`, `last_run_at`,
  `idle_remaining_seconds` for caller-side keepalive decisions.

### 14. Static bwrap Distribution

bwrap is statically compiled with musl (~800KB, zero runtime dependencies) and
embedded in the execd binary via Go `//go:embed`. At startup, execd extracts it
to `<execd binary directory>/bwrap`, sets `chmod +x`, and uses that path for all
bwrap invocations.

Benefits:
- No base image dependency on the `bubblewrap` package.
- bwrap version pinned to execd version — no version drift.
- CVE fixes ship with execd releases, not base image update cycles.

Cost: execd binary grows by ~800KB (negligible).

## Test Plan

### Unit Tests

- bwrap argv builder produces correct segment order for each profile × workspace
  mode combination.
- Segment mutual exclusion: strict vs balanced `/tmp` handling.
- `extra_writable` paths validated against allowlist; out-of-bounds → 400.
- Capabilities reflect probe results (available, commit_supported, persist).
- Upper directory allocation and cleanup lifecycle.
- Merged view resolution: upper priority, whiteout handling, opaque directory
  semantics.
- Env passthrough: deny mode blacklist filtering, allow mode whitelist injection.
- `run.envs` overrides `env_passthrough` for conflicting keys.
- Idle GC fires at correct intervals; `idle_timeout_seconds = 0` disables.
- Concurrent diff + run (read locks) do not block each other.
- Commit (write lock) blocks concurrent run and filesystem operations.

### Integration Tests

- End-to-end session lifecycle: create → run → run → diff → commit → delete.
- Overlay CoW: write in session does not modify original workspace; commit
  merges correctly.
- PID isolation: `ps` inside session shows only session processes.
- `/tmp` isolation: files written to `/tmp` in one session are invisible to
  another.
- Filesystem proxy: upload → download round-trip through overlay; delete creates
  whiteout; search merges upper + lower.
- `persist.enabled = false`: upper destroyed on session close; diff returns 404.
- `commit_supported = false` (simulated gVisor): commit returns 503; diff works.
- Multi-session: two concurrent sessions have independent namespaces.
- Idle GC: session auto-destroyed after timeout; `run` resets timer.

### Manual Validation

- Verify bwrap namespace startup overhead is < 1ms under typical workload.
- Verify upper size enforcement triggers SIGKILL at configured limit.
- Verify commit correctly handles overlayfs whiteout and opaque semantics.
- Verify multi-user scenario: workspace write isolation confirmed, read-only
  cross-visibility documented.

## Drawbacks

- bwrap shares the kernel with the outer container. This is explicitly not a
  security boundary replacement — it is a performance and isolation enhancement
  within a trusted Pod.
- Overlay upper is not persistent across Pod migrations. Cross-Pod artifact
  persistence requires caller-side upload to object storage.
- Workspace isolation in multi-user scenarios is write-only; other users'
  workspaces are read-only visible via the root bind mount.
- seccomp uses a blocklist, not an allowlist — it does not prevent unknown
  syscall exploitation.
- No per-session cgroup resource limits in v1.

## Alternatives

### SDK-side bwrap integration

Investigated in
[sdk-bwrap-integration-feasibility.md](https://github.com/opensandbox-group/opensandbox/blob/main/docs/investigation/2026-04-30-sdk-bwrap-integration-feasibility.md).
Complementary but not overlapping — SDK-side cannot provide the filesystem proxy,
artifact recovery, or session management that execd-side integration offers.

### Per-path allowlist instead of read-only root

Rejected. AI agent commands have unpredictable path dependencies (`/usr/local/
bin/`, `/etc/ssl/`, `/opt/`). Maintaining a per-path allowlist is impractical and
results in "command not found" failures. Read-only root with selective overlay is
the same strategy used by Gemini CLI's LinuxSandboxManager.

### cgroup-based isolation

cgroup provides resource limits but not filesystem or PID isolation. bwrap
namespaces and cgroup are complementary; cgroup wrapper is planned for v2.

### User namespace isolation instead of real setuid

bwrap natively supports `--unshare-user` with `--uid`/`--gid` for user namespace
UID mapping. This would allow execd to run as non-root. Rejected because:

- Nested user namespaces require CRI/runtime support (`user.max_user_namespaces`,
  runtime seccomp profiles allowing `clone(CLONE_NEWUSER)`). gVisor and hardened
  runtimes often restrict this.
- User namespace UID mapping is virtual — all sessions map to the same host UID,
  so file permissions do not provide real isolation between sessions.
- Real setuid provides stronger guarantees: different sessions have different
  host UIDs, so file permission checks are enforced by the kernel at the host
  level.
- The tradeoff (execd must run as root) is acceptable because execd already runs
  in a dedicated sandbox container, not on shared infrastructure.

### Dedicated container per execution

Full container isolation (runc/gVisor) provides stronger guarantees but at
40–100ms+ startup cost, which is prohibitive for RL step-level execution at
scale. bwrap fills the gap between no isolation and full container isolation.

## Infrastructure Needed

- execd must run as root or with `CAP_SYS_ADMIN` (required to create mount/PID
  namespaces without user namespace).
- emptyDir volume mounted at `--isolation-upper-root` in Pod spec for overlay
  persistence.
- Optional: seccomp BPF file at `/etc/execd/seccomp.bpf` (shipped with execd).

## Upgrade & Migration Strategy

This change is fully additive:

- Existing execd endpoints (`/command`, `/session`, `/code`, `/pty`, `/files`)
  are unchanged. Zero modifications to existing code paths.
- Existing SDKs continue to work. New typed methods
  (`CreateIsolatedSession`, `RunInIsolatedSession`, `IsolatedSessionFiles.*`)
  are additive.
- Callers opt in by using `/v1/isolated/*` endpoints. No implicit behavior
  change.
- If bwrap is unavailable at startup (probe failure), `/v1/isolated/*` returns
  503 and existing APIs work normally.
- Base images do not need to pre-install bubblewrap — execd handles distribution
  internally via static embedding.

Rollout sequence:

1. Deploy updated execd with embedded bwrap.
2. Update SDK with new typed methods.
3. Callers migrate to `/v1/isolated/*` at their own pace.
4. Update Pod specs to mount emptyDir at `--isolation-upper-root` for overlay
   persistence (optional — isolation works without it, only persist is
   unavailable).
