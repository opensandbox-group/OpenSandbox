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

"""Shared canonical-selector vectors; Unicode normalization belongs to Go."""

import importlib.util
import json
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location(
    "host_selectors", ROOT / "mitmscripts" / "host_selectors.py"
)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class HostSelectorTest(unittest.TestCase):
    def test_conformance(self):
        cases = json.loads((ROOT / "testdata" / "host_selectors.json").read_text())
        for case in cases["normalize"]:
            if not case.get("error"):
                with self.subTest(normalized=case):
                    selector = module.parse_canonical(case["want"])
                    self.assertEqual(selector.text, case["want"])
        for case in cases["canonical"]:
            with self.subTest(case=case):
                if case["valid"]:
                    self.assertEqual(
                        module.parse_canonical(case["input"]).text, case["input"]
                    )
                else:
                    with self.assertRaises(ValueError):
                        module.parse_canonical(case["input"])
        for case in cases["matches"]:
            with self.subTest(case=case):
                selector = module.parse_canonical(case["selector"])
                self.assertEqual(selector.matches(case["host"]), case["want"])
        for case in cases["overlaps"]:
            with self.subTest(case=case):
                left = module.parse_canonical(case["left"])
                right = module.parse_canonical(case["right"])
                self.assertEqual(left.overlaps(right), case["want"])
                self.assertEqual(right.overlaps(left), case["want"])

    def test_length(self):
        host = ".".join(["a" * 63, "b" * 63, "c" * 63, "d" * 61])
        module.parse_canonical(host)
        for value in (host + "x", "*." + host):
            with self.assertRaises(ValueError):
                module.parse_canonical(value)


if __name__ == "__main__":
    unittest.main()
