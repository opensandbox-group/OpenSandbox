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

import os
from pathlib import Path
import tempfile
import unittest

from workspace_archive import capture, restore


class WorkspaceArchiveTest(unittest.TestCase):
    def test_two_checkpoints_restore_independently(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            work = root / "workspace"
            work.mkdir()
            store = root / "bucket"
            store.mkdir()
            (work / ".marker").write_bytes(os.urandom(128 * 1024))
            (work / "link").symlink_to(".marker")
            os.link(work / ".marker", work / "hardlink")
            first = store / "first.tar"
            first_digest = capture(work, first)
            volume = root / "volume"
            volume.mkdir()
            restore(volume, first, first_digest)
            resumed = volume / "data"
            self.assertEqual((resumed / "link").read_bytes(), (work / ".marker").read_bytes())
            self.assertTrue(os.path.samefile(resumed / "hardlink", resumed / ".marker"))
            (resumed / "hardlink").unlink()
            (resumed / ".marker").unlink()
            (resumed / "link").unlink()
            (resumed / "new").write_text("second pause")
            second = store / "second.tar"
            digest = capture(resumed, second)
            restore(volume, second, digest)
            restore(volume, second, digest)
            self.assertEqual([p.name for p in resumed.iterdir()], ["new"])
            with self.assertRaisesRegex(ValueError, "digest mismatch"):
                restore(volume, second, first_digest)
            self.assertEqual((resumed / "new").read_text(), "second pause")

    def test_upload_failure_preserves_source(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            work = root / "workspace"
            work.mkdir()
            (work / "marker").write_text("original")
            with self.assertRaises(OSError):
                capture(work, root / "missing-store" / "archive.tar")
            self.assertEqual((work / "marker").read_text(), "original")


if __name__ == "__main__":
    unittest.main()
