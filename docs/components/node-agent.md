---
title: Node Agent
description: Deploy and operate the node-level OpenSandbox data collector with durable file or Alibaba Cloud OSS storage.
---

# Node Agent

Node Agent is an optional Linux DaemonSet that runs one or more Sources compiled
into its image and sends their sandbox records to one configured Sink. The
published image currently includes the `container-logs` Source, which reads
stdout and stderr from non-pooled OpenSandbox Pods on the same node, and the
`file` and Alibaba Cloud `oss` Sinks.

::: warning Experimental capacity
The recovery protocol is implemented, but production resource defaults require
the published OSEP-0019 benchmark and 24-hour soak results. Treat the chart
values as starting values until those results are available.

The default `block` policy applies backpressure to Source intake as a whole.
Per-sandbox queue limits bound memory use, but do not promise bounded latency
isolation between sandboxes.
:::

## Install

The umbrella chart disables Node Agent by default. For a durable local-file
validation:

```bash
helm install nodeagent ./kubernetes/charts/opensandbox-node-agent \
  --namespace opensandbox-system \
  --create-namespace \
  --set config.clusterID=prod-a \
  --set sink.type=file
```

With the default `container-logs` Source, the DaemonSet mounts `/var/log/pods`
read-only. Its checkpoint directory and durable-file target are separate
writable host paths. Their lifecycle must be at least as long as the kubelet
logs being collected.

`config.sources` selects the Sources compiled into the image and defaults to
`[container-logs]`. An unknown name leaves the Agent unready instead of
starting a partial pipeline. When `container-logs` is not selected, the chart
does not mount the Pod log directory or render its Source-specific settings.
Disabling or renaming a Source while its checkpoint namespace or stream-kind
bindings remain also leaves the Agent unready; drain and remove that Source's
state before changing the enabled set.

For OSS, create a Secret with `access-key-id`, `access-key-secret`, and
optionally `session-token`, then configure:

```yaml
sink:
  type: oss
  oss:
    endpoint: https://oss-cn-example.aliyuncs.com
    bucket: sandbox-logs
    keyPrefix: logs
    existingSecret: nodeagent-oss
```

Agent credentials require object append/read/write permissions and permission
to read the bucket's versioning, WORM, and lifecycle configuration. The
settings themselves remain disabled as described below. Agent credentials must
not include object deletion. The Agent refuses to start Sources when bucket
versioning or WORM is enabled, or when a lifecycle rule overlaps the managed
cluster prefix.

Restart the Node Agent DaemonSet after rotating this Secret. Kubernetes does
not update Secret-backed environment variables in running containers.

## Storage layout

Each record kind has a compile-time storage format that defines its encoding,
Content-Type, metadata, and object family. The current `container-log` format
uses the same layout in both durable targets:

```text
<cluster>/<namespace>/<sandbox_id>/<pod_uid>/
  sandbox.log
  sandbox.<generation>.log
  sandbox.finalized.<revision>.json
```

The marker is a cumulative immutable snapshot. Consumers select the highest
continuous numeric Revision and verify every object's size and CRC64. Its
`coverage_started_at` field fixes the adoption boundary shared by all
Revisions for that stream.

| Status | Meaning |
| --- | --- |
| `complete` | No intentional drop or known source gap. At-least-once delivery may still contain duplicates. |
| `complete-with-drops` | At least one record was intentionally dropped, with the affected source interval accounted for. |
| `incomplete` | At least one known source interval could not be read or its coverage could not be proved. |

An Agent restart while a stream is still open interrupts continuous
monitoring. The recovered data is still delivered, but the next marker is
`incomplete` with `monitor-interrupted`; finding the bytes after restart cannot
prove that no short-lived rotation disappeared during the outage. A Revision
that had already entered finalization keeps its frozen outcome.

## Recovery and target changes

`NODEAGENT_STATE_DIR/checkpoint.db` stores bbolt recovery metadata, not log
payloads. It binds the node to one target identity. The Agent creates the
database on first start; a damaged or target-mismatched database keeps
readiness false and is never silently reset. Deleting an existing database
discards recovery identity and can leave the replacement unable to reconcile
objects already present in the sink.

Before changing target, stop new sandbox placement on the node, wait for zero
active/reopenable streams and GC backlog, confirm tracked log directories are
gone, stop Node Agent, then archive and remove the entire state directory.
Per-stream state reset is not supported.

For the durable file sink, automatic cleanup currently applies only to
`container-log` object families. It starts after the fixed late-repair deadline,
`NODEAGENT_FILE_RETENTION`, disappearance of the tracked CRI log directory, and
successful finalization. Cleanup writes a bbolt tombstone, moves the complete
object family into `.gc`, syncs the parent directories, and then removes the
staged family. It never deletes one generation to make room. Other record
formats require format-specific cleanup support.

OSS cleanup is offline. Run `nodeagent-oss-cleanup` with separate delete
credentials and a durable `--state-file`. First run without `--apply` to persist
and inspect the manifest. Apply requires
`--confirm-target-drained=<target-id>`. The tool deletes all Revision markers
and verifies their absence before deleting data objects; interrupted work
resumes from the manifest.

## Health

- `/healthz` reports process liveness; collection failures and blocked progress
  are surfaced by `/readyz`.
- `/readyz` also requires valid configuration, synchronized Pod identity,
  writable state, no active Source or Sink retry loop, a recoverable Sink, and
  no unresolved runtime failure.

Readiness reasons use low-cardinality codes and never include credentials,
paths, bucket names, or raw backend errors.

When OTLP metrics export is configured, Node Agent reports record and byte
counts, intentional drops, retries, current queue bytes, and Sink Consume
latency. These instruments do not use sandbox IDs, Pod IDs, paths, buckets, or
StreamRefs as labels.

## Validation

`components/nodeagent/test/kind-smoke.sh` builds the Agent, deploys the chart to
Kind, verifies normal-Pod collection and Pool-Pod exclusion, restarts the
DaemonSet, and validates that the recovered stream is marked `incomplete` for
the monitoring interruption. The real-OSS suite additionally
requires a dedicated disposable base prefix and test-bucket credentials;
protocol unit tests use a deterministic fake backend for unknown AppendObject
outcomes and conflicting positions.

Run the Kind validation from the repository root. Docker Desktop or another
Docker daemon must already be running, and `docker`, `kind`, `helm`, `kubectl`,
and `jq` must be available on `PATH`.

```bash
components/nodeagent/test/kind-smoke.sh
```

The script creates an isolated `nodeagent-smoke` cluster and deletes it on
exit. Set `KEEP_KIND_CLUSTER=1` to retain a failed cluster for inspection. A
zero exit status proves the file-sink path, Pool exclusion, Agent restart
recovery, and final marker checks completed. It does not exercise OSS.

The fake OSS protocol tests need no credentials:

```bash
cd components/nodeagent
go test ./pkg/sink/oss
```

### Real OSS smoke test

Use a base prefix reserved for these tests. Each run creates a unique child
prefix, so earlier retained runs do not collide. Bucket versioning and WORM
must be disabled, and no lifecycle rule may overlap the managed prefix. The
credentials need the same append, read, write, and bucket-configuration-read
permissions documented for the Agent. They do not need `DeleteObject`.

```bash
export NODEAGENT_TEST_OSS_ENDPOINT=https://oss-cn-example.aliyuncs.com
export NODEAGENT_TEST_OSS_BUCKET=sandbox-logs-test
export NODEAGENT_TEST_OSS_PREFIX=opensandbox/nodeagent-smoke
export OSS_ACCESS_KEY_ID=...
export OSS_ACCESS_KEY_SECRET=...
# Optional for temporary credentials:
export OSS_SESSION_TOKEN=...

cd components/nodeagent
go test -tags=integration -run '^TestRealOSSSmoke$' -v ./pkg/sink/oss
```

The test appends one canonical log record, publishes Revision 1, reads both
objects back, and verifies the marker size and CRC64 against OSS metadata. It
intentionally retains the objects because the Agent test credentials have no
delete permission. The successful test output prints the `target-id`,
`family-prefix`, and `container` needed for cleanup.

With separate delete credentials in the same `OSS_ACCESS_KEY_*` environment
variables, first persist and inspect a cleanup plan:

```bash
cd components/nodeagent
make build

./bin/nodeagent-oss-cleanup \
  --endpoint "$NODEAGENT_TEST_OSS_ENDPOINT" \
  --bucket "$NODEAGENT_TEST_OSS_BUCKET" \
  --family-prefix '<printed-family-prefix>' \
  --container '<printed-container>' \
  --target-id '<printed-target-id>' \
  --state-file "$PWD/nodeagent-cleanup.db"
```

After draining the target and reviewing the plan, repeat the command with
`--apply --confirm-target-drained='<printed-target-id>'`. The durable state file
allows an interrupted cleanup to resume. Published Node Agent images also
contain this binary at `/usr/local/bin/nodeagent-oss-cleanup`.

If an interrupted, unfinished cleanup reports a data object that was not in
the persisted plan, verify that the target is still fully drained. Then rerun
the command with `--extend-data-plan` and the same drain confirmation, but
without `--apply`. This persists and prints the expanded plan without deleting
data. Review every key, then run the separate `--apply` step. Completed cleanup
tasks are terminal and cannot be reopened.

Without OSS credentials, the integration test can still be compiled with
`go test -tags=integration -run '^$' ./pkg/sink/oss`. This checks build
compatibility only; it is not an OSS connectivity or protocol smoke-test pass.
