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
import shutil
import sys
import tarfile
import tempfile


def capture(workspace, archive):
    if workspace.is_symlink() or not workspace.is_dir():
        raise ValueError("workspace must be a directory")

    def check(member):
        # Apply the restore policy before a successful pause can release the Pod.
        tarfile.data_filter(member, workspace)
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


def restore(volume, archive, expected):
    # The init container mounts only a fresh emptyDir and the read-only archive PVC.
    with tempfile.TemporaryDirectory(dir=volume) as stage, archive.open("rb") as stored:
        digest = hashlib.sha256()

        class Reader:
            def read(self, size):
                data = stored.read(size)
                digest.update(data)
                return data

        reader = Reader()
        with tarfile.open(fileobj=reader, mode="r|") as bundle:
            bundle.extractall(stage, filter="data")
        while reader.read(1024 * 1024):
            pass
        if digest.hexdigest() != expected:
            raise ValueError("workspace digest mismatch")
        target = volume / "data"
        if target.exists():
            shutil.rmtree(target)
        os.rename(stage, target)


if __name__ == "__main__":
    operation, root, path, *args = sys.argv[1:]
    if operation == "save":
        print(capture(Path(root), Path(path)))
    elif operation == "restore":
        restore(Path(root), Path(path), *args)
    else:
        raise ValueError("unknown workspace operation")
