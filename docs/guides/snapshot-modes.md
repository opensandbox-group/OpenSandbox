---
title: Snapshot Modes
description: Choose between full snapshots (memory + filesystem, millisecond restore) and rootfsOnly snapshots (writable filesystem only, fresh-boot restore) — and learn when each is the right choice.
---

# Snapshot Modes

Snapshots capture a sandbox so new sandboxes can be created from it. OpenSandbox supports **two capture modes**, and the choice decides what a restored sandbox comes back with:

| Mode | Rootfs (writable layer) | Memory & processes | Restore latency | Backend |
|---|---|---|---|---|
| `full` (default) | ✓ | ✓ — RAM, vCPU, and running processes are checkpointed | milliseconds (restore) | Docker, Kubernetes, Fast Sandbox |
| `rootfsOnly` | ✓ | ✗ — memory image is discarded | seconds (cold boot: fresh boot + entrypoint re-run) | Fast Sandbox only |

Use `mode="rootfsOnly"` when you care about the files the sandbox accumulated — installed packages, build outputs, workspace state — and will accept a fresh boot on restore. Use the default `full` when the running process state itself is the value (an agent mid-session, a server with warm caches).

## Creating a snapshot in each mode

```python
snapshot = await manager.create_snapshot(
    sandbox_id, name="with-processes"            # mode omitted → "full"
)
rootfs = await manager.create_snapshot(
    sandbox_id, name="files-only", mode="rootfsOnly"
)
```

```typescript
const snapshot = await manager.createSnapshot(sandboxId, { name: "with-processes" });
const rootfs = await manager.createSnapshot(sandboxId, { name: "files-only", mode: "rootfsOnly" });
```

The parameter is available on both the `SandboxManager` APIs and the per-sandbox shortcut in every SDK (`sandbox.createSnapshot(...)`). See each SDK README's snapshot section for the full examples.

## What rootfsOnly loses — and what still survives

A `rootfsOnly` snapshot preserves the writable filesystem layer and discards everything else:

| Survives | Lost |
|---|---|
| Files written anywhere in the sandbox | Running processes and their in-memory state |
| Installed packages (`apt`, `pip`, `npm`, ...) | Open sockets and active connections |
| Build artifacts and caches on disk | Kernel/OS state outside the writable layer |
| Modified config files | Environment of the old process tree (unless written to disk) |

On restore, the sandbox boots **fresh**: `guest-init` starts `execd` again, and the user entrypoint **re-runs on every boot** — the same contract a brand-new sandbox has. So startup scripts must be idempotent, and anything a process kept only in memory must be re-materialized from disk by your entrypoint.

## RootfsOnly and pause/wake-on-access

[Auto-pause and wake-on-access](/guides/auto-pause-wake) works by restoring a **memory image** — which a `rootfsOnly` snapshot does not contain. That interplay follows two rules:

- A **live sandbox** that was created from a `rootfsOnly` snapshot can still be paused and woken normally while it runs — wake restores from its (in-memory) checkpoint.
- A **rootfsOnly snapshot itself** cannot serve as the artifact a wake restores from: waking requires the memory image. Restoring from a `rootfsOnly` snapshot is an explicit, seconds-scale cold boot.

The SDKs validate client-side that an invalid combination (rootfs-only artifact + wake-on-access expectation) fails early instead of failing at first traffic.

## When to pick rootfsOnly

- **Workspace persistence** — keep the environment between agent tasks without paying for memory capture on every checkpoint.
- **Build-state checkpointing** — snapshot after a long dependency install or compile; the next restore starts warm on disk.
- **Template seeding** — bake dependencies into a snapshot and derive short-lived sandboxes from it; each child boots fast from the shared rootfs.
- **Portability** — a `rootfsOnly` artifact does not carry CPU-family or Firecracker-version constraints that a full (memory-including) snapshot does; architecture compatibility is all that matters.

## Requirements and failure semantics

**Capture** requires the **fast-sandbox backend** (Firecracker). On a deployment that cannot produce one, the capture **fails with an explicit error** — it never silently saves a full snapshot instead.

**Restore** works on both fast-sandbox pool types: `ResourceAware` pools accept per-create CPU/memory overrides (omitted dimensions inherit the captured quota; a controller without the RPC returns 503), while `LegacySlot` pools restore through the standard create path — matching or omitted limits pass, and CPU/memory overrides are rejected with HTTP 400.
