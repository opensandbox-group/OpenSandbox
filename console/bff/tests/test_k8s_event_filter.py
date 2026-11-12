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

import unittest
from datetime import datetime, timezone
from types import SimpleNamespace

from app.k8s_resources import (
    _event_matches_sandbox,
    _event_row,
    _event_sort_key,
    pod_name_candidates_for_sandbox,
)


def _event(**overrides):
    fields = {
        "type": "Warning",
        "reason": "FailedScheduling",
        "message": "0/3 nodes are available",
        "count": None,
        "first_timestamp": None,
        "last_timestamp": None,
        "event_time": None,
        "series": None,
        "involved_object": SimpleNamespace(kind="Pod", name="abc-0"),
        "metadata": SimpleNamespace(creation_timestamp=datetime(2026, 1, 1, tzinfo=timezone.utc)),
    }
    fields.update(overrides)
    return SimpleNamespace(**fields)


class K8sEventFilterTests(unittest.TestCase):
    def test_pod_name_candidates_include_batch_suffix(self):
        self.assertEqual(pod_name_candidates_for_sandbox("abc"), ["abc-0", "abc"])

    def test_event_matches_pooled_pod_name(self):
        ev = SimpleNamespace(
            involved_object=SimpleNamespace(kind="Pod", name="abc-0"),
        )
        names = set(pod_name_candidates_for_sandbox("abc"))
        self.assertTrue(_event_matches_sandbox(ev, names))
        self.assertFalse(
            _event_matches_sandbox(
                SimpleNamespace(involved_object=SimpleNamespace(kind="Pod", name="other-0")),
                names,
            )
        )


class K8sEventTimeTests(unittest.TestCase):
    def test_legacy_event_keeps_its_timestamps(self):
        first = datetime(2026, 1, 2, 3, 4, 5, tzinfo=timezone.utc)
        last = datetime(2026, 1, 2, 3, 9, 5, tzinfo=timezone.utc)
        row = _event_row(_event(first_timestamp=first, last_timestamp=last, count=4), "acme", "ns")
        self.assertEqual(row["firstTimestamp"], first.isoformat())
        self.assertEqual(row["lastTimestamp"], last.isoformat())

    def test_events_v1_event_uses_event_time(self):
        occurred = datetime(2026, 1, 2, 3, 4, 5, 123456, tzinfo=timezone.utc)
        row = _event_row(_event(event_time=occurred), "acme", "ns")
        self.assertEqual(row["firstTimestamp"], occurred.isoformat())
        self.assertEqual(row["lastTimestamp"], occurred.isoformat())

    def test_events_v1_series_uses_last_observed_time(self):
        occurred = datetime(2026, 1, 2, 3, 4, 5, tzinfo=timezone.utc)
        observed = datetime(2026, 1, 2, 3, 20, 0, tzinfo=timezone.utc)
        ev = _event(
            event_time=occurred,
            series=SimpleNamespace(count=7, last_observed_time=observed),
        )
        row = _event_row(ev, "acme", "ns")
        self.assertEqual(row["firstTimestamp"], occurred.isoformat())
        self.assertEqual(row["lastTimestamp"], observed.isoformat())
        self.assertEqual(_event_sort_key(ev), observed)


if __name__ == "__main__":
    unittest.main()
