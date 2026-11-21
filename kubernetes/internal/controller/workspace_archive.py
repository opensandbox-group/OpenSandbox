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

"""Workspace checkpoint helper executed inside the selected workload image."""

import hashlib
import os
from pathlib import Path
import sys
import tarfile
import tempfile


def capture(workspace, archive):
    if workspace.is_symlink() or not workspace.is_dir():
        raise ValueError("workspace must be a directory")

    def check(member):
        # Apply the restore policy before a successful pause can release the Pod.
        workspace_filter(member, workspace)
        return member

    digest = hashlib.sha256()
    with archive.open("xb") as output:
        class Writer:
            def write(self, data):
                digest.update(data)
                return output.write(data)

        with tarfile.open(fileobj=Writer(), mode="w|") as bundle:
            bundle.add(workspace, arcname=".", filter=check)
        output.flush()
        os.fsync(output.fileno())
    with archive.open("rb") as stored:
        if hashlib.file_digest(stored, "sha256").digest() != digest.digest():
            raise ValueError("stored workspace digest mismatch")
    return digest.hexdigest()


def workspace_filter(member, destination):
    filtered = tarfile.data_filter(member, destination)
    # Keep path, link and file-type checks. Restore ordinary access bits but
    # never grant setuid/setgid privileges from a checkpoint.
    mode = filtered.mode
    if filtered.isdir() or filtered.isreg() or filtered.islnk():
        mode = member.mode & (0o1777 if filtered.isdir() else 0o777)
    if not (0 <= member.uid < 2**32 - 1 and 0 <= member.gid < 2**32 - 1):
        raise ValueError("invalid workspace owner")
    return filtered.replace(mode=mode, uid=member.uid, gid=member.gid, deep=False)


def restore(volume, archive, expected):
    # Only this init container publishes data in the Pod's emptyDir. The app
    # mounts that directory by subPath and can keep editing it after resume.
    target = volume / "data"
    if os.path.lexists(target):
        if target.is_symlink() or not target.is_dir():
            raise ValueError("invalid workspace restore target")
        return
    with tempfile.TemporaryDirectory(dir=volume) as temporary, archive.open("rb") as stored:
        # Keep the parent private even when the workspace itself is shared.
        stage = Path(temporary) / "data"
        stage.mkdir()
        digest = hashlib.sha256()

        class Reader:
            def read(self, size):
                data = stored.read(size)
                digest.update(data)
                return data

        reader = Reader()
        with tarfile.open(fileobj=reader, mode="r|", errorlevel=2) as bundle:
            bundle.extractall(stage, filter=workspace_filter, numeric_owner=True)
        while reader.read(1024 * 1024):
            pass
        if digest.hexdigest() != expected:
            raise ValueError("workspace digest mismatch")
        # Publishing by rename makes the verified directory the completion marker.
        os.rename(stage, target)


if __name__ == "__main__":
    operation, root, path, *args = sys.argv[1:]
    if operation == "save":
        print(capture(Path(root), Path(path)))
    elif operation == "restore":
        restore(Path(root), Path(path), *args)
    else:
        raise ValueError("unknown workspace operation")
