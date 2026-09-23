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

"""Run on Linux: python3 -m unittest discover -s scripts/tests -p 'test_fast_sandbox_state_root_setup.py'.

Root-only cases exercise provisioning with stubbed disk commands; no mounts or
large allocations are made. Run as root to include these cases.
"""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "fast-sandbox-state-root-setup.sh"
STUB = """#!/usr/bin/python3
import json, os, pathlib, shutil, sys
name = pathlib.Path(sys.argv[0]).name
with open(os.environ['CALLS'], 'a') as log:
    log.write(json.dumps([name] + sys.argv[1:]) + '\\n')
if name == os.environ.get('FAIL_COMMAND'):
    sys.exit(1)
if name == 'df':
    print('Avail')
    print(os.environ.get('FREE_BYTES', str(200 * 1024**3)))
elif name == 'mountpoint':
    sys.exit(0 if os.environ.get('MOUNTED') == '1' else 1)
elif name == 'cp':
    shutil.copyfile(sys.argv[-2], sys.argv[-1])
"""


@unittest.skipUnless(os.name == "posix", "Linux shell tool")
class StateRootSetupTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="state-root-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.target = self.root / "state"
        self.backing = self.root / "disk.xfs"
        self.calls = self.root / "calls.jsonl"
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        for name in ("df", "mountpoint", "fallocate", "mkfs.xfs", "mount", "cp"):
            stub = bin_dir / name
            stub.write_text(STUB)
            stub.chmod(0o755)
        self.env = dict(os.environ, PATH=f"{bin_dir}:/usr/sbin:/usr/bin:/sbin:/bin", CALLS=str(self.calls))

    def run_setup(self, size="50G", dry_run=True):
        return subprocess.run(
            ["bash", str(SCRIPT)] + (["--dry-run"] if dry_run else [])
            + [size, str(self.target), str(self.backing)],
            env=self.env, text=True, capture_output=True, timeout=10,
        )

    def operations(self):
        if not self.calls.exists():
            return []
        return [json.loads(line) for line in self.calls.read_text().splitlines()]

    def assert_rejected(self, message, **kwargs):
        result = self.run_setup(**kwargs)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn(message, result.stderr)
        self.assertFalse(any(op[0] in ("fallocate", "mkfs.xfs", "mount") for op in self.operations()))

    def test_dry_run_is_read_only(self):
        result = self.run_setup()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Plan:", result.stdout)
        self.assertFalse(self.target.exists())
        self.assertFalse(self.backing.exists())

    def test_invalid_sizes(self):
        for size in ("0G", "10G", "-1G", "1T", "050G", "999999999999999G"):
            with self.subTest(size=size):
                self.assertNotEqual(self.run_setup(size=size).returncode, 0)

    def test_exact_space_boundary(self):
        self.env["FREE_BYTES"] = str(60 * 1024**3 - 1)
        self.assert_rejected("insufficient space")
        self.env["FREE_BYTES"] = str(60 * 1024**3)
        self.assertEqual(self.run_setup().returncode, 0)

    def test_existing_backing_is_preserved(self):
        self.backing.write_text("important")
        self.assert_rejected("already exists")
        self.assertEqual(self.backing.read_text(), "important")

    def test_nonempty_target_is_preserved(self):
        self.target.mkdir()
        marker = self.target / "important"
        marker.write_text("keep")
        self.assert_rejected("not empty")
        self.assertEqual(marker.read_text(), "keep")

    def test_mounted_target(self):
        self.env["MOUNTED"] = "1"
        self.assert_rejected("already mounted")

    def test_backing_inside_target(self):
        self.backing = self.target / "disk.xfs"
        self.assert_rejected("outside the mountpoint")

    def test_symlinked_parent(self):
        link = self.root / "link"
        link.symlink_to(self.root, target_is_directory=True)
        self.backing = link / "disk.xfs"
        self.assert_rejected("symlinked paths")

    def test_dangling_symlink(self):
        self.backing.symlink_to(self.root / "missing")
        self.assert_rejected("symlinked paths")

    def test_unsafe_fstab_path(self):
        self.target = self.root / "with space"
        self.assert_rejected("paths must be absolute")

    @unittest.skipUnless(hasattr(os, "geteuid") and os.geteuid() == 0, "requires root; disk operations are stubbed")
    def test_provision_and_refuse_second_run(self):
        result = self.run_setup(dry_run=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"{self.backing} {self.target} xfs loop,noatime 0 0", result.stdout)
        names = [op[0] for op in self.operations()]
        self.assertLess(names.index("fallocate"), names.index("mkfs.xfs"))
        self.assertLess(names.index("mkfs.xfs"), names.index("mount"))
        self.assertLess(names.index("mount"), names.index("cp"))
        self.assertIn(["mkfs.xfs", "-m", "reflink=1", str(self.backing)], self.operations())
        self.assertEqual(list(self.target.iterdir()), [])
        result = self.run_setup(dry_run=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(sum(op[0] == "mkfs.xfs" for op in self.operations()), 1)

    @unittest.skipUnless(hasattr(os, "geteuid") and os.geteuid() == 0, "requires root; disk operations are stubbed")
    def test_allocation_failure_never_formats(self):
        self.env["FAIL_COMMAND"] = "fallocate"
        result = self.run_setup(dry_run=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.backing.exists())
        self.assertNotIn("mkfs.xfs", [op[0] for op in self.operations()])

    @unittest.skipUnless(hasattr(os, "geteuid") and os.geteuid() == 0, "requires root; disk operations are stubbed")
    def test_reflink_failure_is_not_success(self):
        self.env["FAIL_COMMAND"] = "cp"
        result = self.run_setup(dry_run=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("OK:", result.stdout)
        self.assertIn("retained", result.stderr)
        self.assertTrue(self.backing.exists())
        self.assertEqual(list(self.target.iterdir()), [])

    @unittest.skipUnless(hasattr(os, "geteuid") and os.geteuid() == 0, "requires root; disk operations are stubbed")
    def test_mount_failure_retains_image(self):
        self.env["FAIL_COMMAND"] = "mount"
        result = self.run_setup(dry_run=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.backing.exists())
        self.assertNotIn("cp", [op[0] for op in self.operations()])

    @unittest.skipUnless(hasattr(os, "geteuid") and os.geteuid() == 0, "requires root; disk operations are stubbed")
    def test_locked_target_is_not_formatted(self):
        import fcntl

        self.target.mkdir()
        fd = os.open(self.target, os.O_RDONLY)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.assert_rejected("another setup is using", dry_run=False)
            self.assertFalse(self.backing.exists())
        finally:
            os.close(fd)


if __name__ == "__main__":
    unittest.main()
