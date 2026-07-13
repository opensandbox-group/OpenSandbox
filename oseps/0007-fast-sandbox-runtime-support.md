---
title: Fast Sandbox Runtime Support
authors:
  - "@fengcone"
creation-date: 2026-02-08
last-updated: 2026-02-08
status: provisional
---
# OSEP-0007: Fast Sandbox Runtime Support

<!-- toc -->

- [Summary](#summary)
- [Motivation](#motivation)
  - [Why Fast-Sandbox is Fast](#why-fast-sandbox-is-fast)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [How Fast-Sandbox Achieves Millisecond-Scale Latency](#how-fast-sandbox-achieves-millisecond-scale-latency)
  - [Kubernetes Ecosystem Integration](#kubernetes-ecosystem-integration)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)

<!-- /toc -->

## Summary

Add first-class support for [fast-sandbox](https://github.com/fengcone/fast-sandbox) as a high-performance runtime backend for OpenSandbox. By leveraging fast-sandbox's gRPC Fast-Path API and pre-warmed Agent pools, OpenSandbox can achieve **millisecond-scale cold start latency** (compared to ~1 second with OpenSandbox's BatchSandbox pool, or 2-5 seconds with standard K8s runtime) for AI Agents, Serverless functions, and other latency-sensitive workloads while maintaining the existing SDK and API contract.

**Performance Characteristics** (with cached images on Agent nodes):

- **Fast Mode**: <50ms (container-first, async CRD)
- **Strong Mode**: ~50-100ms base + K8s API write latency (typically 20-50ms via etcd)

> **Note**: The millisecond-scale latency assumes the container image is already cached on the Agent's host node. Cold starts with uncached images incur additional image pull time.

## Motivation

OpenSandbox currently supports Docker and Kubernetes runtimes. While the Kubernetes runtime provides scalability, sandbox creation typically takes 2-5 seconds due to:

- K8s scheduler latency (~100-500ms)
- etcd write and watch propagation (~50-200ms)
- Kubelet pod creation and container runtime startup (~1-3s)
- Image pull when cache miss occurs (~1-10s)

### OpenSandbox's Existing Pool Optimization

OpenSandbox's Kubernetes runtime already supports a **pool-based optimization** via the `poolRef` field in BatchSandbox CRD. When `poolRef` is specified:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: my-sandbox
spec:
  poolRef: my-pool              # Reference to pre-warmed pool
  taskTemplate:
    spec:
      process:
        command: ["python", "app.py"]
```

**How it works**:

- Users create a pool of pre-provisioned pods (managed by BatchSandbox controller)
- When creating a sandbox, OpenSandbox assigns a task from the pool
- Only `entrypoint` and `env` are customizable; image and resources are pre-defined
- Controller and OpenSandbox Server watch K8s API for state changes

**Performance with pool** (measured):

- Approximately **1 second** latency for pool-based allocation
- Eliminates scheduler wait and pod startup time
- Still requires K8s API write + watch propagation overhead
- Image must be pre-pulled in pool pods

This is an effective optimization for many use cases. However, fast-sandbox aims to push latency even lower through additional innovations described below.

For AI Agent and Serverless scenarios that require rapid sandbox provisioning, reducing even the K8s API overhead is valuable.

### Why Fast-Sandbox is Fast

fast-sandbox achieves millisecond-scale cold start through three key design innovations:

**Comparison: OpenSandbox Pool vs fast-sandbox**


| Aspect                          | OpenSandbox BatchSandbox Pool                      | fast-sandbox                                |
| ------------------------------- |----------------------------------------------------|---------------------------------------------|
| **Allocation mechanism**        | K8s API write → Controller watch → Task assignment | gRPC → in-memory Registry → Agent HTTP      |
| **Latency (with cached image)** | ~1 second (measured)                               | <50ms Fast, ~50 + API write (Strong)        |
| **Scheduling**                  | K8s Scheduler places pool pods (one-time)          | In-memory Registry with image affinity      |
| **Image awareness**             | Pool pods have fixed image                         | Registry scores by image cache availability |
| **Customization**               | entrypoint, env only                               | entrypoint, env, image, ports per request   |
| **Container creation**          | pre-warmed                                         | Direct containerd socket                    |
| **Consistency**                 | Strong (K8s etcd)                                  | Fast (eventual) or Strong (K8s etcd)        |
| **Failure recovery**            | K8s Controller reconciliation                      | Node Janitor + AutoRecreate policy          |

Both approaches use pre-provisioned resource pools to eliminate cold start overhead. fast-sandbox's key advantage is bypassing the K8s API path for container creation while maintaining visibility through async CRD writes.

#### 1. Direct API Allocation, Bypassing K8s Control Plane

Traditional K8s sandbox creation follows the slow path:

```
Client → K8s API Server → etcd → Scheduler → etcd → Kubelet → Container Runtime
 (~2-5 seconds total)
```

fast-sandbox uses a gRPC Fast-Path API that bypasses the K8s control plane:

**Fast Mode** (image cached on Agent node):

```
Client → gRPC Fast-Path → Registry (in-memory) → Agent HTTP → Containerd (<50ms)
```

**Strong Mode** (image cached on Agent node):

```
Client → gRPC Fast-Path → K8s API → etcd → Registry (in-memory) → Agent HTTP → Containerd
       ( <50ms base + 20-50ms API write)
```

**With uncached image** (both modes): Additional image pull time applies.

The Controller maintains an **in-memory Registry** for scheduling, eliminating:

- etcd write/read latency
- scheduler queue wait time
- watch propagation delays

This is similar to how "burst" instances work in cloud providers - resources are pre-provisioned and allocation happens at memory speed.

#### 2. In-Memory Scheduling with Image Affinity

fast-sandbox's Registry implements a smart scheduling algorithm:

```
score = allocated_count + (image_not_cached ? 1000 : 0)
```

Key characteristics:

- **In-memory allocation**: No disk I/O, no database queries (~1ms for 100 agents)
- **Image affinity scoring**: Prioritizes agents with cached images
- **Atomic slot management**: Avoids port conflicts through pre-reserved slots
- **Zero image pull latency**: When image is cached (common case), container starts immediately

This is fundamentally different from K8s scheduler which:

- Runs as a separate process with IPC overhead
- Doesn't track image cache state
- Schedules pods without considering image availability

#### 3. Kubernetes Ecosystem Reuse with Direct Containerd Access

fast-sandbox achieves speed while maintaining K8s compatibility:


| Aspect                     | fast-sandbox Approach                                          | K8s Benefit                               |
| -------------------------- | -------------------------------------------------------------- | ----------------------------------------- |
| **Resource Accounting**    | Agent Pods tracked in K8s                                      | Resource visibility via`kubectl get pods` |
| **Scheduling Constraints** | Node selectors, taints, tolerations via K8s                    | K8s scheduler places Agent Pods optimally |
| **Container Creation**     | Direct containerd socket access (bypasses kubelet)             | <10ms container creation vs ~500ms        |
| **Security Containers**    | Supports gVisor/Kata Containers via containerd runtime handler | Same workflow, different runtime class    |
| **Network Namespace**      | Reuses Agent Pod's network namespace                           | K8s CNI plugins work transparently        |

The key insight: **use K8s for what it's good at** (resource accounting, cluster management, scheduling constraints), but **bypass K8s for the hot path** (container creation).

### Goals

- Support creating, querying, and terminating sandboxes backed by fast-sandbox via the OpenSandbox server API
- Preserve existing OpenSandbox SDK and API behavior - no breaking changes
- Enable sub-100ms sandbox creation latency (strong consistency mode, with cached image) or sub-50ms (fast mode, with cached image)
- Support both Fast (ultra-low latency, eventual consistency) and Strong (guaranteed consistency) modes
- Provide flexible deployment: users can bring their own fast-sandbox or use OpenSandbox-provided charts

### Non-Goals

- Replacing or removing existing Docker or Kubernetes runtimes
- Implementing a full Kubernetes operator for fast-sandbox (it has its own controller)
- Changing the OpenSandbox sandbox lifecycle API or SDKs in a breaking way
- Direct management of fast-sandbox Agent Pods (handled by fast-sandbox controller)

## Requirements

- Must use the existing OpenSandbox lifecycle API and SDKs without breaking changes
- Must support OpenSandbox's execd-based command execution and file operations
- Must integrate with OpenSandbox's ingress component for routing
- Must support the standard OpenSandbox configuration model
- Must handle status mapping between fast-sandbox and OpenSandbox states

## Proposal

Introduce a `fast-sandbox` workload provider implementation that communicates directly with the fast-sandbox Controller via gRPC Fast-Path API. The provider is exposed as a new option under the Kubernetes runtime (`kubernetes.workload_provider = "fast-sandbox"`).

**Architecture Overview**:

```
+-------------------------------------------------------------------------+
|                        OpenSandbox Control Plane                        |
+-------------------------------------------------------------------------+
|                                                                         |
|   +--------------+    gRPC Fast-Path (9090)    +---------------------+  |
|   | OpenSandbox  | ------------------------>   |  fast-sandbox       |  |
|   |   Server     | <-------------------------  |  Controller         |  |
|   |              |    endpoints (IP:Port)      |                     |  |
|   +------+-------+                             +-------+-------------+  |
|          |                                             |                |
|          | SDK                                         | Registry       |
|          |                                             | (in-memory)    |
|          v                                             v                |
|   +--------------+    HTTP (5758)             +----------------------+  |
|   | OpenSandbox  | ---------------------->    |  Agent Pods          |  |
|   |  SDK         |    execd (44772)           |  (K8s Managed)       |  |
|   +--------------+                            +----------+-----------+  |
|                                                        |                |
|                                                        | containerd     |
|                                                        v                |
|                                                 +----------------+      |
|                                                 | User Container |      |
|                                                 | with execd     |      |
|                                                 +----------------+      |
|                                                                         |
+-------------------------------------------------------------------------+
                                ^
                                | K8s API Server (for Agent Pod mgmt only)
                                |
+-------------------------------------------------------------------------+
|                    Kubernetes Control Plane (async path)                |
|  - Agent Pod lifecycle (create/monitor/delete)                          |
|  - Resource accounting (CPU/memory requests visible in kubectl)         |
|  - Scheduling constraints (node selectors, taints, tolerations)         |
+-------------------------------------------------------------------------+
```

**Data Flow Comparison** (assuming cached image):

```
Standard K8s Runtime:
OpenSandbox Server → K8s API → etcd → Scheduler → etcd → Kubelet → containerd
      (2-5 seconds)

Fast-Sandbox Runtime (Fast Mode):
OpenSandbox Server → gRPC Fast-Path → Registry → Agent HTTP → containerd
      (<50ms, async CRD)

Fast-Sandbox Runtime (Strong Mode):
OpenSandbox Server → gRPC Fast-Path → K8s API → etcd → Watch → Agent → containerd
      (~50-100ms base + 20-50ms API write)
```

### Notes/Constraints/Caveats

- The fast-sandbox Controller and Agent Pods must be deployed separately (either by the user or via OpenSandbox-provided Helm charts)
- fast-sandbox uses its own CRD types (`Sandbox`, `SandboxPool`) for resource pool management - OpenSandbox does not manipulate these directly
- gRPC communication requires network reachability from OpenSandbox Server to fast-sandbox Controller
- The execd binary must be present in sandbox containers (typically via image or init container)

### Risks and Mitigations


| Risk                                                                     | Mitigation                                                                                                                         |
| ------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| fast-sandbox Controller becomes a single point of failure                | fast-sandbox Controller is designed for high availability; OpenSandbox can implement retries with fallback to standard K8s runtime |
| gRPC API changes in fast-sandbox could break integration                 | Version pinning in deployment; compatibility matrix documentation                                                                  |
| Network partition between OpenSandbox Server and fast-sandbox Controller | Configurable timeouts; health check endpoint integration                                                                           |
| State drift if sandboxes are managed outside OpenSandbox                 | OpenSandbox tracks sandbox IDs; periodic state reconciliation via gRPC GetSandbox                                                  |
| Fast mode orphaned containers                                            | fast-sandbox's Node Janitor DaemonSet cleans up orphaned resources                                                                 |

## Design Details

### How Fast-Sandbox Achieves Millisecond-Scale Latency

The fast-sandbox architecture is built around three performance-critical design choices:

#### 1. Bypassing K8s Control Plane for Hot Path

```
┌──────────────────────────────────────────────────────────────────────────┐
│                    Fast Mode Creation Flow (image cached)                │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                          |
│  Prerequisite: Image is cached on Agent's host node (via containerd)     │
│                                                                          |
│  1. OpenSandbox Server → gRPC CreateSandbox request                      │
│     └─────────────────────────────────────────────────> ~1ms             │
│                                                                          |
│  2. Registry.Allocate() - in-memory scheduling                           │
│     └─────────────────────────────────────────────────> ~1ms             │
│     • Filter by pool, namespace, capacity, port conflicts                │
│     • Score by: allocated + (no_image_cache ? 1000 : 0)                  │
│     • Atomic mutex-based allocation                                      │
│                                                                          |
│  3. Controller → Agent HTTP POST /api/v1/agent/create                    │
│     └─────────────────────────────────────────────────> ~10-30ms         │
│                                                                          |
│  4. Agent → containerd.Create() with cached image                        │
│     └─────────────────────────────────────────────────> ~5-10ms          │
│     • Direct socket access to host containerd                            │
│     • No image pull (cached)                                             │
│     • Reuse Agent Pod's network namespace                                │
│                                                                          |
│  5. Controller returns response with endpoints                           │
│     <───────────────────────────────────────────────── ~1ms              │
│                                                                          |
│  Total: <50ms (end-to-end, with cached image)                            │
│                                                                          |
│  (Async: Controller creates K8s CRD for reconciliation/audit trail)      │
│                                                                          |
│  If image is NOT cached: Image pull time is added to step 4              │
└──────────────────────────────────────────────────────────────────────────┘
```

Compare to standard K8s:

```
1. API Server write to etcd              ~20ms
2. Scheduler watch and decision          ~100-500ms
3. Scheduler write to etcd               ~20ms
4. Kubelet watch and pod creation        ~50-200ms
5. Container runtime start               ~500ms-3s
6. Image pull (if cache miss)            ~1-10s
Total: 2-5s (best case, cache hit)
```

#### 2. Registry Scheduling Algorithm

```go
// From fast-sandbox internal/controller/agentpool/registry.go (simplified)

func Allocate(sandbox *Sandbox) (*AgentInfo, error) {
    bestSlot := nil
    minScore := 1000000

    for _, agent := range registry.agents {
        // Skip if pool/namespace mismatch or at capacity
        if agent.PoolName != sandbox.PoolRef ||
           agent.Namespace != sandbox.Namespace ||
           agent.Allocated >= agent.Capacity {
            continue
        }
        // Check port conflicts 
		if contains(agent.UsedPorts, sandbox.ExposedPorts) {
			continue
        }
        // Score: prefer lower allocation + cached image
        score := agent.Allocated
        if !contains(agent.Images, sandbox.Image) {
            score += 1000  // Heavy penalty for uncached image
        }

        if score < minScore {
            minScore = score
            bestSlot = agent
        }
    }

    return bestSlot, nil
}
```

**Performance characteristics** (from fast-sandbox benchmarks):

- 100 Agents: ~1.3ms allocation time
- 1000 Agents: ~14ms allocation time

#### 3. Direct Containerd Integration

Agent Pods run with privileged access to host containerd socket:

```go
// From fast-sandbox internal/agent/runtime/containerd_runtime.go

client, _ := containerd.New("/run/containerd/containerd.sock",
    containerd.WithDefaultNamespace("k8s.io"))

// Direct container creation - bypasses kubelet entirely
container, _ := client.NewContainer(
    ctx,
    sandboxID,
    containerd.WithImage(image),           // Already cached
    containerd.WithNewSnapshot(...),       // Instant with cache
    containerd.WithRuntime("runc", nil),   // Or "io.containerd.runsc.v2" for gVisor
)

task, _ := container.NewTask(ctx, cio.NewCreator(...))
task.Start(ctx)
```

This approach:

- Eliminates kubelet API overhead (~50-200ms)
- Enables image cache reuse (Agent Pod shares node's containerd image store)
- Supports alternative runtimes (gVisor, Kata Containers) via runtime handler

### Kubernetes Ecosystem Integration

Despite bypassing the K8s control plane for the hot path, fast-sandbox maintains full compatibility:

#### Resource Accounting via K8s Pods

Agent Pods are normal K8s Pods:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: fast-sandbox-agent-node-1
  labels:
    app: fast-sandbox-agent
    pool-ref: default-pool
spec:
  containers:
  - name: agent
    image: fast-sandbox/agent:latest
    resources:
      requests:
        cpu: "2000m"
        memory: "4Gi"
      limits:
        cpu: "4000m"
        memory: "8Gi"
    volumeMounts:
    - name: containerd-socket
      mountPath: /run/containerd/containerd.sock
  volumes:
  - name: containerd-socket
    hostPath:
      path: /run/containerd/containerd.sock
```

These Pods are visible in `kubectl get pods` and count against:

- Node resource allocation (visible to cluster autoscaler)
- Resource quotas (namespace limits enforced)
- Scheduler decisions (node affinity, taints, tolerations)

#### CRD for Reconciliation and Auditing

fast-sandbox defines two CRDs:

```yaml
# SandboxPool - manages Agent Pod lifecycle
apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata:
  name: default-pool
  namespace: default
spec:
  capacity:
    poolMin: 2
    poolMax: 10
    bufferMin: 1
    bufferMax: 3
  maxSandboxesPerPod: 5
  runtimeType: container           # or "gvisor" for secure containers
  agentTemplate:
    spec:
      containers:
      - name: agent
        image: fast-sandbox/agent:latest
        imagePullPolicy: IfNotPresent
        env:
        - name: AGENT_CAPACITY
          value: "5"
        volumeMounts:
        - name: containerd-socket
          mountPath: /run/containerd/containerd.sock
      volumes:
      - name: containerd-socket
        hostPath:
          path: /run/containerd/containerd.sock

---
# Sandbox - audit trail for sandbox creation
apiVersion: sandbox.fast.io/v1alpha1
kind: Sandbox
metadata:
  name: my-sandbox
  namespace: default
  labels:
    sandbox.fast.io/created-by: fastpath-fast  # or fastpath-strong
spec:
  image: python:3.11
  poolRef: default-pool
  command: ["python", "-m", "http.server", "8000"]
  exposedPorts: [8000]
  failurePolicy: AutoRecreate         # or "Manual"
  recoveryTimeoutSeconds: 60
status:
  phase: Running
  sandboxID: abc123...               # Actual container ID
  assignedPod: fast-sandbox-agent-node-1
  nodeName: node-1
  endpoints:
  - "10.244.1.5:8000"
```

These CRDs serve as:

- **Audit trail**: Reconciliation between gRPC state and K8s
- **Self-healing**: Controller can detect and clean up orphaned sandboxes
- **Observability**: Standard K8s tools (kubectl, metrics-server) work

#### Security Container Support

fast-sandbox supports gVisor/Kata Containers via containerd runtime handlers:

```go
// Fast mode: runc (default)
containerd.WithRuntime("runc", nil)

// Secure mode: gVisor
containerd.WithRuntime("io.containerd.runsc.v2", nil)

// VM mode: Kata Containers
containerd.WithRuntime("io.containerd.kata.v2", nil)
```

This allows OpenSandbox to offer different security isolation levels without changing the integration layer.

#### Node Janitor: Orphan Container Cleanup

Fast mode creates containers before writing CRD, which can result in orphaned containers if:
- Agent Pod is unexpectedly deleted (crash, node drain, eviction)
- CRD write fails after container creation
- Network partition prevents CRD reconciliation

To handle these cases, fast-sandbox provides a **Node Janitor DaemonSet** that runs on each node

**How Janitor detects orphans:**

| Orphan Type | Detection Method | Cleanup Trigger |
|-------------|-------------------|-----------------|
| Agent Pod disappeared | Pod UID not found in K8s API | Immediate (after timeout) |
| Sandbox CRD deleted | CRD not found by sandbox name | Immediate (after timeout) |
| UID mismatch (recreated CRD) | Container label ≠ CRD UID | Immediate (after timeout) |
| Fast mode timeout | Container created > 10s ago without CRD | After orphan timeout |

**Janitor scan process:**

1. List all containers with label `fast-sandbox.io/managed=true` via containerd
2. For each container, check:
   - Agent Pod exists (via K8s API)
   - Sandbox CRD exists with matching UID
   - Container age > orphan timeout (default 10s for Fast mode)
3. If orphan detected: enqueue cleanup task
4. Cleanup process:
   - SIGKILL the task
   - Delete task from containerd
   - Delete container with snapshot cleanup
   - Remove FIFO files from `/run/containerd/fifo/`

**Configuration parameters:**

| Parameter | Default | Description |
|-----------|---------|-------------|
| `--scan-interval` | 2m | Full container scan interval |
| `--orphan-timeout` | 10s | Wait before treating Fast-mode container as orphan |
| `NODE_NAME` | (required) | Node this janitor pod runs on |

**Why the timeout?** Fast mode creates containers before CRD writes. The 10-second (configurable) timeout allows time for the async CRD write to complete, preventing false positives during normal operation.

### Configuration Extension

Add `FastSandboxRuntimeConfig` to `server/opensandbox_server/config.py`:

```python
class FastSandboxRuntimeConfig(BaseModel):
    """fast-sandbox runtime configuration."""

    controller_endpoint: str = Field(
        default="localhost:9090",
        description="fast-sandbox Controller gRPC endpoint.",
    )
    default_pool_ref: str = Field(
        default="default-pool",
        description="Default SandboxPool name for sandbox allocation.",
    )
    default_consistency_mode: Literal["fast", "strong"] = Field(
        default="strong",
        description=(
            "Default consistency mode. 'fast' = sub-50ms with cached image, eventual consistency. "
            "'strong' = ~50-100ms base + K8s API write latency (typically 20-50ms), guaranteed consistency."
        ),
    )
    execd_port: int = Field(
        default=44772,
        description="execd port in sandbox containers.",
    )
```

Update `AppConfig` to include the new config block and validation logic.

### TOML Configuration Example

```toml
[server]
host = "0.0.0.0"
port = 8080
api_key = "your-secret-key"

[runtime]
type = "kubernetes"
execd_image = "opensandbox/execd:v1.0.21"

[kubernetes]
namespace = "default"
workload_provider = "fast-sandbox"

[fast_sandbox]
controller_endpoint = "fast-sandbox-controller.opensandbox.svc:9090"
default_pool_ref = "default-pool"
default_consistency_mode = "strong"  # "fast" = sub-50ms (cached), "strong" = ~50-100ms + API write
execd_port = 44772
```

### New Code Structure

```
server/opensandbox_server/services/k8s/
├── fastsandbox_provider.py      # New: FastSandboxProvider WorkloadProvider implementation
├── fastsandbox_client.py        # New: gRPC client wrapper for fast-sandbox Controller
├── provider_factory.py          # Modified: Register "fast-sandbox" provider
└── ...
```

### API Mapping


| OpenSandbox API                         | fast-sandbox gRPC  | Description                       |
| --------------------------------------- | ------------------ | --------------------------------- |
| `POST /sandboxes`                       | `CreateSandbox`    | Create sandbox, returns endpoints |
| `GET /sandboxes/{id}`                   | `GetSandbox`       | Query sandbox status              |
| `DELETE /sandboxes/{id}`                | `DeleteSandbox`    | Delete sandbox                    |
| `POST /sandboxes/{id}/renew-expiration` | `UpdateSandbox`    | Update expiration time            |
| `GET /sandboxes/{id}/endpoints/{port}`  | (local resolution) | Resolve from CreateResponse       |

### Request Parameter Mapping

```python
# OpenSandbox CreateSandboxRequest → fast-sandbox CreateRequest
{
    "image": {"uri": "python:3.11"},              # → image
    "entrypoint": ["python", "-m", "http.server"], # → command
    "env": {"PYTHONUNBUFFERED": "1"},             # → envs
    "resourceLimits": {"cpu": "500m"},            # → (Agent pool capacity)
    "timeout": 3600,                             # → expireTimeSeconds
    "extensions": {
        "pool_ref": "default-pool",              # → poolRef
        "consistency_mode": "strong",            # → consistencyMode (override)
        "failure_policy": "auto_recreate"        # → failurePolicy
    }
}
```

### Status Mapping


| fast-sandbox Phase | OpenSandbox State |
| ------------------ | ----------------- |
| Running            | Running           |
| Pending / Creating | Pending           |
| Failed / Lost      | Failed            |
| (deleted)          | Terminated        |

### Extensions Field Support

The `extensions` field in `CreateSandboxRequest` supports fast-sandbox specific options:


| Extension Key      | Type                       | Description                                 |
| ------------------ | -------------------------- | ------------------------------------------- |
| `pool_ref`         | string                     | Target SandboxPool name (overrides default) |
| `consistency_mode` | "fast"\| "strong"          | Consistency mode (overrides default)        |
| `failure_policy`   | "manual"\| "auto_recreate" | Failure recovery policy                     |

## Test Plan

- **Unit Tests**: FastSandboxClient gRPC wrapper, request/response mapping, status translation
- **Integration Tests**: Deploy fast-sandbox in Kind cluster, test create/get/delete/renew flows
- **E2E Tests**: Full OpenSandbox SDK flow using fast-sandbox runtime
- **Performance Tests**: Measure sandbox creation latency vs standard K8s runtime

### Test Scenarios

1. Basic lifecycle: create → status query → delete
2. Expiration renewal
3. Fast vs Strong consistency modes
4. Pool selection via extensions
5. Image affinity: second sandbox on same node (should be faster)
6. Failure: controller unavailable, invalid pool ref
7. execd connectivity after sandbox creation
8. Concurrent sandbox creation (stress test)

### Performance Benchmarks

Target metrics (to be verified in tests):


| Scenario                              | Target Latency         | Notes                                           |
| ------------------------------------- | ---------------------- | ----------------------------------------------- |
| OpenSandbox BatchSandbox Pool         | ~1 second              | Measured with K8s API + watch overhead          |
| Cold start, image cached, Fast mode   | <50ms                  | Container-first, async CRD                      |
| Cold start, image cached, Strong mode | ~50-100ms + API write  | CRD-first, ~20-50ms additional for K8s API/etcd |
| Cold start, image NOT cached          | Base + image pull time | Image pull depends on size and network          |
| Warm start (reuse same Agent)         | <30ms                  | Agent already allocated                         |
| Registry allocation (100 Agents)      | ~1.3ms                 | In-memory scheduling                            |
| Registry allocation (1000 Agents)     | ~14ms                  | In-memory scheduling                            |

> **Important**: The millisecond-scale latencies above assume the container image is already cached on the Agent's host node. In production, pre-pulling images or using a common set of base images is recommended for consistent performance.

## Drawbacks

- **Added Dependency**: Requires deploying and managing fast-sandbox Controller and Agent Pods and Janitor DaemonSet
- **Operational Complexity**: Teams need to understand both OpenSandbox and fast-sandbox concepts
- **gRPC Protocol**: Introduces gRPC dependency (vs pure HTTP/REST for K8s API)
- **Limited Ecosystem**: fast-sandbox is a newer project with smaller community than vanilla K8s
- **Fast Mode Orphans**: Fast consistency mode can create orphaned containers if CRD write fails (mitigated by Node Janitor)

## Alternatives

1. **Continue with standard K8s runtime only**: Rejected due to 2-5s cold start latency
2. **Use only fast-sandbox CRD path (via K8s API)**: Rejected because it loses the Fast-Path gRPC performance benefit
3. **Build OpenSandbox-native fast-path**: Rejected due to reinventing complex scheduling and container management logic
4. **External adapter service**: Rejected due to additional operational components

## Infrastructure Needed

- **CI/CD**: Kind cluster with fast-sandbox installed for integration tests
- **Documentation**: Deployment guide for fast-sandbox + OpenSandbox integration
- **Helm Charts** (optional): Unified charts deploying OpenSandbox Server + fast-sandbox components

## Upgrade & Migration Strategy

- **Backwards Compatible**: Default runtime unchanged; opt-in via configuration
- **No Migration**: Existing Docker/K8s runtime users unaffected
- **Enable by Config**: Simply set `kubernetes.workload_provider = "fast-sandbox"` and add `[fast_sandbox]` block
- **Rollback**: Switch back to `kubernetes` or `docker` runtime type with no data loss
