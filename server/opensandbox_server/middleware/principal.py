# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.

"""
Authenticated principal (API key or trusted identity) for lifecycle authz and audit.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from typing import TYPE_CHECKING, Literal, Optional

from opensandbox_server.services.validators import LABEL_VALUE_RE

if TYPE_CHECKING:
    from opensandbox_server.config import AuthzConfig

AuthRole = Literal["read_only", "operator", "service_admin"]
PrincipalSource = Literal["api_key", "user"]


@dataclass(frozen=True, slots=True)
class Principal:
    """
    Runtime identity for authorization. ``service_admin`` (API key) bypasses owner/team scope.
    """

    source: PrincipalSource
    subject: str
    role: AuthRole
    canonical_owner: str
    canonical_team: Optional[str] = None
    """When ``None`` (no team in trusted headers), only ``access.owner`` is enforced for scope."""

    @property
    def is_service_admin(self) -> bool:
        return self.role == "service_admin"


def canonicalize_scoped_value(raw: str) -> str:
    """
    Map an arbitrary string to a stable Kubernetes label value (≤63 chars) for metadata scope keys.

    Deterministic: the same input always maps to the same output. If the value is already
    a valid label value, it is returned unchanged.
    """
    s = (raw or "").strip()
    if s == "":
        return ""
    if len(s) <= 63 and _is_valid_label_value(s):
        return s
    digest = hashlib.sha256(s.encode("utf-8")).hexdigest()[:32]
    return digest


def _is_valid_label_value(value: str) -> bool:
    if len(value) > 63:
        return False
    return bool(LABEL_VALUE_RE.match(value))


def resolve_effective_role(
    raw_subject: str,
    roles_header_value: Optional[str],
    authz: "AuthzConfig",
) -> AuthRole:
    """
    Derive the effective role from static subject lists, then the roles header, then default.
    """
    if raw_subject in authz.operator_subjects:
        return "operator"
    if raw_subject in authz.read_only_subjects:
        return "read_only"
    if roles_header_value:
        parts = {p.strip().lower() for p in roles_header_value.split(",") if p.strip()}
        if "operator" in parts or "op" in parts:
            return "operator"
        if "read_only" in parts or "readonly" in parts or "read-only" in parts:
            return "read_only"
    d = (authz.default_role or "read_only").lower()
    if d == "operator":
        return "operator"
    return "read_only"


def principal_for_api_key() -> Principal:
    return Principal(
        source="api_key",
        subject="api-key",
        role="service_admin",
        canonical_owner="",
        canonical_team=None,
    )


def build_user_principal(
    raw_subject: str,
    raw_team: Optional[str],
    roles_header: Optional[str],
    authz: "AuthzConfig",
) -> Principal:
    if not (raw_subject or "").strip():
        raise ValueError("raw_subject is required for user principal")
    subj = (raw_subject or "").strip()
    team_raw = (raw_team or "").strip() or None
    role = resolve_effective_role(subj, roles_header, authz)
    owner = canonicalize_scoped_value(subj)
    if not owner:
        raise ValueError("invalid subject after canonicalization")
    team = canonicalize_scoped_value(team_raw) if team_raw else None
    return Principal(
        source="user",
        subject=subj,
        role=role,
        canonical_owner=owner,
        canonical_team=team,
    )
