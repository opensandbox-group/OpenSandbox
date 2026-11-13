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

"""
API routes for the OpenSandbox Diagnostics API.

Structured routing context plus inline JSON diagnostic descriptors for
sandbox logs and events, selected by a required ``scope`` query parameter.
"""

from fastapi import APIRouter, Query, status
from fastapi.responses import JSONResponse

from opensandbox_server.api.lifecycle import sandbox_service
from opensandbox_server.services.diagnostics import DiagnosticContext, DiagnosticResult

router = APIRouter(tags=["Diagnostics"])


@router.get(
    "/sandboxes/{sandbox_id}/diagnostics/context",
    response_model=DiagnosticContext,
    response_model_exclude_none=True,
    responses={404: {"description": "Sandbox not found"}, 501: {"description": "Not implemented"}},
)
def get_sandbox_diagnostic_context(sandbox_id: str) -> DiagnosticContext:
    """Identify the runtime backend and its diagnostic identifiers."""
    return sandbox_service.get_sandbox_diagnostic_context(sandbox_id)


def _diagnostic_inline_response(
    result: DiagnosticResult,
) -> JSONResponse:
    """Build a Diagnostics API descriptor for inline plain-text content."""
    payload: dict[str, object] = {
        "sandboxId": result.sandbox_id,
        "kind": result.kind,
        "scope": result.scope,
        "delivery": "inline",
        "contentType": "text/plain; charset=utf-8",
        "content": result.content,
        "contentLength": len(result.content.encode("utf-8")),
        "truncated": result.truncated,
    }
    if result.warnings:
        payload["warnings"] = list(result.warnings)
    return JSONResponse(status_code=status.HTTP_200_OK, content=payload)


@router.get(
    "/sandboxes/{sandbox_id}/diagnostics/logs",
    status_code=status.HTTP_200_OK,
    responses={
        200: {
            "description": "Diagnostic log content descriptor",
            "content": {"application/json": {}},
        },
        400: {"description": "Unsupported diagnostics scope"},
        404: {"description": "Sandbox not found"},
    },
)
def get_sandbox_logs(
    sandbox_id: str,
    scope: str = Query(
        ...,
        description=(
            "Required diagnostic scope selector, e.g. 'container' or 'all'. "
            "Supported scopes are runtime-defined."
        ),
    ),
) -> JSONResponse:
    """Retrieve a diagnostic descriptor for sandbox log text."""
    result = sandbox_service.get_sandbox_log_diagnostics(sandbox_id, scope)
    return _diagnostic_inline_response(result)


@router.get(
    "/sandboxes/{sandbox_id}/diagnostics/events",
    status_code=status.HTTP_200_OK,
    responses={
        200: {
            "description": "Diagnostic event content descriptor",
            "content": {"application/json": {}},
        },
        400: {"description": "Unsupported diagnostics scope"},
        404: {"description": "Sandbox not found"},
    },
)
def get_sandbox_events(
    sandbox_id: str,
    scope: str = Query(
        ...,
        description=(
            "Required diagnostic scope selector, e.g. 'runtime' or 'all'. "
            "Supported scopes are runtime-defined."
        ),
    ),
) -> JSONResponse:
    """Retrieve a diagnostic descriptor for sandbox event text."""
    result = sandbox_service.get_sandbox_event_diagnostics(sandbox_id, scope)
    return _diagnostic_inline_response(result)
