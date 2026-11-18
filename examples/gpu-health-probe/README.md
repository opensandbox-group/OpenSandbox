# GPU Health Probe

A layered GPU health probe for OpenSandbox sandboxes, and a runnable example of
the exec-based probe convention from
[docs/guides/gpu-health.md](../../docs/guides/gpu-health.md).

A GPU sandbox can report `Running` and still be unable to run a single kernel:
"the agent is up" is not "the GPU is usable". This probe answers *which layer*
is broken, so a "my GPU sandbox does not work" ticket becomes a one-line
diagnosis.

| Layer | What is checked | Failure usually means |
|-------|-----------------|-----------------------|
| platform | Can the sandbox be reached at all? Observed by the caller — if the probe cannot be started, the platform/guest agent layer failed | The sandbox is not running or execd is unavailable |
| device | NVIDIA control/UVM nodes, `/dev/nvidiaN` device nodes, driver version, `nvidia-smi -L` inventory, requested-vs-observed GPU count | The GPU is not attached to this sandbox (request/runtime/node problem), or the count drifted |
| compute | A short kernel launch through the full stack (PyTorch smoke test by default; any custom command) | Driver/library mismatch: the device is visible but compute does not work |

## Files

| File | Purpose |
|------|---------|
| `gpu_probe.py` | The probe itself. Standard library only; designed to run *inside* the sandbox |
| `main.py` | End-to-end demo: create a GPU sandbox with the Python SDK, upload the probe, run it, attribute the result |
| `test_gpu_probe.py` | Hermetic unit tests for the probe's parsing and layer logic |

## Prerequisites

- An OpenSandbox server whose runtime exposes GPUs: Docker with
  `nvidia-container-toolkit`, or a Kubernetes cluster with a GPU device plugin.
- Python SDK: `uv pip install opensandbox`.

## Quick start

```shell
export SANDBOX_DOMAIN=your-server:8080   # default: localhost:8080
export SANDBOX_API_KEY=your-api-key      # if the server requires auth
export GPU_EXPECT=1                      # resourceLimits.gpu to request and verify
python main.py
```

The demo creates a sandbox with `resourceLimits.gpu`, uploads `gpu_probe.py`
into it, and runs:

```shell
python3 /tmp/gpu-health-probe/gpu_probe.py --expect 1 --compute auto
```

`SANDBOX_IMAGE` selects the image (default:
`nvidia/cuda:12.4.1-base-ubuntu22.04`). That image has no compute runtime, so
layer 3 reports `skipped`; point `SANDBOX_IMAGE` at an image with PyTorch, or
set `GPU_COMPUTE_MODE=skip` to make the intent explicit.

The probe reads only in-sandbox state, so you can also run it from any other
channel (your own orchestrator, a lifecycle hook, a debugging shell) as long as
`gpu_probe.py` is available inside the sandbox.

## Output contract

stdout is exactly one JSON document; a human-readable summary goes to stderr.

```json
{
  "probe": "gpu-health-probe",
  "version": "1.0",
  "status": "pass",
  "exitCode": 0,
  "expectedCount": 1,
  "layers": {
    "device": {"status": "pass", "checks": [{"name": "requested-count", "status": "pass", "detail": "observed 1 device(s), requested 1"}]},
    "compute": {"status": "pass", "checks": [{"name": "compute-probe", "status": "pass", "detail": "PyTorch CUDA smoke test passed: {\"deviceCount\": 1, \"checksum\": -12.34}"}]}
  },
  "observed": {"deviceNodeCount": 1, "driverVersion": "535.104.05", "smiGpuCount": 1},
  "warnings": []
}
```

Exit codes are the stable contract:

| Exit code | Meaning |
|-----------|---------|
| `0` | All executed layers passed (a `skipped` compute layer does not fail the probe) |
| `2` | Device layer failed — GPU not attached/visible, or requested-vs-observed count drift |
| `3` | Compute layer failed — device visible but a kernel launch does not work |
| `4` | The probe itself failed unexpectedly |

## Requested vs. observed GPU count

`resourceLimits.gpu` records what was *requested*; nothing verifies what was
actually attached. Pass the requested count with `--expect N` and the device
layer compares it against the `/dev/nvidiaN` nodes it observes:

```
requested 2 GPU(s) but observed 1 device node(s): requested vs attached drift
```

For long-lived sandboxes, running the probe periodically (or at attach time in
your own orchestration) turns silent accounting drift into a visible, alertable
signal. `resourceLimits.gpu=all` cannot be verified inside the sandbox — the
resolved count depends on the host inventory — so `--expect all` records the
sentinel but skips the comparison.

## Compute layer

`--compute auto` runs a PyTorch smoke test when PyTorch is importable. For
images without PyTorch, pass any command that exercises the workload's real
compute path; exit code `0` means pass:

```shell
python3 gpu_probe.py --expect 1 --compute 'python3 -c "import myframework; myframework.selftest()"'
```

Use `--compute skip` when only layers 1-2 are meaningful for the image.

## Tests

```shell
python -m pytest examples/gpu-health-probe/test_gpu_probe.py
```

The tests are hermetic: they model device state with temporary files and inject
`nvidia-smi` output, so they run on machines without GPUs.
