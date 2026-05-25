# Copyright 2026 Alibaba Group Holding Ltd.
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
