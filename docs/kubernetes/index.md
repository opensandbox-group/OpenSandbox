---
title: Kubernetes Controller
description: Kubernetes operator for automated sandbox lifecycle management, resource pooling, batch creation, and optional task orchestration.
---

# OpenSandbox Kubernetes Controller

OpenSandbox Kubernetes Controller is a Kubernetes operator that manages sandbox environments through custom resources. It provides **automated sandbox lifecycle management**, **resource pooling for fast provisioning**, **batch sandbox creation**, and **optional task orchestration** capabilities in Kubernetes clusters.

## Key Features

- **Flexible Sandbox Creation**: Choose between pooled and non-pooled sandbox creation modes
- **Batch and Individual Delivery**: Support both single sandbox (for real-user interactions) and batch sandbox delivery (for high-throughput agentic-RL scenarios)
- **Optional Task Scheduling**: Integrated task orchestration with optional shard task templates for heterogeneous task distribution and customized sandbox delivery (e.g., process injection)
- **Resource Pooling**: Maintain pre-warmed resource pools for rapid sandbox provisioning
- **Pause and Resume**: Persist sandbox filesystem state via rootfs snapshots, releasing cluster resources between sessions
- **Comprehensive Monitoring**: Real-time status tracking of sandboxes and tasks

## Features

### Batch Sandbox Management
The BatchSandbox custom resource allows you to create and manage multiple identical sandbox environments. Key capabilities include:
- **Flexible Creation Modes**: Support both pooled (using resource pools) and non-pooled sandbox creation
- **Single and Batch Delivery**: Create single sandboxes (replicas=1) or batches of sandboxes (replicas=N) as needed
- **Scalable Replica Management**: Easily control the number of sandbox instances through replica configuration
- **Automatic Expiration**: Set TTL (time-to-live) for automatic cleanup of expired sandboxes
- **Optional Task Scheduling**: Built-in task execution engine with support for optional task templates
- **Detailed Status Reporting**: Comprehensive metrics on replicas, allocations, and task states

### Resource Pooling
The Pool custom resource maintains a pool of pre-warmed compute resources to enable rapid sandbox provisioning:
- Configurable buffer sizes (minimum and maximum) to balance resource availability and cost
- Pool capacity limits to control overall resource consumption
- Automatic resource allocation and deallocation based on demand
- Real-time status monitoring showing total, allocated, and available resources

### Pod Eviction
Pool supports graceful pod eviction for scenarios like node maintenance or resource reclamation:

**How it works:**
- Users label a pod with `pool.opensandbox.io/evict` to request eviction
- The controller skips pods already allocated to BatchSandbox (protecting in-use workloads)
- Idle pods are deleted, triggering the pool to replenish capacity
- Pods marked for eviction are excluded from new allocations

**Custom eviction behavior:**
You can implement custom eviction strategies by:
1. Setting `pool.opensandbox.io/eviction-handler` label on the Pool to select your handler
2. Implementing the `EvictionHandler` interface with `NeedsEviction()` and `Evict()` methods
3. Registering your handler in the factory function

### Task Orchestration
Integrated task management system that executes custom workloads within sandboxes:
- **Optional Execution**: Task scheduling is completely optional - sandboxes can be created without tasks
- **Process-Based Tasks**: Support for process-based tasks that execute within the sandbox environment
- **Heterogeneous Task Distribution**: Customize individual tasks for each sandbox in a batch using shardTaskPatches

#### Process lifecycle hooks

Process tasks can define `preStart` and `postStop` exec hooks under `spec.taskTemplate.spec.process.lifecycle`:

```yaml
spec:
  taskTemplate:
    spec:
      process:
        command: ["/bin/sh", "-c"]
        args: ["run-task"]
        lifecycle:
          preStart:
            exec:
              command: ["/bin/sh", "-c", "prepare-inputs"]
            timeoutSeconds: 30
          postStop:
            exec:
              command: ["/bin/sh", "-c", "persist-outputs"]
            timeoutSeconds: 30
```

`preStart` runs before the main process starts. A failed or timed-out hook prevents the main process from starting and is reported in the task status.

`postStop` is attempted whenever a task reaches a terminal state, including natural success or failure, `preStart` or process start failure, timeout, and task deletion. The task executor preserves the original terminal status and records the successful or failed hook outcome so a recorded `postStop` attempt is not repeated during later reconciliation or recovery. This makes the hooks suitable for staging files into a sandbox before execution and persisting outputs to a mounted volume after execution.

### Advanced Scheduling
Intelligent resource management features:
- Minimum and maximum buffer settings to ensure resource availability while controlling costs
- Pool-wide capacity limits to prevent resource exhaustion
- Automatic scaling based on demand

## Pause and Resume (Rootfs Snapshot)

OpenSandbox supports **pause and resume** for Kubernetes sandboxes by persisting the container root filesystem as an OCI image.

```text
Time ---------------------------------------------------------------->

Sandbox lifecycle:   [Running]--[Pausing]--[Paused]--[Resuming]--[Running]
                         |                     |
                  commit rootfs          rewrite template images
                  push to registry       recreate runtime from snapshot
                  release pods/alloc
```

### How it works

1. **Pause**: The server patches `BatchSandbox.spec.pause=true`. The controller creates an internal `SandboxSnapshot`, runs a commit Job on the same node, commits the container rootfs, and pushes it to the configured OCI registry. After the snapshot is ready, the controller transitions the same `BatchSandbox` to `Paused` and releases runtime Pods / pooled allocations.
2. **Resume**: The server patches `BatchSandbox.spec.pause=false`. The controller reads the latest `SandboxSnapshot`, rewrites the `BatchSandbox` template images to the snapshot image URIs, recreates the runtime, and transitions the sandbox back to `Running`. The public `sandboxId` remains stable across pause/resume cycles.

::: warning
Current pause/resume support is limited to `BatchSandbox.spec.replicas=1`. The OpenSandbox server creates Kubernetes sandboxes with `replicas: 1`; direct `BatchSandbox` CRs with any other replica count are rejected by the controller pause entry because the internal pause snapshot records one source Pod's container images.
:::

### The SandboxSnapshot CRD

The `SandboxSnapshot` CR is the central resource for pause/resume lifecycle:

| Field | Location | Description |
|-------|----------|-------------|
| `spec.sandboxName` | Spec | Target `BatchSandbox` name in the same namespace |
| `status.phase` | Status | `Pending` -> `Committing` -> `Succeed` / `Failed` |
| `status.conditions` | Status | `Ready` / `Failed` conditions with reason and message |
| `status.containers` | Status | Committed image URIs per container |
| `status.sourcePodName` | Status | Pod name resolved by controller |
| `status.sourceNodeName` | Status | Node selected for the commit Job |

### Prerequisites

1. **OCI Registry**: An accessible container registry for storing snapshot images.
2. **Kubernetes Secrets**: Docker config secrets for push and pull access.
3. **Controller configuration**: Configure the controller manager with snapshot registry and secret flags.
4. **Controller RBAC**: The controller requires `secrets: get` permission (included in the Helm chart and `make manifests` output).

### Controller Configuration

The snapshot controller supports the following command-line flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--snapshot-registry` | `""` | OCI registry prefix used for snapshot images |
| `--snapshot-push-secret` | `""` | Secret name used by commit Jobs to push snapshots |
| `--resume-pull-secret` | `""` | Secret name injected into resumed sandboxes for image pulls |
| `--image-committer-image` | `image-committer:dev` | Image used for commit operations (must contain `nerdctl` tool) |
| `--commit-job-timeout` | `10m` | Timeout duration for commit jobs |
| `--snapshot-registry-insecure` | `false` | Pass insecure registry mode to snapshot commit Jobs |

These flags are configured at controller startup. The `image-committer-image` must be a trusted container image with `nerdctl` to perform rootfs commit and push operations. Commit Jobs mount the host containerd socket on the source node, so the image effectively has node-level runtime access. Pin the image by digest or enforce a trusted registry/admission policy in production.

### Quick Setup

```bash
# Create push secret
kubectl create secret docker-registry registry-snapshot-push-secret \
  --docker-server=<your-registry> \
  --docker-username=<user> \
  --docker-password=<token>

# Create pull secret (can reuse push secret)
kubectl create secret docker-registry registry-pull-secret \
  --docker-server=<your-registry> \
  --docker-username=<user> \
  --docker-password=<token>
```

Then configure the controller manager with:

```yaml
- --snapshot-registry=<your-registry>/sandboxes
- --snapshot-registry-insecure=true # only for HTTP/self-signed local registries
- --snapshot-push-secret=registry-snapshot-push-secret
- --resume-pull-secret=registry-pull-secret
```

::: info
Snapshot image retention is registry-managed. Deleting a `SandboxSnapshot` removes the Kubernetes commit/unpause Jobs, but it does not delete pushed OCI images from the registry. Configure registry retention/GC for tags such as `snap-gen<N>` according to your environment.
:::

## Getting Started

![Deploy Example](../public/images/deploy-example.gif)

### Prerequisites
- go version v1.24.0+
- docker version 17.03+
- kubectl version v1.11.3+
- Access to a Kubernetes v1.21.1+ cluster

If you don't have access to a Kubernetes cluster, you can use [kind](https://kind.sigs.k8s.io/) to create a local Kubernetes cluster for testing purposes. Kind runs Kubernetes nodes in Docker containers, making it easy to set up a local development environment.

To install kind:
- Download the release binary for your OS from the [releases page](https://github.com/kubernetes-sigs/kind/releases) and move it into your `$PATH`
- Or use a package manager:
  - macOS (Homebrew): `brew install kind`
  - Windows (winget): `winget install Kubernetes.kind`

After installing kind, create a cluster with:
```sh
kind create cluster
```

::: tip Kind Users
If you're using a kind cluster, you need to load the controller and task-executor images into the kind node after building them with `make docker-build`. Kind runs Kubernetes nodes in Docker containers and cannot directly access images from your local Docker daemon.

```sh
kind load docker-image <controller-image-name>:<tag>
kind load docker-image <task-executor-image-name>:<tag>
```
:::

### Deployment

This project requires two separate images - one for the controller and another for the task-executor component.

#### Option 1: Deploy with Helm (Recommended)

**Install from GitHub Release:**

You can install OpenSandbox Controller directly from GitHub Releases. Check the [Releases page](https://github.com/opensandbox-group/OpenSandbox/releases?q=helm%2Fopensandbox-controller&expanded=true) for all available versions.

```sh
# Replace <version> with the desired version (e.g., 0.1.0)
helm install opensandbox-controller \
  https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-controller/<version>/opensandbox-controller-<version>.tgz \
  --namespace opensandbox-system \
  --create-namespace
```

**Customize Installation:**

Use `--set` flags to customize the configuration:

```sh
helm install opensandbox-controller \
  https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz \
  --namespace opensandbox-system \
  --create-namespace \
  --set controller.replicaCount=2 \
  --set controller.resources.limits.cpu=1000m \
  --set controller.resources.limits.memory=512Mi
```

Or use a values file for complex configurations:

```sh
cat > custom-values.yaml <<EOF
controller:
  replicaCount: 2
  resources:
    limits:
      cpu: 1000m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
  logLevel: debug
EOF

helm install opensandbox-controller \
  https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz \
  --namespace opensandbox-system \
  --create-namespace \
  -f custom-values.yaml
```

**Install from source (for development):**

1. Build and push your images:
   ```sh
   cd kubernetes
   make docker-build docker-push CONTROLLER_IMG=<some-registry>/opensandbox-controller:tag
   make docker-build-task-executor docker-push-task-executor TASK_EXECUTOR_IMG=<some-registry>/opensandbox-task-executor:tag
   ```

2. Install with Helm:
   ```sh
   helm install opensandbox-controller ./charts/opensandbox-controller \
     --set controller.image.repository=<some-registry>/opensandbox-controller \
     --set controller.image.tag=<tag> \
     --namespace opensandbox-system \
     --create-namespace
   ```

**Verify Installation:**

```sh
kubectl get pods -n opensandbox-system
kubectl get deployment -n opensandbox-system
kubectl logs -n opensandbox-system -l control-plane=controller-manager -f
```

**Upgrade:**

```sh
helm upgrade opensandbox-controller \
  https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-controller/<new-version>/opensandbox-controller-<new-version>.tgz \
  --namespace opensandbox-system
```

**Uninstall:**

```sh
helm uninstall opensandbox-controller -n opensandbox-system
```

#### Option 2: Deploy with Kustomize

1. Build and push your images:
   ```sh
   make docker-build docker-push CONTROLLER_IMG=<some-registry>/opensandbox-controller:tag
   make docker-build-task-executor docker-push-task-executor TASK_EXECUTOR_IMG=<some-registry>/opensandbox-task-executor:tag
   ```

2. Install the CRDs into the cluster:
   ```sh
   make install
   ```

3. Deploy the Manager to the cluster:
   ```sh
   make deploy CONTROLLER_IMG=<some-registry>/opensandbox-controller:tag
   ```

::: warning
`make deploy` only rewrites the controller image. Build and publish `TASK_EXECUTOR_IMG` separately if your Pool / BatchSandbox templates refer to it. You may also need cluster-admin privileges before running the commands.
:::

### Creating BatchSandbox and Pool Resources

#### Basic Example
Create a simple non-pooled sandbox without task scheduling:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: basic-batch-sandbox
spec:
  replicas: 2
  template:
    spec:
      containers:
      - name: sandbox-container
        image: nginx:latest
        ports:
        - containerPort: 80
```

Apply and check status:
```sh
kubectl apply -f basic-batch-sandbox.yaml
kubectl get batchsandbox basic-batch-sandbox -o wide
```

After the sandboxes are ready, find the endpoint information in the annotations:
```sh
kubectl get batchsandbox basic-batch-sandbox -o jsonpath='{.metadata.annotations.sandbox\.opensandbox\.io/endpoints}'
```

#### Pooled Sandbox Without Task
First, create a resource pool:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: example-pool
spec:
  template:
    spec:
      containers:
      - name: sandbox-container
        image: nginx:latest
        ports:
        - containerPort: 80
  capacitySpec:
    bufferMax: 10
    bufferMin: 2
    poolMax: 20
    poolMin: 5
```

Optional: add `scaleStrategy` to limit the pace of scaling:
```yaml
  scaleStrategy:
    maxUnavailable: "20%"  # or absolute number like 5
```

Create a batch of sandboxes using the pool:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: pooled-batch-sandbox
spec:
  replicas: 3
  poolRef: example-pool
```

::: warning Per-request network policies
Pool pods are created before allocation. The lifecycle API therefore rejects `networkPolicy` together with `extensions.poolRef`; it cannot inject an egress sidecar into an existing pool pod. Configure required network controls in the Pool pod template before pods are created, or use a non-pooled sandbox for per-request policies.
:::

#### Pooled Sandbox with Heterogeneous Tasks
Create a batch of sandboxes with process-based heterogeneous tasks. For task execution to work properly, the task-executor must be deployed as a sidecar container in the pool template and share the process namespace with the sandbox container:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: task-example-pool
spec:
  template:
    spec:
      shareProcessNamespace: true
      containers:
      - name: sandbox-container
        image: ubuntu:latest
        command: ["sleep", "3600"]
      - name: task-executor
        image: <task-executor-image>:<tag>
        securityContext:
          capabilities:
            add: ["SYS_PTRACE"]
  capacitySpec:
    bufferMax: 10
    bufferMin: 2
    poolMax: 20
    poolMin: 5
```

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: task-batch-sandbox
spec:
  replicas: 2
  poolRef: task-example-pool
  taskTemplate:
    spec:
      process:
        command: ["echo", "Default task"]
  shardTaskPatches:
  - spec:
      process:
        command: ["echo", "Custom task for sandbox 1"]
  - spec:
      process:
        command: ["echo", "Custom task for sandbox 2"]
        args: ["with", "additional", "arguments"]
```

### Monitoring Resources

```sh
kubectl get pools
kubectl get batchsandboxes
kubectl describe pool example-pool
kubectl describe batchsandbox example-batch-sandbox
```

## Performance

When both use resource pools, the total time comparison for delivering 100 Sandboxes:

| Test Scenario | Total Time (seconds) |
|---------------|---------------------|
| SIG Agent-Sandbox (concurrency=1) | 76.35 |
| SIG Agent-Sandbox (concurrency=10) | 23.17 |
| SIG Agent-Sandbox (concurrency=50) | 33.85 |
| BatchSandbox | 0.92 |

The time complexity of SIG Agent-Sandbox and BatchSandbox for batch delivery of N Sandboxes is O(N) and O(1) respectively.

## Project Structure

```
kubernetes/
  api/v1alpha1/          # Custom resource definitions (BatchSandbox, Pool)
  cmd/controller/        # Main controller manager entry point
  cmd/task-executor/     # Task executor binary
  config/crd/            # CRD manifests
  config/manager/        # Controller manager configuration
  config/rbac/           # RBAC manifests
  config/samples/        # Sample YAML manifests
  internal/controller/   # Core controller implementations
  internal/scheduler/    # Resource allocation and scheduling logic
  internal/task-executor/# Task execution engine internals
  pkg/task-executor/     # Shared task executor packages
  test/                  # Test suites and utilities
```

## License
This project is open source under the Apache 2.0 License.
