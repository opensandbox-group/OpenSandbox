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

import pytest
from fastapi import HTTPException

from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.k8s.provider_common import (
    _translate_resource_limits_for_k8s,
)


def test_translate_resource_limits_passes_gpu_count():
    result = _translate_resource_limits_for_k8s(
        {"cpu": "1", "memory": "1Gi", "gpu": "2"}
    )
    assert result["nvidia.com/gpu"] == "2"


def test_translate_resource_limits_strips_raw_gpu_key():
    # Regression guard: the raw "gpu" key must not leak into the pod's
    # V1ResourceRequirements where Kubernetes would treat it as an
    # unknown extended resource.
    result = _translate_resource_limits_for_k8s({"gpu": "2"})
    assert "gpu" not in result
    assert result == {"nvidia.com/gpu": "2"}


def test_translate_resource_limits_preserves_cpu_memory():
    result = _translate_resource_limits_for_k8s({"cpu": "500m", "memory": "512Mi"})
    assert result == {"cpu": "500m", "memory": "512Mi"}


def test_translate_resource_limits_no_gpu_key_unchanged():
    inputs = {"cpu": "2", "memory": "4Gi"}
    result = _translate_resource_limits_for_k8s(inputs)
    assert result == inputs
    # Must be a new dict — callers pass it to both limits= and requests=,
    # and we don't want surprise mutation of the input.
    assert result is not inputs


def test_translate_resource_limits_rejects_all():
    with pytest.raises(HTTPException) as excinfo:
        _translate_resource_limits_for_k8s({"gpu": "all"})
    assert excinfo.value.status_code == 400
    detail = excinfo.value.detail
    assert detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "positive integer" in detail["message"]


@pytest.mark.parametrize("bad_value", ["0", "-1", "bad", ""])
def test_translate_resource_limits_drops_invalid_gpu(bad_value):
    result = _translate_resource_limits_for_k8s(
        {"cpu": "1", "gpu": bad_value}
    )
    assert "nvidia.com/gpu" not in result
    assert "gpu" not in result
    assert result == {"cpu": "1"}


def test_translate_resource_limits_empty_dict():
    assert _translate_resource_limits_for_k8s({}) == {}
