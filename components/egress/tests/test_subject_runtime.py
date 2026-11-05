# Copyright 2026 The OpenSandbox Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Pure handoff validation and production addon lifecycle, without mitmdump."""

import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "mitmscripts"))
import subject_runtime as addon


class SubjectRuntimeTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="fsb-")
        self.addCleanup(self.directory.cleanup)
        self.bundle = {
            "MODE": "subject-v2", "SOCKET": self.directory.name + "/receiver.sock",
            "TOKEN": "a" * 32, "CONTROL_GENERATION": "control",
            "MAX_SUBJECTS": "4096", "MAX_SNAPSHOT_BYTES": "8388608",
            "RETAINED_PAYLOAD_BUDGET": "68719476736",
        }
        self.addCleanup(addon.done)

    def environment(self, bundle=None):
        return {addon._PREFIX + name: value for name, value in
                (self.bundle if bundle is None else bundle).items()}

    def test_complete_bundle_constructs_owner_and_stops_socket(self):
        with patch.dict(os.environ, self.environment(), clear=True):
            addon.load(None)
        self.assertEqual(addon._owner._max_subjects, 4096)
        self.assertEqual(addon._owner.max_snapshot_bytes, 8388608)
        self.assertEqual(addon._owner.lifecycle_status("control")["lastSequence"], 0)
        self.assertEqual(os.stat(self.bundle["SOCKET"]).st_mode & 0o777, 0o600)
        addon.done()
        self.assertFalse(os.path.lexists(self.bundle["SOCKET"]))
        addon.done()

    def test_every_field_is_required(self):
        for field in self.bundle:
            with self.subTest(field=field):
                bundle = self.bundle.copy()
                del bundle[field]
                with patch.dict(os.environ, self.environment(bundle), clear=True):
                    with self.assertRaisesRegex(SystemExit, "invalid revision runtime configuration"):
                        addon.load(None)
                self.assertIsNone(addon._owner)
                self.assertFalse(os.path.lexists(self.bundle["SOCKET"]))
        with patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(SystemExit):
                addon.load(None)

    def test_invalid_bundle_fails_closed_and_redacts_values(self):
        invalid = [
            ("MODE", "sidecar"), ("SOCKET", "relative"), ("SOCKET", "/" + "x" * 103),
            ("TOKEN", "secret-short"), ("CONTROL_GENERATION", ""),
            ("CONTROL_GENERATION", "g" * 129), ("MAX_SUBJECTS", "0"),
            ("MAX_SUBJECTS", "-1"), ("MAX_SUBJECTS", "04096"),
            ("MAX_SNAPSHOT_BYTES", "8.0"), ("MAX_SNAPSHOT_BYTES", "９"),
            ("MAX_SNAPSHOT_BYTES", str(sys.maxsize)),
            ("RETAINED_PAYLOAD_BUDGET", "68719476735"),
            ("RETAINED_PAYLOAD_BUDGET", str(sys.maxsize + 1)),
            ("RETAINED_PAYLOAD_BUDGET", "1" * 100), ("UNKNOWN", "value"),
        ]
        for field, value in invalid:
            with self.subTest(field=field, value=value):
                bundle = dict(self.bundle, **{field: value})
                with patch.dict(os.environ, self.environment(bundle), clear=True):
                    with self.assertRaises(SystemExit) as failure:
                        addon.load(None)
                self.assertEqual(str(failure.exception), addon._ERROR)
                self.assertIsNone(addon._owner)
                self.assertFalse(os.path.lexists(self.bundle["SOCKET"]))

    def test_rejects_mixed_sidecar_bundle_and_duplicate_load(self):
        mixed = self.environment()
        mixed["OPENSANDBOX_EGRESS_REVISION_IPC_SOCKET"] = "/stale"
        with patch.dict(os.environ, mixed, clear=True):
            with self.assertRaises(SystemExit):
                addon.load(None)
        with patch.dict(os.environ, self.environment(), clear=True):
            addon.load(None)
            with self.assertRaises(SystemExit):
                addon.load(None)

    def test_socket_setup_failure_does_not_publish_owner(self):
        Path(self.bundle["SOCKET"]).write_text("keep")
        with patch.dict(os.environ, self.environment(), clear=True):
            with self.assertRaises(SystemExit):
                addon.load(None)
        self.assertEqual(Path(self.bundle["SOCKET"]).read_text(), "keep")
        self.assertIsNone(addon._server)
        self.assertIsNone(addon._owner)
