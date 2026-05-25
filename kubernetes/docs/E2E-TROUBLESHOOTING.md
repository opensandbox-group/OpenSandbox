# E2E Test Troubleshooting Guide

This document describes how to diagnose and resolve E2E test failures for the OpenSandbox Kubernetes component.

## E2E Test Overview

E2E tests are organized into three categories:

| Test Suite | Path | Run Command | Dependencies |
|------------|------|-------------|--------------|
| Core E2E | `test/e2e/` | `make test-e2e-main` | Kind + Docker |
| Task-Executor E2E | `test/e2e_task/` | included in `make test-e2e` | Docker |
| gVisor Runtime E2E | `test/e2e_runtime/gvisor/` | `make test-gvisor` | Kind + Docker + gVisor |

## General Troubleshooting Steps

### 1. Check the Test Failure Output

E2E tests use the Ginkgo framework. On failure, detailed assertion information is printed. Focus on:

- Failed `Eventually` assertions: a condition was not satisfied within the timeout
- Failed `Expect` assertions: an immediate condition was not met
- Debug information automatically collected in `AfterEach` (Core E2E only):
  - Controller manager pod logs
  - Kubernetes events
  - Pod describe output

### 2. Re-run a Single Failed Case

Use Ginkgo's focus mechanism to run only the failing case:

```bash
# Focus by description text
make test-e2e-main GINKGO_ARGS="-ginkgo.focus='Pool eviction'"

# Focus by regex
make test-e2e-main GINKGO_ARGS="-ginkgo.focus='should handle pod eviction'"
```

### 3. Preserve the Cluster for Manual Investigation

By default, E2E tests destroy the Kind cluster after completion. To preserve the cluster for manual debugging, create the cluster and deploy manually instead of using the full test runner:

```bash
# Manually create a Kind cluster
kind create cluster --name sandbox-k8s-test-e2e --image kindest/node:v1.22.4

# Install CRDs and deploy the controller
make install
make deploy CONTROLLER_IMG=<your-image>

# Manually apply the failing test data YAML
kubectl apply -f test/e2e/testdata/pool-basic.yaml

# Manually inspect resource state
kubectl get pools -A
kubectl get batchsandboxes -A
kubectl get pods -A
```

Clean up after investigation:

```bash
kind delete cluster --name sandbox-k8s-test-e2e
```

## Core E2E Common Issues

### Controller Pod Not Ready

**Symptom**: `Eventually(verifyControllerUp)` times out.

**Diagnosis**:

```bash
# Check pod status
kubectl get pods -n opensandbox-system -l control-plane=controller-manager

# View pod events
kubectl describe pod -n opensandbox-system -l control-plane=controller-manager

# View logs
kubectl logs -n opensandbox-system -l control-plane=controller-manager
```

**Common causes**:
- Image not loaded into the Kind cluster: run `kind load docker-image <image> --name sandbox-k8s-test-e2e`
- Image pull policy is `IfNotPresent` but the image does not exist
- Insufficient resource limits causing OOMKilled

### Pool Pods Stuck Not Running

**Symptom**: `Eventually` waiting for Pool Pod Running times out.

**Diagnosis**:

```bash
# Check pod status and reason
kubectl get pods -n <namespace> -l sandbox.opensandbox.io/pool-name=<pool-name>
kubectl describe pod <pod-name> -n <namespace>

# Common states
# - ImagePullBackOff: image pull failed
# - Pending: insufficient node resources
# - CrashLoopBackOff: container startup failed
```

**Common causes**:
- Sandbox image address is incorrect or cannot be pulled. Check the `utils.SandboxImage` variable and `{{.SandboxImage}}` in `testdata/*.yaml`
- Kind cluster node resources are insufficient (default Kind clusters have limited resources)

### BatchSandbox Allocation State Not as Expected

**Symptom**: `alloc-status` annotation is empty or the pod count is wrong.

**Diagnosis**:

```bash
# Check BatchSandbox allocation annotation
kubectl get batchsandbox <name> -n <namespace> -o jsonpath='{.metadata.annotations.sandbox\.opensandbox\.io/alloc-status}'

# Check Pool status
kubectl get pool <pool-name> -n <namespace> -o yaml

# Check controller logs for scheduling information
kubectl logs -n opensandbox-system -l control-plane=controller-manager | grep -i "schedule\|allocate\|insufficient"
```

**Common causes**:
- Insufficient available pods in the Pool. Check the `available` field in Pool status
- Allocation recovery failed. Search for "recovery" in controller startup logs
- Replica count exceeds PoolMax. Check the Pool's `capacitySpec.poolMax`

### Eventual Consistency Issues (Eventually Timeout but Manual Verification Passes)

**Symptom**: `Eventually` times out in the E2E test, but manually checking the resource state shows it already matches expectations.

**Common causes**:
- Default `EventuallyTimeout` is 2 minutes; complex scenarios may require longer
- Kind cluster performance is insufficient, causing controller processing delays
- Code was modified but the image was not rebuilt and reloaded into the Kind cluster

**Resolution**:

```bash
# Ensure the latest image is used
make docker-build-controller CONTROLLER_IMG=controller:dev
kind load docker-image controller:dev --name sandbox-k8s-test-e2e

# Or rebuild before testing
make test-e2e-main
```

## Task-Executor E2E Common Issues

### Docker Container Startup Failure

**Symptom**: Docker build or run command fails.

**Diagnosis**:

```bash
# Check if Docker is available
docker info

# Clean up leftover containers from previous runs
docker rm -f task-e2e-target task-e2e-executor
docker volume rm task-e2e-vol
```

### Task State Not as Expected

**Symptom**: Task creation or execution state is abnormal.

**Diagnosis**:

```bash
# Access the task-executor API directly (when container is running)
curl http://localhost:5758/tasks

# Check a specific task
curl http://localhost:5758/tasks/<task-name>

# View executor container logs
docker logs task-e2e-executor
```

### Process Not Visible in Sidecar Mode

**Symptom**: ProcessExecutor in sidecar mode cannot discover or manage the main container's processes.

**Diagnosis**:
- Confirm the `--pid=container:` parameter correctly points to the target container
- Confirm `--enable-sidecar-mode=true` is set
- Confirm `--main-container-name` matches the `SANDBOX_MAIN_CONTAINER` environment variable in the target container

## gVisor E2E Common Issues

### gVisor Runtime Not Installed

**Symptom**: Pod creation fails with RuntimeClass `gvisor` not found.

**Diagnosis**:

```bash
# Check RuntimeClass
kubectl get runtimeclass gvisor

# Reinstall
kubectl apply -f test/e2e_runtime/gvisor/testdata/runtimeclass.yaml
```

### runsc Binary Missing

**Symptom**: Pod is in `CreateContainerError` state with events indicating `runsc` not found.

**Diagnosis**:
- Confirm `make download-gvisor` has been executed
- Confirm `make setup-gvisor` has been executed and Kind nodes contain the runsc binary

## Environment Issues

### Stale Kind Clusters

If a previous test exited abnormally, stale Kind clusters may remain:

```bash
# List all Kind clusters
kind get clusters

# Delete stale clusters
kind delete cluster --name sandbox-k8s-test-e2e
kind delete cluster --name gvisor-test
```

### Docker Resource Exhaustion

```bash
# Check Docker disk usage
docker system df

# Clean up
docker system prune -a
docker builder prune -a
```

### Port Conflicts

Task-Executor E2E uses port 5758, Core E2E uses port 8081 (health probe). If ports are occupied:

```bash
# Check port usage
lsof -i :5758
lsof -i :8081

# Kill the occupying process
kill <PID>
```

## Useful Debug Commands

```bash
# View full controller logs
kubectl logs -n opensandbox-system -l control-plane=controller-manager -f

# View all OpenSandbox-related resources
kubectl get pools,batchsandboxes,pods -A

# View events for a specific resource
kubectl describe pool <pool-name> -n <namespace>
kubectl describe batchsandbox <sbx-name> -n <namespace>

# Verify CRDs are correctly installed
kubectl get crd batchsandboxes.sandbox.opensandbox.io -o yaml
kubectl get crd pools.sandbox.opensandbox.io -o yaml

# Check controller RBAC permissions
kubectl auth can-i --as=system:serviceaccount:opensandbox-system:opensandbox-opensandbox-controller-controller-manager create pods
kubectl auth can-i --as=system:serviceaccount:opensandbox-system:opensandbox-opensandbox-controller-controller-manager update batchsandboxes
```
