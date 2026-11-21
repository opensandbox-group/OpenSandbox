---
title: Workspace-only Kubernetes pause with a bucket-backed volume
authors:
  - "@LuisQin01"
creation-date: 2026-11-21
last-updated: 2026-11-21
status: draft
---

# OSEP-0032 Workspace-only Kubernetes pause with a bucket-backed volume

## Summary

[Issue 1169](https://github.com/opensandbox-group/OpenSandbox/issues/1169) requests a pause strategy for Kubernetes installations that can persist workspace files to object storage. A dedicated BatchSandbox selects the application container and a checkpoint volume through `spec.workspacePause`. A CSI driver supplies a namespaced PVC backed by an object bucket. The application can start with a regular directory at `/workspace`.

## Proposal

An internal pause snapshot records an archive on the selected PVC. The snapshot reconciler verifies controller ownership and executes a bounded export in the running Pod through the Kubernetes API. The application image supplies Python 3.12+. The helper archives files, flushes and closes the object, then reads it back and compares SHA-256. The controller publishes the claim name, archive name, digest, and source Pod UID in one status update. A unique archive per attempt permits retries and prevents an interrupted write from replacing a published checkpoint.

The BatchSandbox controller releases the source Pod after the snapshot succeeds. A failed export produces a retryable pause failure while the source remains available. On resume the controller keeps the application image and adds an init container with the same image and security context. The init container verifies the digest and extracts the workspace into an emptyDir. The application mounts the restored directory before its entrypoint starts. Subsequent pauses use the same workflow.

## Operational constraints

A dedicated claim or bucket prefix bounds each sandbox's storage access. The workload has write access to its own checkpoint store. Cluster administrators configure CSI credentials, writability, durable close, retention and cross-node access. Applications must quiesce workspace writers before pause. Running processes, container packages and nested mounts are outside the workspace contract. An export occupies a snapshot reconciliation worker for at most the configured commit timeout. A failed restore keeps the application from starting and leaves the archive available for diagnosis.

## Alternatives and compatibility

A workspace mounted directly on an existing PVC is useful for continuously durable files, but cannot capture an existing container directory at pause time. A separate helper with cloud SDKs could use object storage directly and remove the Python image prerequisite at the cost of another image and a credential distribution path. This proposal uses the current BatchSandbox lifecycle and the volume support from OSEP-0003. Existing rootfs and QEMU users continue to select their existing behavior by omitting the opt-in field. Public snapshot creation remains independent of workspace pause.

## Test plan

Verify two pause/resume cycles, distinct source and restored Pods, retained workspace contents and the original base image. Exercise upload failure and checksum mismatch. Run the Kubernetes pause/resume E2E suite with a dedicated bucket-backed claim in its test namespace to validate the CSI driver.
