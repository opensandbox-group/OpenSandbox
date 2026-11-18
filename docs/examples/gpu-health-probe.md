---
title: GPU Health Probe
description: Run a layered GPU health probe inside a sandbox and attribute "the GPU is not working" to the platform, device, or compute layer.
---

# GPU Health Probe

A GPU sandbox can report `Running` while no GPU work is possible. This example runs a small, standard-library-only probe **inside** the sandbox and reports which layer is healthy: platform, device, or compute. It is the reference implementation of the [GPU sandbox health checks](/guides/gpu-health) convention.

The complete source, including unit tests, lives in [`examples/gpu-health-probe`](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/gpu-health-probe).

## What it checks

| Layer | Checks |
|-------|--------|
| platform | Reaching the sandbox at all (observed by the caller — if `exec` fails, this layer failed) |
| device | `/dev/nvidiactl`, `/dev/nvidia-uvm`, `/dev/nvidiaN` nodes, driver version, `nvidia-smi -L` inventory, requested vs. observed GPU count |
| compute | A short kernel launch through the full stack on every visible device — PyTorch smoke test by default, or any custom command |

## Prerequisites

- An OpenSandbox server whose runtime exposes GPUs: Docker with `nvidia-container-toolkit`, or a Kubernetes cluster with a GPU device plugin (Kubernetes requires a positive integer for `resourceLimits.gpu`; the `all` sentinel is Docker-only).
- A GPU image that includes `python3` (the probe runs inside the sandbox). The demo's default image bundles Python and PyTorch; a lighter CUDA image works for layers 1-2 but needs Python.
- Python SDK: `uv pip install opensandbox`.
- For a first run on Kubernetes, note that the image pull happens inside the create request and is bounded by the server's `sandbox_create_timeout_seconds` (60s by default): pre-pull large GPU images on the nodes or raise that setting.

## Run it

```shell
cd examples/gpu-health-probe
export SANDBOX_DOMAIN=your-server:8080   # default: localhost:8080
export SANDBOX_API_KEY=your-api-key      # if the server requires auth
export GPU_EXPECT=1                      # resourceLimits.gpu to request and verify
python main.py
```

The demo creates a sandbox with `resourceLimits.gpu`, uploads `gpu_probe.py` into it, runs the probe with `--expect` and `--compute auto`, prints the report, and maps the exit code to an operator action. `SANDBOX_IMAGE` selects the image (default: `pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime`, so all three layers run out of the box); images without a compute runtime report layer 3 as `skipped`, and the demo prints that the result covers the device layer only. Set `GPU_COMPUTE_MODE=skip` to make that intent explicit.

The probe reads only in-sandbox state, so you can also run it from any other channel (your own orchestrator, a lifecycle hook, a debugging shell) as long as `gpu_probe.py` is available inside the sandbox:

```shell
python3 /tmp/gpu-health-probe/gpu_probe.py --expect 1 --compute auto
```

## Probe options

| Option | Meaning |
|--------|---------|
| `--expect N` | Compare the observed GPU count with the count requested at creation (`resourceLimits.gpu`); `all` records the sentinel but skips the comparison |
| `--compute auto` | Run a PyTorch smoke test when PyTorch is importable, otherwise report layer 3 as `skipped` |
| `--compute skip` | Disable layer 3 |
| `--compute '<command>'` | Run a workload-native compute check; exit code `0` means pass |
| `--compute-timeout SECONDS` | Overall timeout for the compute-layer probe, including the PyTorch import in auto mode (default: 60) |
| `--no-smi` | Skip the `nvidia-smi` inventory check (device nodes remain authoritative) |
| `--quiet` | Suppress the human-readable summary on stderr |

## Output contract

stdout is exactly one JSON document; a one-line human summary goes to stderr. The exit code is the alerting signal:

| Exit code | Meaning |
|-----------|---------|
| `0` | All executed layers passed (a `skipped` compute layer does not fail the probe) |
| `2` | Device layer failed — GPU not attached/visible, or requested-vs-observed count drift |
| `3` | Compute layer failed — device visible, kernel launch broken |
| `4` | Probe error |

```json
{
  "probe": "gpu-health-probe",
  "version": "1.0",
  "status": "pass",
  "exitCode": 0,
  "expectedCount": 1,
  "layers": {
    "device": {"status": "pass", "checks": [{"name": "requested-count", "status": "pass", "detail": "observed 1 device(s), requested 1"}]},
    "compute": {"status": "pass", "checks": [{"name": "compute-probe", "status": "pass", "detail": "PyTorch CUDA smoke test passed: {...}"}]}
  },
  "observed": {"deviceNodeCount": 1, "driverVersion": "535.104.05", "smiGpuCount": 1},
  "warnings": []
}
```

## Requested vs. observed GPU count

`resourceLimits.gpu` records what was *requested*; nothing verifies what was *actually attached*. Pass the requested count with `--expect N` and the device layer compares it against the `/dev/nvidiaN` nodes it observes:

```
requested 2 GPU(s) but observed 1 device node(s): requested vs attached drift
```

For long-lived sandboxes, running the probe periodically (or at attach time in your own orchestration) turns silent accounting drift into a visible, alertable signal. `resourceLimits.gpu=all` cannot be verified inside the sandbox — the resolved count depends on the host inventory — so `--expect all` records the sentinel but skips the comparison; the sentinel itself is Docker-runtime only. Under MIG, `resourceLimits.gpu` counts MIG instances, so when MIG instances are visible the probe compares `--expect` against the MIG instance count and reports physical devices separately.

## Tests

```shell
python -m pytest examples/gpu-health-probe/test_gpu_probe.py
```

The tests are hermetic: they model device state with temporary files and inject `nvidia-smi` output, so they run on machines without GPUs.

## From a one-off check to a health signal

- Run the probe periodically (or at attach) from your orchestrator.
- Alert on exit code `2` (device) and `3` (compute) separately — they page different teams.
- Pass `--expect N` so requested-vs-observed GPU count drift is detected instead of silently corrupting capacity accounting.
- A probe that cannot be started is a platform-layer alarm.

See [GPU sandbox health checks](/guides/gpu-health) for the full convention and the failure-attribution table.
