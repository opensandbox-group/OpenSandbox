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
"""create-bench.py — fsb sandbox create-latency bench for the fast-sandbox-env
kind deployment, written against the OpenSandbox Python SDK.

Run AFTER `./integration-env.sh up`:

    cd OpenSandbox/tests/python
    uv run python ../../scripts/fast-sandbox-env/create-bench.py

Creates (or reuses) one Succeeded fsb template, then creates N sandboxes with
CONCURRENCY parallel runs. Each sample = Sandbox.create_from_template()
returning with execd /ping readiness (the SDK's built-in delivery criterion
through the signed gateway route); the sandbox is killed right after
readiness. Reports avg/p50/p90/p99/min/max.

Env:
  N=100  CONCURRENCY=10
  DOMAIN=127.0.0.1:18080  PROTOCOL=http  API_KEY=fast-sandbox-env
  TEMPLATE_ID=            reuse a Succeeded template (skip the build)
  SOURCE_IMAGE=opensandbox/fsb-sandbox-golden:latest
  PUBLISH=s3://sandbox-images/publish
  WARMUP=2                untimed warmup creates (first artifact pull)
  READY_TIMEOUT_COLD=900  first-create readiness budget (artifact pull)
  READY_TIMEOUT_WARM=300  readiness budget for warm creates
  PING_INTERVAL_MS=10     readiness poll; SDK default is 200 ms
  REQUEST_TIMEOUT=30     individual HTTP request timeout, in seconds
  DISABLE_METRICS=0       set to 1 to exclude SDK metrics initialization
  REPORT_PATH=           optional JSON report with raw attempts and settings
"""

import asyncio
import json
import math
import os
import statistics
import sys
import time
from datetime import timedelta
from pathlib import Path

from opensandbox.config import ConnectionConfig
from opensandbox.manager import SandboxManager
from opensandbox.models.templates import (
    CreateTemplateRequest,
    TemplatePhase,
    TemplateReadiness,
)
from opensandbox.sandbox import Sandbox

DOMAIN = os.environ.get("DOMAIN", "127.0.0.1:18080")
PROTOCOL = os.environ.get("PROTOCOL", "http")
API_KEY = os.environ.get("API_KEY", "fast-sandbox-env")
N = int(os.environ.get("N", "100"))
CONCURRENCY = int(os.environ.get("CONCURRENCY", "10"))
BATCH_PAUSE = float(os.environ.get("BATCH_PAUSE", "5"))
TEMPLATE_ID = os.environ.get("TEMPLATE_ID", "")
SOURCE_IMAGE = os.environ.get("SOURCE_IMAGE", "opensandbox/fsb-sandbox-golden:latest")
PUBLISH = os.environ.get("PUBLISH", "s3://sandbox-images/publish")
WARMUP = int(os.environ.get("WARMUP", "2"))
READY_TIMEOUT_COLD = int(os.environ.get("READY_TIMEOUT_COLD", "900"))
READY_TIMEOUT_WARM = int(os.environ.get("READY_TIMEOUT_WARM", "300"))
PING_INTERVAL_MS = int(os.environ.get("PING_INTERVAL_MS", "10"))
REQUEST_TIMEOUT = float(os.environ.get("REQUEST_TIMEOUT", "30"))
DISABLE_METRICS = os.environ.get("DISABLE_METRICS", "0") == "1"
REPORT_PATH = os.environ.get("REPORT_PATH", "")

CONNECTION_CONFIG = ConnectionConfig(
    domain=DOMAIN,
    protocol=PROTOCOL,
    api_key=API_KEY,
    request_timeout=timedelta(seconds=REQUEST_TIMEOUT),
    disable_metrics=DISABLE_METRICS,
)


async def ensure_template(manager: SandboxManager) -> str:
    template_id = os.environ.get("TEMPLATE_ID", "")
    if template_id:
        print(f"==> reusing template {template_id}")
        return template_id

    created = await manager.create_template(
        CreateTemplateRequest(
            image=SOURCE_IMAGE,
            publish=PUBLISH,
            format="native",
            resource_limits={"cpu": "1", "memory": "512Mi", "disk": "2Gi"},
            readiness=TemplateReadiness(warmup_seconds=15),
            metadata={"origin": "create-bench"},
        )
    )
    template_id = created.template_id
    print(f"==> templateId: {template_id} — waiting for build")
    while True:
        info = await manager.get_template(template_id)
        phase = info.status.phase
        if phase == TemplatePhase.SUCCEEDED:
            print("==> template build Succeeded")
            return template_id
        if phase == TemplatePhase.FAILED:
            sys.exit(f"ERROR: template {template_id} build Failed: {info.status.message}")
        print(".", end="", flush=True)
        await asyncio.sleep(2)


async def one_create(manager: SandboxManager, template_id: str, ready_timeout: int):
    """Returns (latency_seconds, sandbox_id) or raises on failure."""
    start = time.monotonic()
    sandbox = await Sandbox.create_from_template(
        template_id,
        timeout=timedelta(hours=1),
        ready_timeout=timedelta(seconds=ready_timeout),
        connection_config=CONNECTION_CONFIG,
        # Tighten the SDK's 200ms default poll to match the 10ms cadence of
        # the shell verify flow (verify_one_sandbox).
        health_check_polling_interval=timedelta(milliseconds=PING_INTERVAL_MS),
    )
    latency = time.monotonic() - start
    await sandbox.close()
    return latency, sandbox.id


async def main():
    manager = await SandboxManager.create(CONNECTION_CONFIG)
    template_id = await ensure_template(manager)

    # warmup: absorb the first artifact pull on every fastlet node (untimed)
    for i in range(WARMUP):
        t0 = time.monotonic()
        try:
            latency, sandbox_id = await one_create(manager, template_id, READY_TIMEOUT_COLD)
        except Exception as err:
            sys.exit(f"ERROR: warmup {i + 1}/{WARMUP} failed: {err}")
        print(f"==> warmup {i + 1}/{WARMUP}: ready in {time.monotonic() - t0:.2f}s "
              f"(sdk={latency:.2f}s, id={sandbox_id}) — killing")
        await manager.kill_sandbox(sandbox_id)

    print(f"==> measuring {N} creates in batches of {CONCURRENCY}, "
          f"pausing {BATCH_PAUSE}s between batches (slot reclamation)")
    samples, failures, attempts = [], [], []

    async def run(i):
        try:
            latency, sandbox_id = await one_create(manager, template_id, READY_TIMEOUT_WARM)
        except Exception as err:
            failures.append(i)
            attempts.append({
                "index": i,
                "success": False,
                "error_type": type(err).__name__,
            })
            print(f"[{i}/{N}] FAILED: {err}", flush=True)
            return
        samples.append(latency)
        attempts.append({"index": i, "success": True, "ready_seconds": latency})
        await manager.kill_sandbox(sandbox_id)
        print(f"[{i}/{N}] create={latency:.3f}s id={sandbox_id} (killed)", flush=True)

    for batch_start in range(1, N + 1, CONCURRENCY):
        batch = range(batch_start, min(batch_start + CONCURRENCY, N + 1))
        await asyncio.gather(*(run(i) for i in batch))
        if batch.stop < N + 1:
            print(f"==> batch done, resting {BATCH_PAUSE}s for slot reclamation")
            await asyncio.sleep(BATCH_PAUSE)

    def pct(p):
        ordered = sorted(samples)
        return ordered[max(0, math.ceil(p / 100 * len(ordered)) - 1)]

    summary = {
        "mean": statistics.mean(samples),
        "p50": pct(50),
        "p90": pct(90),
        "p99": pct(99),
        "min": min(samples),
        "max": max(samples),
    } if samples else {}
    if REPORT_PATH:
        report = {
            "measurement": "SDK create through guest /ping readiness",
            "settings": {
                "n": N,
                "concurrency": CONCURRENCY,
                "warmup": WARMUP,
                "batch_pause_seconds": BATCH_PAUSE,
                "ping_interval_ms": PING_INTERVAL_MS,
                "request_timeout_seconds": REQUEST_TIMEOUT,
                "disable_metrics": DISABLE_METRICS,
                "template_id": template_id,
            },
            "successes": len(samples),
            "failures": len(failures),
            "percentile_method": "nearest_rank",
            "successful_latency_seconds": summary,
            "attempts": sorted(attempts, key=lambda attempt: attempt["index"]),
        }
        Path(REPORT_PATH).write_text(json.dumps(report, indent=2) + "\n")
    await manager.close()

    if not samples:
        sys.exit(f"ERROR: all {len(failures)} creates failed")

    print(f"\n=== fsb create latency over {len(samples)} runs, {len(failures)} failed "
          f"(seconds, concurrency {CONCURRENCY}) ===")
    print(f"avg = {statistics.mean(samples):.3f}")
    print(f"p50 = {pct(50):.3f}")
    print(f"p90 = {pct(90):.3f}")
    print(f"p99 = {pct(99):.3f}")
    print(f"min = {min(samples):.3f}   max = {max(samples):.3f}")
    if failures:
        sys.exit(1)


if __name__ == "__main__":
    asyncio.run(main())
