# Development Guide - OpenSandbox Kubernetes Operator

This guide covers environment setup, project structure, architecture, coding standards, testing, and deployment workflows for the OpenSandbox Kubernetes operator.

## Development Workflow

Follow this workflow for every feature or significant change:

1. **Write a design proposal** — For any new feature or significant change, create a proposal document following the [template](./docs/proposals/YYYYMMDD-template.md) in `docs/proposals/`. Name it `YYYYMMDD-title.md` where `YYYYMMDD` is the date the proposal was first drafted. The proposal should cover motivation, API changes, annotation/label contract changes, implementation details, risks, and test plan. Submit the proposal as a PR for review before proceeding with implementation.

2. **Define API and interfaces** — Determine CRD spec changes, annotation contract changes, and Go interface definitions first. Review with the team before proceeding. Run `make manifests generate` after changing `apis/` types.

3. **Write E2E test cases** — Write end-to-end test cases in black-box style before implementing the feature. E2E tests exercise the full controller stack against a real cluster and validate user-visible behavior, not internals. Add test data YAML to `test/e2e/testdata/` and test cases to `test/e2e/e2e_test.go` (or `test/e2e_task/` for task-executor features).

4. **Implement with TDD** — Write unit tests first, then implement the logic to make them pass. Unit tests use envtest and Ginkgo/Gomega. Run `make test` frequently during development.

5. **Run UT and E2E to verify** — Run the full unit test suite (`make test`) and the E2E suite (`make test-e2e`) to confirm the feature works end-to-end. Both must pass before submitting.

6. **Troubleshoot E2E failures** — If E2E tests fail, refer to [docs/E2E-TROUBLESHOOTING.md](./docs/E2E-TROUBLESHOOTING.md) for diagnosis and resolution steps.

Repeat steps 2–6 until the feature is complete and all tests pass.

## Prerequisites

- **Go 1.24+** — match the version in `go.mod`
- **Docker** — for building images and running e2e tests
- **Kind** — for e2e test clusters (`go install sigs.k8s.io/kind@v0.20.0`)
- **kubectl** — for manual cluster interaction
- **Helm 3+** — for chart-based deployment
- Access to a Kubernetes cluster (Kind, minikube, or remote)

## Quick Start

```bash
cd kubernetes

# Install tools and download envtest binaries
make setup-envtest

# Verify build
make build

# Run unit tests
make test

# Run locally against the current kubeconfig
make run
```

## Project Structure

```
kubernetes/
├── apis/sandbox/v1alpha1/         # CRD type definitions (source of truth for API shapes)
│   ├── batchsandbox_types.go
│   └── pool_types.go
├── cmd/
│   ├── controller/main.go         # Controller manager entry point
│   └── task-executor/main.go      # Task-executor entry point
├── internal/
│   ├── controller/                # Core reconcilers and allocator
│   │   ├── batchsandbox_controller.go   # BatchSandbox reconciler
│   │   ├── pool_controller.go          # Pool reconciler
│   │   ├── allocator.go               # In-memory allocation store + annotation syncer
│   │   ├── allocator_mock.go          # gomock-generated mocks
│   │   ├── apis.go                    # Annotation/label constants, parse helpers
│   │   ├── pool_update.go            # Rolling update strategy
│   │   ├── eviction/                  # Pod eviction interface + factory + default
│   │   └── strategy/                  # Strategy interfaces + factories + defaults
│   │       ├── pool_strategy.go
│   │       └── task_scheduling_strategy.go
│   ├── scheduler/                 # In-process task scheduler
│   │   ├── interface.go           # TaskScheduler interface
│   │   ├── types.go               # Task, TaskState types
│   │   └── default_scheduler.go
│   ├── task-executor/             # Task execution runtime (runs inside sandbox pods)
│   │   ├── config/                # CLI flags, env vars, klog setup
│   │   ├── manager/               # TaskManager interface + in-memory implementation
│   │   ├── runtime/               # Executor interface + process/container impls
│   │   ├── server/                # HTTP handler + router
│   │   ├── storage/               # File-based task persistence
│   │   ├── types/                 # Internal task and status types
│   │   └── utils/
│   └── utils/
│       ├── expectations/          # Scale expectation tracking
│       ├── fieldindex/            # Cache field index registration
│       ├── controller/            # Controller key helpers
│       ├── logging/               # Zap + lumberjack file rotation
│       └── requeueduration/       # Per-key requeue duration store
├── pkg/
│   ├── client/                    # Generated clientset, informer, lister
│   ├── task-executor/             # Public task types (Task, Process) consumed by scheduler
│   └── utils/                     # Shared utilities (AnnotationEndpoints, etc.)
├── config/                        # Kustomize overlays
│   ├── crd/bases/                 # Generated CRD YAML
│   ├── default/                   # Default deployment overlay
│   ├── manager/                   # Controller manager deployment
│   ├── rbac/                      # ClusterRole bindings
│   └── samples/                   # Example resources
├── charts/opensandbox-controller/ # Helm chart
├── test/
│   ├── e2e/                       # Core e2e tests (Kind-based)
│   ├── e2e_task/                  # Task-executor e2e tests
│   ├── e2e_runtime/               # RuntimeClass e2e (gVisor)
│   └── kind/                      # Kind cluster configs
└── docs/                          # Design documents
    ├── proposals/                 # Design proposals for new features and significant changes
    │   ├── YYYYMMDD-template.md   # Proposal template
    │   └── ...                    # Named as YYYYMMDD-title.md
    └── ...
```

## Architecture

### Controller Manager

Two controllers run inside the controller manager (`cmd/controller/main.go`):

1. **BatchSandboxReconciler** — owns Pod objects
   - Scales pods in non-pooled mode
   - Parses pool allocation from annotations
   - Drives in-process task scheduling
   - Updates status (replicas, allocated, ready, task counts)
   - Handles expiry and finalizer cleanup

2. **PoolReconciler** — owns Pod objects, watches BatchSandbox objects
   - Schedules sandbox allocation (compute → persist → sync)
   - Manages pool scaling (buffer min/max, pool min/max)
   - Handles rolling updates when pool template changes
   - Handles pod eviction
   - Updates pool status (total, allocated, available, updated)

### Allocation Flow

```
PoolReconciler.Reconcile
  └─ scheduleSandbox
       └─ Allocator.Schedule
            ├─ allocate  (assign available pods to sandboxes)
            ├─ deallocate (release pods from sandboxes)
            ├─ PersistPoolAllocation (write to in-memory store)
            └─ SyncSandboxAllocation (write annotation to BatchSandbox, concurrent)
```

Allocation state is stored in memory (`InMemoryAllocationStore`) and persisted to BatchSandbox annotations:
- `sandbox.opensandbox.io/alloc-status`: `{"pods":["pod-1","pod-2"]}`
- `sandbox.opensandbox.io/alloc-release`: `{"pods":["pod-3"]}`

On startup, `InMemoryAllocationStore.Recover` rebuilds the in-memory state from all BatchSandbox annotations.

### Task Execution

The BatchSandboxReconciler drives task scheduling through the in-process `TaskScheduler`:

```
BatchSandboxReconciler.Reconcile
  └─ reconcileTasks
       └─ TaskScheduler.Schedule
            └─ assigns tasks to pods via task-executor HTTP API
```

The `task-executor` binary runs as a sidecar inside sandbox pods. It exposes an HTTP API on port 5758 for task lifecycle management (create, list, stop). It supports two runtime modes:
- **Process executor**: runs commands directly on the host
- **Container executor**: manages containers via CRI

The `compositeExecutor` dispatches to the appropriate runtime based on the task type (tasks with `Process` field use process executor, tasks with `PodTemplateSpec` use container executor).

### Strategy Pattern

Extensible strategy interfaces with factory functions:

| Interface | Factory | Default | Location |
|-----------|---------|---------|----------|
| `PoolStrategy` | `NewPoolStrategy()` | `DefaultPoolStrategy` | `strategy/` |
| `TaskSchedulingStrategy` | `NewTaskSchedulingStrategy()` | `DefaultTaskSchedulingStrategy` | `strategy/` |
| `EvictionHandler` | `NewEvictionHandler()` | `defaultEvictionHandler` | `eviction/` |
| `PoolUpdateStrategy` | `NewPoolUpdateStrategy()` | `recreateUpdateStrategy` | `pool_update.go` |
| `Allocator` | `NewDefaultAllocator()` | `defaultAllocator` | `allocator.go` |

To add a custom strategy implementation:
1. Implement the interface
2. Add a case to the factory function (typically dispatching based on a label or annotation)

### Cache Field Indexes

Custom field indexes are registered at startup in `internal/utils/fieldindex/register.go`:

| Index Name | Resource | Purpose |
|------------|----------|---------|
| `ownerRefUID` | Pod | List pods by owner UID (used by both reconcilers) |
| `poolRef` | BatchSandbox | List BatchSandboxes by pool name (used by PoolReconciler) |

## Coding Standards

### Go Style

- Run `make fmt` before committing (runs `go fmt ./...`)
- Run `make vet` (runs `go vet ./...`)
- Run `make lint` (runs `golangci-lint`)

### Import Organization

Three groups separated by blank lines:

```go
import (
    // Standard library
    "context"
    "fmt"

    // Third-party
    corev1 "k8s.io/api/core/v1"
    "sigs.k8s.io/controller-runtime/pkg/client"

    // Internal
    sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
    "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)
```

### Error Handling

Always handle errors explicitly. Wrap with context using `fmt.Errorf("...: %w", err)`:

```go
// Good
result, err := someOperation()
if err != nil {
    return fmt.Errorf("failed to do something: %w", err)
}

// Bad — silent failure
result, _ := someOperation()
```

Aggregate errors with `errors.Join()` when collecting multiple non-fatal errors (see `pool_controller.go` and `batchsandbox_controller.go`).

### Logging

Use structured logging via `logf.FromContext(ctx)` from controller-runtime:

```go
log := logf.FromContext(ctx)
log.Info("Schedule result", "pool", pool.Name, "allocated", len(allocStatus.PodAllocation))
log.Error(err, "Failed to get pool allocation")
log.V(1).Info("debug-level message", "key", value)
```

The task-executor uses `klog/v2` with structured logging:

```go
klog.InfoS("task created", "name", task.Name)
klog.ErrorS(err, "failed to inspect task", "name", name)
```

### Reconciler Idempotency

Controllers may reconcile the same object concurrently. Reconcilers must be idempotent:
- Always re-fetch the latest state before mutating
- Use `retry.RetryOnConflict` for status updates (see `pool_controller.go`)
- Use expectation tracking (`ScaleExpectations`) to avoid duplicate creates

### Interface + Factory + Default Pattern

Follow the existing pattern for extensible subsystems:

```go
// 1. Define interface
type EvictionHandler interface {
    NeedsEviction(pod *corev1.Pod) bool
    Evict(ctx context.Context, pod *corev1.Pod) error
}

// 2. Factory function dispatches based on object labels
func NewEvictionHandler(_ context.Context, c client.Client, pool *sandboxv1alpha1.Pool) EvictionHandler {
    switch pool.Labels[LabelEvictionHandler] {
    default:
        return newDefaultEvictionHandler(c)
    }
}

// 3. Default implementation (unexported struct, exported constructor)
type defaultEvictionHandler struct { client client.Client }
func newDefaultEvictionHandler(c client.Client) EvictionHandler { ... }
```

### Generated Code

Do not manually edit generated code. Regenerate with:

```bash
make manifests   # CRD YAML, RBAC, webhook configs
make generate    # DeepCopy methods
```

Generated paths:
- `config/crd/bases/` — CRD YAML from `apis/` type annotations
- `pkg/client/` — clientset, informer, lister (codegen)
- `internal/controller/allocator_mock.go` — gomock mocks (regenerate with `mockgen`)

## Testing

### Unit Tests (envtest + Ginkgo/Gomega)

Unit tests use `envtest` (local API server + etcd) with Ginkgo BDD framework. Test files live alongside source code.

```bash
# Full unit test suite
make test

# Focused test (standard testing functions)
go test ./internal/controller/ -run TestAllocatorSchedule -v
go test ./internal/controller/eviction/ -run TestDefaultEvictionHandler -v

# Focused test (Ginkgo suite — entrypoint is TestControllers, use -ginkgo.focus)
go test ./internal/controller/ -run TestControllers -v -ginkgo.focus='Pool allocate'
go test ./internal/scheduler/ -run TestDefaultScheduler -v

# With coverage
go test -coverprofile=cover.out ./internal/controller/
go tool cover -html=cover.out -o cover.html
```

Test setup is in `internal/controller/suite_test.go`:
- Starts envtest environment with CRDs from `config/crd/bases/`
- Creates a controller-runtime Manager
- Registers both BatchSandboxReconciler and PoolReconciler
- Starts the manager in a goroutine

### E2E Tests (Kind-based)

E2E tests require Docker and Kind. They deploy the controller to a Kind cluster and test real reconciliation:

```bash
# Full e2e suite (core + task-executor + gVisor)
make test-e2e

# Core e2e only (test/e2e/)
make test-e2e-main

# Task-executor e2e only
go test ./test/e2e_task/ -v -ginkgo.v -timeout 30m

# With custom ginkgo filter
make test-e2e GINKGO_ARGS="-ginkgo.focus='Pool'"
```

E2E test data is in `test/e2e/testdata/`. Tests use Go templates for parameterized resource creation.

### Writing Tests

For controller tests, use the envtest-based approach:

```go
It("should allocate pods from pool", func() {
    // Create pool and sandbox
    pool := &sandboxv1alpha1.Pool{...}
    Expect(k8sClient.Create(ctx, pool)).Should(Succeed())
    
    sbx := &sandboxv1alpha1.BatchSandbox{...}
    Expect(k8sClient.Create(ctx, sbx)).Should(Succeed())
    
    // Assert eventual state
    Eventually(func() int32 {
        _ = k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), pool)
        return pool.Status.Allocated
    }).Should(Equal(int32(1)))
})
```

For allocator tests, use gomock mocks:

```go
ctrl := gomock.NewController(t)
mockStore := NewMockAllocationStore(ctrl)
mockStore.EXPECT().GetAllocation(gomock.Any(), gomock.Any()).Return(&PoolAllocation{...}, nil)
```

## Common Development Tasks

### Adding a New CRD Field

1. Edit the type in `apis/sandbox/v1alpha1/` (add `+kubebuilder` annotations)
2. Regenerate manifests and DeepCopy:
   ```bash
   make manifests generate
   ```
3. Implement controller logic to handle the new field
4. Add unit tests
5. Update CRD YAML in Helm chart (`charts/opensandbox-controller/templates/crds/`)

### Adding a New Strategy Implementation

1. Implement the existing interface (e.g., `PoolStrategy`, `EvictionHandler`)
2. Add a case to the factory function based on a label key
3. Add unit tests for the new implementation
4. Update the factory test

### Adding a New E2E Test

1. Add test data YAML to `test/e2e/testdata/`
2. Add test case in `test/e2e/e2e_test.go`
3. Run with `make test-e2e-main`

### Changing Annotation Contracts

The controller communicates allocation state through annotations on BatchSandbox objects. These are stability-sensitive:

| Annotation Key | JSON Shape | Writer | Reader |
|---|---|---|---|
| `sandbox.opensandbox.io/alloc-status` | `{"pods":["pod-1"]}` | `allocator.go` via `apis.go` | `batchsandbox_controller.go` |
| `sandbox.opensandbox.io/alloc-release` | `{"pods":["pod-3"]}` | `batchsandbox_controller.go` | `allocator.go` |

When changing annotation shapes, update all readers and writers, and add migration logic if the change is not backward-compatible.

## Build and Deploy

### Building Binaries

```bash
make build                    # Controller manager binary at bin/manager
make task-executor-build     # Task-executor binary at bin/task-executor
```

### Building Docker Images

```bash
# Controller image
make docker-build-controller CONTROLLER_IMG=opensandbox/controller:dev

# Task-executor image
make docker-build-task-executor TASK_EXECUTOR_IMG=opensandbox/task-executor:dev

# Or use the build script (recommended, supports multi-arch)
COMPONENT=controller TAG=v0.1.0 PUSH=false ./build.sh
COMPONENT=task-executor TAG=v0.1.0 PUSH=false ./build.sh
```

### Deploying with Kustomize

```bash
make install     # Install CRDs
make deploy      # Deploy controller to current cluster
make undeploy    # Remove controller
```

### Deploying with Helm

```bash
make helm-install
# Or with custom values
helm install opensandbox-controller ./charts/opensandbox-controller \
  --set controller.image.repository=myregistry/controller \
  --set controller.image.tag=v0.1.0 \
  --namespace opensandbox-system --create-namespace
```

See `docs/HELM-DEPLOYMENT.md` for full Helm documentation.

### Controller Configuration

Key flags (see `cmd/controller/main.go`):

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `0` | Metrics endpoint address |
| `--health-probe-bind-address` | `:8081` | Health probe address |
| `--leader-elect` | `false` | Enable leader election |
| `--kube-client-qps` | `100` | K8s client QPS |
| `--kube-client-burst` | `200` | K8s client burst |
| `--concurrency` | — | Per-controller concurrency, e.g. `batchsandbox=32;pool=128` |
| `--enable-file-log` | `false` | Enable file log rotation |

### Task-Executor Configuration

Key flags (see `internal/task-executor/config/config.go`):

| Flag / Env | Default | Description |
|------------|---------|-------------|
| `--data-dir` / `DATA_DIR` | `/var/lib/sandbox/tasks` | Task data directory |
| `--listen-addr` / `LISTEN_ADDR` | `0.0.0.0:5758` | HTTP listen address |
| `--enable-sidecar-mode` / `ENABLE_SIDECAR_MODE` | `false` | Sidecar runner mode |
| `--main-container-name` / `MAIN_CONTAINER_NAME` | `main` | Main container name (sidecar mode) |

## Debugging

### Local Controller Debugging

Run against the current kubeconfig:

```bash
make run
```

Or with custom flags:

```bash
go run ./cmd/controller/main.go \
  --leader-elect=false \
  --concurrency='batchsandbox=1;pool=1' \
  --zap-log-level=debug
```

### Remote Controller Debugging with Delve

Use the debug Dockerfile:

```bash
docker build -f Dockerfile.debug -t opensandbox-controller:debug .
# Then attach Delve to port 2345
```

### Task-Executor Debugging

Port-forward to the task-executor in a sandbox pod:

```bash
kubectl port-forward <sandbox-pod> 5758:5758

# List tasks
curl http://localhost:5758/tasks

# Create task
curl -X POST http://localhost:5758/tasks -d '{"name":"test","process":{"command":["echo","hello"]}}'
```

### Common Issues

**Controller not receiving BatchSandbox events for pool scheduling**: Check that `spec.poolRef` is set. The PoolReconciler's watch predicate filters BatchSandboxes where `poolRef` is empty.

**Allocation recovery fails on startup**: `InMemoryAllocationStore.Recover` reads all BatchSandbox annotations. If annotations are malformed, the controller will exit. Check `sandbox.opensandbox.io/alloc-status` annotation format.

**Scale expectations blocking creates**: If the controller restarts mid-scale, expectations may be stale. They time out after `expectationTimeout` (default 30s).

## Contributing

1. Fork the repository
2. Create a feature branch from `main`
3. Implement changes following coding standards
4. Run `make manifests generate fmt vet lint test`
5. Add tests for new functionality
6. Submit PR with clear description

### Code Review Focus Areas

- CRD type changes: ensure manifests are regenerated
- Annotation contract changes: ensure all readers/writers are updated
- Reconciler changes: verify idempotency and conflict handling
- Allocator changes: verify recovery (restart) path
- Breaking changes to CRD spec or annotation shapes
