---
title: Proposal Template
authors:
  - "@XXX"
reviewers:
  - "@YYY"
creation-date: yyyy-mm-dd
last-updated: yyyy-mm-dd
status: provisional|experimental|implementable|implemented|deferred|rejected|withdrawn|replaced
see-also:
  - "/docs/proposals/YYYYMMDD-related-proposal.md"
replaces:
  - "/docs/proposals/YYYYMMDD-replaced-proposal.md"
superseded-by:
  - "/docs/proposals/YYYYMMDD-superseding-proposal.md"
---

# Title

<!-- BEGIN Remove before PR -->
To get started with this template:

1. **Make a copy of this template.**
   Copy this template into `docs/proposals` and name it `YYYYMMDD-my-title.md`,
   where `YYYYMMDD` is the date the proposal was first drafted.
2. **Fill out the required sections.**
3. **Create a PR.**
   Aim for single topic PRs to keep discussions focused.
   If you disagree with what is already in a document, open a new PR with suggested changes.

The `Metadata` section above is intended to support tooling around the proposal process.
See the proposal process for details on each of these items.

<!-- END Remove before PR -->

## Table of Contents

- [Title](#title)
  - [Table of Contents](#table-of-contents)
  - [Summary](#summary)
  - [Motivation](#motivation)
    - [Goals](#goals)
    - [Non-Goals](#non-goals)
  - [Proposal](#proposal)
    - [User Stories](#user-stories)
    - [API Changes](#api-changes)
    - [Annotation / Label Contract Changes](#annotation--label-contract-changes)
    - [Implementation Details](#implementation-details)
    - [Risks and Mitigations](#risks-and-mitigations)
  - [Alternatives](#alternatives)
  - [Upgrade Strategy](#upgrade-strategy)
  - [Test Plan](#test-plan)
  - [Implementation History](#implementation-history)

## Summary

A user-focused summary of the proposal. This section should be understandable without reading the full document and suitable for inclusion in release notes or a roadmap.

## Motivation

Describe why the change is important and the benefits to users.

### Goals

- List the specific high-level goals of the proposal.
- How will we know that this has succeeded?

### Non-Goals

- What is explicitly out of scope for this proposal?
- Listing non-goals helps to focus discussion and make progress.

## Proposal

This is where the proposal is described in detail. Use diagrams to communicate concepts, flows, and states where appropriate.

### User Stories

Detail the things that people will be able to do if this proposal is implemented.

#### Story 1

#### Story 2

### API Changes

If this proposal involves CRD schema changes in `apis/`, describe them here with example YAML. Include:

- New types, fields, or status conditions
- Changes to existing fields (additive only — removal or renaming is breaking)
- Whether `make manifests generate` needs to run

If there are no API changes, state "None".

### Annotation / Label Contract Changes

If this proposal adds or modifies annotation keys (e.g., `sandbox.opensandbox.io/*`) or label keys (e.g., `sandbox.opensandbox.io/pool-name`, `sandbox.opensandbox.io/pool-revision`), list them here with:

- The key name
- The JSON shape (for annotations)
- Which components write and read them

Annotation and label contracts are treated as internal but stability-sensitive. Changes must update all writers (`allocator.go`, `apis.go`) and readers (`batchsandbox_controller.go`, etc.).

If there are no annotation/label changes, state "None".

### Implementation Details

Describe the implementation approach:

- Which reconciler(s) are affected and how (`BatchSandboxReconciler`, `PoolReconciler`)
- Changes to the allocation flow (`Schedule` → `Allocator` → `PersistPoolAllocation` → `SyncSandboxAllocation`)
- Changes to the task scheduling or task-executor components
- Changes to strategies, eviction handlers, or the scheduler
- Any new files or packages being introduced

Keep reconciler logic idempotent — delegate to helpers, strategies, or allocators rather than putting business logic directly in `Reconcile()`.

### Risks and Mitigations

- What are the risks of this proposal and how do we mitigate them?
- Consider backward compatibility (existing BatchSandbox / Pool objects must continue to work).
- Consider concurrent reconciliation — controllers may reconcile the same object concurrently.

## Alternatives

List other approaches that were considered and why they were not chosen.

## Upgrade Strategy

- How will existing clusters upgrade to use this feature?
- Are there any changes to CRD schemas that require conversion webhooks?
- What happens to existing BatchSandbox or Pool objects that don't have the new fields?

## Test Plan

Describe the testing approach:

- **Unit tests (envtest):** Which controller or allocator scenarios need coverage?
- **Focused unit tests:** Any specific test functions that should be added (e.g., `TestAllocatorSchedule`, `TestDefaultEvictionHandler`)?
- **E2E tests:** Does this change require new Kind-based e2e tests?

Bug fixes must include focused regression tests.

## Implementation History

- [ ] yyyy-mm-dd: Proposed idea in an issue or community meeting
- [ ] yyyy-mm-dd: Draft proposal PR opened
- [ ] yyyy-mm-dd: Community feedback incorporated
- [ ] yyyy-mm-dd: Proposal marked as implementable
- [ ] yyyy-mm-dd: Implementation PR opened
