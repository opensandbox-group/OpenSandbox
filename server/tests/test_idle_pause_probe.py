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

"""ActivityProber tests: routing, sentinel header, and verdict mapping."""

from datetime import datetime, timedelta, timezone

import httpx
import pytest

from opensandbox_server.api.schema import Endpoint
from opensandbox_server.integrations.idle_pause.inventory import SandboxCandidate
from opensandbox_server.integrations.idle_pause.policy import (
    ActivityProbe,
    validate_idle_policy,
)
from opensandbox_server.integrations.idle_pause.probe import (
    MAX_PROBE_MESSAGE_CHARS,
    SKIP_ACTIVITY_HEADER,
    SKIP_ACTIVITY_VALUE,
    ActivityProber,
    PROBE_OUTCOME_ACTIVE,
    PROBE_OUTCOME_ERROR,
    PROBE_OUTCOME_NOT_ACTIVE,
)


def _candidate() -> SandboxCandidate:
    return SandboxCandidate(
        sandbox_id="fsb_x",
        namespace="ns1",
        uid="uid-1",
        generation=7,
        state="Running",
        expires_at=datetime.now(timezone.utc) + timedelta(hours=1),
        policy=validate_idle_policy(
            idle_timeout_seconds=300,
            on_idle="pause",
            active_probe=ActivityProbe(port=8080, path="/active", timeout_seconds=2),
        ),
    )


def _prober(
    handler,
    endpoint: Endpoint,
) -> ActivityProber:
    transport = httpx.MockTransport(handler)
    return ActivityProber(
        endpoint_builder=lambda namespace, sandbox_id, port: endpoint,
        client=httpx.AsyncClient(transport=transport),
    )


@pytest.mark.asyncio
async def test_active_answer_verdict_and_request_shape():
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["sentinel"] = request.headers.get(SKIP_ACTIVITY_HEADER)
        seen["route"] = request.headers.get("OpenSandbox-Ingress-To")
        return httpx.Response(200, json={"active": True})

    prober = _prober(
        handler,
        Endpoint(
            endpoint="https://gateway.example.com",
            headers={"OpenSandbox-Ingress-To": "route-scope"},
        ),
    )
    try:
        verdict = await prober.probe(_candidate())
    finally:
        await prober.aclose()

    assert verdict.outcome == PROBE_OUTCOME_ACTIVE
    assert seen["url"] == "https://gateway.example.com/active"
    assert seen["sentinel"] == SKIP_ACTIVITY_VALUE
    assert seen["route"] == "route-scope"


@pytest.mark.asyncio
async def test_uri_mode_joins_scope_and_probe_path():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"active": False})

    prober = _prober(
        handler,
        Endpoint(endpoint="https://gateway.example.com/f1.scope"),
    )
    try:
        verdict = await prober.probe(_candidate())
    finally:
        await prober.aclose()

    assert verdict.outcome == PROBE_OUTCOME_NOT_ACTIVE


@pytest.mark.asyncio
async def test_scheme_less_gateway_address_defaults_to_http():
    """`ingress.gateway.address` rejects a scheme in config; the probe must
    still produce an absolute URL (httpx rejects scheme-less targets)."""
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        return httpx.Response(200, json={"active": True})

    prober = _prober(handler, Endpoint(endpoint="gateway.example.com"))
    try:
        verdict = await prober.probe(_candidate())
    finally:
        await prober.aclose()

    assert verdict.outcome == PROBE_OUTCOME_ACTIVE
    assert seen["url"] == "http://gateway.example.com/active"


@pytest.mark.asyncio
async def test_uri_mode_scheme_less_address_defaults_to_http():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"active": False})

    prober = _prober(handler, Endpoint(endpoint="gateway.example.com/f1.scope"))
    try:
        verdict = await prober.probe(_candidate())
    finally:
        await prober.aclose()

    assert verdict.outcome == PROBE_OUTCOME_NOT_ACTIVE


@pytest.mark.asyncio
async def test_not_active_carries_capped_message():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"active": False, "message": "m" * 400})

    prober = _prober(handler, Endpoint(endpoint="https://gateway.example.com"))
    try:
        verdict = await prober.probe(_candidate())
    finally:
        await prober.aclose()

    assert verdict.outcome == PROBE_OUTCOME_NOT_ACTIVE
    assert verdict.message is not None
    assert len(verdict.message) == MAX_PROBE_MESSAGE_CHARS
    assert verdict.audit_note().startswith("probe=not_active message=")


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "response",
    [
        httpx.Response(500, json={"active": False}),
        httpx.Response(200, text="not json"),
        httpx.Response(200, json=["active"]),
        httpx.Response(200, json={"active": "yes"}),
        httpx.Response(200, json={}),
    ],
)
async def test_invalid_answers_are_errors(response):
    def handler(request: httpx.Request) -> httpx.Response:
        return response

    prober = _prober(handler, Endpoint(endpoint="https://gateway.example.com"))
    try:
        verdict = await prober.probe(_candidate())
    finally:
        await prober.aclose()

    assert verdict.outcome == PROBE_OUTCOME_ERROR


@pytest.mark.asyncio
async def test_transport_failure_is_error():
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("boom")

    prober = _prober(handler, Endpoint(endpoint="https://gateway.example.com"))
    try:
        verdict = await prober.probe(_candidate())
    finally:
        await prober.aclose()

    assert verdict.outcome == PROBE_OUTCOME_ERROR


@pytest.mark.asyncio
async def test_endpoint_builder_failure_is_error():
    def build(namespace: str, sandbox_id: str, port: int) -> Endpoint:
        raise ValueError("no ingress gateway configured")

    prober = ActivityProber(endpoint_builder=build)
    try:
        verdict = await prober.probe(_candidate())
    finally:
        await prober.aclose()

    assert verdict.outcome == PROBE_OUTCOME_ERROR
