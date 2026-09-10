# /// script
# requires-python = ">=3.10"
# dependencies = ["jsonschema>=4.18,<5", "PyYAML>=6,<7"]
# ///
#
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
#
"""Run contract regression tests: uv run specs/tests/test_execd_schema.py."""

import unittest
from pathlib import Path

import yaml
from jsonschema import Draft202012Validator


class RunCommandSchemaTest(unittest.TestCase):
    def test_exclusive_inputs(self):
        spec = yaml.safe_load((Path(__file__).parents[1] / "execd-api.yaml").read_text())
        schema = spec["components"]["schemas"]["RunCommandRequest"]
        Draft202012Validator.check_schema(schema)
        validator = Draft202012Validator(schema)
        cases = [
            ({"command": "echo hello"}, True),
            ({"argv": ["printf", "%s", ""]}, True),
            ({"argv": ["printf", "%s", "$HOME", "hello world"], "cwd": "/tmp"}, True),
            ({}, False),
            ({"command": "echo hello", "argv": ["printf"]}, False),
            ({"command": "", "argv": ["printf"]}, False),
            ({"command": "echo hello", "argv": []}, False),
            ({"command": None, "argv": ["printf"]}, False),
            ({"command": "echo hello", "argv": None}, False),
            ({"command": ""}, False),
            ({"command": None}, False),
            ({"argv": []}, False),
            ({"argv": None}, False),
            ({"argv": [None]}, False),
        ]
        for payload, valid in cases:
            with self.subTest(payload=payload):
                self.assertEqual(validator.is_valid(payload), valid)


if __name__ == "__main__":
    unittest.main()
