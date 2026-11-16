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

"""The live OpenAPI document must match the published spec's resource schemas.

The deployed /openapi.json is built from these Pydantic models, while the
SDKs are generated from specs/sandbox-lifecycle.yml. When the two diverge,
generated client docs promise semantics the server rejects. These tests pin
the schema wiring on the live document:

- ``CreateSandboxRequest.resourceRequests`` points at its own
  ``ResourceRequests`` schema (Kubernetes reservation semantics; the shared
  ``ResourceLimits`` component's Docker writable-layer wording must not leak
  into it).
- ``CreateFsbTemplateRequest.resourceLimits`` and ``FsbTemplate.resourceLimits``
  point at ``FsbTemplateResourceLimits`` (guest machine sizing; the
  storage/ephemeral-storage aliases are rejected, not honored).
"""

from fastapi.testclient import TestClient

from opensandbox_server.api.schema import (
    CreateFsbTemplateRequest,
    CreateSandboxRequest,
    FsbTemplate,
    FsbTemplateResourceLimits,
    ResourceRequests,
)


def _property_ref(schema: dict, model: str, field: str) -> str:
    prop = schema[model]["properties"][field]
    assert "anyOf" in prop, f"expected an optional (anyOf-wrapped) ref for {model}.{field}"
    return prop["anyOf"][0]["$ref"]


def _openapi(client: TestClient) -> dict:
    response = client.get("/openapi.json")
    assert response.status_code == 200
    return response.json()


def test_resource_requests_uses_dedicated_schema(client: TestClient) -> None:
    schema = _openapi(client)["components"]["schemas"]

    ref = _property_ref(schema, "CreateSandboxRequest", "resourceRequests")
    assert ref == "#/components/schemas/ResourceRequests"

    assert "ResourceRequests" in schema
    description = schema["ResourceRequests"]["description"]
    assert "ignored by the Docker runtime" in description
    assert "does not create a writable-layer quota" in description


def test_resource_limits_keeps_docker_quota_description(client: TestClient) -> None:
    schema = _openapi(client)["components"]["schemas"]

    ref = _property_ref(schema, "CreateSandboxRequest", "resourceLimits")
    assert ref == "#/components/schemas/ResourceLimits"
    assert "Writable-layer disk quota" in schema["ResourceLimits"]["description"]


def test_template_resource_limits_use_template_schema(client: TestClient) -> None:
    schema = _openapi(client)["components"]["schemas"]

    request_ref = _property_ref(schema, "CreateFsbTemplateRequest", "resourceLimits")
    template_ref = _property_ref(schema, "FsbTemplate", "resourceLimits")
    assert request_ref == "#/components/schemas/FsbTemplateResourceLimits"
    assert template_ref == "#/components/schemas/FsbTemplateResourceLimits"

    description = schema["FsbTemplateResourceLimits"]["description"]
    assert "writable-layer quota aliases and are not template keys" in description
    assert "HTTP 400" in description


def test_resource_requests_parses_into_dedicated_model() -> None:
    request = CreateSandboxRequest.model_validate(
        {
            "image": {"uri": "python:3.11"},
            "entrypoint": ["python", "-c", "print('hi')"],
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
            "resourceRequests": {"cpu": "250m"},
        }
    )
    assert isinstance(request.resource_requests, ResourceRequests)
    assert request.resource_requests.root == {"cpu": "250m"}

    payload = request.model_dump(by_alias=True, exclude_none=True)
    assert payload["resourceRequests"] == {"cpu": "250m"}


def test_template_resource_limits_parse_into_dedicated_model() -> None:
    request = CreateFsbTemplateRequest.model_validate(
        {
            "image": "python:3.11",
            "resourceLimits": {"cpu": "1", "memory": "512Mi", "disk": "2Gi"},
            "publish": "oci://registry.example.com/templates:latest",
        }
    )
    assert isinstance(request.resource_limits, FsbTemplateResourceLimits)
    assert request.resource_limits.root == {"cpu": "1", "memory": "512Mi", "disk": "2Gi"}

    template = FsbTemplate.model_validate(
        {
            "templateId": "tpl_00000000-0000-0000-0000-000000000000",
            "image": "python:3.11",
            "resourceLimits": {"cpu": "1"},
            "publish": "oci://registry.example.com/templates:latest",
            "format": "overlaybd",
            "status": {"phase": "Pending"},
            "createdAt": "2026-11-11T00:00:00Z",
            "updatedAt": "2026-11-11T00:00:00Z",
        }
    )
    assert isinstance(template.resource_limits, FsbTemplateResourceLimits)
    assert template.model_dump(by_alias=True)["resourceLimits"] == {"cpu": "1"}
