# Kubernetes AGENTS

You are working on the OpenSandbox Kubernetes operator, snapshot controller flow, and task-executor. Treat CRD types and annotation/label contracts as public interfaces, and prefer additive, backward-compatible changes.

For detailed development setup, architecture deep-dive, coding standards, testing guide, and deployment workflows, see [DEVELOPMENT.md](./DEVELOPMENT.md).

## Scope

- `apis/`: CRD type definitions (BatchSandbox, Pool, SandboxSnapshot)
- `cmd/controller/`: controller manager entry point
- `cmd/task-executor/`: task-executor entry point
- `internal/controller/`: BatchSandbox and Pool reconcilers, allocator, eviction, update, and strategy logic
- `internal/controller/*pause*`, `internal/controller/*snapshot*`: pause/resume and rootfs snapshot reconciliation
- `internal/scheduler/`: in-process task scheduler (assigns tasks to sandbox pods)
- `internal/task-executor/`: task execution runtime (process/container), manager, and HTTP server
- `internal/utils/`: shared helpers (pod, finalizer, field index, expectations, logging)
- `pkg/client/`: generated clientset, informer, and lister
- `pkg/task-executor/`: task-executor public types and config
- `pkg/utils/`: public-ish helper contracts used by server-side Kubernetes integration
- `config/`: Kustomize overlays, RBAC, CRD bases, samples
- `charts/opensandbox-controller/`: Helm chart for deployment
- `cmd/image-committer/` and `Dockerfile.image-committer`: image used by pause/resume rootfs commit jobs
- `test/e2e/`: end-to-end tests (Kind-based)
- `test/e2e_task/`: task-executor e2e tests
- `test/e2e_runtime/`: runtime-class e2e tests (gVisor)
- `docs/`: design documents and troubleshooting guides
- `docs/proposals/`: design proposals for new features and significant changes (see [template](./docs/proposals/YYYYMMDD-template.md))

If the task changes CRD schemas in `apis/`, also run `make manifests` and `make generate` to keep CRD YAML and DeepCopy methods in sync.

For E2E test failure diagnosis, see [docs/E2E-TROUBLESHOOTING.md](./docs/E2E-TROUBLESHOOTING.md).

## Key Paths

- `apis/sandbox/v1alpha1/`: CRD Go types and source of truth for API shapes
- `internal/controller/batchsandbox_controller.go`: BatchSandbox reconciler (scale, pool alloc parsing, task scheduling, status)
- `internal/controller/pool_controller.go`: Pool reconciler (sandbox scheduling, scale, update, eviction, status)
- `internal/controller/allocator.go`: in-memory allocation store, annotation syncer, and default allocator
- `internal/controller/strategy/`: strategy interfaces and defaults (PoolStrategy, TaskSchedulingStrategy)
- `internal/controller/eviction/`: pod eviction handler interface and default
- `internal/controller/pool_update.go`: rolling update logic for pool pods
- `internal/scheduler/`: TaskScheduler interface and default implementation (task-to-pod assignment, recovery)
- `internal/task-executor/`: task-executor manager, runtime (process/container), HTTP handler

## Annotation Contracts

The controller communicates allocation state through annotations on BatchSandbox objects. These are treated as internal but stability-sensitive:

- `sandbox.opensandbox.io/alloc-status`: JSON `{"pods":["pod-1","pod-2"]}` — current pod allocation
- `sandbox.opensandbox.io/alloc-release`: JSON `{"pods":["pod-3"]}` — pods released back to pool
- `sandbox.opensandbox.io/endpoints`: JSON endpoint list consumed by server-side endpoint resolution

Do not change annotation keys or JSON shapes without updating both writers and all readers, including controller tests and any server-side Kubernetes integration that parses them.

## Label Contracts

- `sandbox.opensandbox.io/pool-name`: labels pool-owned pods
- `sandbox.opensandbox.io/pool-revision`: revision hash for rolling updates
- `batch-sandbox.sandbox.opensandbox.io/pod-index`: pod index within a BatchSandbox
- `pool.opensandbox.io/evict`: marks idle pool pods for eviction
- `pool.opensandbox.io/eviction-handler`: selects pool eviction handler implementation

## Commands

Unit tests (envtest-based, uses Ginkgo/Gomega):

```bash
cd kubernetes
make setup-envtest
make test
```

Focused unit test (standard `testing` functions):

```bash
cd kubernetes
go test ./internal/controller/ -run TestAllocatorSchedule -v
go test ./internal/controller/eviction/ -run TestDefaultEvictionHandler -v
```

Focused unit test (Ginkgo suite in `internal/controller/` — entrypoint is `TestControllers`):

```bash
cd kubernetes
go test ./internal/controller/ -run TestControllers -v -ginkgo.focus='Pool allocate'
```

Build:

```bash
cd kubernetes
make build
```

Lint:

```bash
cd kubernetes
make lint
```

End-to-end tests (requires Kind and Docker):

```bash
cd kubernetes
make test-e2e        # full suite: core + task-executor + gVisor
make test-e2e-main   # core e2e only (test/e2e/)
```

Run controller locally:

```bash
cd kubernetes
make run
```

Deploy via Kustomize:

```bash
cd kubernetes
make deploy
```

Deploy via Helm:

```bash
cd kubernetes
make helm-install
```

Regenerate CRD manifests and DeepCopy:

```bash
cd kubernetes
make manifests generate
```

Pause/resume focused checks:

```bash
cd kubernetes
go test ./internal/controller/ -run 'Test(DispatchPauseResume|HandlePause|HandleResume|ContinueResume|CompletePause|SyncPauseOrClear|SandboxSnapshot)' -v
make test-e2e-pause-resume
```

## Architecture Overview

Core reconciliation flows:

1. **BatchSandboxReconciler**: Owns Pod objects. Handles pod scaling (non-pooled mode), pool allocation parsing, task scheduling, status updates, expiry cleanup, and pause/resume handoff.
2. **PoolReconciler**: Owns Pod objects and watches BatchSandbox objects. Handles pod allocation to sandboxes, pool scaling (buffer/pool min/max), rolling updates, eviction, and status.
3. **SandboxSnapshot flow**: Internal CR and commit Job orchestration used by pause/resume to persist and restore root filesystems.

Allocation flow: PoolReconciler.Schedule → Allocator.Schedule → allocate/deallocate → PersistPoolAllocation → SyncSandboxAllocation (writes annotation to BatchSandbox).

The task-executor runs as a separate binary and in-pod HTTP server. The BatchSandboxReconciler drives task scheduling through the in-process TaskScheduler, which dispatches task execution requests to the task-executor running inside sandbox pods.

## Guardrails

Always:

- Run `make manifests generate` after changing `apis/` types.
- Run `make test` after controller or allocator changes.
- Update CRD YAML, Helm values/templates, Kustomize manifests, and docs together when controller flags or CRD behavior changes.
- Add focused regression tests for bug fixes in controller or allocator logic.
- Keep reconciler logic idempotent — controllers may reconcile the same object concurrently.
- Preserve annotation backward compatibility; add new fields rather than renaming existing ones.
- Use envtest for unit tests; reserve Kind-based e2e for integration validation.

Ask first:

- Changing CRD spec fields (additive changes are fine; removal or renaming is breaking)
- Changing annotation keys or JSON shapes
- Changing pool allocation or scheduling semantics
- Changing pause/resume snapshot semantics, controller snapshot flags, or image-committer trust assumptions
- Large reorganizations across `controller/`, `scheduler/`, and `task-executor/`

Never:

- Change annotation keys or JSON shapes without updating all readers and writers
- Change CRD types without running `make manifests generate`
- Put business logic directly in reconciler Reconcile() — delegate to helpers, strategies, or allocators
- Mix unrelated controller changes into the same PR
