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

"""Renew-intent JSON parsing tests."""

from datetime import datetime, timedelta, timezone

import pytest

from opensandbox_server.integrations.renew_intent.intent import parse_renew_intent_json
from opensandbox_server.integrations.renew_intent.consumer import RenewIntentConsumer


def test_parse_matches_ingress_intent_shape():
    raw = (
        '{"sandbox_id":"abc","observed_at":"2026-03-22T12:00:00.123456789Z",'
        '"port":8080,"request_uri":"/x"}'
    )
    intent = parse_renew_intent_json(raw)
    assert intent is not None
    assert intent.sandbox_id == "abc"
    assert intent.port == 8080
    assert intent.request_uri == "/x"
    assert intent.observed_at.tzinfo is not None


def test_parse_rejects_bad_json():
    assert parse_renew_intent_json("not json") is None


@pytest.mark.parametrize(
    "observed_at,expect_stale",
    [
        (datetime.now(timezone.utc) - timedelta(seconds=400), True),
        (datetime.now(timezone.utc) - timedelta(seconds=10), False),
    ],
)
def test_stale_gate(observed_at, expect_stale):
    assert RenewIntentConsumer._is_stale(observed_at) is expect_stale
