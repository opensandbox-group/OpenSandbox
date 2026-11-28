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
from unittest.mock import Mock, patch

from kubernetes.client.exceptions import ApiException

from app.k8s_resources import (
    _event_matches_sandbox,
    _event_row,
    _event_sort_key,
    _pod_names_for_sandbox,
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
    def test_paused_pod_events_match_the_source_in_the_owned_snapshot(self):
        v1 = Mock()
        v1.list_namespaced_pod.return_value.items = []
        snapshot = {
            "spec": {"sandboxName": "abc"},
            "metadata": {"ownerReferences": [
                {"kind": "BatchSandbox", "controller": True, "name": "abc", "uid": "abc"},
            ]},
            "status": {"sourcePodName": "worker-v2-ba7816bf8f01cfea-0"},
        }
        with patch("app.k8s_resources.client.CustomObjectsApi") as custom_api:
            get = custom_api.return_value.get_namespaced_custom_object
            get.return_value = snapshot
            names = _pod_names_for_sandbox(v1, "ns", "abc")
            get.assert_called_once_with("sandbox.opensandbox.io", "v1alpha1", "ns", "sandboxsnapshots", "abc-pause")
            ev = _event(involved_object=SimpleNamespace(kind="Pod", name=snapshot["status"]["sourcePodName"]))
            self.assertTrue(_event_matches_sandbox(ev, names))
            snapshot["status"]["sourcePodName"] = "pool-warm-12345"
            self.assertNotIn("pool-warm-12345", _pod_names_for_sandbox(v1, "ns", "abc"))

    def test_optional_snapshot_access_preserves_legacy_event_filter(self):
        v1 = Mock()
        v1.list_namespaced_pod.return_value.items = []
        with patch("app.k8s_resources.client.CustomObjectsApi") as custom_api:
            for status in (403, 404):
                with self.subTest(status=status):
                    custom_api.return_value.get_namespaced_custom_object.side_effect = ApiException(status=status)
                    self.assertEqual(_pod_names_for_sandbox(v1, "ns", "abc"), {"abc", "abc-0"})

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
