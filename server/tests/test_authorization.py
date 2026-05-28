# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.

import pytest
from fastapi import status
from fastapi.exceptions import HTTPException

from opensandbox_server.api.schema import ImageSpec, Sandbox, SandboxStatus
from opensandbox_server.config import AuthzConfig
from opensandbox_server.middleware.authorization import (
    LifecycleAction,
    authorize_action,
    sandbox_in_scope,
)
from opensandbox_server.middleware.principal import build_user_principal, principal_for_api_key


def _box(owner: str, team: str | None = "t1") -> Sandbox:
    from datetime import datetime, timedelta, timezone

    now = datetime.now(timezone.utc)
    meta = {"access.owner": owner}
    if team is not None:
        meta["access.team"] = team
    return Sandbox(
        id="s1",
        image=ImageSpec(uri="x"),
        status=SandboxStatus(state="Running"),
        metadata=meta,
        entrypoint=["sh"],
        expiresAt=now + timedelta(hours=1),
        createdAt=now,
    )


def test_service_admin_bypasses_scope():
    p = principal_for_api_key()
    z = AuthzConfig()
    assert sandbox_in_scope(p, _box("other"), z.owner_metadata_key, z.team_metadata_key)


def test_user_in_scope_owner_and_team():
    z = AuthzConfig()
    p = build_user_principal("u1", "t1", "read_only", z)
    assert sandbox_in_scope(p, _box(p.canonical_owner, "t1"), z.owner_metadata_key, z.team_metadata_key)
    assert not sandbox_in_scope(
        p, _box("someone-else", "t1"), z.owner_metadata_key, z.team_metadata_key
    )


def test_read_only_cannot_create():
    z = AuthzConfig()
    p = build_user_principal("u1", "t1", "read_only", z)
    with pytest.raises(HTTPException) as ei:
        authorize_action(
            p,
            LifecycleAction.CREATE,
            owner_key=z.owner_metadata_key,
            team_key=z.team_metadata_key,
        )
    assert ei.value.status_code == status.HTTP_403_FORBIDDEN


def test_operator_can_create():
    z = AuthzConfig()
    p = build_user_principal("u1", "t1", "operator", z)
    authorize_action(
        p,
        LifecycleAction.CREATE,
        owner_key=z.owner_metadata_key,
        team_key=z.team_metadata_key,
    )


def test_none_principal_allows_mutation_for_legacy_dev():
    authorize_action(
        None,
        LifecycleAction.CREATE,
        owner_key="access.owner",
        team_key="access.team",
    )
