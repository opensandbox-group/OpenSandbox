---
title: Kubernetes Resume Benchmark
description: Reproduce end-to-end rootfs resume latency measurements for the standard Kubernetes BatchSandbox backend.
---

# Kubernetes Resume Benchmark

The opt-in [runner](https://github.com/opensandbox-group/OpenSandbox/blob/main/tests/python/benchmarks/kubernetes_resume.py) measures
standard Kubernetes **standalone, rootfs** pause/resume. It creates one sandbox
per trial, writes a unique marker outside volume mounts, pauses it, waits for
its Pods to disappear, resumes it, and checks the marker through a foreground
command. It deletes the sandbox after each trial. QEMU VMState, pooled creation,
and Fast Sandbox are outside this baseline.

## Prerequisites

- A lifecycle server configured for the Kubernetes BatchSandbox provider, with
  the controller and OCI snapshot registry configured as in [Pause and Resume](/guides/pause-resume).
- `kubectl` connected to the same cluster, with permission to read BatchSandbox,
  SandboxSnapshots, Pods, nodes, and cluster version in the sandbox namespace. Pass an explicit
  context to avoid measuring against a different cluster.
- An immutable image digest containing `/bin/sh`, `cat`, and `sleep`. The server
  must inject execd. Do not put the marker directory under a volume mount.
- A runner with network access to the lifecycle server and sandbox endpoints;
  `--server-proxy` measures the server proxy route instead of direct access.

This is an explicit benchmark, not part of the default E2E suite. Use a dedicated
test cluster. No node caches are evicted by the runner.

## Run

Create an environment file containing the versions and conditions needed to
repeat the experiment. For example, `environment.json`:

```json
{
  "server_revision": "<git SHA or image digest>",
  "controller_image": "<image digest>",
  "execd_image": "<image digest>",
  "image_committer_image": "<image digest>",
  "runtime_class": "runc (default)",
  "node_resources": "<CPU, RAM, disk type>",
  "registry": "<implementation, placement, network conditions>",
  "snapshot_configuration": "<non-secret controller flags>",
  "cache_evidence": "Uncontrolled; no cache intervention",
  "background_load": "<other workloads and concurrency>"
}
```

Do not include credentials. The output also records the runner revision, Python
and SDK versions, Kubernetes version, node runtime versions, context, image,
resource limits, proxy mode, timeouts, and polling interval.

```bash
cd tests/python
uv sync --frozen
export OPEN_SANDBOX_DOMAIN=localhost:8080
export OPEN_SANDBOX_API_KEY='<your API key>'
uv run python benchmarks/kubernetes_resume.py \
  --image '<image>@sha256:<digest>' \
  --namespace default --context '<test-context>' \
  --environment environment.json --scenario uncontrolled \
  --warmups 3 --samples 30 --output resume-uncontrolled.jsonl
```

Use `--protocol https` for a TLS server and `--server-proxy` when direct endpoints
are inaccessible. Run `--help` for resource, polling, and timeout options. The
script disables SDK retries and telemetry for this experiment. Polling remains
explicit and includes its delay in the measured observation time.

## Measurement boundaries

All durations use the runner's monotonic clock and start immediately before
`SandboxManager.resume_sandbox`. The resume portion has one shared timeout:

| Field | End of interval |
| --- | --- |
| `api_ack_ms` | Resume API returns; acceptance is not readiness |
| `running_ms` | First lifecycle GET reporting `Running` |
| `ready_ms` | A fresh `Sandbox.connect` resolves endpoints and passes execd health checks |
| `command_ms` | A foreground command returns successfully and prints the exact persisted marker |

These are cumulative times, not additive stages. `command_ms` includes an execd
round trip and is the primary user-visible result. It does not measure an
application-specific Agent initialization; add that workload in a separate
experiment. Lifecycle observation and SDK readiness are sequential, so the
polling interval and HTTP overhead affect results. No attempt is made to infer
scheduler or image-pull durations from these client timings.

Creation, marker writing, pause, Pod deletion, optional cache preparation, and
evidence collection, and cleanup are outside the resume interval. Waiting for source Pods to disappear
avoids declaring a terminating source instance to be the restored instance.
The runner verifies the created ID exists as a standalone rootfs BatchSandbox
in the selected namespace before pausing it.

## Cache conditions

`--scenario` is an experiment label, not automatic proof of cache state.
Choose `uncontrolled`, `snapshot-cache-hit`, or `snapshot-cache-miss`. The latter
two require `--prepare-script` pointing to a Python preparation/verification
script you supply. It runs after pause and Pod deletion, before the timer, as:

```text
python prepare.py <sandbox-id> <namespace> <context> <scenario>
```

The script must exit zero only after establishing and checking the declared
condition. It can inspect the sandbox's owned SandboxSnapshot CR to find the
committed image digest. Its stdout is retained as `preparation_evidence` in
that trial; keep it concise and free of secrets. There is a bounded preparation
timeout. A failure prevents resume and is retained as a failed trial.

For a cache-hit run, pre-pull the **committed snapshot image**, not just the base
image, onto every eligible node and verify it is present. For a cache-miss run,
use fresh isolated workers or a controlled cache setup and verify the snapshot
is absent before resume. Node pinning, image pull policy, registry caching, and
layer sharing affect the result; record them in the environment file. Every
trial creates a new snapshot, so warmup trials alone do not establish snapshot
cache hits. Do not mix scenarios, resource profiles, or proxy modes when
comparing percentiles. The runner does not validate a preparation script's
claims; inspect its evidence and the retained restored Pod node/image information.
Collect kubelet events separately if you need image-pull or scheduling attribution.

## Results and interpretation

The new output path is opened exclusively to avoid overwriting previous results.
JSONL contains an environment record, a flushed record for every completed
trial (including excluded warmups), and a summary. Trials include their sandbox
ID, stage of failure, completed timings, snapshot artifacts, restored Pod
node/image information, and cleanup errors. Failed cleanup
stops the run and prints the ID for manual deletion; creation response failures
also stop and retain a `benchmark_run` metadata value to locate an ambiguously
created sandbox; it never silently moves on
to accumulating leaked sandboxes.

P50 and P95 use nearest-rank percentiles over **successful measured trials**.
The summary separately reports measured failure rate and cleanup failures. A
failure at a later stage does not enter partial timings into the percentile
population. Warmups never enter the measured statistics. An all-failed run has
null percentiles. A nonzero exit status indicates a trial or cleanup failure,
including warmups. Interruptions leave the flushed records on disk; only a
final summary with `completed_requested_trials: true` indicates a completed run.

Thirty samples are a starting point, not a stable tail-latency claim. Increase
sample count, repeat independent runs, retain raw samples, and report successes,
timeouts, failures, and environment alongside percentiles. Compare baseline and
candidate revisions under the same conditions without machine-speed thresholds
in CI. This guide provides a harness; it contains no measured performance claim.
