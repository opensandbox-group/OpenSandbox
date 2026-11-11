# Copyright 2026 The OpenSandbox Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

"""Server-side network policy relay for the fast-sandbox profile.

The recorded policy is egress-held: the create-time binding input seeds it
once, and every runtime change goes over the FastPath handler relay
(``InvokeHandlerAction`` → Fastlet → egress ``/policy/{sandboxUid}``) with
the relay response as the synchronous ack. The Sandbox CR's
``spec.actionBindings`` stays the create-time bootstrap only — runtime
mutations through ``replace_action_bindings`` would be superseded by the
egress at the next binding replay.

Responsibilities owned here (the egress owns validation, persistence, and
the revision state machine):

- shape validation of the public request bodies (``normalized_policy`` —
  the egress re-validates authoritatively and NACKs with 400 keeping
  last-good);
- ``PATCH`` composed server-side: relay ``GET`` → merge in memory → relay
  ``PUT`` carrying the read revision as ``expectedRevision`` (the
  per-sandbox in-process lock serializes local merges; the revision
  precondition is the cross-replica backstop — a stale writer gets 409).

No policy state is persisted server-side; reads reflect exactly what the
egress enforces.
"""

from __future__ import annotations

import json

from fastapi import HTTPException
from starlette import status

from opensandbox_server.api.schema import NetworkRule
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.fast_sandbox.fastpath_client import (
    FastPathError,
    FastPathHandlerError,
)
from opensandbox_server.services.fast_sandbox.network_policy import (
    delete_policy_rules,
    merge_policy_rules,
    policy_status,
)
from opensandbox_server.services.fast_sandbox.vault import EGRESS_HANDLER

# Relay loopback path prefix on the egress handler (components/egress
# constants.PolicyRelayPathPrefix). The subject identity is the Sandbox UID —
# the same value the Fastlet delivers in the action envelope's sandbox.uid.
_POLICY_PATH_PREFIX = "/policy/"


def policy_relay_error(status_code: int, message: str) -> HTTPException:
    """Map an egress relay status onto the public HTTP contract.

    The egress reports revision conflicts as 412 (handler-relay internal
    contract); the public network-policy operations declare 409, so the
    conflict is remapped here instead of leaking an undocumented status to
    generated clients.
    """
    if status_code == status.HTTP_412_PRECONDITION_FAILED:
        return HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={"code": SandboxErrorCodes.FSB_API_ERROR, "message": message},
        )
    if status_code == 400:
        return HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"code": SandboxErrorCodes.INVALID_PARAMETER, "message": message},
        )
    if 500 <= status_code <= 599:
        return HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail={
                "code": SandboxErrorCodes.FSB_API_ERROR,
                "message": message or "Network policy backend unavailable.",
            },
        )
    mapped = status_code if 400 <= status_code <= 499 else status.HTTP_500_INTERNAL_SERVER_ERROR
    return HTTPException(
        status_code=mapped,
        detail={"code": SandboxErrorCodes.FSB_API_ERROR, "message": message},
    )


def _resolve_subject(service, sandbox_id: str) -> tuple[str, str]:
    """Resolve (namespace, uid) for the relay target; 404 when unknown."""
    metadata = service._get_cr(sandbox_id)["metadata"]
    return metadata["namespace"], metadata["uid"]


def _relay(
    service,
    sandbox_id: str,
    *,
    method: str,
    payload: bytes | None = None,
) -> tuple[int, bytes]:
    """Run one relay op and return (status, body) for any 2xx outcome."""
    namespace, uid = _resolve_subject(service, sandbox_id)
    try:
        result = service.fastpath_client.invoke_handler_action(
            namespace,
            sandbox_id,
            expected_uid=uid,
            handler=EGRESS_HANDLER,
            method=method,
            path=f"{_POLICY_PATH_PREFIX}{uid}",
            payload=payload,
        )
    except FastPathHandlerError as exc:
        raise policy_relay_error(exc.status_code, exc.message) from exc
    except FastPathError as exc:
        raise service._fastpath_http_error(exc) from exc
    return int(result.status), bytes(result.body)


def _relay_get_full(service, sandbox_id: str) -> dict:
    """Relay GET of the recorded state: {"revision": N, "policy": {...}}."""
    _, body = _relay(service, sandbox_id, method="GET")
    try:
        state = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise policy_relay_error(
            status.HTTP_503_SERVICE_UNAVAILABLE,
            "Network policy relay returned an invalid record.",
        ) from exc
    if not isinstance(state, dict) or not isinstance(state.get("policy"), dict):
        raise policy_relay_error(
            status.HTTP_503_SERVICE_UNAVAILABLE,
            "Network policy relay record is missing the policy body.",
        )
    if not isinstance(state.get("revision"), int):
        raise policy_relay_error(
            status.HTTP_503_SERVICE_UNAVAILABLE,
            "Network policy relay record is missing the revision.",
        )
    return state


def relay_get(service, sandbox_id: str) -> dict:
    """Public read: the recorded policy in the policy_status shape."""
    state = _relay_get_full(service, sandbox_id)
    return policy_status(state["policy"])


def relay_put(service, sandbox_id: str, normalized: dict) -> dict:
    """PUT a full replace (creates the record when absent)."""
    _, body = _relay(
        service,
        sandbox_id,
        method="PUT",
        payload=json.dumps({"policy": normalized}).encode("utf-8"),
    )
    try:
        ack = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        ack = None
    policy = (ack or {}).get("policy")
    return policy_status(policy if isinstance(policy, dict) else normalized)


def relay_patch(service, sandbox_id: str, rules: list[NetworkRule]) -> dict:
    """PATCH composed server-side: GET → merge → PUT(expectedRevision)."""
    with service.policy_lock(sandbox_id):
        state = _relay_get_full(service, sandbox_id)
        merged = merge_policy_rules(state["policy"], rules)
        payload = json.dumps(
            {"policy": merged, "expectedRevision": state["revision"]}
        ).encode("utf-8")
        _, body = _relay(service, sandbox_id, method="PUT", payload=payload)
    try:
        ack = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        ack = None
    policy = (ack or {}).get("policy")
    return policy_status(policy if isinstance(policy, dict) else merged)


def relay_delete_rules(service, sandbox_id: str, targets: list[str]) -> dict:
    """Remove rules by target (idempotent): GET → filter → PUT."""
    with service.policy_lock(sandbox_id):
        state = _relay_get_full(service, sandbox_id)
        kept = delete_policy_rules(state["policy"], targets)
        payload = json.dumps(
            {"policy": kept, "expectedRevision": state["revision"]}
        ).encode("utf-8")
        _, body = _relay(service, sandbox_id, method="PUT", payload=payload)
    try:
        ack = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        ack = None
    policy = (ack or {}).get("policy")
    return policy_status(policy if isinstance(policy, dict) else kept)
