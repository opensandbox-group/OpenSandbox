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

"""Real HTTP/1 parser coverage for the live H1 authority gate.

The unit suite feeds fake request objects to ``system.requestheaders``;
this file proves the gate against requests built by the real
``mitmproxy.net.http.http1.read.read_request_head`` parser with
transparent-mode ``data.host``/``data.port``/``data.scheme`` emulated on
the data object directly (no host setters).

The checks run in an isolated subprocess (``fixtures/real_wire_worker.py
h1-parse``) because unittest discovery loads every ``test_*`` module into
one process where the fake ``mitmproxy`` package installed by the unit
harness would poison real library imports.
"""

from __future__ import annotations

import subprocess
import sys
import unittest
from pathlib import Path

WORKER = Path(__file__).resolve().parent / "fixtures" / "real_wire_worker.py"


class H1WireParseTest(unittest.TestCase):
    """Real-parser request heads through the live authority gate."""

    def test_h1_wire_parse_modes(self) -> None:
        result = subprocess.run(
            [sys.executable, str(WORKER), "h1-parse"],
            capture_output=True, text=True, timeout=120,
        )
        self.assertEqual(
            result.returncode, 0,
            f"worker failed:\n{result.stdout}\n{result.stderr}",
        )
        for marker in (
            "h1-allow-origin-form",
            "h1-allow-absolute-form-explicit-443",
            "h1-allow-uppercase-root-dot",
            "h1-allow-punycode",
            "h1-deny-host-wrong-host",
            "h1-deny-host-duplicate",
            "h1-deny-host-huge-port",
            "h1-deny-cross-authority-out-of-scope-path",
            "h1-deny-version-http19",
        ):
            self.assertIn(marker, result.stdout)


if __name__ == "__main__":
    unittest.main()
