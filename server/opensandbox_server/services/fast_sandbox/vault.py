# Copyright 2026 The OpenSandbox Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

"""Server-side Credential Vault relay for the fast-sandbox profile (OSEP-0022).

The vault is egress-held: the server pushes complete vault revisions over the
FastPath handler relay (``InvokeHandlerAction`` → Fastlet → egress
``/vault/{sandboxUid}``) and treats the relay response as the synchronous ack.

Responsibilities owned here (the egress owns validation, persistence, and the
revision state machine):

- shape validation of the public request bodies (the egress re-validates
  authoritatively and NACKs with 400 keeping last-good);
- sanitize-on-read: credential values never leave the server on any public
  surface — the internal relay ``GET`` returns the full revision and every
  read is stripped here before it becomes a response;
- ``PATCH`` composed server-side: relay ``GET`` → merge in memory → relay
  ``PUT`` carrying the read revision as ``expectedRevision`` (the per-sandbox
  in-process lock serializes local merges; the revision precondition is the
  cross-replica backstop — a stale writer gets 412).

Plaintext values traverse the server only transiently during the merge; no
vault state is persisted server-side.
"""

from __future__ import annotations

import json
from typing import Any, Optional

from fastapi import HTTPException
from starlette import status

from opensandbox_server.api.schema import (
    CredentialVaultCreateRequest,
    CredentialVaultMutationRequest,
    CredentialVaultState,
)
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.fast_sandbox.fastpath_client import (
    FastPathError,
    FastPathHandlerError,
)

# The Pool-declared Action Handler hosting the egress surface (same handler
# the networkpolicy binding rides).
EGRESS_HANDLER = "egress"

# Relay loopback path prefix on the egress handler (components/egress
# constants.VaultRelayPathPrefix). The subject identity is the Sandbox UID —
# the same value the Fastlet delivers in the action envelope's sandbox.uid.
_VAULT_PATH_PREFIX = "/vault/"


def vault_relay_error(status_code: int, message: str) -> HTTPException:
    """Map an egress relay status onto the public HTTP contract."""
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
                "message": message or "Credential Vault backend unavailable.",
            },
        )
    mapped = status_code if 400 <= status_code <= 499 else status.HTTP_500_INTERNAL_SERVER_ERROR
    return HTTPException(
        status_code=mapped,
        detail={"code": SandboxErrorCodes.FSB_API_ERROR, "message": message},
    )


def _sanitize_auth(auth: Optional[dict]) -> Optional[dict]:
    """Mirror the egress sanitizeAuth: type always, name for apiKey only."""
    if not isinstance(auth, dict):
        return None
    metadata: dict[str, Any] = {"type": auth.get("type")}
    if auth.get("type") == "apiKey" and auth.get("name") is not None:
        metadata["name"] = auth.get("name")
    return metadata


def sanitize_full_revision(full: dict) -> dict:
    """Strip credential values from an internal relay revision.

    The relay ``GET`` body carries per-record ``{name, source}`` credentials
    and ``{name, match, auth}`` bindings without per-record revisions, so the
    sanitized projection stamps every record with the top-level revision (the
    egress does the same after a snapshot reload).
    """
    revision = full.get("revision")
    credentials = []
    for record in full.get("credentials") or []:
        source = record.get("source") if isinstance(record.get("source"), dict) else {}
        credentials.append(
            {
                "name": record.get("name"),
                "sourceType": source.get("type", ""),
                "revision": revision,
            }
        )
    bindings = []
    for record in full.get("bindings") or []:
        bindings.append(
            {
                "name": record.get("name"),
                "revision": revision,
                "match": record.get("match"),
                "auth": _sanitize_auth(record.get("auth")),
            }
        )
    return CredentialVaultState.model_validate(
        {"revision": revision, "credentials": credentials, "bindings": bindings}
    ).model_dump(by_alias=True, exclude_none=True)


def _normalize_ack(body: bytes) -> dict:
    """Pass a relay write ack through the sanitized response model.

    The egress ack is already sanitized; round-tripping it through the model
    guarantees no value-shaped field can survive onto a public surface.
    """
    try:
        payload = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise HTTPException(
            status_code=status.HTTP_502_BAD_GATEWAY,
            detail={
                "code": SandboxErrorCodes.FSB_API_ERROR,
                "message": "Credential Vault ack was not valid JSON.",
            },
        ) from exc
    return CredentialVaultState.model_validate(payload).model_dump(
        by_alias=True, exclude_none=True
    )


# -- merge (mirrors the egress applyCredentialMutations/applyBindingMutations
#    semantics: delete → replace → add, every mentioned name resolved at most
#    once, add rejects existing names, replace/delete require them) ---------


def _merge_records(
    records: dict[str, dict],
    mutation_set: Any,
    *,
    kind: str,
    dump,
) -> dict[str, dict]:
    mentioned: set[str] = set()

    for raw_name in mutation_set.delete or []:
        name = raw_name.strip()
        if not name:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": f"{kind} delete name cannot be blank",
                },
            )
        if name in mentioned:
            raise _mentioned_twice(kind, name)
        mentioned.add(name)
        if name not in records:
            raise _missing(kind, name)
        del records[name]

    for entry in mutation_set.replace or []:
        dump_result = dump(entry)
        name = dump_result["name"]
        if name in mentioned:
            raise _mentioned_twice(kind, name)
        mentioned.add(name)
        if name not in records:
            raise _missing(kind, name)
        records[name] = dump_result

    add_seen: set[str] = set()
    for entry in mutation_set.add or []:
        dump_result = dump(entry)
        name = dump_result["name"]
        if name in mentioned:
            raise _mentioned_twice(kind, name)
        if name in add_seen:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": f"duplicate {kind} add name {name!r}",
                },
            )
        add_seen.add(name)
        if name in records:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": f"{kind} {name!r} already exists",
                },
            )
        records[name] = dump_result

    return records


def _mentioned_twice(kind: str, name: str) -> HTTPException:
    return HTTPException(
        status_code=status.HTTP_400_BAD_REQUEST,
        detail={
            "code": SandboxErrorCodes.INVALID_PARAMETER,
            "message": f"{kind} {name!r} mentioned by multiple operations",
        },
    )


def _missing(kind: str, name: str) -> HTTPException:
    return HTTPException(
        status_code=status.HTTP_400_BAD_REQUEST,
        detail={
            "code": SandboxErrorCodes.INVALID_PARAMETER,
            "message": f"{kind} {name!r} does not exist",
        },
    )


def _index_records(entries: Optional[list]) -> dict[str, dict]:
    indexed: dict[str, dict] = {}
    for entry in entries or []:
        name = entry.get("name")
        if isinstance(name, str):
            indexed[name] = entry
    return indexed


def merge_mutation(
    full: dict, mutation: CredentialVaultMutationRequest
) -> dict:
    """Apply a public PATCH onto a full relay revision; returns the PUT body.

    Existing records round-trip verbatim (values included — the merge runs in
    server memory only); replaced/added records are re-serialized from the
    validated request shapes.
    """
    credentials = _index_records(full.get("credentials"))
    bindings = _index_records(full.get("bindings"))

    if mutation.credentials is not None:
        _merge_records(
            credentials,
            mutation.credentials,
            kind="credential",
            dump=lambda c: c.model_dump(by_alias=True, exclude_none=True),
        )
    if mutation.bindings is not None:
        _merge_records(
            bindings,
            mutation.bindings,
            kind="binding",
            dump=lambda b: b.model_dump(by_alias=True, exclude_none=True),
        )

    body: dict[str, Any] = {
        "credentials": [
            credentials[name]
            for name in sorted(credentials)
        ],
        "bindings": [
            bindings[name]
            for name in sorted(bindings)
        ],
    }
    if mutation.expected_revision is not None:
        body["expectedRevision"] = mutation.expected_revision
    return body


# -- relay ops (called from FastSandboxService; sync, raise HTTPException) --


def _resolve_subject(service, sandbox_id: str) -> tuple[str, str]:
    """Resolve (namespace, uid) for the relay target; 404 when unknown."""
    metadata = service._get_cr(sandbox_id)["metadata"]
    return metadata["namespace"], metadata["uid"]


def _relay(
    service,
    sandbox_id: str,
    *,
    method: str,
    payload: Optional[bytes] = None,
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
            path=f"{_VAULT_PATH_PREFIX}{uid}",
            payload=payload,
        )
    except FastPathHandlerError as exc:
        raise vault_relay_error(
            exc.status_code,
            exc.message,
        ) from exc
    except FastPathError as exc:
        raise service._fastpath_http_error(exc) from exc
    return int(result.status), bytes(result.body)


def relay_create(
    service, sandbox_id: str, request: CredentialVaultCreateRequest
) -> dict:
    """POST create-only; 409 when a vault already exists."""
    payload = request.model_dump_json(by_alias=True, exclude_none=True).encode("utf-8")
    _, body = _relay(service, sandbox_id, method="POST", payload=payload)
    return _normalize_ack(body)


def _relay_get_full(service, sandbox_id: str) -> dict:
    """Relay GET of the full revision INCLUDING values — internal only."""
    _, body = _relay(service, sandbox_id, method="GET")
    try:
        return json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise HTTPException(
            status_code=status.HTTP_502_BAD_GATEWAY,
            detail={
                "code": SandboxErrorCodes.FSB_API_ERROR,
                "message": "Credential Vault relay returned an invalid revision.",
            },
        ) from exc


def relay_get(service, sandbox_id: str) -> dict:
    """Public read: the full revision sanitized (values stripped)."""
    return sanitize_full_revision(_relay_get_full(service, sandbox_id))


def relay_patch(
    service, sandbox_id: str, request: CredentialVaultMutationRequest
) -> dict:
    """PATCH composed server-side: GET → merge → PUT(expectedRevision).

    The merge runs against the full revision in memory; the read revision
    rides the PUT as the cross-replica backstop (a concurrent writer wins the
    commit and the stale PATCH sees 412).
    """
    with service.vault_lock(sandbox_id):
        full = _relay_get_full(service, sandbox_id)
        merged = merge_mutation(full, request)
        _, ack = _relay(
            service, sandbox_id, method="PUT", payload=json.dumps(merged).encode("utf-8")
        )
    return _normalize_ack(ack)


def relay_delete(service, sandbox_id: str) -> None:
    _relay(service, sandbox_id, method="DELETE")


def _project(state: dict, records_key: str) -> dict:
    return {"revision": state["revision"], records_key: state[records_key]}


def relay_list_credentials(service, sandbox_id: str) -> dict:
    return _project(relay_get(service, sandbox_id), "credentials")


def relay_get_credential(service, sandbox_id: str, name: str) -> dict:
    state = relay_get(service, sandbox_id)
    for record in state["credentials"]:
        if record["name"] == name:
            return record
    raise HTTPException(
        status_code=status.HTTP_404_NOT_FOUND,
        detail={
            "code": SandboxErrorCodes.FSB_API_ERROR,
            "message": f"Credential {name!r} not found.",
        },
    )


def relay_list_bindings(service, sandbox_id: str) -> dict:
    return _project(relay_get(service, sandbox_id), "bindings")


def relay_get_binding(service, sandbox_id: str, name: str) -> dict:
    state = relay_get(service, sandbox_id)
    for record in state["bindings"]:
        if record["name"] == name:
            return record
    raise HTTPException(
        status_code=status.HTTP_404_NOT_FOUND,
        detail={
            "code": SandboxErrorCodes.FSB_API_ERROR,
            "message": f"Credential binding {name!r} not found.",
        },
    )
