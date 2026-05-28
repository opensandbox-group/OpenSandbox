# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.

from opensandbox_server.config import AuthzConfig
from opensandbox_server.middleware.principal import (
    build_user_principal,
    canonicalize_scoped_value,
    principal_for_api_key,
    resolve_effective_role,
)


def test_canonicalize_scoped_value_passes_valid_label_unchanged():
    assert canonicalize_scoped_value("ab") == "ab"
    assert canonicalize_scoped_value("My-Team_1") == "My-Team_1"


def test_canonicalize_scoped_value_deterministic_for_long_or_invalid():
    a = canonicalize_scoped_value("not a valid label value because it has spaces")
    b = canonicalize_scoped_value("not a valid label value because it has spaces")
    assert a == b
    assert len(a) <= 63
    a2 = canonicalize_scoped_value("a" * 200)
    b2 = canonicalize_scoped_value("a" * 200)
    assert a2 == b2


def test_principal_for_api_key_is_service_admin():
    p = principal_for_api_key()
    assert p.is_service_admin
    assert p.role == "service_admin"


def test_resolve_effective_role_default_read_only():
    z = AuthzConfig()
    assert resolve_effective_role("anyone", None, z) == "read_only"


def test_resolve_effective_role_operator_subjects():
    z = AuthzConfig(operator_subjects=["alice"], default_role="read_only")
    assert resolve_effective_role("alice", None, z) == "operator"


def test_resolve_effective_role_roles_header_operator():
    z = AuthzConfig()
    assert resolve_effective_role("u", "read_only, operator", z) == "operator"


def test_build_user_principal_injects_scope_and_respects_role():
    z = AuthzConfig()
    p = build_user_principal("Alice", "t1", "operator", z)
    assert p.role == "operator"
    assert p.canonical_owner
    assert p.canonical_team == "t1"


def test_build_user_principal_team_optional():
    z = AuthzConfig()
    p = build_user_principal("Bob", None, "read_only", z)
    assert p.canonical_team is None
    assert p.canonical_owner == "Bob"
