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

"""Offline coverage for benchmark measurement, failures, and resource cleanup."""

import asyncio
import io
import json
from types import SimpleNamespace
from typing import cast
from unittest.mock import AsyncMock

import httpx
import pytest
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException

from benchmarks import kubernetes_resume as bench


@pytest.fixture
def args(tmp_path):
    return bench.arguments(
        [
            "--image",
            "example/image@sha256:" + "a" * 64,
            "--namespace",
            "test",
            "--context",
            "test-cluster",
            "--environment",
            str(tmp_path / "environment.json"),
            "--output",
            str(tmp_path / "result.jsonl"),
            "--scenario",
            "uncontrolled",
            "--warmups",
            "0",
            "--samples",
            "2",
            "--poll-interval",
            "0.001",
        ]
    )


def test_summary_excludes_warmups_and_partial_failed_timings():
    records = [
        {"warmup": False, "success": True, **dict.fromkeys(bench.TIMINGS, n)}
        for n in range(1, 21)
    ]
    records += [
        {"warmup": True, "success": True, **dict.fromkeys(bench.TIMINGS, 10000)},
        {"warmup": False, "success": False, "api_ack_ms": 90000},
        {"warmup": True, "success": False, "cleanup_errors": ["failed cleanup"]},
    ]
    summary = bench.summarize(records)
    assert summary["measured_trials"] == 21
    assert summary["failure_rate"] == 1 / 21
    assert summary["warmup_failures"] == summary["cleanup_failures"] == 1
    assert summary["timings"]["command_ms"] == {"p50_ms": 10, "p95_ms": 19}
    assert summary["timings"]["api_ack_ms"] == summary["timings"]["command_ms"]
    empty = bench.summarize([{"warmup": False, "success": False}])
    assert empty["timings"]["command_ms"] == {"p50_ms": None, "p95_ms": None}
    assert empty["failure_rate"] == 1


@pytest.mark.parametrize(
    "option,value",
    [
        ("--samples", "0"),
        ("--warmups", "-1"),
        ("--poll-interval", "nan"),
        ("--resume-timeout", "0"),
        ("--scenario", "snapshot-cache-hit"),
        ("--image", "example/image:latest"),
    ],
)
def test_invalid_parameters(args, option, value):
    argv = [
        "--image",
        args.image,
        "--namespace",
        args.namespace,
        "--context",
        args.context,
        "--environment",
        str(args.environment),
        "--output",
        str(args.output),
        "--scenario",
        "uncontrolled",
        option,
        value,
    ]
    with pytest.raises(SystemExit):
        bench.arguments(argv)


@pytest.fixture
def lifecycle(monkeypatch):
    source = SimpleNamespace(id="sandbox", close=AsyncMock())
    restored = SimpleNamespace(id="sandbox", close=AsyncMock())
    manager = SimpleNamespace(pause_sandbox=AsyncMock(), resume_sandbox=AsyncMock())
    monkeypatch.setattr(bench, "create_sandbox", AsyncMock(return_value="sandbox"))
    monkeypatch.setattr(bench, "verify_backend", AsyncMock(return_value="uid"))
    monkeypatch.setattr(
        bench, "snapshot_evidence", AsyncMock(return_value={"name": "snapshot"})
    )
    monkeypatch.setattr(
        bench, "pod_evidence", AsyncMock(return_value=[{"node": "node"}])
    )
    monkeypatch.setattr(bench, "wait_deleted_pods", AsyncMock())
    monkeypatch.setattr(bench, "wait_state", AsyncMock())
    monkeypatch.setattr(bench, "prepare", AsyncMock(return_value=None))
    monkeypatch.setattr(bench, "cleanup", AsyncMock())
    monkeypatch.setattr(
        bench.Sandbox, "connect", AsyncMock(side_effect=[source, restored])
    )
    stored = []

    async def command(sandbox, text, timeout):
        if text.startswith("printf"):
            stored.append(text.split("'")[3])
            return ""
        return stored[0]

    monkeypatch.setattr(bench, "check_command", command)
    return manager, source, restored


async def test_success_uses_fresh_endpoints_and_shared_timer(
    args, lifecycle, monkeypatch
):
    manager, source, restored = lifecycle
    clock = iter([0, 0.01, 0.02, 0.025, 0.03, 0.04])
    monkeypatch.setattr(bench.time, "perf_counter", lambda: next(clock))
    record = await bench.trial(args, manager, ConnectionConfig(), 0, False)
    assert record["success"], record
    assert [record[key] for key in bench.TIMINGS] == [10, 20, 30, 40]
    assert cast(AsyncMock, bench.Sandbox.connect).await_count == 2
    assert (
        cast(AsyncMock, bench.Sandbox.connect)
        .await_args_list[1]
        .kwargs["connect_timeout"]
        .total_seconds()
        == 299.975
    )
    cast(AsyncMock, bench.wait_deleted_pods).assert_awaited_once_with(args, "uid")
    cast(AsyncMock, bench.cleanup).assert_awaited_once_with(
        manager, "sandbox", args.poll_interval
    )
    source.close.assert_awaited_once()
    restored.close.assert_awaited_once()


async def test_marker_mismatch_is_failed_trial_with_partial_timings(
    args, lifecycle, monkeypatch
):
    manager, source, restored = lifecycle
    monkeypatch.setattr(
        bench, "check_command", AsyncMock(side_effect=["", "wrong-marker"])
    )
    record = await bench.trial(args, manager, ConnectionConfig(), 0, False)
    assert not record["success"] and record["stage"] == "command"
    assert "marker" in record["error"]
    assert "ready_ms" in record and "command_ms" not in record
    source.close.assert_awaited_once()
    restored.close.assert_awaited_once()


async def test_resume_timeout_still_cleans_up(args, lifecycle, monkeypatch):
    manager, source, restored = lifecycle
    args.resume_timeout = 0.01

    async def hang(*unused):
        await asyncio.Event().wait()

    manager.resume_sandbox.side_effect = hang
    record = await bench.trial(args, manager, ConnectionConfig(), 0, False)
    assert not record["success"] and record["stage"] == "resume_api"
    assert "TimeoutError" in record["error"]
    cast(AsyncMock, bench.cleanup).assert_awaited_once()
    source.close.assert_awaited_once()
    restored.close.assert_not_awaited()


async def test_connect_failure_after_create_keeps_id_for_cleanup(
    args, lifecycle, monkeypatch
):
    manager, _, _ = lifecycle
    monkeypatch.setattr(
        bench.Sandbox, "connect", AsyncMock(side_effect=RuntimeError("unreachable"))
    )
    record = await bench.trial(args, manager, ConnectionConfig(), 0, False)
    assert record["sandbox_id"] == "sandbox" and not record["success"]
    cast(AsyncMock, bench.cleanup).assert_awaited_once_with(
        manager, "sandbox", args.poll_interval
    )


async def test_unknown_create_outcome_stops_with_metadata_recovery_hint(
    args, lifecycle, monkeypatch
):
    manager, _, _ = lifecycle
    monkeypatch.setattr(bench, "create_sandbox", AsyncMock(side_effect=TimeoutError()))
    record = await bench.trial(args, manager, ConnectionConfig(), 0, False)
    assert record["cleanup_errors"]
    assert record["run_id"] in record["cleanup_errors"][0]
    cast(AsyncMock, bench.cleanup).assert_not_awaited()


async def test_run_flushes_and_stops_on_cleanup_failure(args, monkeypatch):
    manager = SimpleNamespace(close=AsyncMock())
    monkeypatch.setattr(bench.SandboxManager, "create", AsyncMock(return_value=manager))
    monkeypatch.setattr(
        bench,
        "trial",
        AsyncMock(
            return_value={
                "type": "trial",
                "warmup": False,
                "success": False,
                "sandbox_id": "leaked",
                "stage": "pause",
                "cleanup_errors": ["delete timeout"],
            }
        ),
    )
    output = io.StringIO()
    assert await bench.run(args, output) == 1
    cast(AsyncMock, bench.trial).assert_awaited_once()
    manager.close.assert_awaited_once()
    records = [json.loads(line) for line in output.getvalue().splitlines()]
    assert records[1]["completed_requested_trials"] is False
    assert records[1]["cleanup_failures"] == 1


async def test_cleanup_waits_for_not_found():
    manager = SimpleNamespace(
        kill_sandbox=AsyncMock(),
        get_sandbox_info=AsyncMock(
            side_effect=[
                SimpleNamespace(),
                SandboxApiException(status_code=404),
            ]
        ),
    )
    await bench.cleanup(manager, "sandbox", 0)
    assert manager.get_sandbox_info.await_count == 2


async def test_create_request_captures_id_without_readiness(args, monkeypatch):
    def handle(request):
        body = json.loads(request.content)
        assert body["image"] == {"uri": args.image}
        assert body["resourceLimits"] == {"cpu": "1", "memory": "1Gi"}
        assert body["metadata"]["benchmark_run"] == "run-1"
        assert request.headers["OPEN-SANDBOX-API-KEY"] == "test-key"
        assert str(request.url) == "http://test-server/v1/sandboxes"
        return httpx.Response(202, json={"id": "sandbox"})

    original = httpx.AsyncClient
    monkeypatch.setattr(
        bench.httpx,
        "AsyncClient",
        lambda **kw: original(transport=httpx.MockTransport(handle), **kw),
    )
    assert (
        await bench.create_sandbox(
            args, ConnectionConfig(domain="test-server", api_key="test-key"), "run-1"
        )
        == "sandbox"
    )


@pytest.mark.parametrize(
    "spec",
    [
        {"poolRef": "pool"},
        {"replicas": 2},
        {
            "template": {
                "metadata": {
                    "annotations": {
                        "sandbox.opensandbox.io/checkpoint-provider": "qemu"
                    }
                }
            }
        },
        {
            "template": {
                "spec": {"containers": [{"volumeMounts": [{"mountPath": "/tmp"}]}]}
            }
        },
    ],
)
async def test_rejects_incompatible_backends_or_volume_marker(args, monkeypatch, spec):
    monkeypatch.setattr(
        bench, "kubectl", lambda *unused: {"spec": spec, "metadata": {"uid": "uid"}}
    )
    with pytest.raises(RuntimeError):
        await bench.verify_backend(args, "sandbox")


async def test_pod_deletion_ignores_unrelated_pods(args, monkeypatch):
    results = iter(
        [
            {"items": [{"metadata": {"ownerReferences": [{"uid": "source"}]}}]},
            {"items": [{"metadata": {"ownerReferences": [{"uid": "other"}]}}]},
        ]
    )
    monkeypatch.setattr(bench, "kubectl", lambda *unused: next(results))
    await bench.wait_deleted_pods(args, "source")


async def test_preparation_records_evidence(args, tmp_path):
    args.prepare_script = tmp_path / "prepare.py"
    args.prepare_script.write_text(
        "import sys\nprint('verified: ' + ' '.join(sys.argv[1:]))\n"
    )
    args.scenario = "snapshot-cache-hit"
    evidence = await bench.prepare(args, "sandbox")
    assert evidence == "verified: sandbox test test-cluster snapshot-cache-hit"


async def test_empty_cache_evidence_prevents_resume(args, tmp_path):
    args.prepare_script = tmp_path / "prepare.py"
    args.prepare_script.write_text("pass\n")
    args.scenario = "snapshot-cache-hit"
    with pytest.raises(RuntimeError, match="evidence"):
        await bench.prepare(args, "sandbox")


@pytest.mark.parametrize(
    "status",
    [
        {"phase": "Succeed", "format": "rootfs-v1"},
        {"phase": "Succeed"},
    ],
)
async def test_snapshot_evidence_uses_owned_ready_rootfs(args, monkeypatch, status):
    status = {**status, "containers": [{"image": "snapshot@sha256:abc"}]}
    items = [
        {"metadata": {"name": "other", "ownerReferences": [{"uid": "other"}]}},
        {
            "metadata": {"name": "snapshot", "ownerReferences": [{"uid": "source"}]},
            "status": status,
        },
    ]
    monkeypatch.setattr(bench, "kubectl", lambda *unused: {"items": items})
    assert await bench.snapshot_evidence(args, "source") == {
        "name": "snapshot",
        "status": status,
    }


async def test_snapshot_evidence_rejects_qemu(args, monkeypatch):
    item = {
        "metadata": {"name": "snapshot", "ownerReferences": [{"uid": "source"}]},
        "status": {"phase": "Succeed", "format": "qemu-v1"},
    }
    monkeypatch.setattr(bench, "kubectl", lambda *unused: {"items": [item]})
    with pytest.raises(RuntimeError, match="rootfs"):
        await bench.snapshot_evidence(args, "source")


async def test_pod_evidence_retains_node_and_actual_image(args, monkeypatch):
    pod = {
        "metadata": {"name": "restored", "ownerReferences": [{"uid": "source"}]},
        "spec": {
            "nodeName": "worker-2",
            "containers": [
                {
                    "name": "sandbox",
                    "image": "snapshot@sha256:abc",
                    "imagePullPolicy": "IfNotPresent",
                }
            ],
        },
        "status": {"containerStatuses": [{"imageID": "containerd://sha256:abc"}]},
    }
    monkeypatch.setattr(bench, "kubectl", lambda *unused: {"items": [pod]})
    evidence = await bench.pod_evidence(args, "source")
    assert evidence[0]["node"] == "worker-2"
    assert evidence[0]["containers"][0]["image"] == "snapshot@sha256:abc"
    assert evidence[0]["containerStatuses"][0]["imageID"] == "containerd://sha256:abc"
