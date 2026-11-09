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

"""Platform identifier grammar tests (OSEP-0025 line 450).

Off-grammar kids are rejected locally with no fetch, so the grammar is
the first line of defense against random-kid spraying into JWKS lookups.
"""

from __future__ import annotations

import unittest

from opensandbox_resolver_auth import is_valid_kid


class KidGrammarTest(unittest.TestCase):
    def test_valid_kids(self) -> None:
        valid = [
            "pool-a:v2",  # fast-sandbox <pool>:<version> (OSEP 363)
            "Pool_1:10",
            "p:v",
            "sbx_01HZYEXAMPLE",  # sidecar sandboxId
            "sbx-7f3a2c",  # OSEP 371 example
            "sbx-7f3a2c.2",  # sandboxId with incarnation suffix
            "a" * 63,  # per-segment cap
            ("a" * 63) + ":" + ("b" * 63),  # two-segment cap
        ]
        for kid in valid:
            self.assertTrue(is_valid_kid(kid), kid)

    def test_rejected_kids(self) -> None:
        invalid = [
            "",
            ":",
            "a:",
            ":v",
            "a::v",  # only one colon allowed
            "a:b:c",
            "a/b",  # path escape
            "../escape",
            "..",
            ".",
            "a b",  # whitespace
            "a?b",
            "a#b",
            "a%2f",
            "a&b",
            "a+b",
            "a=b",
            "-a",  # must start alphanumeric
            ".a",
            "_a",
            "a" * 128,  # over KID_MAX_LENGTH
            ("a" * 64) + ":v",  # segment over 63 chars
            "a\n",
            "a:b\n",
            None,
            123,
            True,
            ["pool-a:v2"],
        ]
        for kid in invalid:
            self.assertFalse(is_valid_kid(kid), repr(kid))


if __name__ == "__main__":
    unittest.main()
