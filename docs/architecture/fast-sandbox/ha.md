---
title: Fast Sandbox High Availability
description: What survives which failure — CRD-first durability, a restartable control plane, janitor cleanup, and converge-by-observation recovery.
---

# High Availability

Fast Sandbox's availability story follows from one decision: **every sandbox's intent, placement, policy bindings, and observations live in Kubernetes CRDs**, not in the memory of any component. Components are restartable because the durable state is not theirs.

## The control plane is deliberately single

The fast-sandbox controller (reconcilers + FastPath) runs as a single replica without leader election. This is a design position, not an omission:

- Mutations are fast precisely because FastPath can hold placement and candidate state in memory; a replicated control plane would need consensus around exactly that state.
- Nothing durable depends on that memory. A crashed control plane loses only in-flight reconciliation work, which CR-driven reconciliation replays: sandboxes, pools, templates, and snapshots all re-converge from their CRs.

Availability of the *sandboxes* is therefore independent of availability of the *control plane*: running sandboxes keep running, and traffic keeps flowing, while the control plane restarts.

## Converge by observation

Recovery everywhere uses the same pattern — **submit intent, converge by observing the durable record** — instead of leases or leader election:

- **Snapshots**: creating a snapshot submits intent and returns immediately; terminal state is observed from the SandboxSnapshot CR. Two servers coordinating snapshots converge on the deterministic CR: one observes before creating, create races are absorbed as `409`, and the terminal database write is conditional on the expected prior state.
- **Egress**: on restart the egress process wipes its rules and publishes a new incarnation ID; the Fastlet replays every live binding and reached hook, so subjects re-enter deny-first and re-open only after their data plane is confirmed ready.
- **Nodes**: the runtime agent reports Fastlet health to the platform; a lost Fastlet is handled according to the sandbox's `failurePolicy` after `recoveryTimeoutSeconds` — manual by default.

## Cleanup

A janitor runs on a scan interval and removes orphaned artifacts of failed operations (stale pods, leftover resources) after an orphan timeout — the complement to convergence: reconciliation finishes interrupted work, the janitor sweeps what should no longer exist.

## What failure looks like

| Failure | Effect | Recovery |
|---|---|---|
| Control plane restart | Brief loss of mutation capacity; running sandboxes unaffected | CR-driven reconciliation replays intent |
| Fastlet pod lost | Sandboxes on that Fastlet go down | Per-sandbox `failurePolicy` after `recoveryTimeoutSeconds`; checkpoints (if any) resume on another Fastlet |
| Server restart | No effect on sandboxes; open operations resume from durable records | Same observe-and-converge pattern |
| Artifact store outage | New starts degrade to direct pulls; running sandboxes unaffected | DART resumes peer/origin service when the store returns |

Pause/resume deserves a note: pausing checkpoints the runtime and **releases** the Fastlet, so a paused sandbox has no compute footprint at all — and resume lands on whichever Fastlet admission chooses, not necessarily the original one.
