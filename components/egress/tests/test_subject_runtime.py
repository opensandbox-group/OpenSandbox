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
import shutil
import subprocess
import textwrap
import threading
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

    def isolated_runtime(self, body, *, ship_admission=True):
        scripts = Path(self.directory.name) / "scripts"
        scripts.mkdir()
        for source in (Path(__file__).resolve().parents[1] / "mitmscripts").glob("*.py"):
            if ship_admission or source.name != "subject_admission.py":
                shutil.copyfile(source, scripts / source.name)
        result = subprocess.run(
            [sys.executable, "-c", textwrap.dedent(body)], cwd=scripts,
            env={**os.environ, **self.environment()}, capture_output=True, text=True, timeout=10,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_revision_runtime_loads_without_admission_module(self):
        self.isolated_runtime("""
            import sys
            import subject_runtime as addon
            assert "subject_admission" not in sys.modules
            addon.load(None)
            assert addon._owner.lifecycle_status("control")["lastSequence"] == 0
            assert "subject_admission" not in sys.modules
            addon.done()
            assert "subject_admission" not in sys.modules
        """, ship_admission=False)

    def test_disabled_runtime_never_imports_shipped_admission_module(self):
        self.isolated_runtime("""
            import sys
            import subject_runtime as addon
            assert "subject_admission" not in sys.modules
            addon.load(None)
            assert "subject_admission" not in sys.modules
            addon.done()
            assert "subject_admission" not in sys.modules
        """)

    def test_disabled_done_does_not_import_missing_admission_module(self):
        self.isolated_runtime("""
            import sys
            from pathlib import Path
            import subject_runtime as addon
            addon.load(None)
            Path("subject_admission.py").unlink()
            sys.modules.pop("subject_admission", None)
            addon.done()
            addon.done()
            assert addon._owner is None and addon._server is None
            assert "subject_admission" not in sys.modules
        """)

    def test_admission_bundle_is_optional_but_atomic_and_bounded(self):
        fields = dict(TLS_ADMISSION="true", TLS_CAPACITY="32", PROCESS_TLS_CAPACITY="131072",
                      DRAIN_TIMEOUT_SECONDS="30")
        bundle = dict(self.bundle, **fields)
        values = addon._configuration(self.environment(bundle))
        self.assertEqual(values["TLS_CAPACITY"], 32)
        self.assertEqual(values["PROCESS_TLS_CAPACITY"], 131072)
        self.assertEqual(values["DRAIN_TIMEOUT_SECONDS"], 30)
        for text in ("1", "300"):
            values = addon._configuration(self.environment(dict(bundle, DRAIN_TIMEOUT_SECONDS=text)))
            self.assertEqual(values["DRAIN_TIMEOUT_SECONDS"], int(text))
        for name in fields:
            missing = bundle.copy()
            del missing[name]
            with self.assertRaises(SystemExit):
                addon._configuration(self.environment(missing))
        for name, value in [("TLS_ADMISSION", "false"), ("TLS_CAPACITY", "0"),
                            ("TLS_CAPACITY", "01"), ("PROCESS_TLS_CAPACITY", "1048577"),
                            ("DRAIN_TIMEOUT_SECONDS", "0"), ("DRAIN_TIMEOUT_SECONDS", "301"),
                            ("DRAIN_TIMEOUT_SECONDS", "030"), ("DRAIN_TIMEOUT_SECONDS", ""),
                            ("DRAIN_TIMEOUT_SECONDS", "-1"), ("DRAIN_TIMEOUT_SECONDS", "1.0"),
                            ("DRAIN_TIMEOUT_SECONDS", "３"), ("DRAIN_TIMEOUT_SECONDS", " 30")]:
            with self.subTest(name=name, value=value), self.assertRaises(SystemExit):
                addon._configuration(self.environment(dict(bundle, **{name: value})))
        # The installation-only bundle never accepts an admission-only field.
        with self.assertRaises(SystemExit):
            addon._configuration(self.environment(dict(self.bundle, DRAIN_TIMEOUT_SECONDS="30")))

    @staticmethod
    def drain_threads():
        return [thread for thread in threading.enumerate()
                if thread.name == "subject-admission-drain" and thread.is_alive()]

    def admission_environment(self, drain="7"):
        environment = self.environment(dict(
            self.bundle, TLS_ADMISSION="true", TLS_CAPACITY="2", PROCESS_TLS_CAPACITY="4",
            DRAIN_TIMEOUT_SECONDS=drain))
        environment["OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS"] = "[]"
        return environment

    def test_admission_loop_starts_once_reaches_registry_and_stops(self):
        import subject_admission
        self.assertEqual(self.drain_threads(), [])
        with patch.dict(os.environ, self.admission_environment(), clear=True), \
                patch("credential_bound.validate_live_options"):
            addon.load(None)
        runtime = subject_admission.runtime()
        self.assertIsNotNone(runtime)
        self.assertIs(addon._owner._teardown, runtime)  # wired before the socket served
        self.assertEqual(len(self.drain_threads()), 1)
        with self.assertRaises(RuntimeError):
            runtime.start()
        addon._owner.lifecycle("register", json.dumps({
            "controlGeneration": "control", "lifecycleSequence": 1,
            "target": {"subjectKey": "k", "subjectGeneration": "g"}}).encode())
        self.assertEqual(addon._owner._entries["k"].registry._drain_timeout, 7)
        addon.done()
        self.assertIsNone(subject_admission.runtime())
        self.assertEqual(self.drain_threads(), [])
        addon.done()

    def test_installation_mode_starts_no_drain_loop(self):
        with patch.dict(os.environ, self.environment(), clear=True):
            addon.load(None)
        self.assertEqual(self.drain_threads(), [])
        self.assertFalse(addon._admission_enabled)
        self.assertIsNone(addon._owner._teardown)
        addon.done()

    def test_admission_startup_failures_leave_no_loop_or_runtime(self):
        import subject_admission
        real_start = subject_admission.SubjectAdmissionRuntime.start

        def start_then_fail(runtime):
            real_start(runtime)
            raise RuntimeError("late startup failure")

        failures = {
            "socket": lambda: Path(self.bundle["SOCKET"]).write_text("keep"),
            "loop": lambda: None,
        }
        for name, prepare in failures.items():
            with self.subTest(failure=name):
                prepare()
                with patch.dict(os.environ, self.admission_environment(), clear=True), \
                        patch("credential_bound.validate_live_options"), \
                        patch.object(subject_admission.SubjectAdmissionRuntime, "start",
                                     autospec=True, side_effect=start_then_fail
                                     if name == "loop" else real_start):
                    with self.assertRaisesRegex(SystemExit, "invalid revision runtime configuration"):
                        addon.load(None)
                self.assertIsNone(addon._owner)
                self.assertIsNone(addon._server)
                self.assertIsNone(subject_admission.runtime())
                self.assertEqual(self.drain_threads(), [])
                if name == "socket":
                    self.assertEqual(Path(self.bundle["SOCKET"]).read_text(), "keep")
                    Path(self.bundle["SOCKET"]).unlink()
                else:
                    self.assertFalse(os.path.lexists(self.bundle["SOCKET"]))

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
