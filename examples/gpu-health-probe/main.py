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

"""
GPU Health Probe Example

Creates a GPU sandbox, uploads the layered GPU health probe, runs it, and
shows how to attribute a failure to a specific layer from the probe's JSON
report. This is the reference flow for the exec-based GPU probe convention
described in docs/guides/gpu-health.md.

The probe reports three layers:
  platform : "can the sandbox be reached at all" (observed by this script —
             if the exec never starts, the platform layer failed)
  device   : the NVIDIA stack visible inside the sandbox, including the
             requested-vs-observed GPU count
  compute  : a short kernel launch through the full stack

Prerequisites:
1. An OpenSandbox server whose runtime exposes GPUs (Docker with
   nvidia-container-toolkit, or Kubernetes nodes with a GPU device plugin)
2. Python SDK: `uv pip install opensandbox`

Usage:
    export SANDBOX_DOMAIN=your-server:8080        # default: localhost:8080
    export SANDBOX_API_KEY=your-api-key           # if the server requires auth
    export GPU_EXPECT=1                           # resourceLimits.gpu to request and verify
    python main.py
"""

import asyncio
import json
import os
import sys
from datetime import timedelta
from pathlib import Path

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxException
from opensandbox.models import WriteEntry

# Configuration
PROBE_DIR = "/tmp/gpu-health-probe"
PROBE_PATH = f"{PROBE_DIR}/gpu_probe.py"
IMAGE = os.getenv(
    "SANDBOX_IMAGE",
    "nvidia/cuda:12.4.1-base-ubuntu22.04",
)
GPU_EXPECT = os.getenv("GPU_EXPECT", "1")
GPU_COMPUTE_MODE = os.getenv("GPU_COMPUTE_MODE", "auto")  # auto | skip

# Images without a compute runtime (PyTorch etc.) cannot run layer 3;
# `--compute skip` keeps layers 1-2 meaningful on any GPU image.
COMPUTE_FLAG = "--compute skip" if GPU_COMPUTE_MODE == "skip" else "--compute auto"

CONNECTION_CONFIG = ConnectionConfig(
    request_timeout=timedelta(minutes=10),
)

EXIT_DEVICE_FAILED = 2
EXIT_COMPUTE_FAILED = 3

# GPU images are large; give the initial pull room to finish.
SANDBOX_TIMEOUT = timedelta(minutes=60)


def _diagnosis(exit_code: int, report: dict) -> str:
    """Turn the probe's exit code into a one-line operator action."""
    if exit_code == 0:
        return "GPU stack is healthy end to end."
    if exit_code == EXIT_DEVICE_FAILED:
        failed = [
            check["name"]
            for check in report.get("layers", {}).get("device", {}).get("checks", [])
            if check["status"] == "fail"
        ]
        return (
            f"Device layer failed ({', '.join(failed) or 'unknown'}): the GPU is "
            "not usable inside the sandbox. Check resourceLimits.gpu on the "
            "create request, the runtime's GPU configuration, and whether the "
            "sandbox landed on a GPU node. A requested-vs-observed count "
            "mismatch here is attach drift, not an application problem."
        )
    if exit_code == EXIT_COMPUTE_FAILED:
        return (
            "Compute layer failed: the device stack is visible but a kernel "
            "launch does not work. This is the driver/library mismatch class "
            "of failure that process-up and device-visible checks both miss."
        )
    return f"Probe error (exit {exit_code}): see the report above."


async def main() -> None:
    probe_source = (Path(__file__).parent / "gpu_probe.py").read_text(
        encoding="utf-8"
    )
    resource = {"cpu": "2", "memory": "4Gi", "gpu": GPU_EXPECT}

    sandbox = await Sandbox.create(
        IMAGE,
        resource=resource,
        connection_config=CONNECTION_CONFIG,
        timeout=SANDBOX_TIMEOUT,
    )
    print(f"Sandbox created: {sandbox.id} (resourceLimits.gpu={GPU_EXPECT})")
    try:
        await sandbox.files.create_directories([WriteEntry(path=PROBE_DIR)])
        await sandbox.files.write_file(PROBE_PATH, probe_source, mode=0o755)

        command = (
            f"python3 {PROBE_PATH} --expect {GPU_EXPECT} {COMPUTE_FLAG}"
        )
        try:
            execution = await sandbox.commands.run(command)
        except SandboxException as exc:
            # The probe never started: this is the platform layer failing,
            # which the probe itself could not have reported.
            print(f"Could not start the probe inside the sandbox: {exc}")
            print(
                "Platform-layer failure: the sandbox is not reachable or "
                "its agent is not ready. Check sandbox status and runtime "
                "events before debugging the GPU stack."
            )
            sys.exit(1)

        stdout = "".join(message.text or "" for message in execution.logs.stdout)
        exit_code = execution.exit_code
        try:
            report = json.loads(stdout)
        except json.JSONDecodeError:
            print(stdout)
            print(
                f"The probe produced no JSON (exit {exit_code}); "
                "if the sandbox cannot even start the probe, that is a "
                "platform-layer failure — check sandbox status and execd logs."
            )
            sys.exit(1)

        print(json.dumps(report, indent=2))
        print(_diagnosis(int(exit_code), report))
        sys.exit(int(exit_code))
    finally:
        await sandbox.destroy()


if __name__ == "__main__":
    asyncio.run(main())
