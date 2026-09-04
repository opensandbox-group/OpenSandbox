"""Sandbox state must reflect a failed task, not just pod readiness.

A Pool pod is Ready before any sandbox is dispatched to it -- that is what pre-warming
means -- so pod readiness carries no information about the dispatched task. Deriving the
public state from readiness alone reports a healthy "Running" sandbox whose entrypoint
never ran. `taskFailed` is published by the operator (and asserted in the k8s e2e suite)
but the server never reads it.
"""

from unittest.mock import MagicMock

from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider

_ENDPOINTS = {
    "annotations": {"sandbox.opensandbox.io/endpoints": '["10.0.0.1"]'},
    "creationTimestamp": "2025-12-24T10:00:00Z",
}


def test_get_status_reports_failed_when_task_failed_and_phase_unset():
    # Pool-allocated sandboxes are observed with an empty phase, so the pod-readiness
    # fallback decides -- and it sees a Ready pool pod regardless of the task outcome.
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "status": {"replicas": 1, "ready": 1, "allocated": 1, "taskFailed": 1, "taskSucceed": 0},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Failed"
    assert result["reason"] == "TASK_FAILED"


def test_get_status_reports_failed_when_task_failed_under_succeed_phase():
    # applySteadyRuntimePhase sets Succeed from `Ready > 0` alone, so the phase branch
    # would otherwise map a failed task to "Running".
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "status": {"phase": "Succeed", "replicas": 1, "ready": 1, "allocated": 1, "taskFailed": 1},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Failed"
    assert result["reason"] == "TASK_FAILED"


def test_get_status_task_failure_does_not_override_paused_phase():
    # Lifecycle phases are owned by an explicit user operation and stay authoritative.
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "status": {"phase": "Paused", "replicas": 1, "ready": 0, "allocated": 1, "taskFailed": 1},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Paused"


def test_get_status_stays_running_without_task_failure():
    # Regression guard: the common path must be untouched.
    provider = BatchSandboxProvider(MagicMock())
    workload = {
        "status": {"replicas": 1, "ready": 1, "allocated": 1, "taskFailed": 0, "taskSucceed": 1},
        "metadata": _ENDPOINTS,
    }

    result = provider.get_status(workload)

    assert result["state"] == "Running"
    assert result["reason"] == "POD_READY_WITH_IP"
