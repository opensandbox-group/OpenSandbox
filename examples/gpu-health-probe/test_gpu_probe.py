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

"""Unit tests for the GPU health probe's pure logic.

The tests are hermetic: they never touch real NVIDIA devices. Device state is
modeled with files inside a temporary directory and nvidia-smi output is
injected through a fake capture function.

Run from the repository root:

    python -m pytest examples/gpu-health-probe/test_gpu_probe.py
"""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

import pytest

_HERE = Path(__file__).resolve().parent
_SPEC = importlib.util.spec_from_file_location("gpu_probe", _HERE / "gpu_probe.py")
assert _SPEC is not None and _SPEC.loader is not None
gpu_probe = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = gpu_probe
_SPEC.loader.exec_module(gpu_probe)


def _make_dev_dir(path: Path, nodes: List[str]) -> str:
    path.mkdir(parents=True, exist_ok=True)
    for name in nodes:
        (path / name).write_text("", encoding="utf-8")
    return str(path)


def _write_driver_version(path: Path, version: str) -> str:
    content = (
        "NVRM version: NVIDIA UNIX x86_64 Kernel Module  "
        f"{version}  Thu Jul 18 17:07:55 UTC 2024\n"
        "GCC version:  gcc version 11.4.0\n"
    )
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")
    return str(path)


def _fake_capture(
    exit_code: int = 0, stdout: str = "", stderr: str = ""
):
    def capture(command: Any, timeout: float, shell: bool = False):
        return exit_code, stdout, stderr

    return capture


class TestDeviceNodes:
    def test_indexes_sorted_and_filtered(self, tmp_path: Path) -> None:
        dev_dir = _make_dev_dir(
            tmp_path / "dev",
            ["nvidia10", "nvidiactl", "nvidia-uvm", "nvidia2", "nvidia0", "other"],
        )
        assert gpu_probe.list_device_nodes(dev_dir) == [0, 2, 10]

    def test_missing_directory_is_empty(self, tmp_path: Path) -> None:
        assert gpu_probe.list_device_nodes(str(tmp_path / "absent")) == []


class TestSmiParsing:
    PLAIN = (
        "GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaaa)\n"
        "GPU 1: NVIDIA A100-SXM4-40GB (UUID: GPU-bbbb)\n"
    )
    MIG = (
        "GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-cccc)\n"
        "  MIG 1g.5gb Device 0: (UUID: MIG-GPU-cccc/1/0)\n"
        "  MIG 2g.10gb Device 1: (UUID: MIG-GPU-cccc/2/0)\n"
    )

    def test_plain_gpus(self) -> None:
        parsed = gpu_probe.parse_smi_list(self.PLAIN)
        assert parsed["gpuCount"] == 2
        assert parsed["migInstanceCount"] == 0
        assert parsed["gpus"][1]["uuid"] == "GPU-bbbb"
        assert parsed["unparsedLines"] == []

    def test_mig_instances(self) -> None:
        parsed = gpu_probe.parse_smi_list(self.MIG)
        assert parsed["gpuCount"] == 1
        assert parsed["migInstanceCount"] == 2
        assert parsed["migInstances"][0]["profile"] == "1g.5gb"

    def test_unrecognized_lines_reported(self) -> None:
        parsed = gpu_probe.parse_smi_list(self.PLAIN + "No devices were found\n")
        assert parsed["gpuCount"] == 2
        assert parsed["unparsedLines"] == ["No devices were found"]

    def test_empty(self) -> None:
        parsed = gpu_probe.parse_smi_list("")
        assert parsed["gpuCount"] == 0
        assert parsed["migInstanceCount"] == 0


class TestDriverVersion:
    def test_nvrm_line(self, tmp_path: Path) -> None:
        text = "NVRM version: NVIDIA UNIX x86_64 Kernel Module  535.104.05\n"
        assert gpu_probe.driver_version_from_proc(text) == "535.104.05"

    def test_absent(self) -> None:
        assert gpu_probe.driver_version_from_proc("unrelated\n") is None


class TestDeviceLayer:
    def _collect(self, tmp_path: Path, dev_nodes: List[str], expected: Optional[str]):
        dev_dir = _make_dev_dir(tmp_path / "dev", dev_nodes)
        driver_path = _write_driver_version(tmp_path / "proc" / "nvidia" / "version", "535.104.05")
        smi_stdout = "GPU 0: NVIDIA A100 (UUID: GPU-aaaa)\n"
        fake = _fake_capture(0, smi_stdout, "")
        return gpu_probe.collect_device_layer(
            dev_dir=dev_dir,
            driver_version_path=driver_path,
            smi_path="/fake/nvidia-smi",
            expected_count=expected,
            capture=fake,
        )

    def _check(self, layer: Any, name: str) -> Dict[str, Any]:
        for check in layer.checks:
            if check.name == name:
                return check.as_dict()
        raise AssertionError(f"check {name} not found")

    def test_healthy_sandbox_passes(self, tmp_path: Path) -> None:
        layer, observed = self._collect(
            tmp_path, ["nvidiactl", "nvidia-uvm", "nvidia0"], expected="1"
        )
        assert layer.status == gpu_probe.PASS
        assert self._check(layer, "control-node")["status"] == gpu_probe.PASS
        assert self._check(layer, "uvm-node")["status"] == gpu_probe.PASS
        assert self._check(layer, "requested-count")["status"] == gpu_probe.PASS
        assert self._check(layer, "smi-inventory-consistency")["status"] == gpu_probe.PASS
        assert observed["deviceNodeCount"] == 1
        assert observed["driverVersion"] == "535.104.05"
        assert observed["smiGpuCount"] == 1

    def test_no_devices_fails(self, tmp_path: Path) -> None:
        layer, _ = self._collect(tmp_path, [], expected=None)
        assert layer.status == gpu_probe.FAIL
        assert self._check(layer, "control-node")["status"] == gpu_probe.FAIL
        assert self._check(layer, "device-nodes")["status"] == gpu_probe.FAIL

    def test_count_drift_fails(self, tmp_path: Path) -> None:
        layer, _ = self._collect(
            tmp_path,
            ["nvidiactl", "nvidia0", "nvidia1", "nvidia2"],
            expected="2",
        )
        assert layer.status == gpu_probe.FAIL
        detail = self._check(layer, "requested-count")["detail"]
        assert "requested 2" in detail and "observed 3" in detail

    def test_all_sentinel_skips_count(self, tmp_path: Path) -> None:
        layer, _ = self._collect(
            tmp_path, ["nvidiactl", "nvidia0"], expected="all"
        )
        assert layer.status == gpu_probe.PASS
        assert self._check(layer, "requested-count")["status"] == gpu_probe.SKIPPED

    def test_missing_uvm_warns_not_fails(self, tmp_path: Path) -> None:
        layer, _ = self._collect(tmp_path, ["nvidiactl", "nvidia0"], expected=None)
        assert layer.status == gpu_probe.PASS
        assert self._check(layer, "uvm-node")["status"] == gpu_probe.WARN

    def test_partial_attach_signature_warns(self, tmp_path: Path) -> None:
        dev_dir = _make_dev_dir(
            tmp_path / "dev", ["nvidiactl", "nvidia0", "nvidia1"]
        )
        driver_path = _write_driver_version(tmp_path / "proc" / "nvidia" / "version", "550.54.14")
        smi_stdout = "GPU 0: NVIDIA H100 (UUID: GPU-bbbb)\n"
        layer, _ = gpu_probe.collect_device_layer(
            dev_dir=dev_dir,
            driver_version_path=driver_path,
            smi_path="/fake/nvidia-smi",
            expected_count=None,
            capture=_fake_capture(0, smi_stdout, ""),
        )
        check = self._check(layer, "smi-inventory-consistency")
        assert check["status"] == gpu_probe.WARN
        assert "partial attach" in check["detail"]

    def _mig_case(self, tmp_path: Path, expected: str):
        dev_dir = _make_dev_dir(tmp_path / "dev", ["nvidiactl", "nvidia-uvm", "nvidia0"])
        driver_path = _write_driver_version(tmp_path / "proc" / "nvidia" / "version", "550.54.14")
        smi_stdout = (
            "GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-cccc)\n"
            "  MIG 1g.5gb Device 0: (UUID: MIG-GPU-cccc/1/0)\n"
            "  MIG 1g.5gb Device 1: (UUID: MIG-GPU-cccc/2/0)\n"
        )
        return gpu_probe.collect_device_layer(
            dev_dir=dev_dir,
            driver_version_path=driver_path,
            smi_path="/fake/nvidia-smi",
            expected_count=expected,
            capture=_fake_capture(0, smi_stdout, ""),
        )

    def test_mig_count_compares_mig_instances(self, tmp_path: Path) -> None:
        # Two MIG slices on one physical card: requested 2 must pass.
        layer, _ = self._mig_case(tmp_path, expected="2")
        assert layer.status == gpu_probe.PASS
        check = self._check(layer, "requested-count")
        assert check["status"] == gpu_probe.PASS
        assert "2 MIG instance(s)" in check["detail"]

    def test_mig_count_mismatch_fails(self, tmp_path: Path) -> None:
        layer, _ = self._mig_case(tmp_path, expected="1")
        assert layer.status == gpu_probe.FAIL
        assert "2 MIG instance(s)" in self._check(layer, "requested-count")["detail"]

    def test_smi_unavailable_warns(self, tmp_path: Path) -> None:
        dev_dir = _make_dev_dir(tmp_path / "dev", ["nvidiactl", "nvidia0"])
        driver_path = _write_driver_version(tmp_path / "proc" / "nvidia" / "version", "550.54.14")
        layer, observed = gpu_probe.collect_device_layer(
            dev_dir=dev_dir,
            driver_version_path=driver_path,
            smi_path="/fake/nvidia-smi",
            expected_count=None,
            capture=_fake_capture(127, "", "not found"),
        )
        assert layer.status == gpu_probe.PASS
        assert self._check(layer, "smi-inventory")["status"] == gpu_probe.WARN
        assert observed["smiGpuCount"] is None


class TestComputeLayer:
    def test_skip(self) -> None:
        layer = gpu_probe.collect_compute_layer("skip", 10, capture=_fake_capture())
        assert layer.status == gpu_probe.PASS
        assert layer.checks[0].status == gpu_probe.SKIPPED

    def test_auto_without_torch_skips(self) -> None:
        fake = _fake_capture(1, "", "ModuleNotFoundError: No module named 'torch'")
        layer = gpu_probe.collect_compute_layer("auto", 10, capture=fake)
        assert layer.status == gpu_probe.PASS
        assert layer.checks[0].status == gpu_probe.SKIPPED
        assert "--compute" in layer.checks[0].detail

    def test_auto_broken_torch_import_fails(self) -> None:
        fake = _fake_capture(
            1, "", "OSError: libcuda.so.1: cannot open shared object file"
        )
        layer = gpu_probe.collect_compute_layer("auto", 10, capture=fake)
        assert layer.status == gpu_probe.FAIL
        assert "installed but cannot be imported" in layer.checks[0].detail

    def test_auto_import_timeout_fails(self) -> None:
        fake = _fake_capture(124, "", "timed out")
        layer = gpu_probe.collect_compute_layer("auto", 10, capture=fake)
        assert layer.status == gpu_probe.FAIL
        assert "timed out" in layer.checks[0].detail

    def test_auto_failure_attributed(self) -> None:
        calls: List[Tuple[Any, float, bool]] = []

        def capture(command: Any, timeout: float, shell: bool = False):
            calls.append((command, timeout, shell))
            if len(calls) == 1:
                return 0, "", ""
            return 1, "", "torch.cuda.is_available() is False"

        layer = gpu_probe.collect_compute_layer("auto", 10, capture=capture)
        assert layer.status == gpu_probe.FAIL
        detail = layer.checks[0].detail
        assert "exit 1" in detail and "torch.cuda.is_available()" in detail

    def test_auto_import_consuming_budget_fails(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        stamps = iter([0.0, 100.0])
        monkeypatch.setattr(gpu_probe, "_monotonic", lambda: next(stamps))
        fake = _fake_capture(0, "", "")
        layer = gpu_probe.collect_compute_layer("auto", 10, capture=fake)
        assert layer.status == gpu_probe.FAIL
        assert "--compute-timeout" in layer.checks[0].detail

    def test_auto_shares_budget_between_import_and_smoke(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        stamps = iter([0.0, 4.0])
        monkeypatch.setattr(gpu_probe, "_monotonic", lambda: next(stamps))
        seen: List[float] = []

        def capture(command: Any, timeout: float, shell: bool = False):
            seen.append(timeout)
            if len(seen) == 1:
                return 0, "", ""
            return 0, json.dumps({"deviceCount": 1, "checksums": {}}), ""

        layer = gpu_probe.collect_compute_layer("auto", 10, capture=capture)
        assert layer.status == gpu_probe.PASS
        assert seen == [10, 6.0]

    def test_auto_device_count_shortfall_fails(self) -> None:
        calls: List[int] = []

        def capture(command: Any, timeout: float, shell: bool = False):
            calls.append(1)
            if len(calls) == 1:
                return 0, "", ""
            return 0, json.dumps({"deviceCount": 1, "checksums": {"cuda:0": 1.0}}), ""

        layer = gpu_probe.collect_compute_layer(
            "auto", 10, expected_count="2", capture=capture
        )
        assert layer.status == gpu_probe.FAIL
        check = {c.name: c for c in layer.checks}["compute-count"]
        assert check.status == gpu_probe.FAIL
        assert "PyTorch sees 1 device(s) but 2 were requested" in check.detail

    def test_auto_device_count_surplus_warns(self) -> None:
        calls: List[int] = []

        def capture(command: Any, timeout: float, shell: bool = False):
            calls.append(1)
            if len(calls) == 1:
                return 0, "", ""
            return 0, json.dumps({"deviceCount": 3, "checksums": {}}), ""

        layer = gpu_probe.collect_compute_layer(
            "auto", 10, expected_count="1", capture=capture
        )
        assert layer.status == gpu_probe.PASS
        check = {c.name: c for c in layer.checks}["compute-count"]
        assert check.status == gpu_probe.WARN

    def test_torch_device_count_parsing(self) -> None:
        assert gpu_probe._torch_device_count('{"deviceCount": 2}') == 2
        assert gpu_probe._torch_device_count("not json") is None
        assert gpu_probe._torch_device_count('{"deviceCount": true}') is None

    def test_custom_command_pass_and_fail(self) -> None:
        ok = gpu_probe.collect_compute_layer(
            "echo ok", 10, capture=_fake_capture(0, "ok\n", "")
        )
        assert ok.status == gpu_probe.PASS
        bad = gpu_probe.collect_compute_layer(
            "exit 3", 10, capture=_fake_capture(3, "", "boom")
        )
        assert bad.status == gpu_probe.FAIL


class TestRunCapture:
    def test_timeout_terminates_process_group(self) -> None:
        # A compound command that leaves a background child behind: the probe
        # must not return while descendants keep running.
        import time

        start = time.monotonic()
        exit_code, _, stderr = gpu_probe.run_capture(
            "sleep 30 & wait", 1, shell=True
        )
        elapsed = time.monotonic() - start
        assert exit_code == 124
        assert "timed out" in stderr
        assert elapsed < 15

    def test_missing_executable_maps_to_127(self) -> None:
        exit_code, _, _ = gpu_probe.run_capture(
            ["/nonexistent/probe-binary"], 5
        )
        assert exit_code == 127

    def test_success_captures_output(self) -> None:
        exit_code, stdout, _ = gpu_probe.run_capture(
            [sys.executable, "-c", "print('ok')"], 30
        )
        assert exit_code == 0
        assert stdout.strip() == "ok"


class TestReport:
    def test_exit_codes(self, tmp_path: Path) -> None:
        device = gpu_probe.Layer("device", [gpu_probe.Check("c", gpu_probe.PASS, "ok")])
        compute = gpu_probe.Layer("compute", [gpu_probe.Check("c", gpu_probe.PASS, "ok")])
        report = gpu_probe.build_report(device, compute, {}, None)
        assert report["status"] == gpu_probe.PASS
        assert report["exitCode"] == gpu_probe.EXIT_OK

        failed_device = gpu_probe.Layer(
            "device", [gpu_probe.Check("c", gpu_probe.FAIL, "no devices")]
        )
        failed_compute = gpu_probe.Layer(
            "compute", [gpu_probe.Check("c", gpu_probe.FAIL, "kernel failed")]
        )
        report = gpu_probe.build_report(failed_device, failed_compute, {}, None)
        assert report["exitCode"] == gpu_probe.EXIT_DEVICE_FAILED

        report = gpu_probe.build_report(device, failed_compute, {}, None)
        assert report["exitCode"] == gpu_probe.EXIT_COMPUTE_FAILED

    def test_expected_count_shapes(self) -> None:
        device = gpu_probe.Layer("device", [])
        compute = gpu_probe.Layer("compute", [])
        assert gpu_probe.build_report(device, compute, {}, None)["expectedCount"] is None
        assert gpu_probe.build_report(device, compute, {}, "all")["expectedCount"] == "all"
        assert gpu_probe.build_report(device, compute, {}, "4")["expectedCount"] == 4

    def test_warnings_collected(self) -> None:
        device = gpu_probe.Layer(
            "device",
            [
                gpu_probe.Check("a", gpu_probe.PASS, "fine"),
                gpu_probe.Check("b", gpu_probe.WARN, "something to watch"),
            ],
        )
        compute = gpu_probe.Layer("compute", [])
        report = gpu_probe.build_report(device, compute, {}, None)
        assert report["warnings"] == ["device.b: something to watch"]


class TestMain:
    def test_json_stdout_and_exit_code(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: Any
    ) -> None:
        dev_dir = _make_dev_dir(tmp_path / "dev", ["nvidiactl", "nvidia0"])
        driver_path = _write_driver_version(tmp_path / "proc" / "nvidia" / "version", "550.54.14")
        monkeypatch.setattr(
            gpu_probe, "collect_compute_layer", lambda *a, **k: gpu_probe.Layer(
                "compute", [gpu_probe.Check("compute-probe", gpu_probe.SKIPPED, "skipped")]
            )
        )
        exit_code = gpu_probe.main(
            [
                "--expect", "1",
                "--dev-dir", dev_dir,
                "--driver-version-path", driver_path,
                "--no-smi",
                "--quiet",
            ]
        )
        assert exit_code == gpu_probe.EXIT_OK
        payload = json.loads(capsys.readouterr().out)
        assert payload["probe"] == gpu_probe.PROBE_NAME
        assert payload["status"] == gpu_probe.PASS
        assert payload["observed"]["deviceNodeCount"] == 1

    def test_invalid_expect_is_probe_error(
        self, tmp_path: Path, capsys: Any
    ) -> None:
        # Argument errors must not reuse exit 2, which the contract reserves
        # for device-layer failures.
        exit_code = gpu_probe.main(["--expect", "0", "--quiet"])
        assert exit_code == gpu_probe.EXIT_PROBE_ERROR
        payload = json.loads(capsys.readouterr().out)
        assert payload["status"] == "error"
        assert "--expect" in payload["error"]

    def test_unknown_argument_is_probe_error(self, capsys: Any) -> None:
        exit_code = gpu_probe.main(["--bogus", "--quiet"])
        assert exit_code == gpu_probe.EXIT_PROBE_ERROR
        payload = json.loads(capsys.readouterr().out)
        assert payload["status"] == "error"

    @pytest.mark.parametrize("value", ["nan", "inf", "-inf", "0", "-5"])
    def test_non_positive_or_non_finite_timeout_rejected(
        self, value: str, capsys: Any
    ) -> None:
        # These values must be rejected before any subprocess is launched:
        # nan/inf would act as an unbounded timeout inside communicate().
        exit_code = gpu_probe.main(["--compute-timeout", value, "--quiet"])
        assert exit_code == gpu_probe.EXIT_PROBE_ERROR
        payload = json.loads(capsys.readouterr().out)
        assert "compute-timeout" in payload["error"]

    def test_probe_never_crashes(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: Any
    ) -> None:
        def boom(*a: Any, **k: Any) -> Any:
            raise RuntimeError("unexpected")

        monkeypatch.setattr(gpu_probe, "collect_device_layer", boom)
        exit_code = gpu_probe.main(["--quiet"])
        assert exit_code == gpu_probe.EXIT_PROBE_ERROR
        payload = json.loads(capsys.readouterr().out)
        assert payload["status"] == "error"


if __name__ == "__main__":
    sys.exit(pytest.main([__file__]))
