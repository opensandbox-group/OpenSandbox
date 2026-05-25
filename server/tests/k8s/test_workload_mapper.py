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

from opensandbox_server.services.k8s.workload_mapper import (
    _extract_platform_from_workload,
)


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
