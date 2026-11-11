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

from opensandbox_server.integrations.idle_pause.decision import (
    IdleVerdict,
    evaluate_idle,
    is_idle_by_tombstone,
    is_idle_by_value,
)

X = 300
NOW = 1_000_000_000_000  # ms
X_MS = X * 1000


def test_boundary_at_exactly_x_is_idle():
    assert is_idle_by_value(NOW - X_MS, NOW, X) is True
    assert is_idle_by_value(NOW - X_MS + 1, NOW, X) is False


def test_stale_activity_key_is_idle_by_value():
    decision = evaluate_idle(
        last_active_ms=NOW - X_MS - 1, tombstone_ms=None, now_ms=NOW, threshold_seconds=X
    )
    assert decision.verdict is IdleVerdict.IDLE
    assert decision.create_tombstone is False


def test_fresh_activity_key_is_not_idle():
    decision = evaluate_idle(
        last_active_ms=NOW - 1000, tombstone_ms=None, now_ms=NOW, threshold_seconds=X
    )
    assert decision.verdict is IdleVerdict.NOT_IDLE


def test_missing_key_is_unknown_and_creates_tombstone():
    decision = evaluate_idle(
        last_active_ms=None, tombstone_ms=None, now_ms=NOW, threshold_seconds=X
    )
    assert decision.verdict is IdleVerdict.UNKNOWN
    assert decision.create_tombstone is True


def test_fresh_tombstone_is_unknown_never_pause():
    decision = evaluate_idle(
        last_active_ms=None, tombstone_ms=NOW - 1000, now_ms=NOW, threshold_seconds=X
    )
    assert decision.verdict is IdleVerdict.UNKNOWN
    assert decision.create_tombstone is False


def test_aged_tombstone_is_idle():
    assert is_idle_by_tombstone(NOW - X_MS, NOW, X) is True
    decision = evaluate_idle(
        last_active_ms=None, tombstone_ms=NOW - X_MS - 1, now_ms=NOW, threshold_seconds=X
    )
    assert decision.verdict is IdleVerdict.IDLE


def test_activity_reappearance_beats_stale_tombstone():
    decision = evaluate_idle(
        last_active_ms=NOW - 1000, tombstone_ms=NOW - X_MS - 1, now_ms=NOW, threshold_seconds=X
    )
    assert decision.verdict is IdleVerdict.NOT_IDLE
    assert decision.create_tombstone is False
