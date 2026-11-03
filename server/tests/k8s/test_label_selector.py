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

import pytest

from opensandbox_server.services.k8s.label_selector import (
    matches,
    parse_selector,
)
from opensandbox_server.services.k8s.list_helpers import build_sandbox_label_selector


class TestParseSelector:
    def test_empty_selector_returns_match_all_terms(self):
        assert parse_selector("") == []
        assert parse_selector("   ") == []

    def test_bare_key_parses_as_existence_term(self):
        assert parse_selector("opensandbox.io/id") == [
            ("opensandbox.io/id", "exists", None)
        ]

    def test_equality_parses_as_eq_term(self):
        assert parse_selector("team=infra") == [("team", "eq", "infra")]

    def test_double_equals_parses_as_eq_term(self):
        assert parse_selector("team==infra") == [("team", "eq", "infra")]

    def test_comma_joined_clauses_parse_as_and(self):
        assert parse_selector("team=infra,project") == [
            ("team", "eq", "infra"),
            ("project", "exists", None),
        ]

    def test_whitespace_around_clauses_is_tolerated(self):
        assert parse_selector(" team = infra , project ") == [
            ("team", "eq", "infra"),
            ("project", "exists", None),
        ]

    def test_set_based_operator_returns_none(self):
        assert parse_selector("env in (prod, staging)") is None

    def test_negation_returns_none(self):
        assert parse_selector("!retired") is None

    def test_inequality_returns_none(self):
        assert parse_selector("team!=infra") is None

    def test_empty_clause_returns_none(self):
        assert parse_selector("team=infra,") is None
        assert parse_selector(",team=infra") is None


class TestMatches:
    @pytest.mark.parametrize(
        "labels,terms,expected",
        [
            ({"a": "1"}, [], True),
            ({}, [("a", "exists", None)], False),
            ({"a": ""}, [("a", "exists", None)], True),
            ({"a": "1"}, [("a", "exists", None)], True),
            ({"a": "1"}, [("a", "eq", "1")], True),
            ({"a": "2"}, [("a", "eq", "1")], False),
            ({"a": "1", "b": "x"}, [("a", "eq", "1"), ("b", "exists", None)], True),
            ({"a": "1"}, [("a", "eq", "1"), ("b", "exists", None)], False),
        ],
    )
    def test_matches(self, labels, terms, expected):
        assert matches(labels, terms) is expected


class TestBuildSandboxLabelSelector:
    def test_none_and_empty_metadata_return_sandbox_id_label(self):
        assert build_sandbox_label_selector(None) == "opensandbox.io/id"
        assert build_sandbox_label_selector({}) == "opensandbox.io/id"

    def test_single_metadata_appends_equality_term(self):
        assert (
            build_sandbox_label_selector({"team": "infra"})
            == "opensandbox.io/id,team=infra"
        )

    def test_multiple_metadata_sorted_lexicographically(self):
        assert (
            build_sandbox_label_selector({"team": "infra", "env": "prod"})
            == "opensandbox.io/id,env=prod,team=infra"
        )

    def test_metadata_with_dns_prefix_and_punctuation(self):
        assert (
            build_sandbox_label_selector(
                {"app.kubernetes.io/name": "sandbox", "version": "v1.0.0-beta_2"}
            )
            == "opensandbox.io/id,app.kubernetes.io/name=sandbox,version=v1.0.0-beta_2"
        )

    def test_unexpressible_values_and_keys_are_omitted(self):
        metadata = {
            "team": "infra",
            "spaced_val": "not allowed",
            "empty": "",
            "bad key!": "valid",
        }
        assert (
            build_sandbox_label_selector(metadata)
            == "opensandbox.io/id,team=infra"
        )

    def test_sandbox_id_metadata_replaces_existence_term(self):
        assert (
            build_sandbox_label_selector({"opensandbox.io/id": "sbx-test-123"})
            == "opensandbox.io/id=sbx-test-123"
        )
        assert (
            build_sandbox_label_selector(
                {"opensandbox.io/id": "sbx-test-123", "team": "infra"}
            )
            == "opensandbox.io/id=sbx-test-123,team=infra"
        )

