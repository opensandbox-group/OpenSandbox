---
title: Pod Provision Failure Recovery
description: How the Kubernetes controller automatically replaces BatchSandbox pods stuck in provisioning failures (such as image pull) during initial startup.
---

# Pod Provision Failure Recovery

When a BatchSandbox pod lands on an unhealthy node, provisioning can stall before the pod ever becomes Ready. The most common class is a main container image pull that fails persistently — for example `ImagePullBackOff` caused by a full node disk:

```text
failed to prepare extraction snapshot ...: no space left on device
```

The sandbox stays `Pending` forever: kubelet keeps retrying on the same broken node, and nothing recovers the sandbox. The controller replaces such pods automatically during initial startup, so the scheduler gets a chance to place the recreated pod on a healthy node.

## Behavior

The controller watches sandboxes that have **never been Ready** (`Phase` is `Pending` or empty). A pod is considered stuck when it stays in one of the tracked failure conditions longer than the threshold. Today's tracked conditions:

- **Image pull failures** — `ImagePullBackOff` / `ErrImagePull`, typically caused by a broken node (for example a full disk).
- **Kubelet admission rejections** — the scheduler placed the pod, but kubelet's local resource ledger disagreed (insufficient CPU, memory, or ephemeral storage; node cordoned or not ready; node-pressure eviction). These pods land in `Failed` with reasons like `OutOfcpu`, `OutOfmemory`, `OutOfephemeral-storage`, `Evicted`, `NodeNotSchedulable`, `KubeletNotReady`, or `UnexpectedAdmissionError`, and Kubernetes never retries them on its own.

For a stuck pod:

1. The controller deletes it. The sandbox recreates the pod from its template and the pod is rescheduled like any new pod.
2. Replacement attempts are bounded: after `pod-recovery-max-attempts` replacements, the controller stops and records a `PodRecoveryLimitReached` warning event. The sandbox stays `Pending` and needs operator attention.
3. Updating the sandbox template (for example, fixing the image) resets the replacement attempts.

::: tip Intentionally excluded
`CreateContainerConfigError` is a spec problem that rescheduling cannot fix. Terminal failures (`PodFailed`, `ContainerFailed`, `InitContainerFailed`) keep their existing freeze semantics. `CrashLoopBackOff` is not covered: moving a crashing pod rarely helps and can mask application bugs. Scheduling failures (`Unschedulable`) are left to the kube-scheduler, which retries them automatically. Pool-mode sandboxes have a different lifecycle and are out of scope.
:::

### Broken images

An image that cannot be pulled **from any node** (missing tag or manifest, repository does not exist, or denied access) fails identically after every replacement. The controller recognizes these cases and does **not** waste replacement attempts: the pod stays, and an `ImagePullPermanentFailure` warning event names the image error. Fix the image or credentials; once fixed, the pod recovers on its next pull attempt without a restart.

::: warning Interplay with upper-layer Pending watchdogs
If an upper-layer manager tears down sandboxes stuck in `Pending` after its own timeout (for example ~7.5 minutes), note that with the default settings (`1m` threshold, 3 attempts) the controller finishes its recovery attempts within roughly 4 minutes — before such a watchdog fires. Raising `pod-recovery-stuck-threshold` above about a third of the watchdog timeout means replacement attempts may still be in flight when the sandbox gets torn down.
:::

::: warning Large images and cold pulls
A low threshold replaces pods whose image pull simply takes longer than the threshold. While kubelet is actively pulling, the container shows `ContainerCreating` (not counted); only explicit pull failures advance the timer. If your clusters pull very large images from slow registries, raise `pod-recovery-stuck-threshold`.
:::

## Configuration

Feature configuration lives in the `feature-flags` ConfigMap in the controller's namespace (plain `data` key-value entries). Changes are hot-reloaded without restarting the controller:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: feature-flags
  namespace: opensandbox-system  # the controller's namespace
data:
  # How long a pod must stay stuck in a tracked failure condition during
  # initial startup before it is replaced. Defaults to 1m.
  pod-recovery-stuck-threshold: "1m"
  # Maximum number of stuck-pod replacements per BatchSandbox template.
  # Defaults to 3.
  pod-recovery-max-attempts: "3"
```

Missing or invalid entries fall back to the built-in defaults (`1m` / `3`), and deleting the ConfigMap restores the defaults.

Helm (controller chart) renders this ConfigMap from values:

```yaml
controller:
  podRecovery:
    stuckThreshold: "1m"
    maxAttempts: 3
```

## Monitoring

Events recorded on the BatchSandbox:

| Event | Type | Meaning |
| --- | --- | --- |
| `ReplacedStuckPod` | Normal | A stuck pod was deleted and will be recreated on another node. |
| `PodRecoveryLimitReached` | Warning | Replacement attempts are exhausted; the sandbox stays `Pending` and needs operator attention. |
| `ImagePullPermanentFailure` | Warning | The image itself cannot be pulled (missing manifest/repository or denied access); fix the image or credentials. |

Prometheus metrics on the controller metrics endpoint:

| Metric | Labels | Description |
| --- | --- | --- |
| `opensandbox_pod_recovery_replacements_total` | `condition` | Stuck pods replaced during initial startup. |
| `opensandbox_pod_recovery_skips_total` | `condition`, `reason` | Stuck pods detected but not replaced; `reason` is `permanent_failure` (broken image) or `budget_exhausted` (operator attention needed). |

To find affected sandboxes:

```bash
kubectl get events --field-selector reason=ReplacedStuckPod,reason=PodRecoveryLimitReached,reason=ImagePullPermanentFailure
```
