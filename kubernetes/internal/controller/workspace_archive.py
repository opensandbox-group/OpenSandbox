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


class WorkspaceTar(tarfile.TarFile):
    def gettarinfo(self, name=None, arcname=None, fileobj=None):
        member = super().gettarinfo(name, arcname, fileobj)
        if member is None:
            # tarfile silently omits sockets by default.
            raise ValueError(f"unsupported workspace entry: {arcname}")
        return member


def capture(workspace, archive):
    if workspace.is_symlink() or not workspace.is_dir():
        raise ValueError("workspace must be a directory")

    def check(member):
        # Apply the restore policy before a successful pause can release the Pod.
        if member.issym() and os.path.isabs(member.linkname):
            target = Path(member.linkname)
            if target.is_relative_to(workspace):
                member = member.replace(linkname=os.path.relpath(target.relative_to(workspace), Path(member.name).parent), deep=False)
        checked = workspace_link(member) if member.issym() else workspace_filter(member, workspace)
        # Tar headers require concrete values even for metadata ignored during extraction.
        return checked.replace(uname="", gname="", mode=checked.mode if checked.mode is not None else 0o777, deep=False)

    digest = hashlib.sha256()
    with archive.open("xb") as output:
        class Writer:
            def write(self, data):
                digest.update(data)
                return output.write(data)

        with WorkspaceTar.open(fileobj=Writer(), mode="w|") as bundle:
            bundle.add(workspace, arcname=".", filter=check)
        output.flush()
        os.fsync(output.fileno())
    with archive.open("rb") as stored:
        if hashlib.file_digest(stored, "sha256").digest() != digest.digest():
            raise ValueError("stored workspace digest mismatch")
    return digest.hexdigest()


def validate_owner(member):
    if not (0 <= member.uid < 2**32 - 1 and 0 <= member.gid < 2**32 - 1):
        raise ValueError("invalid workspace owner")


def workspace_link(member):
    validate_owner(member)
    name = Path(member.name)
    if name.is_absolute() or not name.parts or ".." in name.parts:
        raise ValueError(f"invalid workspace symlink name: {member.name!r}")
    return member.replace(mode=0o777, deep=False)


def workspace_filter(member, destination):
    validate_owner(member)
    if member.issym():
        raise ValueError(f"unexpected symbolic link during file extraction: {member.name!r}")
    filtered = tarfile.data_filter(member, destination)
    # Keep path, link and file-type checks. Restore ordinary access bits but
    # never grant setuid/setgid privileges from a checkpoint.
    mode = filtered.mode
    if filtered.isdir() or filtered.isreg() or filtered.islnk():
        mode = member.mode & (0o1777 if filtered.isdir() else 0o777)
    return filtered.replace(mode=mode, uid=member.uid, gid=member.gid, deep=False)


def workspace_members(bundle, stage):
    links = []
    for member in bundle:
        if member.issym():
            links.append(workspace_link(member))
        else:
            yield member

    # File contents have been extracted, but final directory permissions have
    # not yet been applied. Never traverse a link to create another one.
    root = stage.resolve()
    for link in links:
        entry = root / link.name
        if entry.parent.resolve(strict=True) != entry.parent:
            raise ValueError(f"invalid symlink parent for {link.name!r}")
        os.symlink(link.linkname, entry)
        if os.geteuid() == 0:
            os.chown(entry, link.uid, link.gid, follow_symlinks=False)


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
            bundle.extractall(stage, members=workspace_members(bundle, stage), filter=workspace_filter, numeric_owner=True)
        while reader.read(1024 * 1024):
            pass
        if digest.hexdigest() != expected:
            raise ValueError("workspace digest mismatch")
        # Publishing by rename makes the verified directory the completion marker.
        os.rename(stage, target)


if __name__ == "__main__":
    if sys.version_info < (3, 12):
        sys.exit("workspace pause and resume require Python 3.12 or newer")
    operation, root, path, *args = sys.argv[1:]
    if operation == "save":
        archive = Path(path)
        archive.parent.mkdir(exist_ok=True)
        digest = capture(Path(root), archive)
        print(digest, archive.stat().st_size)
    elif operation == "restore":
        restore(Path(root), Path(path), *args)
    else:
        raise ValueError("unknown workspace operation")
