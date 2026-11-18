---
title: GPU Health Probe
description: Run a layered GPU health probe inside a sandbox and attribute "the GPU is not working" to the platform, device, or compute layer.
---

# GPU Health Probe

A GPU sandbox can report `Running` while no GPU work is possible. This example
runs a small, standard-library-only probe **inside** the sandbox and reports
which layer is healthy: platform, device, or compute. It is the reference
implementation of the [GPU sandbox health checks](/guides/gpu-health)
convention.

The complete source, including unit tests, lives in
[`examples/gpu-health-probe`](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/gpu-health-probe).

## What it checks

| Layer | Checks |
|-------|--------|
| platform | Reaching the sandbox at all (observed by the caller — if `exec` fails, this layer failed) |
| device | `/dev/nvidiactl`, `/dev/nvidia-uvm`, `/dev/nvidiaN` nodes, driver version, `nvidia-smi -L` inventory, requested vs. observed GPU count |
| compute | A short kernel launch through the full stack — PyTorch smoke test by default, or any custom command |

## Prerequisites

- An OpenSandbox server whose runtime exposes GPUs (Docker with
  `nvidia-container-toolkit`, or a Kubernetes cluster with a GPU device plugin).
- Python SDK: `uv pip install opensandbox`.

## Run it

```shell
cd examples/gpu-health-probe
export SANDBOX_DOMAIN=your-server:8080   # default: localhost:8080
export SANDBOX_API_KEY=your-api-key      # if the server requires auth
export GPU_EXPECT=1                      # resourceLimits.gpu to request and verify
python main.py
```

The demo creates a sandbox with `resourceLimits.gpu`, uploads `gpu_probe.py`
into it, runs the probe with `--expect` and `--compute auto`, and prints the
diagnosis. `SANDBOX_IMAGE` selects the image; images without a compute runtime
(PyTorch) report layer 3 as `skipped` unless you pass `--compute '<command>'`.

## Output contract

stdout is one JSON document; a one-line human summary goes to stderr. The exit
code is the alerting signal:

| Exit code | Meaning |
|-----------|---------|
| `0` | All executed layers passed |
| `2` | Device layer failed — GPU not attached/visible, or requested-vs-observed count drift |
| `3` | Compute layer failed — device visible, kernel launch broken |
| `4` | Probe error |

```json
{
  "probe": "gpu-health-probe",
  "status": "pass",
  "exitCode": 0,
  "expectedCount": 1,
  "layers": {
    "device": {"status": "pass", "checks": []},
    "compute": {"status": "pass", "checks": []}
  },
  "observed": {"deviceNodeCount": 1, "driverVersion": "535.104.05"}
}
```

## From a one-off check to a health signal

- Run the probe periodically (or at attach) from your orchestrator.
- Alert on exit code `2` (device) and `3` (compute) separately — they page
  different teams.
- Pass `--expect N` so requested-vs-observed GPU count drift is detected
  instead of silently corrupting capacity accounting.
- A probe that cannot be started is a platform-layer alarm.

See [GPU sandbox health checks](/guides/gpu-health) for the full convention
and the failure-attribution table.
