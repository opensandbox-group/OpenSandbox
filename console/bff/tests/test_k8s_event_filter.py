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
from types import SimpleNamespace

from app.k8s_resources import _event_matches_sandbox, pod_name_candidates_for_sandbox


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


if __name__ == "__main__":
    unittest.main()
