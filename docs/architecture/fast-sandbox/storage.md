---
title: Fast Sandbox Storage
description: One content-addressed artifact store for golden images and checkpoints; nodes cache privately and exchange blocks peer-to-peer.
---

# Storage

Fast Sandbox reduces storage to one shape: **everything a sandbox runs or resumes from is a content-addressed artifact** in a single S3-compatible store. Golden images, pause checkpoints, and snapshots are all artifacts, referenced by manifest digest — never mutated, always verifiable.

![Artifact storage and delivery](../../public/images/fast-sandbox-artifacts.svg)

## Where artifacts come from

- **Template builds** convert an OCI image (plus guest wiring and, for microVM runtimes, a kernel) into a golden-image artifact and publish it; the SandboxTemplate CR records the manifest reference and digest (see [Templates](/architecture/fast-sandbox/templates)).
- **Pause checkpoints and snapshots** are written by the same machinery: the runtime state is captured, pushed, and recorded digest-first in the SandboxSnapshot CR (see [Pause, Resume, and Snapshots](/architecture/fast-sandbox/checkpoints)). Resume never trusts a mutable tag.

## Where artifacts go

Nodes keep a **private state root** — an XFS-reflink-backed cache local to each node, never shared between nodes. This is deliberate: a fastlet's instant starts come from node-local artifacts, and cross-node state sharing would turn every node into part of every other node's failure domain.

The cost of private caches is duplicate downloads, and that is what **[DART](https://github.com/data-accelerator/dart)** eliminates: a node-local delivery daemon per node, peer-aware through a headless Service roster. Block-level fetches (4 MiB) follow `cache → peer → origin` — each block is fetched from the origin roughly once cluster-wide, then served peer-to-peer. Per-node block source counters make the delivery path observable.

## What is deliberately *not* stored

- **Egress credentials** stay memory-only in the egress process — pushed per subject over the proxy route, never written to a Secret volume or disk.
- **Sandbox runtime writes** (files a sandbox created) live in the sandbox's own slice; they are captured only when an explicit checkpoint or snapshot says so. Storage for sandboxes is an act of the lifecycle, not a background sync.
- **Routing and policy state** lives in the CRDs, not in the artifact store — the store carries bytes, the cluster carries meaning.

## Operating it

The artifact store is plain S3-compatible object storage; capacity planning reduces to artifact sizes and retention. Deleting a SandboxSnapshot record does not delete the pushed artifacts — retention and cleanup of the store are operator policy, kept outside the sandbox lifecycle on purpose.
