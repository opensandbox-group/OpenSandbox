# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules and test fakes expose dynamic attributes.

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

"""Server-routed Credential Vault relay tests (OSEP-0022 fast-sandbox profile).

Covers the embedded handler-result status mapping, sanitize-on-read, the
server-side PATCH merge (delete -> replace -> add, mentioned-once fencing,
expectedRevision cross-replica backstop), the fsb routing, and the sidecar
proxy fallback for non-fsb backends.
"""

from concurrent import futures
from typing import Any
import json
from unittest.mock import Mock, patch

import grpc
import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from kubernetes.client import V1APIResource, V1APIResourceList

from opensandbox_server.api import credential_vault, lifecycle
from opensandbox_server.middleware.request_id import RequestIdMiddleware
from opensandbox_server.services.fast_sandbox.fastpath_client import (
    FastPathClient,
)
from opensandbox_server.services.fast_sandbox.generated import fastpath_pb2 as pb2
from opensandbox_server.services.fast_sandbox.generated import (
    fastpath_pb2_grpc as pb2_grpc,
)
from opensandbox_server.services.composite_service import CompositeSandboxService
from opensandbox_server.services.fast_sandbox.service import FastSandboxService
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.config import (
    AppConfig,
    KubernetesRuntimeConfig,
    RuntimeConfig,
    ServerConfig,
)

_SANDBOX_ID = "fsb-vault-1"
_SANDBOX_UID = "uid-vault-1"

_FULL_REVISION = {
    "revision": 2,
    "credentials": [
        {"name": "tok", "source": {"type": "inline", "value": "sekrit"}},
    ],
    "bindings": [
        {
            "name": "ext",
            "match": {"hosts": ["ext.test"]},
            "auth": {"type": "apiKey", "name": "X-Api-Key", "credential": "tok"},
        },
    ],
}


class _FakeFastPathService(pb2_grpc.FastPathServiceServicer):
    """Scripted FastPath: records relay calls, answers with fixed outcomes."""

    def __init__(self):
        self.calls: list[tuple[str, str, str]] = []
        self.payloads: list[bytes] = []
        self.outcome: Any = (200, b"{}")
        self.outcomes: list[Any] = []
        self.writer = None

    def InvokeHandlerAction(self, request, context):
        if request.path.startswith("/vault-state/"):
            if self.writer is None:
                return pb2.InvokeHandlerActionResponse(result=pb2.HandlerActionResult(status=404, body=b"not selected"))
            self.calls.append((request.method, request.path, request.sandbox.expected_uid))
            self.payloads.append(request.payload)
            status_code, body = self.writer(request)
            return pb2.InvokeHandlerActionResponse(result=pb2.HandlerActionResult(status=status_code, body=body))
        self.calls.append(
            (request.method, request.path, request.sandbox.expected_uid)
        )
        self.payloads.append(request.payload)
        if self.outcomes:
            outcome = self.outcomes.pop(0)
        else:
            outcome = self.outcome
        if isinstance(outcome, grpc.StatusCode):
            context.abort(outcome, "scripted relay failure")
        status_code, body = outcome
        return pb2.InvokeHandlerActionResponse(
            result=pb2.HandlerActionResult(status=status_code, body=body)
        )


def _vault_ack(revision: int = 3) -> bytes:
    return json.dumps(
        {
            "revision": revision,
            "credentials": [
                {"name": "tok", "sourceType": "inline", "revision": revision},
            ],
            "bindings": [],
        }
    ).encode()


@pytest.fixture
def vault_fsb(monkeypatch):
    fake = _FakeFastPathService()
    grpc_server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    pb2_grpc.add_FastPathServiceServicer_to_server(fake, grpc_server)
    port = grpc_server.add_insecure_port("127.0.0.1:0")
    grpc_server.start()

    config = AppConfig(
        server=ServerConfig(
            host="0.0.0.0",
            port=8080,
            api_key="x",
            max_sandbox_timeout_seconds=7200,
        ),
        runtime=RuntimeConfig(
            type="kubernetes", execd_image="ghcr.io/opensandbox/execd:latest"
        ),
        kubernetes=KubernetesRuntimeConfig(namespace="ns-1"),
    )
    fastpath = FastPathClient(endpoint=f"127.0.0.1:{port}")
    with patch.object(K8sClient, "_load_config"):
        k8s = K8sClient(KubernetesRuntimeConfig())
    api = _FakeCustomObjectsApi()
    k8s._custom_objects_api = api
    service = FastSandboxService(config, fastpath_client=fastpath, k8s_client=k8s)
    monkeypatch.setattr(lifecycle, "sandbox_service", service)

    app = FastAPI()
    app.add_middleware(RequestIdMiddleware)
    # Mirror main.py's request-validation handler so the 422 sanitize
    # assertions exercise the production error shape.
    from fastapi.exceptions import RequestValidationError
    from opensandbox_server.main import request_validation_exception_handler

    app.add_exception_handler(
        RequestValidationError, request_validation_exception_handler
    )
    app.include_router(lifecycle.router, prefix="/v1")
    app.include_router(credential_vault.router, prefix="/v1")
    try:
        with TestClient(app) as client:
            yield client, fake, service
    finally:
        service.close()
        grpc_server.stop(None)


class _FakeCustomObjectsApi:
    def __init__(self):
        self.get_api_resources_return = V1APIResourceList(
            group_version="sandbox.fast.io/v1alpha2",
            resources=[
                V1APIResource(
                    name="sandboxes",
                    singular_name="sandbox",
                    kind="Sandbox",
                    namespaced=True,
                    verbs=["get", "list", "watch"],
                )
            ],
        )

    def get_api_resources(self):
        return self.get_api_resources_return

    def list_namespaced_custom_object(self, **kwargs):
        try:
            cr = self.get_namespaced_custom_object(**kwargs)
        except Exception:
            return {"items": []}
        return {"items": [cr] if cr["metadata"]["name"] == _SANDBOX_ID else []}

    def get_namespaced_custom_object(self, **kwargs):
        if kwargs["name"] != _SANDBOX_ID:
            from kubernetes.client import ApiException

            raise ApiException(status=404)
        return {
            "metadata": {
                "name": _SANDBOX_ID,
                "namespace": kwargs["namespace"],
                "uid": _SANDBOX_UID,
                "generation": 1,
                "resourceVersion": "1",
                "creationTimestamp": "2026-10-16T00:00:00Z",
            },
            "spec": {"image": "python:3.11", "actionBindings": []},
            "status": {"runtime": {"state": "Ready"}},
        }


def test_create_relays_create_only_and_returns_sanitized_ack(vault_fsb):
    client, fake, _ = vault_fsb
    fake.outcome = (201, _vault_ack(1))
    response = client.post(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={
            "credentials": [
                {"name": "tok", "source": {"type": "inline", "value": "sekrit"}}
            ],
            "bindings": [
                {
                    "name": "ext",
                    "match": {"hosts": ["ext.test"]},
                    "auth": {"type": "bearer", "credential": "tok"},
                },
            ],
        },
    )

    assert response.status_code == 201
    assert response.json() == {
        "revision": 1,
        "credentials": [{"name": "tok", "sourceType": "inline", "revision": 1}],
        "bindings": [],
    }
    assert fake.calls == [
        ("POST", f"/vault/{_SANDBOX_UID}", _SANDBOX_UID)
    ]
    body = json.loads(fake.payloads[0])
    assert body["credentials"][0]["source"] == {"type": "inline", "value": "sekrit"}
    assert body["bindings"][0]["auth"] == {"type": "bearer", "credential": "tok"}


def test_create_duplicate_maps_to_409_and_unknown_subject_to_404(vault_fsb):
    client, fake, _ = vault_fsb

    fake.outcome = (409, b"credential vault already exists")
    response = client.post(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={"credentials": [], "bindings": []},
    )
    assert response.status_code == 409
    detail = response.json()["detail"]
    assert detail["code"] == "FSB::API_ERROR"
    assert "already exists" in detail["message"]

    fake.outcome = (404, b"unknown subject")
    response = client.post(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={"credentials": [], "bindings": []},
    )
    assert response.status_code == 404

    fake.outcome = (412, b"expectedRevision 1 does not match current revision 2")
    response = client.put = client.post(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={"credentials": [], "bindings": []},
    )
    assert response.status_code == 412


def test_public_reads_sanitize_credential_values(vault_fsb):
    client, fake, _ = vault_fsb
    fake.outcome = (200, json.dumps(_FULL_REVISION).encode())

    response = client.get(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault")
    assert response.status_code == 200
    state = response.json()
    assert "sekrit" not in response.text
    assert state["revision"] == 2
    assert state["credentials"] == [
        {"name": "tok", "sourceType": "inline", "revision": 2}
    ]
    binding = state["bindings"][0]
    assert binding["name"] == "ext"
    assert binding["match"] == {"hosts": ["ext.test"]}
    assert binding["auth"] == {"type": "apiKey", "name": "X-Api-Key"}

    credentials = client.get(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault/credentials")
    assert credentials.json() == {
        "revision": 2,
        "credentials": [{"name": "tok", "sourceType": "inline", "revision": 2}],
    }

    one = client.get(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault/credentials/tok")
    assert one.json() == {"name": "tok", "sourceType": "inline", "revision": 2}

    missing = client.get(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault/credentials/nope"
    )
    assert missing.status_code == 404

    bindings = client.get(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault/bindings")
    assert bindings.json()["bindings"][0]["name"] == "ext"

    binding_one = client.get(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault/bindings/ext"
    )
    assert binding_one.status_code == 200

    binding_missing = client.get(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault/bindings/nope"
    )
    assert binding_missing.status_code == 404


def test_patch_composes_get_merge_put_with_expected_revision(vault_fsb):
    client, fake, _ = vault_fsb
    # GET (read revision 2) then PUT ack (revision 3).
    fake.outcomes = [
        (200, json.dumps(_FULL_REVISION).encode()),
        (200, _vault_ack(3)),
    ]
    response = client.patch(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={
            "expectedRevision": 2,
            "credentials": {"add": [{"name": "tok2", "source": {"type": "inline", "value": "s2"}}]},
            "bindings": {"delete": ["ext"]},
        },
    )

    assert response.status_code == 200, response.text
    assert len(fake.calls) == 2
    assert fake.calls[0] == ("GET", f"/vault/{_SANDBOX_UID}", _SANDBOX_UID)
    assert fake.calls[1] == ("PUT", f"/vault/{_SANDBOX_UID}", _SANDBOX_UID)

    merged = json.loads(fake.payloads[1])
    assert merged["expectedRevision"] == 2
    names = [c["name"] for c in merged["credentials"]]
    assert names == ["tok", "tok2"]
    # Untouched records round-trip verbatim (values included, relay-only).
    assert merged["credentials"][0] == _FULL_REVISION["credentials"][0]
    # The deleted binding is gone.
    assert merged["bindings"] == []


def test_patch_stamps_expected_revision_when_client_omits_it(vault_fsb):
    """A PATCH without expectedRevision is still fenced: the composed PUT
    carries the revision just read, so a concurrent replica's commit turns
    the relay PUT into 412 instead of a silent last-writer-wins overwrite."""
    client, fake, _ = vault_fsb
    fake.outcomes = [
        (200, json.dumps(_FULL_REVISION).encode()),
        (200, _vault_ack(3)),
    ]
    response = client.patch(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={"credentials": {"add": [{"name": "tok2", "source": {"type": "inline", "value": "s2"}}]}},
    )

    assert response.status_code == 200, response.text
    merged = json.loads(fake.payloads[1])
    assert merged["expectedRevision"] == 2


def test_patch_fencing_and_mutation_semantics(vault_fsb):
    client, fake, _ = vault_fsb
    fake.outcome = (200, json.dumps(_FULL_REVISION).encode())

    fake.calls.clear()
    # delete of an unknown name is rejected server-side before any relay.

    fake.calls.clear()
    # delete of an unknown name is rejected server-side before any relay.
    fake.calls.clear()
    response = client.patch(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={"credentials": {"delete": ["missing"]}},
    )
    assert response.status_code == 400
    assert "does not exist" in response.json()["detail"]["message"]
    # The merge consumes the relay GET; no PUT is attempted for a rejected patch.
    assert [call[0] for call in fake.calls] == ["GET"]

    fake.calls.clear()
    # add of an existing name is rejected server-side.
    response = client.patch(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={
            "credentials": {
                "add": [{"name": "tok", "source": {"type": "inline", "value": "x"}}]
            }
        },
    )
    assert response.status_code == 400
    assert "already exists" in response.json()["detail"]["message"]

    fake.calls.clear()
    # A name mentioned by multiple operations is rejected.
    response = client.patch(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={
            "credentials": {
                "delete": ["tok"],
                "replace": [{"name": "tok", "source": {"type": "inline", "value": "y"}}],
            }
        },
    )
    assert response.status_code == 400
    assert "multiple operations" in response.json()["detail"]["message"]
    assert [call[0] for call in fake.calls] == ["GET"]

    fake.calls.clear()
    # replace of a missing name is rejected.
    response = client.patch(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={
            "credentials": {
                "replace": [{"name": "nope", "source": {"type": "inline", "value": "z"}}]
            }
        },
    )
    assert response.status_code == 400

    # An egress 412 (concurrent writer) propagates as 412: the GET succeeds,
    # then another replica commits and the PUT loses the revision race.
    fake.calls.clear()
    fake.payloads.clear()
    fake.outcomes = [
        (200, json.dumps(_FULL_REVISION).encode()),
        (412, b"expectedRevision 2 does not match current revision 3"),
    ]
    response = client.patch(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={"credentials": {"delete": ["tok"]}},
    )
    assert response.status_code == 412
    assert "revision 3" in response.json()["detail"]["message"]


def test_delete_relays_and_maps_errors(vault_fsb):
    client, fake, _ = vault_fsb
    fake.outcome = (204, b"")
    response = client.delete(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault")
    assert response.status_code == 204
    assert fake.calls == [("DELETE", f"/vault/{_SANDBOX_UID}", _SANDBOX_UID)]

    fake.outcome = (404, b"credential vault not found")
    assert (
        client.delete(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault").status_code
        == 404
    )


def test_relay_unavailable_maps_to_503(vault_fsb):
    client, fake, _ = vault_fsb
    fake.outcome = grpc.StatusCode.UNAVAILABLE
    response = client.get(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault")
    assert response.status_code == 503

    fake.outcome = (503, b"vault persistence recovery required")
    response = client.get(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault")
    assert response.status_code == 503

    fake.outcome = (500, b"vault persistence failure")
    response = client.get(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault")
    assert response.status_code == 503


def test_validation_errors_do_not_echo_submitted_values(vault_fsb):
    """A 422 must not carry the submitted input back: pydantic error entries
    contain `input` (the whole body, including write-only credential values),
    which the request-validation handler strips."""
    client, _, _ = vault_fsb
    response = client.post(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={
            "bindings": [],
            "credentials": [{"source": {"type": "inline", "value": "sekrit"}}],
        },
    )

    assert response.status_code == 422
    assert "sekrit" not in response.text
    for error in response.json()["detail"]:
        assert "input" not in error
        assert "ctx" not in error


def test_unknown_sandbox_is_404(vault_fsb):
    client, _, _ = vault_fsb
    response = client.get("/v1/sandboxes/fsb-absent/credential-vault")
    assert response.status_code == 404


def test_non_fsb_sandbox_proxies_the_sidecar_vault(vault_fsb):
    client, _, _ = vault_fsb
    captured = {}

    async def _fake_proxy(request, sandbox_id, port, full_path, *, internal=False):
        captured["sandbox_id"] = sandbox_id
        captured["port"] = port
        captured["path"] = full_path
        captured["internal"] = internal
        from starlette.responses import Response

        return Response(status_code=204)

    with patch(
        "opensandbox_server.api.credential_vault._proxy_http_request",
        side_effect=_fake_proxy,
    ):
        response = client.delete("/v1/sandboxes/sbx-plain/credential-vault")

    assert response.status_code == 204
    assert captured == {
        "sandbox_id": "sbx-plain",
        "port": 18080,
        "path": "credential-vault",
        "internal": True,
    }


def test_composite_service_routes_fsb_to_the_relay(vault_fsb, monkeypatch):
    """A composite backend must keep routing fsb ids to the relay surface."""
    client, fake, service = vault_fsb
    composite = CompositeSandboxService(kubernetes=Mock(), fsb=service)
    monkeypatch.setattr(lifecycle, "sandbox_service", composite)
    fake.outcome = (200, json.dumps(_FULL_REVISION).encode())

    response = client.get(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault")
    assert response.status_code == 200
    assert "sekrit" not in response.text


# The real public route and FastPath gRPC client are exercised; only the
# remote handler is substituted. Go handler tests cover durable file effects.
def _writer_state(state="active", sequence=5):
    return {
        "version": "sandbox.fast.io/vault-writer/v2",
        "owner": {"sandboxUid": _SANDBOX_UID},
        "initializationId": "init-a",
        "state": state,
        "token": {"lineage": "L", "sequence": sequence},
        "vault": {**_FULL_REVISION, "revision": 1} if state == "active" else None,
    }


def test_writer_patch_preserves_first_read_token_across_recreate(vault_fsb):
    client, fake, _ = vault_fsb
    original = _writer_state()
    successor = _writer_state(sequence=7)
    writes = []

    def writer(request):
        if request.method == "GET":
            # This is the snapshot preceding concurrent DELETE/recreate.
            return 200, json.dumps(original).encode()
        update = json.loads(request.payload)
        writes.append(update)
        assert update["vault"]["expectedRevision"] == 1
        assert successor["vault"]["revision"] == 1  # Public revision ABA.
        if update["expectedState"] != successor["token"]:
            return 412, b"private state precondition mismatch"
        pytest.fail("stale PATCH adopted the successor token")

    fake.writer = writer
    response = client.patch(
        f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault",
        json={"bindings": {"delete": ["ext"]}},
    )
    assert response.status_code == 412
    assert len(writes) == 1
    assert writes[0]["expectedState"] == original["token"]
    assert [c[0] for c in fake.calls] == ["GET", "PUT"]
    assert successor["vault"]["bindings"] == _FULL_REVISION["bindings"]


@pytest.mark.parametrize("state", ["empty", "deleted"])
def test_writer_absence_is_public_404_but_create_uses_token(vault_fsb, state):
    client, fake, _ = vault_fsb
    internal = _writer_state(state=state)
    fake.writer = lambda request: (200, json.dumps(internal).encode()) if request.method == "GET" else (201, _vault_ack(1))
    path = f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault"
    assert client.get(path).status_code == 404
    response = client.post(path, json={"credentials": [], "bindings": []})
    assert response.status_code == 201
    assert json.loads(fake.payloads[-1])["expectedState"] == internal["token"]
    assert "token" not in response.text and "lineage" not in response.text


def test_writer_read_sanitizes_and_delete_carries_token(vault_fsb):
    client, fake, _ = vault_fsb
    internal = _writer_state()
    fake.writer = lambda request: (200, json.dumps(internal).encode()) if request.method == "GET" else (204, b"")
    path = f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault"
    response = client.get(path)
    assert response.status_code == 200
    for private in ("sekrit", "lineage", "initializationId", "owner", "token"):
        assert private not in response.text
    assert client.delete(path).status_code == 204
    assert json.loads(fake.payloads[-1])["expectedState"] == internal["token"]


@pytest.mark.parametrize("failure", [503, 412])
def test_writer_uncertainty_does_not_fallback_or_retry(vault_fsb, failure):
    client, fake, _ = vault_fsb
    fake.writer = lambda request: (failure, b"unavailable")
    response = client.post(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault", json={"credentials": [], "bindings": []})
    assert response.status_code == failure
    assert len(fake.calls) == 1
    assert fake.calls[0][1].startswith("/vault-state/")


def test_writer_lost_mutation_ack_is_not_retried(vault_fsb):
    client, fake, _ = vault_fsb
    fake.writer = lambda request: (200, json.dumps(_writer_state()).encode()) if request.method == "GET" else (503, b"ack lost")
    response = client.patch(f"/v1/sandboxes/{_SANDBOX_ID}/credential-vault", json={"bindings": {"delete": ["ext"]}})
    assert response.status_code == 503
    assert [c[0] for c in fake.calls] == ["GET", "PUT"]
