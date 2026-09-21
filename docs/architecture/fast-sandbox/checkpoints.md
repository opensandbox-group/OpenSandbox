---
title: Pause, Resume, and Snapshots
description: How Fast Sandbox checkpoints microVM runtime state to the artifact store, releases Fastlet capacity, resumes on any Fastlet, and publishes snapshots as golden-image artifacts.
---

# Pause, Resume, and Snapshots

Fast Sandbox checkpoints runtime state into the same content-addressed artifact store that golden images use. Pausing a sandbox writes a complete checkpoint and releases its Fastlet, so a paused sandbox has zero compute footprint; resuming restores the checkpoint on whichever Fastlet admission chooses — not necessarily the original host. Snapshots are the public form of the same machinery: they publish a running sandbox's state as a reusable artifact set that new sandboxes can be created from.

The public lifecycle API is unchanged across backends: `POST /sandboxes/{id}/pause`, `/resume`, and `/sandboxes/{id}/snapshots` return **202 Accepted** and the work completes asynchronously. The server submits intent to FastPath over gRPC and observes progress on the durable `sandbox.fast.io` Sandbox record — the same submit-and-converge pattern as every other mutation (see [High Availability](/architecture/fast-sandbox/ha)).

## What a checkpoint contains

A pause checkpoint captures the sandbox's runtime state as one artifact set in the store:

| Part | Content |
|---|---|
| `rootfs` | The sandbox's writable root filesystem |
| `vmstate` | MicroVM device and vCPU state for Firecracker-backed templates |
| `memory` | Guest memory, staged through the node's `/dev/shm` during capture |
| `SHA256SUMS` | Digest manifest over the set |

The Sandbox record's `status.runtime.checkpoint` carries the result: a `checkpointID` derived from the sandbox UID, the spec generation that requested the pause, and a `pauseAttempt` retry epoch; the `manifestRef` (`s3://` URI) of the published set; its digest and size. The checkpoint is written digest-first — **a pause is only reported complete after the artifact set is fully in the store** — and a Paused sandbox without a recorded checkpoint cannot be resumed; it must be reset.

## Pause and resume

**Pause** (`POST /sandboxes/{id}/pause`):

1. The server reads the Sandbox record, fences the call with its `uid`, and sends `PauseSandbox` to FastPath with the request ID.
2. FastPath records the intent (`spec.state: Paused`); the runtime captures rootfs, VM state, and guest memory, publishes the artifact set, records the checkpoint on the Sandbox status, then **releases the Fastlet capacity**.
3. Status converges by observation: `Pausing → Paused` on the Sandbox record. Routes through the ingress are rejected once paused — a paused sandbox is unreachable, not degraded.

**Resume** (`POST /sandboxes/{id}/resume`):

1. The server fences with the UID and submits `ResumeSandbox`.
2. FastPath flips the record back to `Running`; the controller restores the recorded checkpoint **possibly on a different Fastlet** — the artifact store, not node locality, is what makes resume portable.
3. Resume advances the route generation, so clients must re-resolve endpoints; existing routes do not follow the sandbox to its new host.

Only a Ready runtime may pause; UID mismatches and missing checkpoints surface as `409 Conflict`.

## Snapshots

A snapshot turns a running sandbox's state into a durable, reusable artifact set — published under the same content-addressed layout as template golden images, so a restored snapshot and a native template behave identically at create time.

1. `POST /sandboxes/{id}/snapshots` accepts only a Running source, persists a `CREATING` row, and creates a `SandboxSnapshot` record with a deterministic name and a `sandboxRef.uid` fence.
2. The platform drives the record through `Pending → Creating → Publishing → Succeeded/Failed`. Creation may briefly pause the sandbox while state is captured; publishing writes the artifact set plus an index entry keyed by the snapshot's template name.
3. The server converges the row by watching the record, re-checking on read, and CAS-writing the terminal state — a crashed worker leaves the row recoverable, not stuck.
4. `POST /sandboxes {"snapshotId": ...}` restores from a `READY` snapshot: the index entry resolves like a template, and creation routes to Fast Sandbox. The sandbox ID is new; the snapshot is a starting point, not a continuation.

Snapshot names are cluster-wide index keys with last-writer-wins semantics, so FastPath rejects a non-terminal holder of a name before accepting a new one. Deleting a snapshot record does not delete the pushed artifacts — store retention is operator policy (see [Storage](/architecture/fast-sandbox/storage)).

## Why copy-on-write is safe here

- **Checkpoints are immutable artifacts.** Resume and restore never trust a mutable tag; every artifact set is referenced by manifest digest and verifiable against `SHA256SUMS`.
- **Node-local rootfs copies are CoW reflinks.** Each node keeps a private XFS state root where per-sandbox rootfs instances are reflink copies of cached artifacts — instant provisioning without duplicating bytes, and without sharing state across failure domains (see [Firecracker](/architecture/fast-sandbox/firecracker)).
- **Delivery is amortized, not shared.** [DART](https://github.com/data-accelerator/dart) fetches each block from the origin roughly once cluster-wide and serves peers from node-local caches, so a resume after pause pays little more than a local clone.

## Relationship to BatchSandbox pause/resume

Fast Sandbox and the Kubernetes operator run **two independent checkpoint mechanisms**. `sandbox.fast.io` Sandbox records checkpoint microVM runtime state (rootfs + VM state + memory) for template-backed sandboxes; the operator's `SandboxSnapshot` (`sandbox.opensandbox.io`) commits container rootfs — and, for the QEMU contract, VM state — to an OCI registry (see [Pause and Resume](/guides/pause-resume)). They share the lifecycle API and 202 semantics, not the artifact format or the control plane.
