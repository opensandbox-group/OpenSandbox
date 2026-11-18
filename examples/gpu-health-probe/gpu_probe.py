#!/usr/bin/env python3
# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Layered GPU health probe for OpenSandbox sandboxes.

This is the reference implementation of the exec-based GPU probe convention
described in ``docs/guides/gpu-health.md``. The probe runs *inside* the
sandbox and uses only the Python standard library, so it works in any image
that can execute ``python3``.

Layers
------
platform
    Observed by the caller, not by this probe: if the probe cannot be started
    at all, the platform layer (sandbox lifecycle / guest agent) has failed.
device
    The NVIDIA stack is visible in the sandbox: control and UVM device nodes,
    per-GPU device nodes, the driver version, ``nvidia-smi -L`` inventory, and
    (optionally) the observed GPU count against the requested count.
compute
    An actual short kernel launch through the full stack. ``auto`` mode uses
    a PyTorch smoke test when PyTorch is importable; otherwise pass an
    explicit command with ``--compute '<command>'``.

Output contract
---------------
stdout carries exactly one JSON document describing every layer. A
human-readable one-line summary goes to stderr (suppress with ``--quiet``).

Exit codes
----------
0  all executed layers passed (a skipped compute layer does not fail)
2  device layer failed
3  compute layer failed
4  the probe itself failed (unexpected internal error)
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
from dataclasses import dataclass
from typing import Any, Callable, Dict, List, Optional, Tuple

PROBE_NAME = "gpu-health-probe"
PROBE_VERSION = "1.0"

PASS = "pass"
FAIL = "fail"
WARN = "warn"
SKIPPED = "skipped"

EXIT_OK = 0
EXIT_DEVICE_FAILED = 2
EXIT_COMPUTE_FAILED = 3
EXIT_PROBE_ERROR = 4

#: ``/dev/nvidiaN`` device nodes are the kernel-visible attachment units.
_DEVICE_NODE_RE = re.compile(r"^nvidia(\d+)$")
#: ``nvidia-smi -L`` physical GPU line:
#: ``GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-0123...)``
_SMI_GPU_RE = re.compile(r"^GPU (\d+): (.+?) \(UUID: ([^)]+)\)\s*$")
#: ``nvidia-smi -L`` MIG line:
#: ``  MIG 1g.5gb Device 0: (UUID: MIG-GPU-0123.../1/0)``
_SMI_MIG_RE = re.compile(r"^\s+MIG (\S+) Device (\d+): \(UUID: ([^)]+)\)\s*$")
#: ``/proc/driver/nvidia/version`` NVRM line, e.g.
#: ``NVRM version: NVIDIA UNIX x86_64 Kernel Module  535.104.05  ...``
_NVRM_RE = re.compile(r"NVRM version:.*?(\d+\.\d+(?:\.\d+)?)", re.DOTALL)

#: Short kernel launch used by ``--compute auto`` when PyTorch is available.
#: It goes through device selection, allocation, a matmul and a host
#: synchronization, which is the smallest sequence that exercises the whole
#: driver + library + runtime stack.
TORCH_SMOKE_SCRIPT = """\
import json
import sys

try:
    import torch
except Exception as exc:  # noqa: BLE001 - report import failures verbatim
    print(f"import torch failed: {exc}", file=sys.stderr)
    sys.exit(1)

if not torch.cuda.is_available():
    print("torch.cuda.is_available() is False", file=sys.stderr)
    sys.exit(1)

count = torch.cuda.device_count()
x = torch.randn(256, 256, device="cuda")
checksum = float((x @ x).sum().item())
if checksum != checksum:  # NaN check
    print("kernel produced NaN", file=sys.stderr)
    sys.exit(1)
print(json.dumps({"deviceCount": count, "checksum": round(checksum, 4)}))
"""


@dataclass
class Check:
    """One named observation inside a layer."""

    name: str
    status: str
    detail: str

    def as_dict(self) -> Dict[str, str]:
        return {"name": self.name, "status": self.status, "detail": self.detail}


@dataclass
class Layer:
    """A group of checks whose worst status defines the layer status."""

    name: str
    checks: List[Check]

    @property
    def status(self) -> str:
        if any(check.status == FAIL for check in self.checks):
            return FAIL
        return PASS

    def as_dict(self) -> Dict[str, Any]:
        return {
            "status": self.status,
            "checks": [check.as_dict() for check in self.checks],
        }


def list_device_nodes(dev_dir: str) -> List[int]:
    """Return the sorted GPU indexes of ``/dev/nvidiaN`` nodes in ``dev_dir``."""
    try:
        entries = os.listdir(dev_dir)
    except OSError:
        return []
    indexes = []
    for entry in entries:
        match = _DEVICE_NODE_RE.match(entry)
        if match:
            indexes.append(int(match.group(1)))
    return sorted(indexes)


def parse_smi_list(text: str) -> Dict[str, Any]:
    """Parse ``nvidia-smi -L`` output into physical GPUs and MIG instances.

    The parser is deliberately line-oriented: ``nvidia-smi -L`` is one line
    per GPU plus one indented line per MIG instance and has been stable across
    driver generations. Unrecognized lines are ignored and reported through
    ``unparsedLines`` so callers can see that the inventory was partially
    understood.
    """
    gpus: List[Dict[str, Any]] = []
    mig_instances: List[Dict[str, Any]] = []
    unparsed: List[str] = []
    for raw_line in text.splitlines():
        line = raw_line.rstrip()
        if not line:
            continue
        gpu_match = _SMI_GPU_RE.match(line)
        if gpu_match:
            gpus.append(
                {
                    "index": int(gpu_match.group(1)),
                    "name": gpu_match.group(2),
                    "uuid": gpu_match.group(3),
                }
            )
            continue
        mig_match = _SMI_MIG_RE.match(line)
        if mig_match:
            mig_instances.append(
                {
                    "profile": mig_match.group(1),
                    "device": int(mig_match.group(2)),
                    "uuid": mig_match.group(3),
                }
            )
            continue
        unparsed.append(line)
    return {
        "gpus": gpus,
        "migInstances": mig_instances,
        "gpuCount": len(gpus),
        "migInstanceCount": len(mig_instances),
        "unparsedLines": unparsed,
    }


def driver_version_from_proc(text: str) -> Optional[str]:
    """Extract the kernel driver version from ``/proc/driver/nvidia/version``."""
    match = _NVRM_RE.search(text)
    return match.group(1) if match else None


def read_text(path: str) -> Optional[str]:
    """Read a small text file, returning ``None`` when it is not available."""
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as handle:
            return handle.read()
    except OSError:
        return None


def run_capture(
    command: Any, timeout_seconds: float, shell: bool = False
) -> Tuple[int, str, str]:
    """Run a command and capture ``(exit_code, stdout, stderr)``.

    Missing executables map to exit code 127 and timeouts to 124, following
    the shell convention, so callers can treat both as probe-local failures.
    """
    try:
        completed = subprocess.run(
            command,
            shell=shell,
            capture_output=True,
            text=True,
            timeout=timeout_seconds,
            check=False,
        )
        return completed.returncode, completed.stdout, completed.stderr
    except FileNotFoundError as exc:
        return 127, "", str(exc)
    except subprocess.TimeoutExpired as exc:
        return 124, exc.stdout or "", f"timed out after {timeout_seconds}s"
    except OSError as exc:
        return 126, "", str(exc)


def _tail(text: str, limit: int = 400) -> str:
    collapsed = " ".join(text.split())
    if len(collapsed) <= limit:
        return collapsed
    return "..." + collapsed[-limit:]


def collect_device_layer(
    dev_dir: str,
    driver_version_path: str,
    smi_path: Optional[str],
    expected_count: Optional[str],
    capture: Callable[..., Tuple[int, str, str]] = run_capture,
) -> Tuple[Layer, Dict[str, Any]]:
    """Observe the device layer inside the sandbox.

    Returns the layer result and the raw observations that also go into the
    top-level ``observed`` object of the report.
    """
    control_node = os.path.exists(os.path.join(dev_dir, "nvidiactl"))
    uvm_node = os.path.exists(os.path.join(dev_dir, "nvidia-uvm"))
    device_nodes = list_device_nodes(dev_dir)
    version_text = read_text(driver_version_path)
    driver_version = driver_version_from_proc(version_text) if version_text else None

    smi: Optional[Dict[str, Any]] = None
    if smi_path:
        exit_code, stdout, stderr = capture([smi_path, "-L"], 15)
        if exit_code == 0:
            parsed = parse_smi_list(stdout)
            smi = {
                "gpuCount": parsed["gpuCount"],
                "gpuNames": [gpu["name"] for gpu in parsed["gpus"]],
                "uuids": [gpu["uuid"] for gpu in parsed["gpus"]],
                "migInstanceCount": parsed["migInstanceCount"],
            }
            smi["_parsed"] = parsed
        else:
            smi = {"error": f"nvidia-smi -L exited {exit_code}: {_tail(stderr)}"}

    observed: Dict[str, Any] = {
        "deviceNodeCount": len(device_nodes),
        "deviceNodes": [f"nvidia{index}" for index in device_nodes],
        "driverVersion": driver_version,
        "visibleDevicesEnv": os.environ.get("NVIDIA_VISIBLE_DEVICES"),
        "driverCapabilitiesEnv": os.environ.get("NVIDIA_DRIVER_CAPABILITIES"),
        "smiGpuCount": smi.get("gpuCount") if smi else None,
        "smiGpuNames": smi.get("gpuNames") if smi else None,
        "smiUuids": smi.get("uuids") if smi else None,
        "migInstanceCount": smi.get("migInstanceCount") if smi else None,
    }
    if smi and "error" in smi:
        observed["smiError"] = smi["error"]

    checks: List[Check] = []

    if control_node:
        checks.append(Check("control-node", PASS, f"{dev_dir}/nvidiactl is present"))
    else:
        checks.append(
            Check(
                "control-node",
                FAIL,
                f"{dev_dir}/nvidiactl is missing: no NVIDIA device stack is "
                "attached. Check that the sandbox was created with "
                "resourceLimits.gpu, that the runtime exposes GPUs, and that "
                "the sandbox is scheduled on a GPU node.",
            )
        )

    if device_nodes:
        checks.append(
            Check(
                "device-nodes",
                PASS,
                "GPU device nodes: "
                + ", ".join(f"{dev_dir}/nvidia{index}" for index in device_nodes),
            )
        )
    else:
        checks.append(
            Check(
                "device-nodes",
                FAIL,
                f"no {dev_dir}/nvidiaN device nodes are visible in the sandbox",
            )
        )

    if uvm_node:
        checks.append(Check("uvm-node", PASS, f"{dev_dir}/nvidia-uvm is present"))
    else:
        checks.append(
            Check(
                "uvm-node",
                WARN,
                f"{dev_dir}/nvidia-uvm is missing: CUDA needs the UVM device. "
                "The usual cause is NVIDIA_DRIVER_CAPABILITIES excluding "
                "'compute' (or 'all').",
            )
        )

    if driver_version:
        checks.append(
            Check("driver-version", PASS, f"kernel driver {driver_version}")
        )
    else:
        checks.append(
            Check(
                "driver-version",
                WARN,
                f"could not read a driver version from {driver_version_path} "
                "(the path may be absent on some runtimes)",
            )
        )

    if smi is None:
        checks.append(
            Check(
                "smi-inventory",
                WARN,
                "nvidia-smi is not available in this image; device nodes and "
                "the driver version remain authoritative for this layer",
            )
        )
    elif "error" in smi:
        checks.append(Check("smi-inventory", WARN, smi["error"]))
    else:
        parsed = smi["_parsed"]
        checks.append(
            Check(
                "smi-inventory",
                PASS,
                f"nvidia-smi reports {parsed['gpuCount']} GPU(s) and "
                f"{parsed['migInstanceCount']} MIG instance(s)",
            )
        )
        if parsed["unparsedLines"]:
            checks.append(
                Check(
                    "smi-inventory-parse",
                    WARN,
                    "some nvidia-smi -L lines were not recognized: "
                    + _tail("; ".join(parsed["unparsedLines"]), 200),
                )
            )
        if device_nodes:
            if parsed["gpuCount"] == len(device_nodes):
                checks.append(
                    Check(
                        "smi-inventory-consistency",
                        PASS,
                        f"device nodes ({len(device_nodes)}) and nvidia-smi "
                        f"({parsed['gpuCount']}) agree on the GPU count",
                    )
                )
            else:
                checks.append(
                    Check(
                        "smi-inventory-consistency",
                        WARN,
                        f"device nodes ({len(device_nodes)}) and nvidia-smi "
                        f"({parsed['gpuCount']}) disagree on the GPU count; this "
                        "is the signature of a partial attach or a toolkit/driver "
                        "inventory mismatch",
                    )
                )
        if parsed["migInstanceCount"]:
            checks.append(
                Check(
                    "mig-mode",
                    WARN,
                    f"{parsed['migInstanceCount']} MIG instance(s) detected; "
                    "count checks compare physical device nodes, so express "
                    "--expect in the same unit as resourceLimits.gpu",
                )
            )

    if expected_count is not None:
        if expected_count == "all":
            checks.append(
                Check(
                    "requested-count",
                    SKIPPED,
                    "requested resourceLimits.gpu='all': the resolved count "
                    "depends on the host inventory. Pass an explicit "
                    "--expect N to enable the count check.",
                )
            )
        else:
            expected = int(expected_count)
            if len(device_nodes) == expected:
                checks.append(
                    Check(
                        "requested-count",
                        PASS,
                        f"observed {len(device_nodes)} device(s), requested {expected}",
                    )
                )
            else:
                checks.append(
                    Check(
                        "requested-count",
                        FAIL,
                        f"requested {expected} GPU(s) but observed "
                        f"{len(device_nodes)} device node(s): requested vs "
                        "attached drift",
                    )
                )

    return Layer("device", checks), observed


def collect_compute_layer(
    compute_mode: str,
    timeout_seconds: float,
    capture: Callable[..., Tuple[int, str, str]] = run_capture,
) -> Layer:
    """Run the compute probe (layer 3) and attribute failures to this layer."""
    if compute_mode == "skip":
        return Layer(
            "compute",
            [
                Check(
                    "compute-probe",
                    SKIPPED,
                    "compute probe disabled (--compute skip)",
                )
            ],
        )

    if compute_mode == "auto":
        python = shutil.which("python3") or sys.executable
        if not python:
            return Layer(
                "compute",
                [
                    Check(
                        "compute-probe",
                        SKIPPED,
                        "no python3 interpreter is available for the automatic "
                        "probe; pass --compute '<command>' to enable layer 3",
                    )
                ],
            )
        probe_timeout = max(5.0, min(timeout_seconds, 30.0))
        import_exit, _, _ = capture([python, "-c", "import torch"], probe_timeout)
        if import_exit != 0:
            return Layer(
                "compute",
                [
                    Check(
                        "compute-probe",
                        SKIPPED,
                        "PyTorch is not importable in this image; pass "
                        "--compute '<command>' with a workload-native smoke "
                        "test to enable layer-3 reporting",
                    )
                ],
            )
        exit_code, stdout, stderr = capture(
            [python, "-c", TORCH_SMOKE_SCRIPT], timeout_seconds
        )
        if exit_code == 0:
            return Layer(
                "compute",
                [
                    Check(
                        "compute-probe",
                        PASS,
                        "PyTorch CUDA smoke test passed: " + _tail(stdout, 200),
                    )
                ],
            )
        return Layer(
            "compute",
            [
                Check(
                    "compute-probe",
                    FAIL,
                    f"PyTorch CUDA smoke test failed (exit {exit_code}): "
                    + _tail(stderr or stdout),
                )
            ],
        )

    exit_code, stdout, stderr = capture(
        compute_mode, timeout_seconds, shell=True
    )
    if exit_code == 0:
        detail = "compute command passed"
        if stdout.strip():
            detail += ": " + _tail(stdout, 200)
        return Layer("compute", [Check("compute-probe", PASS, detail)])
    return Layer(
        "compute",
        [
            Check(
                "compute-probe",
                FAIL,
                f"compute command failed (exit {exit_code}): "
                + _tail(stderr or stdout),
            )
        ],
    )


def build_report(
    device_layer: Layer,
    compute_layer: Layer,
    observed: Dict[str, Any],
    expected_count: Optional[str],
) -> Dict[str, Any]:
    """Assemble the machine-readable report, including the exit-code contract."""
    if device_layer.status == FAIL:
        exit_code = EXIT_DEVICE_FAILED
    elif compute_layer.status == FAIL:
        exit_code = EXIT_COMPUTE_FAILED
    else:
        exit_code = EXIT_OK

    warnings: List[str] = []
    for layer in (device_layer, compute_layer):
        for check in layer.checks:
            if check.status == WARN:
                warnings.append(f"{layer.name}.{check.name}: {check.detail}")

    return {
        "probe": PROBE_NAME,
        "version": PROBE_VERSION,
        "status": PASS if exit_code == EXIT_OK else FAIL,
        "exitCode": exit_code,
        "expectedCount": (
            None
            if expected_count is None
            else ("all" if expected_count == "all" else int(expected_count))
        ),
        "layers": {
            "device": device_layer.as_dict(),
            "compute": compute_layer.as_dict(),
        },
        "observed": observed,
        "warnings": warnings,
        "note": (
            "The platform layer is observed by the caller: reaching this probe "
            "at all proves the sandbox is running; a probe that never starts "
            "is a platform-layer failure."
        ),
    }


def _summary(report: Dict[str, Any]) -> str:
    layers = report["layers"]
    observed = report["observed"]
    parts = [
        f"{PROBE_NAME}: {report['status'].upper()}",
        f"device={layers['device']['status']}",
        f"compute={layers['compute']['status']}",
    ]
    if observed.get("driverVersion"):
        parts.append(f"driver={observed['driverVersion']}")
    parts.append(
        f"gpus={observed.get('deviceNodeCount')}"
        + (
            f"/{report['expectedCount']}"
            if report.get("expectedCount") not in (None, "all")
            else ""
        )
    )
    failed = [
        f"{layer_name}.{check['name']}: {check['detail']}"
        for layer_name, layer in layers.items()
        for check in layer["checks"]
        if check["status"] == FAIL
    ]
    summary = " ".join(parts)
    if failed:
        summary += " — " + " | ".join(failed)
    return summary


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Layered GPU health probe for OpenSandbox sandboxes "
            "(see docs/guides/gpu-health.md)."
        )
    )
    parser.add_argument(
        "--expect",
        default=None,
        metavar="N|all",
        help=(
            "GPU count requested at sandbox creation (resourceLimits.gpu). "
            "Enables the requested-vs-observed count check; 'all' records the "
            "sentinel but skips the comparison."
        ),
    )
    parser.add_argument(
        "--compute",
        default="auto",
        metavar="auto|skip|COMMAND",
        help=(
            "Compute-layer probe: 'auto' runs a PyTorch smoke test when "
            "PyTorch is importable, 'skip' disables layer 3, anything else is "
            "run as a shell command (exit 0 = pass)."
        ),
    )
    parser.add_argument(
        "--compute-timeout",
        type=float,
        default=60.0,
        metavar="SECONDS",
        help="Timeout for the compute-layer probe (default: 60).",
    )
    parser.add_argument(
        "--dev-dir",
        default="/dev",
        help="Directory holding the NVIDIA device nodes (default: /dev).",
    )
    parser.add_argument(
        "--driver-version-path",
        default="/proc/driver/nvidia/version",
        help="Path to the NVIDIA kernel driver version file.",
    )
    parser.add_argument(
        "--smi-path",
        default=None,
        help=(
            "Path to nvidia-smi; auto-detected from PATH when omitted, "
            "use --no-smi to skip it entirely."
        ),
    )
    parser.add_argument(
        "--no-smi",
        action="store_true",
        help="Skip the nvidia-smi inventory check (device nodes remain authoritative).",
    )
    parser.add_argument(
        "--quiet",
        action="store_true",
        help="Suppress the human-readable summary on stderr.",
    )
    args = parser.parse_args(argv)

    try:
        if args.expect is not None and args.expect != "all":
            try:
                if int(args.expect) <= 0:
                    raise ValueError
            except ValueError:
                parser.error("--expect must be a positive integer or 'all'")

        smi_path = None if args.no_smi else (args.smi_path or shutil.which("nvidia-smi"))
        device_layer, observed = collect_device_layer(
            dev_dir=args.dev_dir,
            driver_version_path=args.driver_version_path,
            smi_path=smi_path,
            expected_count=args.expect,
        )
        compute_layer = collect_compute_layer(args.compute, args.compute_timeout)
        report = build_report(device_layer, compute_layer, observed, args.expect)
    except SystemExit:
        raise
    except Exception as exc:  # noqa: BLE001 - the probe must never crash silently
        report = {
            "probe": PROBE_NAME,
            "version": PROBE_VERSION,
            "status": "error",
            "exitCode": EXIT_PROBE_ERROR,
            "error": f"{type(exc).__name__}: {exc}",
        }
        print(json.dumps(report, indent=2, sort_keys=True))
        return EXIT_PROBE_ERROR

    print(json.dumps(report, indent=2, sort_keys=True))
    if not args.quiet:
        print(_summary(report), file=sys.stderr)
    return int(report["exitCode"])


if __name__ == "__main__":
    sys.exit(main())
