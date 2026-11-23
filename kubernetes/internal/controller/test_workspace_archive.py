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
import stat
import socket
import subprocess
import sys
import tarfile
import tempfile
import unittest

from workspace_archive import capture, restore


class WorkspaceArchiveTest(unittest.TestCase):
    def test_restore_access_permissions(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            work = root / "workspace"
            private = work / "private"
            private.mkdir(parents=True)
            marker = private / "marker"
            marker.write_text("original")
            work.chmod(0o755)
            private.chmod(0o750)
            marker.chmod(0o640)
            volume = root / "volume"
            volume.mkdir()
            archive = root / "checkpoint.tar"
            restore(volume, archive, capture(work, archive))
            for source in (work, private, marker):
                restored = (volume / "data" / source.relative_to(work)).stat()
                self.assertEqual(stat.S_IMODE(restored.st_mode), stat.S_IMODE(source.stat().st_mode))
                self.assertEqual((restored.st_uid, restored.st_gid), (source.stat().st_uid, source.stat().st_gid))

    def test_restore_strips_setid_bits(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            work = root / "workspace"
            work.mkdir()
            script = work / "command"
            script.touch()
            script.chmod(0o6755)
            archive = root / "checkpoint.tar"
            volume = root / "volume"
            volume.mkdir()
            restore(volume, archive, capture(work, archive))
            self.assertEqual(stat.S_IMODE((volume / "data" / "command").stat().st_mode), 0o755)

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
            next_volume = root / "next-pod"
            next_volume.mkdir()
            restore(next_volume, second, digest)
            restored = next_volume / "data"
            self.assertEqual([p.name for p in restored.iterdir()], ["new"])
            self.assertEqual((restored / "new").read_text(), "second pause")

    def test_repeated_init_preserves_live_workspace(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            work = root / "workspace"
            work.mkdir()
            (work / "notes").write_text("at checkpoint")
            (work / "obsolete").touch()
            archive = root / "checkpoint.tar"
            digest = capture(work, archive)
            volume = root / "pod-volume"
            volume.mkdir()
            restore(volume, archive, digest)
            live = volume / "data"
            (live / "notes").write_text("edited after resume")
            (live / "result.csv").write_text("new result")
            (live / "obsolete").unlink()
            restore(volume, archive, digest)
            self.assertEqual((live / "notes").read_text(), "edited after resume")
            self.assertEqual((live / "result.csv").read_text(), "new result")
            self.assertFalse((live / "obsolete").exists())
            archive.unlink()
            restore(volume, archive, digest)

    def test_failed_restore_can_retry_on_same_volume(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            work = root / "workspace"
            work.mkdir()
            (work / "notes").write_text("checkpoint")
            archive = root / "checkpoint.tar"
            digest = capture(work, archive)
            volume = root / "pod-volume"
            volume.mkdir()
            with self.assertRaisesRegex(ValueError, "digest mismatch"):
                restore(volume, archive, "0" * 64)
            self.assertFalse((volume / "data").exists())
            restore(volume, archive, digest)
            self.assertEqual((volume / "data" / "notes").read_text(), "checkpoint")

    def test_workspace_absolute_link_is_relocated(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            work = root / "workspace"
            (work / "bin").mkdir(parents=True)
            (work / "version").write_text("v1")
            (work / "bin" / "current").symlink_to(work / "version")
            archive = root / "checkpoint.tar"
            digest = capture(work, archive)
            (work / "version").unlink()
            volume = root / "volume"
            volume.mkdir()
            restore(volume, archive, digest)
            link = volume / "data" / "bin" / "current"
            self.assertEqual(link.readlink(), Path("../version"))
            self.assertEqual(link.read_text(), "v1")

    def test_venv_interpreter_link_survives_restore(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            work = root / "workspace"
            env = work / ".venv"
            subprocess.run([sys.executable, "-m", "venv", "--symlinks", "--without-pip", env], check=True)
            interpreter = env / "bin" / "python"
            self.assertTrue(interpreter.is_symlink())
            archive = root / "venv.tar"
            volume = root / "volume"
            volume.mkdir()
            restore(volume, archive, capture(work, archive))
            restored = volume / "data" / ".venv"
            self.assertEqual((restored / "bin" / "python").readlink(), interpreter.readlink())
            output = subprocess.check_output([restored / "bin" / "python", "-c", "import sys; print(sys.prefix)"], text=True)
            self.assertEqual(Path(output.strip()).resolve(), restored.resolve())

    def test_unsupported_entries_report_their_names(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            work = root / "workspace"
            work.mkdir()
            os.mkfifo(work / "events-pipe")
            with self.assertRaisesRegex(tarfile.SpecialFileError, "events-pipe"):
                capture(work, root / "fifo.tar")
            (work / "events-pipe").unlink()
            with socket.socket(socket.AF_UNIX) as sock:
                sock.bind(str(work / "local.sock"))
                with self.assertRaisesRegex(ValueError, "local.sock"):
                    capture(work, root / "socket.tar")

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
