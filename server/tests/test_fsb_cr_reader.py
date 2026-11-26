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

"""PoolSnapshot and PoolCRReader: the informer-backed SandboxPool read behind
the fsb create path (OSEP-0026 pool-mode gate, OSEP-0035 handler discovery)."""

from copy import deepcopy
import time
from types import SimpleNamespace
from unittest.mock import Mock, patch

from fastapi import HTTPException
from kubernetes.client import ApiException, CustomObjectsApi
import pytest

from opensandbox_server.config import KubernetesRuntimeConfig
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.fast_sandbox.cr_reader import PoolCRReader, PoolSnapshot
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.k8s.informer import WorkloadInformer

NAMESPACE = "ns-1"
POOL = (NAMESPACE, "default-pool")
LEGACY_PROFILE = {"cpu": "500m", "memory": "512Mi", "pids": 128}
AWARE_PROFILE = {
    "mode": "ResourceAware",
    "cpu": "500m",
    "memory": "512Mi",
    "pids": 128,
    "overcommitRatio": {"cpu": "2", "memory": "2"},
}


def _pool_cr(*, profile=LEGACY_PROFILE, handlers=("egress",), uid="pool-uid") -> dict:
    return {
        "apiVersion": "sandbox.fast.io/v1alpha2",
        "kind": "SandboxPool",
        # Watch events without a resourceVersion would un-sync the cache.
        "metadata": {
            "name": "default-pool",
            "namespace": NAMESPACE,
            "uid": uid,
            "resourceVersion": "1",
        },
        "spec": {
            "actionHandlers": [{"name": name, "targetHTTPPort": 18080} for name in handlers],
            "sandboxResources": deepcopy(profile),
        },
    }


@pytest.fixture
def pool_api(monkeypatch):
    """A K8sClient whose CustomObjectsApi is an in-memory SandboxPool store.

    Informer threads stay off: the test drives the informer by hand, so which
    reads hit the cache and which reach the API stays deterministic.
    """
    pools: dict[tuple[str, str], dict] = {}
    api = Mock(spec=CustomObjectsApi)

    def serve_pool(**kwargs):
        pool = pools.get((kwargs["namespace"], kwargs["name"]))
        if pool is None:
            raise ApiException(status=404)
        return deepcopy(pool)

    def list_pools(**kwargs):
        return {
            "metadata": {"resourceVersion": "1"},
            "items": [
                deepcopy(pool)
                for (namespace, _), pool in pools.items()
                if namespace == kwargs["namespace"]
            ],
        }

    api.get_namespaced_custom_object.side_effect = serve_pool
    api.list_namespaced_custom_object.side_effect = list_pools
    monkeypatch.setattr(WorkloadInformer, "start", lambda self: None)
    with patch.object(K8sClient, "_load_config"):
        client = K8sClient(KubernetesRuntimeConfig())
    client._custom_objects_api = api
    return SimpleNamespace(client=client, api=api, pools=pools, serve_pool=serve_pool)


def _reads(pool_api) -> int:
    return pool_api.api.get_namespaced_custom_object.call_count


def _informer(pool_api) -> WorkloadInformer:
    # The first read lazily starts the namespace informer.
    return pool_api.client._lookup_informer(
        "sandbox.fast.io", "v1alpha2", "sandboxpools", NAMESPACE
    )


@pytest.mark.parametrize(
    "profile,aware",
    [
        # A mode-less profile is LegacySlot, like fast-sandbox's ResourceMode().
        (LEGACY_PROFILE, False),
        ({**LEGACY_PROFILE, "mode": "LegacySlot"}, False),
        (LEGACY_PROFILE | {"mode": "LegacySlot", "overcommitRatio": {"cpu": "2", "memory": "2"}}, False),
        (AWARE_PROFILE, True),
        # ResourceAware is declared only with cpu, memory and overcommitRatio.
        ({**LEGACY_PROFILE, "mode": "ResourceAware"}, False),
        ({"mode": "ResourceAware", "memory": "512Mi", "pids": 128}, False),
        ({"mode": "ResourceAware", "cpu": "500m", "pids": 128}, False),
        ({**AWARE_PROFILE, "cpu": ""}, False),
        ({**AWARE_PROFILE, "overcommitRatio": {}}, False),
        ({"pids": 128}, False),
        # Any non-null quantity is declared, including zero.
        ({**AWARE_PROFILE, "cpu": 0, "memory": 0}, True),
    ],
)
def test_snapshot_gate_matches_fastpath_resource_mode(profile, aware):
    assert PoolSnapshot.from_cr(_pool_cr(profile=profile)).is_resource_aware is aware


def test_snapshot_projects_profile_and_handlers():
    snapshot = PoolSnapshot.from_cr(
        _pool_cr(
            profile={
                "mode": "ResourceAware",
                "cpu": 2,
                "memory": "0.5Gi",
                "pids": 128,
                "overcommitRatio": {"cpu": 2, "memory": "1"},
            },
            handlers=("egress", "runtime-init"),
        )
    )

    assert snapshot.name == "default-pool"
    assert snapshot.namespace == NAMESPACE
    assert snapshot.is_resource_aware
    # int-or-string quantities become the strings FastPath GetPool reports.
    assert snapshot.resources == {"cpu": "2", "memory": "0.5Gi", "pids": "128"}
    assert snapshot.action_handlers == ("egress", "runtime-init")


def test_snapshot_of_a_pool_without_profile_or_handlers_fails_safe():
    snapshot = PoolSnapshot.from_cr({"metadata": {"name": "p", "namespace": NAMESPACE}, "spec": {}})

    assert snapshot.name == "p"
    assert snapshot.is_resource_aware is False
    assert snapshot.resources == {}
    assert snapshot.action_handlers == ()


def test_reader_maps_missing_pool_to_pool_not_found(pool_api):
    with pytest.raises(HTTPException) as exc_info:
        PoolCRReader(lambda: pool_api.client).get(NAMESPACE, "ghost")

    assert exc_info.value.status_code == 404
    # Same contract FastPath GetPool's NotFound produced.
    assert exc_info.value.detail == {
        "code": SandboxErrorCodes.FSB_POOL_NOT_FOUND,
        "message": "SandboxPool 'ghost' not found in namespace 'ns-1'.",
    }


def test_reader_lets_rbac_forbidden_escape(pool_api):
    """Only the 403 escapes: the service owns the FastPath GetPool fallback."""
    pool_api.api.get_namespaced_custom_object.side_effect = ApiException(
        status=403, reason="Forbidden"
    )

    with pytest.raises(ApiException) as exc_info:
        PoolCRReader(lambda: pool_api.client).get(NAMESPACE, "default-pool")

    assert exc_info.value.status == 403


@pytest.mark.parametrize(
    "failure",
    [ApiException(status=500), ConnectionError("apiserver unreachable")],
)
def test_reader_maps_other_read_failures_to_503(pool_api, failure):
    pool_api.api.get_namespaced_custom_object.side_effect = failure

    with pytest.raises(HTTPException) as exc_info:
        PoolCRReader(lambda: pool_api.client).get(NAMESPACE, "default-pool")

    assert exc_info.value.status_code == 503
    assert exc_info.value.detail == {
        "code": SandboxErrorCodes.FSB_API_ERROR,
        "message": "Fsb SandboxPool CRs are unavailable.",
    }


def test_reader_serves_synced_reads_from_the_watch_cache(pool_api):
    pool_api.pools[POOL] = _pool_cr()
    reader = PoolCRReader(lambda: pool_api.client)

    assert reader.get(NAMESPACE, "default-pool").resources == {
        "cpu": "500m",
        "memory": "512Mi",
        "pids": "128",
    }
    assert _reads(pool_api) == 1

    # Once synced, reads come from memory: one GET per pool, not per create.
    assert _informer(pool_api)._full_resync()
    assert reader.get(NAMESPACE, "default-pool").is_resource_aware is False
    assert _reads(pool_api) == 1


def test_reader_follows_handler_and_mode_changes_through_the_watch(pool_api):
    pool_api.pools[POOL] = _pool_cr()
    reader = PoolCRReader(lambda: pool_api.client)
    assert reader.get(NAMESPACE, "default-pool").is_resource_aware is False
    informer = _informer(pool_api)
    assert informer._full_resync()

    # Until the watch reports a change, the synced cache is what creates see.
    pool = pool_api.pools[POOL]
    pool["spec"]["actionHandlers"].append({"name": "runtime-init", "targetHTTPPort": 18080})
    assert reader.get(NAMESPACE, "default-pool").action_handlers == ("egress",)

    informer._handle_event({"type": "MODIFIED", "object": deepcopy(pool)})
    assert reader.get(NAMESPACE, "default-pool").action_handlers == ("egress", "runtime-init")

    pool["spec"]["actionHandlers"].pop()
    informer._handle_event({"type": "MODIFIED", "object": deepcopy(pool)})
    assert reader.get(NAMESPACE, "default-pool").action_handlers == ("egress",)
    assert _reads(pool_api) == 1

    # A same-name recreate with a flipped mode: DELETED drops the cached entry
    # (reads go to the API again) and ADDED publishes the new generation.
    del pool_api.pools[POOL]
    informer._handle_event({"type": "DELETED", "object": deepcopy(pool)})
    with pytest.raises(HTTPException) as exc_info:
        reader.get(NAMESPACE, "default-pool")
    assert exc_info.value.status_code == 404
    assert _reads(pool_api) == 2

    recreated = _pool_cr(profile=AWARE_PROFILE, uid="pool-uid-2")
    pool_api.pools[POOL] = recreated
    informer._handle_event({"type": "ADDED", "object": deepcopy(recreated)})
    assert reader.get(NAMESPACE, "default-pool").is_resource_aware is True
    assert _reads(pool_api) == 2


def test_reader_leaves_a_stale_cache_for_the_live_api(pool_api, monkeypatch):
    """A watch that has been silent past the resync bound is not trusted."""
    pool_api.pools[POOL] = _pool_cr()
    reader = PoolCRReader(lambda: pool_api.client)
    reader.get(NAMESPACE, "default-pool")
    informer = _informer(pool_api)
    assert informer._full_resync()

    pool_api.pools[POOL] = _pool_cr(profile=AWARE_PROFILE, uid="pool-uid-2")
    monkeypatch.setattr(
        informer,
        "_last_contact_at",
        time.monotonic() - informer._staleness_limit_seconds - 1,
    )

    assert reader.get(NAMESPACE, "default-pool").is_resource_aware is True
    assert _reads(pool_api) == 2
