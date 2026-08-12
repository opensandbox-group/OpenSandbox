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
    @staticmethod
    def _pool_workload(**overrides):
        workload = {
            "metadata": {
                "labels": {"opensandbox.io/id": "sandbox-1"},
                "annotations": {
                    "sandbox.opensandbox.io/alloc-status": (
                        '{"pods":["pool-pod-1"],"poolRef":"pool-runc","generation":3}'
                    ),
                },
                "finalizers": ["pool.sandbox.opensandbox.io/pool-allocation"],
                "generation": 3,
                "creationTimestamp": "2026-06-22T00:00:00Z",
            },
            "spec": {"poolRef": "pool-runc"},
            "status": {"allocated": 1, "observedGeneration": 3},
        }
        for section, values in overrides.items():
            workload[section].update(values)
        return workload

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

    def test_returns_confirmed_pool_allocation(self):
        sandbox = _build_sandbox_from_workload(self._pool_workload(), _WorkloadProvider())

        assert sandbox.allocation is not None
        assert sandbox.allocation.mode == "pool"
        assert sandbox.allocation.pool_ref == "pool-runc"
        assert sandbox.allocation.state == "allocated"

    def test_returns_confirmed_pool_allocation_for_object_workload(self):
        workload = self._pool_workload()
        object_workload = SimpleNamespace(
            metadata=SimpleNamespace(
                labels=workload["metadata"]["labels"],
                annotations=workload["metadata"]["annotations"],
                finalizers=workload["metadata"]["finalizers"],
                generation=workload["metadata"]["generation"],
                creation_timestamp=workload["metadata"]["creationTimestamp"],
            ),
            spec=SimpleNamespace(pool_ref=workload["spec"]["poolRef"]),
            status=SimpleNamespace(
                allocated=workload["status"]["allocated"],
                observed_generation=workload["status"]["observedGeneration"],
            ),
        )

        sandbox = _build_sandbox_from_workload(object_workload, _WorkloadProvider())

        assert sandbox.allocation is not None
        assert sandbox.allocation.pool_ref == "pool-runc"

    @pytest.mark.parametrize(
        ("overrides", "description"),
        [
            ({"spec": {"poolRef": "   "}}, "blank pool reference"),
            ({"spec": {"poolRef": "*"}}, "auto-assigned pool reference"),
            ({"metadata": {"deletionTimestamp": "2026-06-23T00:00:00Z"}}, "deleting workload"),
            ({"metadata": {"finalizers": []}}, "missing allocation finalizer"),
            ({"metadata": {"annotations": {}}}, "missing allocation annotation"),
            ({"metadata": {"annotations": {"sandbox.opensandbox.io/alloc-status": "not-json"}}}, "invalid allocation annotation"),
            ({"metadata": {"annotations": {"sandbox.opensandbox.io/alloc-status": '{"pods":["pool-pod-1"]}'}}}, "legacy pods-only allocation annotation"),
            ({"metadata": {"annotations": {"sandbox.opensandbox.io/alloc-status": '{"pods":["pool-pod-1"],"poolRef":"pool-runc"}'}}}, "missing allocation generation"),
            ({"metadata": {"annotations": {"sandbox.opensandbox.io/alloc-status": '{"pods":["pool-pod-1"],"generation":3}'}}}, "missing allocation pool reference"),
            ({"metadata": {"annotations": {"sandbox.opensandbox.io/alloc-status": '{"pods":["pool-pod-1"],"poolRef":"other-pool","generation":3}'}}}, "wrong allocation pool reference"),
            ({"metadata": {"annotations": {"sandbox.opensandbox.io/alloc-status": '{"pods":["pool-pod-1"],"poolRef":"pool-runc","generation":2}'}}}, "wrong allocation generation"),
            ({"metadata": {"annotations": {"sandbox.opensandbox.io/alloc-status": '{"pods":[],"poolRef":"pool-runc","generation":3}'}}}, "empty allocation annotation"),
            ({"metadata": {"annotations": {"sandbox.opensandbox.io/alloc-status": '{"pods":["pool-pod-1","pool-pod-1"],"poolRef":"pool-runc","generation":3}'}}}, "duplicate allocated pods"),
            ({"status": {"allocated": 2}}, "allocated pod count mismatch"),
            ({"status": {"observedGeneration": 2}}, "unobserved generation"),
        ],
    )
    def test_omits_unconfirmed_pool_allocation(self, overrides, description):
        sandbox = _build_sandbox_from_workload(self._pool_workload(**overrides), _WorkloadProvider())

        assert sandbox.allocation is None, description

    def test_omits_allocation_for_nonpool_workload(self):
        workload = self._pool_workload(spec={"poolRef": None})

        sandbox = _build_sandbox_from_workload(workload, _WorkloadProvider())

        assert sandbox.allocation is None


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
