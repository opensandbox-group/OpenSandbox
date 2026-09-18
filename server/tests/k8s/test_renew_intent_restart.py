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

"""Regression repro: renew intent dropped after server restart.

Post-restart the HTTPTenantProvider has no tenant information in memory (only control-plane
API calls populate it). Background renew consumers have no tenant ContextVar, so
sandbox lookup falls back to ``_find_sandbox_namespace`` -> no tenant information ->
default namespace -> 404, and ``AccessRenewController._try_renew_sync`` swallows
the HTTPException silently: the in-use sandbox is never renewed.
"""

import logging
from datetime import datetime, timedelta, timezone
from unittest.mock import MagicMock

from kubernetes.client import ApiException

from opensandbox_server.integrations.renew_intent.controller import AccessRenewController
from opensandbox_server.integrations.renew_intent.logutil import RENEW_SOURCE_REDIS_QUEUE

TENANT_NS = "tenant-alpha"


def _stage_running_sandbox(k8s_service, mock_workload, namespace: str = TENANT_NS) -> datetime:
    """Make the sandbox resolvable and renewable, but only in `namespace`."""
    expires_at = datetime.now(timezone.utc) + timedelta(minutes=10)
    expected_ns = namespace

    def get_workload(sandbox_id, namespace):
        return mock_workload if namespace == expected_ns else None

    k8s_service.workload_provider.get_workload.side_effect = get_workload
    # Cluster-wide label lookup: resolves the sandbox even
    # when no tenant information is cached.
    k8s_service.workload_provider.list_workloads_all_namespaces.return_value = [
        {"metadata": {"namespace": namespace}}
    ]
    k8s_service.workload_provider.get_status.return_value = {
        "state": "Running",
        "reason": "",
        "message": "Running",
        "last_transition_at": datetime.now(timezone.utc),
    }
    k8s_service.workload_provider.get_endpoint_info.return_value = "10.0.0.1:8080"
    k8s_service.workload_provider.get_expiration.return_value = expires_at
    k8s_service.workload_provider.update_expiration.return_value = None
    return expires_at


def _no_tenant_info_provider() -> MagicMock:
    """HTTPTenantProvider right after a server restart: no tenants known."""
    provider = MagicMock()
    provider.list_tenants.return_value = []
    return provider


def test_redis_renew_intent_survives_no_tenant_info(k8s_service, mock_workload):
    _stage_running_sandbox(k8s_service, mock_workload)
    k8s_service.set_tenant_provider(_no_tenant_info_provider())

    extension_service = MagicMock()
    extension_service.get_access_renew_extend_seconds.return_value = 600

    controller = AccessRenewController(k8s_service, extension_service)
    ok = controller.attempt_renew_sync("test-sandbox-123", source=RENEW_SOURCE_REDIS_QUEUE)

    assert ok is True, "renew intent was silently dropped: sandbox unfindable with no tenant information"
    k8s_service.workload_provider.update_expiration.assert_called_once()


class _CapturingHandler(logging.Handler):
    """Collect records directly on the target logger.

    The app's dictConfig disables propagation and strips pre-existing root
    handlers, so pytest's caplog cannot see app records; attach our own.
    """

    def __init__(self) -> None:
        super().__init__(level=logging.WARNING)
        self.records: list = []

    def emit(self, record: logging.LogRecord) -> None:
        self.records.append(record)


def test_redis_renew_intent_warns_when_sandbox_unfindable(k8s_service, mock_workload):
    """Silent drop: unfindable sandbox must log a WARNING, not return quietly."""
    k8s_service.set_tenant_provider(_no_tenant_info_provider())
    k8s_service.workload_provider.get_workload.return_value = None
    k8s_service.workload_provider.list_workloads_all_namespaces.return_value = []

    handler = _CapturingHandler()
    controller_logger = logging.getLogger(
        "opensandbox_server.integrations.renew_intent.controller"
    )
    controller_logger.addHandler(handler)
    try:
        controller = AccessRenewController(k8s_service, MagicMock())
        ok = controller.attempt_renew_sync("missing-sandbox", source=RENEW_SOURCE_REDIS_QUEUE)
    finally:
        controller_logger.removeHandler(handler)

    assert ok is False
    messages = [record.getMessage() for record in handler.records]
    warnings = [m for m in messages if "get_sandbox_failed" in m]
    assert warnings, "expected a WARNING with skip_reason=get_sandbox_failed"
    assert "missing-sandbox" in warnings[0]


def test_cluster_lookup_forbidden_opens_backoff_window(k8s_service, mock_workload, monkeypatch):
    """A 403 (missing RBAC) opens a backoff window: one warning, no retry inside it."""
    calls = {"n": 0}

    def forbidden(*args, **kwargs):
        calls["n"] += 1
        raise ApiException(status=403, reason="Forbidden")

    k8s_service.workload_provider.list_workloads_all_namespaces.side_effect = forbidden

    clock = {"now": 1000.0}
    monkeypatch.setattr(
        "opensandbox_server.services.k8s.kubernetes_service.time.monotonic",
        lambda: clock["now"],
    )
    handler = _CapturingHandler()
    ks_logger = logging.getLogger("opensandbox_server.services.k8s.kubernetes_service")
    ks_logger.addHandler(handler)
    try:
        assert k8s_service._find_sandbox_namespace_cluster_wide("sbx-rbac") is None
        assert calls["n"] == 1
        # Inside the backoff window the cluster LIST is suppressed entirely.
        clock["now"] = 1000.0 + 599.0
        assert k8s_service._find_sandbox_namespace_cluster_wide("sbx-rbac") is None
        assert calls["n"] == 1
        # Past the window the LIST is retried (and re-denied, warning again).
        clock["now"] = 1000.0 + 601.0
        assert k8s_service._find_sandbox_namespace_cluster_wide("sbx-rbac") is None
        assert calls["n"] == 2
    finally:
        ks_logger.removeHandler(handler)

    warnings = [record.getMessage() for record in handler.records]
    assert any("denied" in message for message in warnings), (
        "expected an actionable warning when the cluster-wide lookup is denied"
    )


def test_cluster_lookup_transient_error_keeps_retrying(k8s_service, mock_workload):
    """Non-403 failures are transient: no backoff window, the next intent retries the LIST."""

    calls = {"n": 0}

    def server_error(*args, **kwargs):
        calls["n"] += 1
        raise ApiException(status=500, reason="Internal Server Error")

    k8s_service.workload_provider.list_workloads_all_namespaces.side_effect = server_error

    assert k8s_service._find_sandbox_namespace_cluster_wide("sbx-flaky") is None
    assert k8s_service._find_sandbox_namespace_cluster_wide("sbx-flaky") is None
    assert calls["n"] == 2, "a transient 5xx must not suppress the next lookup"


def test_namespace_resolution_memoized_within_attempt(k8s_service, mock_workload):
    """One execution flow resolves the same id several times; the cluster LIST runs once.

    The memo is per-context and guarded by sandbox id, so each test's unique
    id starts from a clean slot.
    """
    k8s_service.set_tenant_provider(_no_tenant_info_provider())
    k8s_service.workload_provider.get_workload.return_value = None
    k8s_service.workload_provider.list_workloads_all_namespaces.return_value = [
        {"metadata": {"namespace": TENANT_NS}}
    ]

    for _ in range(3):  # get_sandbox / extend-seconds / renew_expiration
        assert k8s_service._resolve_namespace_for_lookup("memo-sbx") == TENANT_NS
    assert k8s_service.workload_provider.list_workloads_all_namespaces.call_count == 1

    # A different id is not served by the memo of the previous id.
    assert k8s_service._resolve_namespace_for_lookup("memo-other") == TENANT_NS
    assert k8s_service.workload_provider.list_workloads_all_namespaces.call_count == 2


def test_renew_attempt_issues_single_cluster_list(k8s_service, mock_workload):
    """End-to-end: a full renew attempt with no tenant information triggers exactly one cluster LIST."""
    _stage_running_sandbox(k8s_service, mock_workload)
    k8s_service.set_tenant_provider(_no_tenant_info_provider())

    extension_service = MagicMock()
    extension_service.get_access_renew_extend_seconds.return_value = 600

    controller = AccessRenewController(k8s_service, extension_service)
    ok = controller.attempt_renew_sync("memo-attempt-sbx", source=RENEW_SOURCE_REDIS_QUEUE)

    assert ok is True
    k8s_service.workload_provider.update_expiration.assert_called_once()
    assert k8s_service.workload_provider.list_workloads_all_namespaces.call_count == 1


def test_single_tenant_lookup_stays_in_configured_namespace(k8s_service, mock_workload):
    """Single-tenant mode must not use the cluster-wide fallback.

    A server without a tenant provider owns exactly its configured
    namespace; resolving a foreign id beyond it could reach other servers'
    sandboxes, and single-tenant proxy routes are unauthenticated.
    """
    k8s_service.workload_provider.get_workload.return_value = None

    assert k8s_service._find_sandbox_namespace("foreign-sbx") is None
    k8s_service.workload_provider.list_workloads_all_namespaces.assert_not_called()

    # With no resolution the caller falls back to the configured namespace
    # and 404s there, exactly the pre-fallback behavior.
    assert (
        k8s_service._resolve_namespace_for_lookup("foreign-sbx")
        == k8s_service.namespace
    )


def test_second_attempt_re_resolves_after_sandbox_moves(k8s_service, mock_workload):
    """Two attempts for the same id must not share the memoized namespace.

    If the sandbox is deleted and recreated in another namespace between
    attempts, the second attempt has to re-resolve instead of trusting the
    first attempt's memo (which would 404 in the old namespace).
    """
    k8s_service.set_tenant_provider(_no_tenant_info_provider())

    extension_service = MagicMock()
    extension_service.get_access_renew_extend_seconds.return_value = 600
    controller = AccessRenewController(k8s_service, extension_service)

    _stage_running_sandbox(k8s_service, mock_workload, namespace=TENANT_NS)
    assert controller.attempt_renew_sync("moved-sbx", source=RENEW_SOURCE_REDIS_QUEUE)
    assert k8s_service.workload_provider.list_workloads_all_namespaces.call_count == 1

    # The sandbox "moves": same id, different namespace.
    _stage_running_sandbox(k8s_service, mock_workload, namespace="tenant-beta")
    assert controller.attempt_renew_sync("moved-sbx", source=RENEW_SOURCE_REDIS_QUEUE)
    # The second attempt went through a fresh cluster LIST, not the memo.
    assert k8s_service.workload_provider.list_workloads_all_namespaces.call_count == 2


def test_cluster_lookup_with_duplicate_matches_is_rejected(k8s_service, mock_workload):
    """Duplicate label matches must not resolve by picking one arbitrarily.

    Two workloads carrying the same sandbox-id label in different namespaces
    is an operator problem; acting on a random one of them could renew the
    wrong sandbox. The lookup must refuse and say why.
    """
    k8s_service.set_tenant_provider(_no_tenant_info_provider())
    k8s_service.workload_provider.get_workload.return_value = None
    k8s_service.workload_provider.list_workloads_all_namespaces.return_value = [
        {"metadata": {"namespace": "tenant-alpha"}},
        {"metadata": {"namespace": "tenant-beta"}},
    ]

    handler = _CapturingHandler()
    ks_logger = logging.getLogger("opensandbox_server.services.k8s.kubernetes_service")
    ks_logger.addHandler(handler)
    try:
        assert k8s_service._find_sandbox_namespace_cluster_wide("dup-sbx") is None
        # Unresolved: callers fall back to the configured namespace and 404 there.
        assert k8s_service._resolve_namespace_for_lookup("dup-sbx") == k8s_service.namespace
    finally:
        ks_logger.removeHandler(handler)

    messages = [record.getMessage() for record in handler.records]
    assert any("matched 2 namespaces" in message for message in messages), (
        "expected an ambiguity warning instead of an arbitrary pick"
    )


def _run_queue_intent(controller: AccessRenewController, sandbox_id: str, namespace: str) -> None:
    """Deliver one intent the way the Redis consumer does (public path)."""
    import asyncio

    from opensandbox_server.integrations.renew_intent.intent import RenewIntent

    intent = RenewIntent(
        sandbox_id=sandbox_id,
        observed_at=datetime.now(timezone.utc),
        port=8080,
        request_uri="/localtest",
        namespace=namespace,
    )
    asyncio.run(controller.process_intent_after_lock(intent))


def test_observed_namespace_resolves_without_scan_or_list(k8s_service, mock_workload):
    """The queue payload carries the namespace ingress observed; a hit must
    skip tenant scan and cluster LIST entirely: one namespaced GET is the
    whole resolution."""
    _stage_running_sandbox(k8s_service, mock_workload, namespace="ingress-said-ns")
    k8s_service.set_tenant_provider(_no_tenant_info_provider())

    extension_service = MagicMock()
    extension_service.get_access_renew_extend_seconds.return_value = 600
    controller = AccessRenewController(k8s_service, extension_service)
    _run_queue_intent(controller, "observed-sbx", namespace="ingress-said-ns")

    k8s_service.workload_provider.update_expiration.assert_called_once()
    # No tenant scan, no cluster-wide LIST: the observed-namespace GET
    # resolved it.
    k8s_service._tenant_provider.list_tenants.assert_not_called()
    k8s_service.workload_provider.list_workloads_all_namespaces.assert_not_called()


def test_stale_observed_namespace_falls_back_to_cluster_list(k8s_service, mock_workload):
    """A stale observed namespace (sandbox moved since ingress served it)
    must not strand the intent: miss there, then the cluster LIST finds the
    new home."""
    _stage_running_sandbox(k8s_service, mock_workload)
    k8s_service.set_tenant_provider(_no_tenant_info_provider())

    extension_service = MagicMock()
    extension_service.get_access_renew_extend_seconds.return_value = 600
    controller = AccessRenewController(k8s_service, extension_service)
    _run_queue_intent(controller, "stale-observed-sbx", namespace="sandbox-old-home")

    k8s_service.workload_provider.update_expiration.assert_called_once()
    # The observed namespace was tried (one GET in the old home) and the
    # cluster LIST picked up the fallback: exactly one LIST, not one per
    # lookup step.
    tried_namespaces = {
        call.kwargs.get("namespace")
        for call in k8s_service.workload_provider.get_workload.call_args_list
    }
    assert "sandbox-old-home" in tried_namespaces
    assert k8s_service.workload_provider.list_workloads_all_namespaces.call_count == 1


def test_single_tenant_ignores_observed_namespace(k8s_service, mock_workload):
    """Single-tenant mode ignores the observed namespace: same rule as the
    cluster LIST — the server never resolves beyond its configured namespace."""
    from opensandbox_server.tenants.context import set_observed_namespace

    k8s_service.workload_provider.get_workload.return_value = None
    try:
        set_observed_namespace("attacker-ns")
        assert k8s_service._find_sandbox_namespace("foreign-sbx") is None
        assert k8s_service._resolve_namespace_for_lookup("foreign-sbx") == k8s_service.namespace
    finally:
        set_observed_namespace(None)

    # Only the configured namespace was ever probed.
    probed = {
        call.kwargs.get("namespace")
        for call in k8s_service.workload_provider.get_workload.call_args_list
    }
    assert probed == {k8s_service.namespace}
    k8s_service.workload_provider.list_workloads_all_namespaces.assert_not_called()
