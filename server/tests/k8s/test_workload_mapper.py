# Copyright 2026 Alibaba Group Holding Ltd.
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

import json
from types import SimpleNamespace

import pytest

from opensandbox_server.services.k8s.workload_mapper import (
    _build_sandbox_from_workload,
    _extract_platform_from_workload,
)


class _WorkloadProvider:
    @staticmethod
    def get_expiration(_workload):
        return None

    @staticmethod
    def get_status(_workload):
        return {
            "state": "Running",
            "reason": "",
            "message": "Running",
            "last_transition_at": None,
        }


class TestBuildSandboxFromWorkload:
    def test_restores_extensions_from_annotations(self):
        workload = {
            "metadata": {
                "labels": {"opensandbox.io/id": "sandbox-1"},
                "annotations": {
                    "opensandbox.io/extensions.custom-label": "中文数据",
                    "opensandbox.io/access-renew-extend-seconds": "1800",
                },
                "creationTimestamp": "2026-06-22T00:00:00Z",
            },
            "spec": {"template": {"spec": {"containers": [{"image": "python:3.11", "command": ["python"]}]}}},
        }

        sandbox = _build_sandbox_from_workload(workload, _WorkloadProvider())

        assert sandbox.extensions == {"opensandbox.extensions.custom-label": "中文数据"}

    def test_returns_confirmed_pool_allocation_for_dict_workload(self):
        sandbox = _build_sandbox_from_workload(_allocated_workload(), _WorkloadProvider())

        assert sandbox.allocation is not None
        assert sandbox.allocation.model_dump(by_alias=True) == {
            "mode": "pool",
            "poolRef": "pool-runc",
            "state": "allocated",
        }

    def test_returns_confirmed_pool_allocation_for_object_workload(self):
        workload = SimpleNamespace(
            metadata=SimpleNamespace(
                labels={"opensandbox.io/id": "sandbox-1"},
                annotations={
                    "sandbox.opensandbox.io/alloc-status": json.dumps(
                        {"pods": ["pod-1"], "poolRef": "pool-runc", "generation": 4}
                    )
                },
                finalizers=["pool.sandbox.opensandbox.io/pool-allocation"],
                deletion_timestamp=None,
                creation_timestamp="2026-06-22T00:00:00Z",
            ),
            spec=SimpleNamespace(pool_ref="pool-runc", containers=[]),
            status=SimpleNamespace(allocated=1),
        )

        sandbox = _build_sandbox_from_workload(workload, _WorkloadProvider())

        assert sandbox.allocation is not None
        assert sandbox.allocation.pool_ref == "pool-runc"

    @pytest.mark.parametrize(
        ("name", "mutate"),
        [
            ("wrong annotation pool reference", lambda w: _set_annotation(w, {"pods": ["pod-1"], "poolRef": "other", "generation": 4})),
            ("missing annotation pool reference", lambda w: _set_annotation(w, {"pods": ["pod-1"], "generation": 4})),
            ("legacy pods-only annotation", lambda w: _set_annotation(w, {"pods": ["pod-1"]})),
            ("deleting", lambda w: w["metadata"].update({"deletionTimestamp": "2026-06-23T00:00:00Z"})),
            ("missing finalizer", lambda w: w["metadata"].update({"finalizers": []})),
            ("missing allocation annotation", lambda w: w["metadata"].update({"annotations": {}})),
            ("invalid annotation JSON", lambda w: w["metadata"]["annotations"].update({"sandbox.opensandbox.io/alloc-status": "{"})),
            ("empty pods", lambda w: _set_annotation(w, {"pods": [], "poolRef": "pool-runc", "generation": 4})),
            ("empty pod name", lambda w: _set_annotation(w, {"pods": [""], "poolRef": "pool-runc", "generation": 4})),
            ("invalid pod name", lambda w: _set_annotation(w, {"pods": ["Pod-1"], "poolRef": "pool-runc", "generation": 4})),
            ("duplicate pod names", lambda w: _set_annotation(w, {"pods": ["pod-1", "pod-1"], "poolRef": "pool-runc", "generation": 4})),
            ("allocated count mismatch", lambda w: w["status"].update({"allocated": 2})),
            ("wildcard pool reference", lambda w: w["spec"].update({"poolRef": "*"})),
            ("non-pool workload", lambda w: w["spec"].pop("poolRef")),
        ],
    )
    def test_omits_unconfirmed_pool_allocation(self, name, mutate):
        workload = _allocated_workload()
        mutate(workload)

        sandbox = _build_sandbox_from_workload(workload, _WorkloadProvider())

        assert sandbox.allocation is None, name

    def test_renewal_generation_drift_remains_confirmed(self):
        workload = _allocated_workload()
        workload["metadata"]["generation"] = 12
        _set_annotation(
            workload,
            {"pods": ["pod-1"], "poolRef": "pool-runc", "generation": 4},
        )

        sandbox = _build_sandbox_from_workload(workload, _WorkloadProvider())

        assert sandbox.allocation is not None
        assert sandbox.allocation.pool_ref == "pool-runc"

    @pytest.mark.parametrize(
        "annotation_key",
        [
            "sandbox.opensandbox.io/alloc-release",
            "sandbox.opensandbox.io/alloc-released",
        ],
    )
    def test_omits_allocation_when_release_state_intersects_allocation(
        self, annotation_key
    ):
        workload = _allocated_workload()
        workload["metadata"]["annotations"][annotation_key] = json.dumps(
            {"pods": ["pod-1"]}
        )

        sandbox = _build_sandbox_from_workload(workload, _WorkloadProvider())

        assert sandbox.allocation is None

    @pytest.mark.parametrize(
        "annotation_key",
        [
            "sandbox.opensandbox.io/alloc-release",
            "sandbox.opensandbox.io/alloc-released",
        ],
    )
    def test_omits_allocation_when_release_state_is_malformed(self, annotation_key):
        workload = _allocated_workload()
        workload["metadata"]["annotations"][annotation_key] = "{"

        sandbox = _build_sandbox_from_workload(workload, _WorkloadProvider())

        assert sandbox.allocation is None

    @pytest.mark.parametrize(
        "annotation_key",
        [
            "sandbox.opensandbox.io/alloc-release",
            "sandbox.opensandbox.io/alloc-released",
        ],
    )
    def test_returns_allocation_for_non_intersecting_release_state(self, annotation_key):
        workload = _allocated_workload()
        workload["metadata"]["annotations"][annotation_key] = json.dumps(
            {"pods": ["pod-2"]}
        )

        sandbox = _build_sandbox_from_workload(workload, _WorkloadProvider())

        assert sandbox.allocation is not None
        assert sandbox.allocation.pool_ref == "pool-runc"


def _allocated_workload(pool_ref="pool-runc"):
    return {
        "metadata": {
            "labels": {"opensandbox.io/id": "sandbox-1"},
            "annotations": {
                "sandbox.opensandbox.io/alloc-status": json.dumps(
                    {"pods": ["pod-1"], "poolRef": pool_ref, "generation": 4}
                )
            },
            "finalizers": ["pool.sandbox.opensandbox.io/pool-allocation"],
            "creationTimestamp": "2026-06-22T00:00:00Z",
        },
        "spec": {"poolRef": pool_ref, "template": None},
        "status": {"allocated": 1},
    }


def _set_annotation(workload, allocation):
    workload["metadata"]["annotations"]["sandbox.opensandbox.io/alloc-status"] = json.dumps(allocation)


class TestExtractPlatformFromWorkload:
    """Regression tests for _extract_platform_from_workload.

    The BatchSandbox CRD declares spec.template as an optional preserve-unknown-fields
    object. In pool mode, the BatchSandbox CR is created with only ``poolRef`` and
    ``taskTemplate`` under spec; the Kubernetes API server may then return the object
    with ``spec.template`` explicitly set to ``None`` (because the field is part of the
    schema but unset). Earlier code did ``spec.get("template", {}).get("spec")`` which
    crashed in that case because the default ``{}`` is only returned when the key is
    absent, not when its value is ``None``.
    """

    def test_pool_mode_workload_with_null_template_returns_none(self):
        """Pool-mode BatchSandbox CR has spec.template == None; must not crash."""
        workload = {
            "metadata": {"name": "sb-1", "namespace": "opensandbox-system"},
            "spec": {
                "replicas": 1,
                "poolRef": "pool-runc",
                "template": None,  # <-- this used to crash
                "taskTemplate": {},
            },
            "status": {"replicas": 1, "ready": 1, "allocated": 1},
        }
        # Should return None (no platform info), not raise.
        assert _extract_platform_from_workload(workload) is None

    def test_pool_mode_workload_without_template_key_returns_none(self):
        """Pool-mode BatchSandbox CR may also omit spec.template entirely."""
        workload = {
            "metadata": {"name": "sb-1"},
            "spec": {
                "replicas": 1,
                "poolRef": "pool-runc",
            },
        }
        assert _extract_platform_from_workload(workload) is None

    def test_template_mode_with_full_platform_returns_platform(self):
        """Template-mode workload with nodeSelector returns the declared platform."""
        workload = {
            "metadata": {"name": "sb-1"},
            "spec": {
                "replicas": 1,
                "template": {
                    "spec": {
                        "nodeSelector": {
                            "kubernetes.io/os": "linux",
                            "kubernetes.io/arch": "amd64",
                        },
                    },
                },
            },
        }
        platform = _extract_platform_from_workload(workload)
        assert platform is not None
        assert platform.os == "linux"
        assert platform.arch == "amd64"

    def test_pod_template_alias_still_works(self):
        """Some workload types use ``podTemplate`` instead of ``template``."""
        workload = {
            "spec": {
                "podTemplate": {
                    "spec": {
                        "nodeSelector": {
                            "kubernetes.io/os": "linux",
                            "kubernetes.io/arch": "arm64",
                        },
                    },
                },
            },
        }
        platform = _extract_platform_from_workload(workload)
        assert platform is not None
        assert platform.os == "linux"
        assert platform.arch == "arm64"

    def test_null_spec_returns_none(self):
        """spec itself being None must not crash."""
        workload = {"metadata": {"name": "sb-1"}, "spec": None}
        assert _extract_platform_from_workload(workload) is None

    def test_empty_workload_returns_none(self):
        workload = {}
        assert _extract_platform_from_workload(workload) is None
