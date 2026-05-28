# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.

from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest
from fastapi import status
from fastapi.exceptions import HTTPException

from opensandbox_server.api.lifecycle_helpers import authorize_mutating_action, merge_list_scope_from_request
from opensandbox_server.api.schema import ListSandboxesRequest, PaginationRequest, SandboxFilter
from opensandbox_server.config import AppConfig, AuthzConfig, IngressConfig, RuntimeConfig, ServerConfig
from opensandbox_server.middleware.authorization import LifecycleAction
from opensandbox_server.middleware.principal import build_user_principal


def _min_config() -> AppConfig:
    return AppConfig(
        server=ServerConfig(),
        authz=AuthzConfig(
            owner_metadata_key="access.owner",
            team_metadata_key="access.team",
        ),
        runtime=RuntimeConfig(type="docker", execd_image="x"),
        ingress=IngressConfig(mode="direct"),
    )


def _make_request(principal=None) -> MagicMock:
    req = MagicMock()
    req.state = SimpleNamespace(principal=principal)
    req.headers = {}
    return req


def test_authorize_mutating_action_logs_and_reraises_on_403():
    from unittest.mock import patch

    z = _min_config()
    p = build_user_principal("u1", None, "read_only", z.authz)
    req = _make_request(p)
    with patch("opensandbox_server.api.lifecycle_helpers.logger") as mock_log:
        with pytest.raises(HTTPException) as ei:
            authorize_mutating_action(
                req, p, LifecycleAction.CREATE,
                owner_key=z.authz.owner_metadata_key,
                team_key=z.authz.team_metadata_key,
                sandbox_id=None,
            )
    assert ei.value.status_code == status.HTTP_403_FORBIDDEN
    mock_log.info.assert_called_once()
    logged_msg = str(mock_log.info.call_args)
    assert "forbidden" in logged_msg


def test_authorize_mutating_action_passes_for_operator():
    z = _min_config()
    p = build_user_principal("u1", None, "operator", z.authz)
    req = _make_request(p)
    # Should not raise
    authorize_mutating_action(
        req, p, LifecycleAction.CREATE,
        owner_key=z.authz.owner_metadata_key,
        team_key=z.authz.team_metadata_key,
    )


def test_merge_list_scope_injects_owner_for_user():
    z = _min_config()
    p = build_user_principal("alice", "t1", "read_only", z.authz)
    list_req = ListSandboxesRequest(
        filter=SandboxFilter(state=None, metadata={"k": "v"}),
        pagination=PaginationRequest(page=1, pageSize=20),
    )
    http_request = MagicMock()
    http_request.state = SimpleNamespace(principal=p)
    out = merge_list_scope_from_request(http_request, list_req, z)
    assert out.filter.metadata
    assert out.filter.metadata.get("k") == "v"
    assert out.filter.metadata.get("access.owner") == p.canonical_owner
    assert out.filter.metadata.get("access.team") == p.canonical_team
