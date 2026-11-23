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

"""Pool Pod readiness does not prove that its assigned task succeeded."""

from unittest.mock import MagicMock

from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider

_ENDPOINTS = {
    "annotations": {"sandbox.opensandbox.io/endpoints": '["10.0.0.1"]'},
    "creationTimestamp": "2025-12-24T10:00:00Z",
}


def test_get_status_reports_failed_when_task_failed_and_phase_unset():
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "status": {"replicas": 1, "ready": 1, "allocated": 1, "taskFailed": 1, "taskSucceed": 0},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Failed"
    assert result["reason"] == "TASK_FAILED"


def test_get_status_reports_failed_when_task_failed_under_succeed_phase():
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "status": {"phase": "Succeed", "replicas": 1, "ready": 1, "allocated": 1, "taskFailed": 1},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Failed"
    assert result["reason"] == "TASK_FAILED"


def test_get_status_task_failure_does_not_override_paused_phase():
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "status": {"phase": "Paused", "replicas": 1, "ready": 0, "allocated": 1, "taskFailed": 1},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Paused"


def test_get_status_reports_terminated_when_expire_time_has_passed():
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "spec": {"expireTime": "2020-01-01T00:00:00Z"},
        "status": {"phase": "Succeed", "replicas": 1, "ready": 1, "allocated": 1},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Terminated"
    assert result["reason"] == "SANDBOX_EXPIRED"
    assert result["last_transition_at"] == "2020-01-01T00:00:00Z"


def test_get_status_reports_terminated_when_allocated_pod_is_past_expire_time():
    # Pod has IP but is not Ready yet (Allocated). Create wait treats Allocated as success,
    # so expiry must be checked before the ready/IP split.
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "spec": {"expireTime": "2020-01-01T00:00:00Z"},
        "status": {"replicas": 1, "ready": 0, "allocated": 1},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Terminated"
    assert result["reason"] == "SANDBOX_EXPIRED"


def test_get_status_reports_terminated_when_ready_pod_is_past_expire_time():
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "spec": {"expireTime": "2020-01-01T00:00:00Z"},
        "status": {"replicas": 1, "ready": 1, "allocated": 1},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Terminated"
    assert result["reason"] == "SANDBOX_EXPIRED"


def test_get_status_stays_running_when_expire_time_is_still_in_the_future():
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "spec": {"expireTime": "2099-01-01T00:00:00Z"},
        "status": {"phase": "Succeed", "replicas": 1, "ready": 1, "allocated": 1},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Running"


def test_get_status_stays_running_without_task_failure():
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "status": {"replicas": 1, "ready": 1, "allocated": 1, "taskFailed": 0, "taskSucceed": 1},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Running"
    assert result["reason"] == "POD_READY_WITH_IP"
