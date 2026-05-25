---
title: Pool Auto-Assign for BatchSandbox
authors:
  - "@Spground"
creation-date: 2026-04-27
status: provisional
---

# Pool Auto-Assign for BatchSandbox

## Table of Contents

- [Pool Auto-Assign for BatchSandbox](#pool-auto-assign-for-batchsandbox)
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

Add automatic Pool selection for BatchSandbox. When `BatchSandboxSpec.PoolRef` is set to `*`, the controller automatically selects the most suitable Pool for the BatchSandbox through a predicate-filtering + score-ranking plugin mechanism, without requiring users to specify a Pool name explicitly. Plugins are organized via Profiles, which are configured through a cluster-level ConfigMap, allowing different filtering and scoring strategies for different scenarios.

## Motivation

Currently, using a Pool with BatchSandbox requires explicitly specifying a Pool name in `poolRef`. In multi-Pool scenarios (e.g., warm-up Pools split by image, GPU-resource Pools, Pools split by acceleration labels), users must determine which Pool to use themselves, which introduces the following problems:

- Users need to understand the topology and specifications of all Pools in the cluster, increasing the barrier to entry
- When operators adjust Pool split strategies, all BatchSandbox `poolRef` values must be updated accordingly
- There is no way to automatically match the optimal Pool based on BatchSandbox attributes (e.g., labels, resource requirements, images)

By introducing automatic Pool selection, users only need to set `poolRef: *`, and the controller automatically completes the Pool selection based on configurable plugin strategies.

### Goals

- When `poolRef: *`, the controller automatically selects a Pool for the BatchSandbox
- Provide a two-phase plugin framework: predicate for filtering and score for ranking
- Organize plugin combinations through Profiles, supporting different strategies for different scenarios
- Provide built-in predicate and score plugin implementations
- Configure plugins via a cluster-level ConfigMap, enabling extension without modifying controller code

### Non-Goals

- Cross-namespace Pool selection is out of scope; BatchSandbox can only select Pools in the same namespace
- Multi-Pool joint allocation is out of scope (one BatchSandbox is allocated to exactly one Pool)
- Dynamic plugin loading is out of scope (plugins are compiled into the controller binary; ConfigMap only controls enablement/disablement and parameters)
- The existing Pool-to-BatchSandbox allocation flow (`Schedule` → `Allocator` → `PersistPoolAllocation` → `SyncSandboxAllocation`) will not be modified

## Proposal

### User Stories

#### Story 1: Auto-match Pool by image in warm-up scenarios

Operators split warm-up Pools by image (e.g., `pool-python-3.10`, `pool-node-18`). Users create a BatchSandbox without specifying a Pool name:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: my-task
spec:
  poolRef: "*"
  template:
    spec:
      containers:
        - image: python:3.10
```

The controller uses the default profile, which includes the image predicate, and automatically assigns the BatchSandbox to the image-matched `pool-python-3.10`.

#### Story 2: Select Pool by label in custom label scenarios

Users add a resource-acceleration label to the BatchSandbox and need it assigned to an acceleration-capable Pool:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: accelerated-task
  labels:
    sandbox.opensandbox.io/resource-speedup: "true"
spec:
  poolRef: "*"
```

The controller uses the custom profile, which includes the labelselector predicate (configured with `keys: resource-speedup`), and automatically filters Pools with the corresponding label.

### API Changes

**1. Semantic extension of `BatchSandboxSpec.PoolRef`**

The `PoolRef` field is currently of type `string`, where an empty value means non-Pool mode and a non-empty value specifies a concrete Pool name. This proposal extends its semantics:

| PoolRef Value | Behavior |
|---|---|
| `""` (empty) | Non-Pool mode; the controller creates Pods directly |
| `"*"` | Auto-assign Pool mode; triggers the AssignPool flow |
| Other non-empty string | Explicitly specifies a Pool name (existing behavior unchanged) |

No CRD schema change is required — the `string` type of the `PoolRef` field already accommodates the `*` value.

**2. New Profile configuration ConfigMap**

The controller reads a ConfigMap in its namespace to obtain Profile configuration. The ConfigMap's `data.profiles` field is a JSON array:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: opensandbox-assign-profile
  namespace: system
data:
  profiles: |
    [
      {
        "name": "default",
        "plugins": {
          "predicate": ["image", "resource", "nodeselector"],
          "score": [{"name": "resbalance", "weight": 100}]
        },
        "pluginConf": [
          {"name": "resbalance", "args": {"strategy": "LeastAllocated"}}
        ]
      },
      {
        "name": "custom-profile",
        "plugins": {
          "predicate": ["labelselector", "resource", "nodeselector"],
          "score": [{"name": "resbalance", "weight": 100}]
        },
        "pluginConf": [
          {"name": "labelselector", "args": {"keys": "resource-speedup"}},
          {"name": "resbalance", "args": {"strategy": "MostAllocated"}}
        ]
      }
    ]
```

`make manifests generate` does not need to be run, as no `apis/` type changes are involved.

### Annotation / Label Contract Changes

None. This proposal does not add or modify any annotation/label keys. The AssignPool selection result is ultimately written back to `BatchSandboxSpec.PoolRef` (the controller replaces `*` with the actual Pool name), and subsequent flows continue to use the existing annotation contracts (`sandbox.opensandbox.io/alloc-status`, `sandbox.opensandbox.io/alloc-release`).

### Implementation Details

**Overall Flow**

When `PoolRef == "*"`, the AssignPool logic is inserted into the BatchSandboxReconciler's Reconcile flow before the poolStrategy check:

```
Reconcile(BatchSandbox)
  → if PoolRef == "*"
      → AssignPool(sbx)
          → loadProfile(profileName)
          → listPools(namespace)
          → predicate phase: execute each predicate plugin (AND semantics), filter candidate Pools
          → score phase: execute each score plugin (weighted sum), rank candidate Pools
          → select the Pool with the highest score
      → update PoolRef from "*" to the actual Pool name
  → proceed with the existing pool allocation flow
```

**Predicate Plugin Interface**

```go
type Predicate interface {
    Predicate(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) bool
}
```

Built-in implementations:

| Plugin Name | Description |
|---|---|
| `labelselector` | Checks whether the BatchSandbox's label keys match the Pool's label keys, based on a whitelist of keys configured in the ConfigMap |
| `resource` | Checks whether the BatchSandbox's resource requirements can be satisfied by the Pool's Pod resource spec (bin-packing) |
| `nodeselector` | Checks whether the BatchSandbox template's nodeSelector/nodeAffinity matches the Pool's Pod labels |
| `image` | Checks whether the BatchSandbox template's container image matches the Pool's Pod image |

Multiple predicates are combined with AND semantics: a Pool must pass all enabled predicates to become a candidate.

**Score Plugin Interface**

```go
type Scorer interface {
    Score(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) float64
}
```

Built-in implementations:

| Plugin Name | Description |
|---|---|
| `resbalance` | Scores Pools based on their resource allocation ratio. Supports two strategies: `MostAllocated` (pack, prefer Pools with higher usage) and `LeastAllocated` (spread, prefer Pools with lower usage) |

Each score plugin is configured with a weight in the Profile. The final score is the weighted sum across all plugins:

```
finalScore = Σ(scorer_i.Score() * weight_i)
```

The Pool with the highest score is selected. If scores are tied, the Pool with the lexicographically smaller name is chosen.

**Profile Loading**

The controller loads Profile configuration from the ConfigMap into memory at startup and watches the ConfigMap for changes to support hot-reloading. If the ConfigMap does not exist or the `profiles` field is empty, a built-in default profile is used (containing `image`, `resource`, `nodeselector` predicates and the `resbalance` scorer).

**Affected Components**

- `BatchSandboxReconciler`: checks `PoolRef == "*"` at the Reconcile entry point and invokes AssignPool
- `internal/controller/strategy/pool_strategy_default.go`: `IsPooledMode()` must recognize `"*"` as Pool mode
- New `internal/controller/assign/` package: contains the AssignPool logic, Predicate interface and built-in implementations, Scorer interface and built-in implementations, and Profile loading logic

**PoolRef Write-Back**

After AssignPool completes, the controller updates the BatchSandbox's `PoolRef` from `"*"` to the actual Pool name. This ensures that subsequent Reconcile cycles follow the existing flow and avoids re-executing AssignPool on every Reconcile. This update is a one-time operation — if the update fails, Reconcile retries; if it succeeds, `PoolRef` is already a concrete name and AssignPool is no longer triggered.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| **Concurrency**: multiple BatchSandboxes selecting the same Pool may lead to insufficient Pool resources | AssignPool only selects and writes back a Pool name; actual resource allocation is handled by the existing Allocator flow, which already has concurrency control. If resources are insufficient during allocation, the PoolReconciler triggers scale-up |
| **PoolRef write-back failure**: AssignPool selected a Pool but updating the BatchSandbox failed | Reconcile retries and re-executes AssignPool; the operation is idempotent |
| **No eligible Pool**: all Pools are filtered out by predicates | Record a `PoolAssignFailed` condition in the BatchSandbox status; wait for Pool changes to re-trigger Reconcile |
| **ConfigMap misconfiguration**: Profile configuration has invalid format or references non-existent plugins | Validate the ConfigMap at controller startup; log warnings for format errors and fall back to the built-in default profile |

## Alternatives

**Alternative A: Add a `poolSelector` field to BatchSandbox Spec**

Select Pools via label selector, similar to Pod's `nodeSelector`. The advantage is declarative and Kubernetes-idiomatic; the disadvantage is that it only supports label matching and cannot handle semantically richer filtering like resource bin-packing or image matching, limiting extensibility.

**Alternative B: Let the upper-layer system (e.g., E2B) choose the Pool and fill in `poolRef`**

Do not implement auto-selection in the controller; instead, let the caller handle it. The advantage is simpler controller logic; the disadvantage is shifting complexity to every caller, which must be aware of the cluster's Pool topology — this contradicts the project's goal of lowering the barrier to entry.

**Why the proposed approach was chosen**: The predicate + score plugin framework is flexible enough to cover multiple matching dimensions (label, resource, image) while the Profile mechanism supports strategy customization for different scenarios, avoiding a one-size-fits-all design.

## Upgrade Strategy

- This proposal does not modify the CRD schema; no conversion webhook is required
- Existing BatchSandboxes with an empty or concrete `PoolRef` behave exactly as before
- `PoolRef: "*"` is a new semantic that only triggers the AssignPool flow when explicitly used
- The ConfigMap is optional; when absent, the built-in default profile is used, so upgrades are unaffected

## Test Plan

- **Unit tests (envtest)**:
  - `TestAssignPool`: verify predicate filtering and score ranking logic
  - `TestLabelSelectorPredicate`: verify label-matching predicate
  - `TestResourcePredicate`: verify resource bin-packing predicate
  - `TestImagePredicate`: verify image-matching predicate
  - `TestNodeSelectorPredicate`: verify nodeSelector-matching predicate
  - `TestResBalanceScorer`: verify MostAllocated / LeastAllocated scoring strategies
  - `TestProfileLoading`: verify ConfigMap Profile loading and hot-reloading
  - `TestPoolStrategyIsPooledMode`: verify that `"*"` is recognized as Pool mode
- **Integration tests**:
  - Verify that a BatchSandbox with `poolRef: "*"` is automatically assigned to the correct Pool
  - Verify that the status is correctly reflected when no eligible Pool is available
- **E2E tests**:
  - Deploy multiple Pools in a Kind cluster and verify automatic Pool selection for BatchSandboxes

## Implementation History

- [ ] 2026-04-27: Draft proposal
