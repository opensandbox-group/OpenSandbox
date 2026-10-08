---
title: Node Agent configuration
description: Deploy and configure the OpenSandbox Node Agent — enable the DaemonSet, choose a file or OSS sink, opt into syscall collection, and operate it safely.
---

# Node Agent configuration

The [Node Agent](/architecture/data-plane/node-agent) is an optional DaemonSet that collects sandbox stdout/stderr (and, optionally, syscalls) on every Linux node and writes them to durable storage. This guide covers enabling it, choosing a sink, and the operational rules that protect its delivery guarantees. For the full values reference, see the [node-agent chart README](https://github.com/opensandbox-group/OpenSandbox/tree/main/manifests/charts/node-agent).

## Prerequisites

- A Kubernetes cluster from the [OpenSandbox deployment](/deployment/) — the Agent is order-independent, so you can enable it at any time.
- One Agent per Linux node is automatic: the chart pins `kubernetes.io/os: linux`.
- Each node needs local disk for checkpoint state (`hostPaths.state`, default `/var/lib/opensandbox/nodeagent`), which must survive Pod restarts — it is a `hostPath` volume.

The Agent discovers sandbox Pods by identity (`opensandbox.io/id` label, `sandbox` container) and skips pooled warm Pods. No per-Pod configuration is needed.

## Enable the Agent

**Umbrella chart** — set the sub-chart condition:

```bash
helm upgrade opensandbox opensandbox \
  --namespace opensandbox-system \
  --set opensandbox-node-agent.enabled=true
```

**Standalone chart** — install it directly:

```bash
helm install node-agent manifests/charts/node-agent \
  --namespace opensandbox-system \
  --create-namespace
```

Verify the DaemonSet rolled out and the Agent reports ready. Readiness is meaningful: it only turns green when configuration, recovery state, sink target, and host reserves are all valid.

```bash
kubectl rollout status daemonset -l app.kubernetes.io/component=node-agent -n opensandbox-system

kubectl port-forward -n opensandbox-system daemonset/node-agent 8080:8080
curl http://127.0.0.1:8080/readyz   # {"ready":true,"reasons":[]}
```

::: tip Always set a real cluster ID
`config.clusterID` (default `dev-cluster`) names your cluster in every record and object path, and is part of the Agent's target identity. Set it once, before go-live — changing it later is a target change and requires draining (see [Changing sink or cluster ID](#changing-sink-or-cluster-id)).
:::

## Choose a sink

Exactly one sink target per Agent. Records land under
`<keyPrefix>/<cluster>/<namespace>/<sandbox_id>/<pod_uid>/` as numbered generations plus a `sandbox.finalized.<revision>.json` completeness marker per stream.

The examples below use the umbrella-chart key prefix (`opensandbox-node-agent.`); if you installed the standalone chart, drop that prefix.

### Local files (default)

```yaml
# values-node-agent.yaml
opensandbox-node-agent:
  enabled: true
  config:
    clusterID: prod-cluster
  sink:
    type: file
    file:
      path: /var/lib/opensandbox/nodeagent-data
      maxBytes: 1073741824      # rotate a generation at 1 GiB
      maxFiles: 16              # per stream
      maxTotalBytes: 10737418240
      retention: 24h            # cleanup after the marker's repair window
```

Keep `sink.file.path` and `hostPaths.fileData` in sync — the chart uses them for the mount and the write target. Retention-driven cleanup runs inside the Agent and only ever removes whole, already-finalized families.

The file sink is for single-node debugging and small clusters. For anything durable across node loss, use OSS.

### Alibaba Cloud OSS

Create the credentials first. Collection credentials must be able to append, read, and write — **never delete**. Deletion happens through a separate offline tool (below), with its own credentials.

```bash
kubectl create secret generic nodeagent-oss-credentials \
  --namespace opensandbox-system \
  --from-literal=access-key-id=... \
  --from-literal=access-key-secret=...        # optional: --from-literal=session-token=...
```

```yaml
opensandbox-node-agent:
  enabled: true
  config:
    clusterID: prod-cluster
  sink:
    type: oss
    oss:
      endpoint: https://oss-cn-zhangjiakou.aliyuncs.com   # HTTPS origin required
      bucket: my-sandbox-logs
      keyPrefix: logs
      existingSecret: nodeagent-oss-credentials
```

The bucket must satisfy the Agent's integrity assumptions — startup fails permanently otherwise:

- versioning disabled, no WORM retention policy
- no lifecycle rule whose prefix overlaps `<keyPrefix>/<clusterID>/` (lifecycle deletion would corrupt the marker guarantees)

Objects are written with appendable writes and sealed into immutable generations; markers are written with overwrite forbidden, so consumers can verify them by size and checksum. Because collection credentials cannot delete, cleanup is an explicit, marker-aware offline operation — run [`nodeagent-oss-cleanup`](https://github.com/opensandbox-group/OpenSandbox/tree/main/components/nodeagent/cmd/oss-cleanup) from an operator machine with delete-capable credentials. It plans before it deletes and requires an explicit target-drain confirmation.

## Opt into syscall collection

The default source set is `container-logs` only. Adding `syscalls` enables an embedded eBPF program that records one event per syscall made by each sandbox:

```yaml
opensandbox-node-agent:
  config:
    sources:
      - container-logs
      - syscalls
```

The chart handles the Kubernetes plumbing automatically when `syscalls` is present: it mounts the host cgroup v2 hierarchy and tracefs read-only and adds the `BPF` and `PERFMON` capabilities. The nodes themselves must provide:

- Linux kernel 5.11 or newer
- cgroup v2 unified hierarchy
- tracefs mounted at `/sys/kernel/tracing`

Syscall records land in their own `<cluster>/_streams/syscall/…` tree and carry their own markers. Expect the syscall marker to be `incomplete` by design in some cases — for example, the tracer attaches only after Kubernetes reports the container ID, so the interval before attachment is reported as a gap rather than claimed as covered.

## Key values

| Value | Default | What it controls |
|---|---|---|
| `config.sources` | `["container-logs"]` | Enabled sources; the image must contain every named source |
| `config.clusterID` | `dev-cluster` | Cluster name in records, paths, and target identity |
| `config.memoryBudgetBytes` | 256 MiB | Global queue budget for buffered records |
| `config.perSandboxQueueBytes` | 16 MiB | Per-sandbox queue budget before backpressure |
| `config.perSandboxRateLimit` | `0` (off) | Per-sandbox records rate limit |
| `config.dropPolicy` | `block` | `block` applies backpressure; `drop` discards and accounts for it in markers |
| `config.sinkTimeout` / `config.retryMaxInterval` | 30s / 30s | Sink write timeout and retry backoff cap |
| `config.stateDir` + `hostPaths.state` | `/var/lib/opensandbox/nodeagent` | Checkpoint state location (keep the two in sync) |
| `config.maxLineBytes` | 1 MiB | Longest log line; longer lines are dropped and accounted |
| `config.endedStateRetention` | 24h | How long a finished stream can still accept late data before its state is pruned |
| `resources` | 50m/128Mi – 1/512Mi | DaemonSet resources (experimental defaults — benchmark before production sizing) |

The defaults follow the safe posture: `block` backpressure means a slow sink slows collection instead of losing data. Only switch to `drop` if you understand that dropped records are gone — they are reported in the stream marker, but not recoverable.

## Metrics

The Agent exports OTLP metrics (`opensandbox.nodeagent.*`: records, bytes, drops, retries, queue depth, sink latency) with no sandbox or Pod identifiers as labels. It requires an explicit endpoint — the node-IP fallback used by other components is disabled:

```yaml
opensandbox-node-agent:
  extraEnv:
    - name: OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
      value: http://otel-collector.observability:4318/v1/metrics
```

See [component telemetry](/guides/component-telemetry) for shared exporter behavior.

## Operating rules

These protect the delivery guarantee — violating them turns streams `incomplete` or leaves the Agent not-ready (which is the intended, visible outcome):

- **Never remove or rotate the state directory.** It holds checkpoints and finalize intents, not payloads. Deleting it mid-stream forfeits recovery.
- **Do not rotate or delete the file sink's output out-of-band.** The Agent owns its layout and cleanup.
- **Readiness is diagnostic.** `kubectl get daemonset` + `curl /readyz` gives named reasons (`invalid-config`, `state-unavailable`, `operation-retrying`, `store-stale`, …). An unready Agent never silently drops data.
- **Expect a restart to be honest, not lossless.** After an Agent restart, affected markers report `incomplete` — the guarantee is at-least-once within the provable coverage window, and markers never claim more.

### Changing sink or cluster ID

The target identity is a digest of the sink configuration plus cluster ID. If you change `sink.*` or `config.clusterID`, the Agent refuses to reuse old state — by design, so it cannot append one cluster's records onto another target. To change targets safely:

1. Stop collection (scale the DaemonSet to zero) and let streams finalize.
2. Drain the old target (for OSS, complete the offline cleanup/drain confirmation).
3. Update values and redeploy.
