---
title: GPU Sandbox Health Checks
description: A layered health-check convention for GPU-backed sandboxes — attribute "the GPU is not working" to the platform, device, or compute layer instead of guessing.
---

# GPU Sandbox Health Checks

A GPU sandbox can report `Running` while no GPU work is possible: the container is up, the guest agent responds, and the device stack is mounted, yet a kernel launch still fails. The sandbox lifecycle status describes the *platform*, not the *GPU*. This guide defines a layered health-check convention for GPU-backed sandboxes, plus a reference probe implementation ([example](/examples/gpu-health-probe)) you can run inside any sandbox.

## The three layers

Check in layers, cheapest first. Each layer answers a different question, and the layer that fails tells you where to look.

| Layer | Question | Signals | Fails when |
|-------|----------|---------|------------|
| **Platform** | Can the sandbox be reached at all? | Sandbox lifecycle status; `exec`/execd availability | The sandbox is not running, the guest agent is down, or the workload never started |
| **Device** | Is the GPU attached and visible? | `/dev/nvidiactl`, `/dev/nvidia-uvm`, `/dev/nvidiaN` nodes, driver version, `nvidia-smi -L`, observed count vs. requested count | The GPU was not attached (request, runtime, or node problem), the driver/toolkit mount is incomplete, or the attached count drifted from the requested count |
| **Compute** | Does an actual kernel launch work? | A short workload through the real stack on every visible device (e.g. allocate a tensor, matmul, synchronize) | Driver/library/toolkit version mismatch, unusable UVM, or a framework built for a different device stack |

The middle layer catches most real drift. Process-up checks (layer 1) and device-visible checks (layer 2) both pass for the most confusing failure family — a driver/library mismatch — because the nodes exist and the driver answers queries. Only a compute probe closes that gap, and it must exercise every visible device: a sandbox where only the first GPU works is not a healthy multi-GPU sandbox.

## The exec-probe convention

The convention is deliberately minimal: the probe is a program that runs **inside** the sandbox and is invoked through the normal `exec` channel. No server-side hook is required, it works on every runtime, and the same artifact runs from an SDK, an orchestrator, a lifecycle hook, or a debugging shell.

The probe contract:

- **stdout is exactly one JSON document.** Layer results, observed values, and warnings. Machine-readable by construction.
- **stderr carries a human summary.** One line naming the failing layer and check.
- **Exit codes are the stable alerting signal:**

| Exit code | Meaning | Action |
|-----------|---------|--------|
| `0` | All executed layers passed | — |
| `2` | Device layer failed | GPU not attached/visible, or requested-vs-observed count drift |
| `3` | Compute layer failed | Device visible, kernel launch broken — driver/library mismatch class |
| `4` | Probe error | Fix the probe invocation, not the sandbox |

A probe that never starts at all is itself a platform-layer failure — the caller observes layer 1 by the simple fact of whether `exec` succeeded.

The reference implementation is [`examples/gpu-health-probe/gpu_probe.py`](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/gpu-health-probe). It uses only the Python standard library, so any image that can execute `python3` can run it.

## Running the probe

Upload the probe into the sandbox and execute it with the GPU count that was requested at creation time:

```python
from datetime import timedelta
from pathlib import Path

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models import WriteEntry

sandbox = await Sandbox.create(
    "your-gpu-image",
    resource={"cpu": "2", "memory": "4Gi", "gpu": "1"},
    connection_config=ConnectionConfig(
        domain="your-server:8080",
        api_key="your-api-key",
        request_timeout=timedelta(minutes=10),
    ),
)

probe_dir = "/tmp/gpu-health-probe"
probe_source = Path("examples/gpu-health-probe/gpu_probe.py").read_text()
await sandbox.files.create_directories([WriteEntry(path=probe_dir)])
await sandbox.files.write_file(f"{probe_dir}/gpu_probe.py", probe_source, mode=755)

await sandbox.commands.run(
    f"python3 {probe_dir}/gpu_probe.py --expect 1 --compute auto"
)
```

The file API takes octal permission *digits* as a decimal integer, so `mode=755` means `0o755`.

- `--expect N` enables the requested-vs-observed count check (see below).
- `--compute auto` runs a per-device PyTorch smoke test when PyTorch is importable; otherwise layer 3 is reported as `skipped`. Pass `--compute '<command>'` to run a workload-native check instead, or `--compute skip` when only layers 1-2 are meaningful for the image.

The full output contract and options are documented in the [example page](/examples/gpu-health-probe).

To turn this into continuous health monitoring, run the probe periodically (or on attach) from your orchestrator and alert on exit codes `2` and `3`. Treat a probe that fails to start as a platform-layer alarm, not a GPU alarm.

## Failure attribution

| Symptom | Layer | Typical cause | Next step |
|---------|-------|---------------|-----------|
| Probe cannot be started; sandbox not reachable | Platform | Sandbox failed to start, guest agent down | Inspect sandbox status and runtime events |
| `control-node` / `device-nodes` failed | Device | No GPU attached to this sandbox | Verify `resourceLimits.gpu`, the runtime GPU configuration, and that the sandbox is scheduled on a GPU node |
| `requested-count` failed (`requested N, observed M`) | Device | Partial attach, or the node inventory changed after creation | Re-verify the pool's real card counts; do not account the sandbox as N GPUs |
| `uvm-node` warning | Device | `NVIDIA_DRIVER_CAPABILITIES` excludes `compute` | Include `compute` (or `all`) in the driver capabilities |
| `smi-inventory-consistency` warning | Device | Device nodes and `nvidia-smi` disagree | Toolkit/driver inventory mismatch or partial attach; inspect the runtime configuration |
| Device passes, `compute-probe` or `compute-count` fails | Compute | Driver/library/toolkit mismatch; a secondary GPU is attached but unusable through the framework | Align the image's CUDA/framework versions with the host driver; check per-device health |

## Requested vs. observed GPU count

The platform records what was *requested* (`resourceLimits.gpu`); nothing records or verifies what was *actually attached*. In practice the two drift: partial attaches, node inventory changes, and reporting lag all produce sandboxes whose observed count differs from the request, which silently corrupts per-sandbox accounting and pool capacity planning.

The device layer closes this by comparing the requested count (`--expect N`) against the observed `/dev/nvidiaN` nodes:

```
requested 2 GPU(s) but observed 1 device node(s): requested vs attached drift
```

The compute layer cross-checks the framework's visible device count against the same expectation, so a sandbox where PyTorch sees fewer GPUs than were requested fails instead of being certified by a working device 0.

Two caveats:

- The `resourceLimits.gpu=all` sentinel is Docker-runtime only — the Kubernetes runtime rejects it and requires a positive integer. It also cannot be verified inside the sandbox — the count it resolves to depends on the host inventory — so `--expect all` records the sentinel but skips the comparison.
- Under MIG, `resourceLimits.gpu` counts MIG instances while `/dev/nvidiaN` nodes count parent cards. When MIG instances are visible, the probe compares `--expect` against the MIG instance count (and still reports the physical device count separately), so two slices on one card satisfy `--expect 2` instead of failing as attach drift.

## Runtime notes

- **Docker with nvidia-container-toolkit** and **Kubernetes with a GPU device plugin**: the device layer sees the same nodes (`/dev/nvidiaN`, control and UVM nodes) that the runtime injects; no special handling is needed.
- **gVisor**: GPU support requires nvproxy (`--nvproxy=true`); the device stack is then visible to the sandbox and the probe runs unchanged. See [Secure container runtimes](/guides/secure-container) for the runtime configuration.
- **Kata**: GPUs are passed through to the guest kernel; the probe runs unchanged.

## Limitations

- The probe observes the *sandbox-side* view. It cannot attest host-level device health beyond what the driver exposes, and it does not replace node-level GPU monitoring (ECC errors, XID events, thermal state).
- The output is a point-in-time check. Run it periodically if you need continuous assurance, and keep the exit codes stable in your alerting rules.
- Layer 3 quality depends on the probe command you choose: a framework smoke test is stronger than a driver query, and a workload-native self-test is stronger still.
