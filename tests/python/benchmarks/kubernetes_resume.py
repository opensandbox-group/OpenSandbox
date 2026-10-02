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

"""Opt-in standalone rootfs resume benchmark; see docs/guides/kubernetes-resume-benchmark.md."""

import argparse
import asyncio
import importlib.metadata
import json
import math
import platform
import subprocess
import sys
import time
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

import httpx
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException
from opensandbox.manager import SandboxManager
from opensandbox.models.execd import RunCommandOpts
from opensandbox.transport.retry import RetryPolicy

TIMINGS = ("api_ack_ms", "running_ms", "ready_ms", "command_ms")
MARKER_PATH = "/tmp/opensandbox-resume-benchmark-marker"


def percentile(values, quantile):
    """Nearest rank, with no interpolation or fabricated all-failed value."""
    if not values:
        return None
    ordered = sorted(values)
    return ordered[max(0, math.ceil(len(ordered) * quantile) - 1)]


def summarize(records):
    measured = [r for r in records if not r["warmup"]]
    successful = [r for r in measured if r["success"]]
    failed = len(measured) - len(successful)
    return {
        "type": "summary",
        "measured_trials": len(measured),
        "successful_trials": len(successful),
        "failed_trials": failed,
        "failure_rate": failed / len(measured) if measured else None,
        "warmup_failures": sum(not r["success"] for r in records if r["warmup"]),
        "cleanup_failures": sum(bool(r.get("cleanup_errors")) for r in records),
        "percentile_method": "nearest-rank; successful measured trials only",
        "timings": {
            key: {
                "p50_ms": percentile([r[key] for r in successful], 0.5),
                "p95_ms": percentile([r[key] for r in successful], 0.95),
            }
            for key in TIMINGS
        },
    }


def kubectl(args, *command):
    result = subprocess.run(
        [
            "kubectl",
            "--context",
            args.context,
            "--request-timeout=15s",
            *command,
            "-o",
            "json",
        ],
        capture_output=True,
        text=True,
        check=True,
        timeout=20,
    )
    return json.loads(result.stdout)


async def wait_state(manager, sandbox_id, target, interval):
    while True:
        info = await manager.get_sandbox_info(sandbox_id)
        if info.status.state == target:
            return
        if info.status.state in {"Failed", "Terminated", "Stopping"}:
            raise RuntimeError(
                f"Sandbox entered {info.status.state}: {info.status.reason}"
            )
        await asyncio.sleep(interval)


async def verify_backend(args, sandbox_id):
    bs = await asyncio.to_thread(
        kubectl,
        args,
        "get",
        "batchsandboxes.sandbox.opensandbox.io",
        sandbox_id,
        "-n",
        args.namespace,
    )
    spec = bs["spec"]
    annotations = (
        (spec.get("template") or {}).get("metadata", {}).get("annotations", {})
    )
    provider = annotations.get("sandbox.opensandbox.io/checkpoint-provider", "rootfs")
    if spec.get("poolRef") or spec.get("replicas", 1) != 1 or provider != "rootfs":
        raise RuntimeError(
            "Benchmark requires a standalone, single-replica rootfs BatchSandbox"
        )
    # A volume mounted above the marker would defeat rootfs preservation validation.
    for container in spec.get("template", {}).get("spec", {}).get("containers", []):
        for mount in container.get("volumeMounts", []):
            root = mount["mountPath"].rstrip("/")
            if MARKER_PATH == root or MARKER_PATH.startswith(root + "/"):
                raise RuntimeError(f"Marker path is covered by volume mount {root}")
    return bs["metadata"]["uid"]


async def wait_deleted_pods(args, owner_uid):
    while True:
        pods = await asyncio.to_thread(
            kubectl, args, "get", "pods", "-n", args.namespace
        )
        if not any(
            any(
                owner["uid"] == owner_uid
                for owner in pod["metadata"].get("ownerReferences", [])
            )
            for pod in pods["items"]
        ):
            return
        await asyncio.sleep(args.poll_interval)


async def snapshot_evidence(args, owner_uid):
    snapshots = await asyncio.to_thread(
        kubectl,
        args,
        "get",
        "sandboxsnapshots.sandbox.opensandbox.io",
        "-n",
        args.namespace,
    )
    owned = [
        item
        for item in snapshots["items"]
        if any(
            ref["uid"] == owner_uid
            for ref in item["metadata"].get("ownerReferences", [])
        )
    ]
    if len(owned) != 1:
        raise RuntimeError("Expected one owned internal pause snapshot")
    snapshot = owned[0]
    status = snapshot.get("status", {})
    if (
        status.get("phase") != "Succeed"
        or status.get("format", "rootfs-v1") != "rootfs-v1"
    ):
        raise RuntimeError("Expected a ready rootfs snapshot")
    return {"name": snapshot["metadata"]["name"], "status": status}


async def pod_evidence(args, owner_uid):
    pods = await asyncio.to_thread(kubectl, args, "get", "pods", "-n", args.namespace)
    owned = [
        pod
        for pod in pods["items"]
        if any(
            ref["uid"] == owner_uid
            for ref in pod["metadata"].get("ownerReferences", [])
        )
    ]
    if not owned:
        raise RuntimeError("No restored Pod found for evidence collection")
    return [
        {
            "name": pod["metadata"]["name"],
            "node": pod["spec"].get("nodeName"),
            "containers": [
                {
                    "name": c["name"],
                    "image": c["image"],
                    "imagePullPolicy": c.get("imagePullPolicy"),
                }
                for c in pod["spec"]["containers"]
            ],
            "containerStatuses": pod.get("status", {}).get("containerStatuses", []),
        }
        for pod in owned
    ]


async def prepare(args, sandbox_id):
    if not args.prepare_script:
        return None
    process = await asyncio.create_subprocess_exec(
        sys.executable,
        str(args.prepare_script),
        sandbox_id,
        args.namespace,
        args.context,
        args.scenario,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    try:
        stdout, stderr = await asyncio.wait_for(
            process.communicate(), args.prepare_timeout
        )
    except BaseException:
        if process.returncode is None:
            process.kill()
        await process.wait()
        raise
    if process.returncode:
        raise RuntimeError(
            f"Preparation exited {process.returncode}: {stderr.decode(errors='replace')}"
        )
    evidence = stdout.decode(errors="replace").strip()
    if args.scenario != "uncontrolled" and not evidence:
        raise RuntimeError("Cache scenario preparation must emit evidence on stdout")
    return evidence


async def check_command(sandbox, command, timeout):
    execution = await sandbox.commands.run(
        command, opts=RunCommandOpts(timeout=timedelta(seconds=timeout))
    )
    if execution.error or execution.exit_code != 0:
        raise RuntimeError(
            f"Validation command failed: exit={execution.exit_code}, error={execution.error}"
        )
    return "".join(message.text for message in execution.logs.stdout).strip()


async def resume_and_check(args, manager, config, record, marker, clients):
    started = time.perf_counter()

    def elapsed():
        return (time.perf_counter() - started) * 1000

    record["stage"] = "resume_api"
    await manager.resume_sandbox(record["sandbox_id"])
    record["api_ack_ms"] = elapsed()
    record["stage"] = "running"
    await wait_state(manager, record["sandbox_id"], "Running", args.poll_interval)
    record["running_ms"] = elapsed()
    record["stage"] = "ready"
    # Re-resolve endpoints using a new client; never reuse the source Pod's endpoint.
    remaining = max(0.001, args.resume_timeout - (time.perf_counter() - started))
    sandbox = await Sandbox.connect(
        record["sandbox_id"],
        connection_config=config,
        connect_timeout=timedelta(seconds=remaining),
        health_check_polling_interval=timedelta(seconds=args.poll_interval),
    )
    clients.append(sandbox)
    record["ready_ms"] = elapsed()
    record["stage"] = "command"
    actual = await check_command(sandbox, f"cat {MARKER_PATH}", args.resume_timeout)
    if actual != marker:
        raise RuntimeError("Persisted rootfs marker did not match")
    record["command_ms"] = elapsed()


async def create_sandbox(args, config, run_id):
    # Capture the ID immediately after acceptance, before endpoint resolution.
    # This keeps cleanup under the runner's control if SDK initialization fails.
    async with httpx.AsyncClient(timeout=args.request_timeout) as client:
        response = await client.post(
            config.get_base_url().rstrip("/") + "/sandboxes",
            headers={"OPEN-SANDBOX-API-KEY": config.get_api_key()},
            json={
                "image": {"uri": args.image},
                "entrypoint": ["/bin/sh", "-c", "sleep 86400"],
                "timeout": args.lifetime,
                "resourceLimits": {"cpu": args.cpu, "memory": args.memory},
                "metadata": {
                    "benchmark": "kubernetes-rootfs-resume",
                    "benchmark_run": run_id,
                },
            },
        )
        response.raise_for_status()
        return response.json()["id"]


async def cleanup(manager, sandbox_id, interval):
    try:
        await manager.kill_sandbox(sandbox_id)
    except SandboxApiException as exc:
        if exc.status_code != 404:
            raise
        return
    while True:
        try:
            await manager.get_sandbox_info(sandbox_id)
        except SandboxApiException as exc:
            if exc.status_code == 404:
                return
            raise
        await asyncio.sleep(interval)


async def trial(args, manager, config, index, warmup):
    record = {
        "type": "trial",
        "index": index,
        "warmup": warmup,
        "success": False,
        "scenario": args.scenario,
        "stage": "create",
        "sandbox_id": None,
        "started_at": datetime.now(timezone.utc).isoformat(),
        "cleanup_errors": [],
        "run_id": uuid.uuid4().hex,
    }
    clients = []
    marker = uuid.uuid4().hex
    try:
        record["sandbox_id"] = await create_sandbox(args, config, record["run_id"])
        record["stage"] = "setup"
        owner_uid = await asyncio.wait_for(
            verify_backend(args, record["sandbox_id"]), args.setup_timeout
        )
        sandbox = await Sandbox.connect(
            record["sandbox_id"],
            connection_config=config,
            connect_timeout=timedelta(seconds=args.setup_timeout),
            health_check_polling_interval=timedelta(seconds=args.poll_interval),
        )
        clients.append(sandbox)
        record["sandbox_id"] = sandbox.id
        record["stage"] = "setup"
        await asyncio.wait_for(
            check_command(
                sandbox, f"printf '%s' '{marker}' > {MARKER_PATH}", args.setup_timeout
            ),
            args.setup_timeout,
        )
        record["stage"] = "pause"
        await asyncio.wait_for(manager.pause_sandbox(sandbox.id), args.pause_timeout)
        await asyncio.wait_for(
            wait_state(manager, sandbox.id, "Paused", args.poll_interval),
            args.pause_timeout,
        )
        record["stage"] = "pod_deletion"
        await asyncio.wait_for(wait_deleted_pods(args, owner_uid), args.pause_timeout)
        record["stage"] = "snapshot_evidence"
        record["snapshot"] = await asyncio.wait_for(
            snapshot_evidence(args, owner_uid), args.setup_timeout
        )
        record["stage"] = "preparation"
        record["preparation_evidence"] = await prepare(args, sandbox.id)
        await asyncio.wait_for(
            resume_and_check(args, manager, config, record, marker, clients),
            args.resume_timeout,
        )
        record["stage"] = "pod_evidence"
        record["restored_pods"] = await asyncio.wait_for(
            pod_evidence(args, owner_uid), args.setup_timeout
        )
        record["success"] = True
        record["stage"] = "complete"
    except Exception as exc:
        record["error"] = f"{type(exc).__name__}: {exc}"
        if record["sandbox_id"] is None:
            # A create request can have been accepted before its response is lost.
            record["cleanup_errors"].append(
                f"Creation outcome unknown; inspect metadata benchmark_run={record['run_id']}"
            )
    finally:
        if record["sandbox_id"]:
            try:
                await asyncio.wait_for(
                    cleanup(manager, record["sandbox_id"], args.poll_interval),
                    args.cleanup_timeout,
                )
            except Exception as exc:
                record["cleanup_errors"].append(
                    f"kill {record['sandbox_id']}: {type(exc).__name__}: {exc}"
                )
        for client in clients:
            try:
                await asyncio.wait_for(client.close(), args.cleanup_timeout)
            except Exception as exc:
                record["cleanup_errors"].append(f"close: {type(exc).__name__}: {exc}")
    return record


def environment(args):
    supplied = json.loads(args.environment.read_text(encoding="utf-8"))
    if not isinstance(supplied, dict) or not supplied:
        raise ValueError("Environment file must contain a nonempty JSON object")
    nodes = kubectl(args, "get", "nodes")
    revision = subprocess.run(
        ["git", "rev-parse", "HEAD"], capture_output=True, text=True, check=True
    ).stdout.strip()
    dirty = bool(
        subprocess.run(
            ["git", "status", "--porcelain", "--untracked-files=no"],
            capture_output=True,
            text=True,
            check=True,
        ).stdout
    )
    return {
        "type": "environment",
        "schema_version": 1,
        "started_at": datetime.now(timezone.utc).isoformat(),
        "runner_revision": revision,
        "runner_dirty": dirty,
        "python": platform.python_version(),
        "sdk_version": importlib.metadata.version("opensandbox"),
        "kubernetes": kubectl(args, "version"),
        "nodes": [
            {"name": n["metadata"]["name"], "runtime": n["status"]["nodeInfo"]}
            for n in nodes["items"]
        ],
        "settings": {
            k: str(v) if isinstance(v, Path) else v for k, v in vars(args).items()
        },
        "environment": supplied,
    }


async def run(args, output):
    config = ConnectionConfig(
        protocol=args.protocol,
        use_server_proxy=args.server_proxy,
        request_timeout=timedelta(seconds=args.request_timeout),
        retry_policy=RetryPolicy.disabled(),
        disable_metrics=True,
        endpoint_cache_disabled=True,
    )
    manager = await SandboxManager.create(config)
    records = []
    try:
        for index in range(args.warmups + args.samples):
            record = await trial(args, manager, config, index, index < args.warmups)
            records.append(record)
            output.write(json.dumps(record) + "\n")
            output.flush()
            print(
                f"trial {index}: {record['stage']}, success={record['success']}",
                file=sys.stderr,
            )
            if record["cleanup_errors"]:
                print(
                    f"Cleanup failed; stop and inspect sandbox {record['sandbox_id']}",
                    file=sys.stderr,
                )
                break
    finally:
        await manager.close()
    summary = summarize(records)
    summary["completed_requested_trials"] = len(records) == args.warmups + args.samples
    output.write(json.dumps(summary) + "\n")
    output.flush()
    print(json.dumps(summary, indent=2))
    return int(any(not r["success"] or r["cleanup_errors"] for r in records))


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--image",
        required=True,
        help="Immutable workload image digest; must have /bin/sh, cat, sleep",
    )
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--context", required=True)
    parser.add_argument("--environment", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument(
        "--scenario",
        required=True,
        choices=["uncontrolled", "snapshot-cache-hit", "snapshot-cache-miss"],
    )
    parser.add_argument(
        "--prepare-script",
        type=Path,
        help="User-supplied Python cache preparation/verification script",
    )
    parser.add_argument("--protocol", choices=["http", "https"], default="http")
    parser.add_argument("--server-proxy", action="store_true")
    parser.add_argument("--cpu", default="1")
    parser.add_argument("--memory", default="1Gi")
    for name, default in [
        ("samples", 30),
        ("warmups", 3),
        ("setup-timeout", 300),
        ("pause-timeout", 300),
        ("resume-timeout", 300),
        ("prepare-timeout", 300),
        ("cleanup-timeout", 60),
        ("request-timeout", 30),
        ("lifetime", 1800),
    ]:
        parser.add_argument(f"--{name}", type=int, default=default)
    parser.add_argument("--poll-interval", type=float, default=0.2)
    args = parser.parse_args(argv)
    if any(
        getattr(args, n) <= 0
        for n in [
            "samples",
            "setup_timeout",
            "pause_timeout",
            "resume_timeout",
            "prepare_timeout",
            "cleanup_timeout",
            "request_timeout",
            "lifetime",
        ]
    ):
        parser.error("Samples and timeouts must be positive")
    if (
        args.warmups < 0
        or not math.isfinite(args.poll_interval)
        or args.poll_interval <= 0
    ):
        parser.error(
            "Warmups must be nonnegative and polling interval finite and positive"
        )
    if "@sha256:" not in args.image or len(args.image.rsplit("@sha256:", 1)[1]) != 64:
        parser.error("Use an immutable image@sha256:<64 hex characters>")
    try:
        int(args.image.rsplit("@sha256:", 1)[1], 16)
    except ValueError:
        parser.error("Image digest must be hexadecimal")
    if args.scenario != "uncontrolled" and not args.prepare_script:
        parser.error("Cache scenarios require --prepare-script")
    if args.prepare_script and not args.prepare_script.is_file():
        parser.error("Preparation script does not exist")
    return args


def main():
    args = arguments()
    metadata = environment(args)
    with args.output.open("x", encoding="utf-8") as output:
        output.write(json.dumps(metadata) + "\n")
        output.flush()
        return asyncio.run(run(args, output))


if __name__ == "__main__":
    sys.exit(main())
