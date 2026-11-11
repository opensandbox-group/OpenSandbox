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

"""Verify an existing remote volume; retain its marker for remote-side checks."""

import asyncio
import os
import shlex
from datetime import timedelta
from uuid import uuid4

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import PVC, Volume


def verification_script(marker_name: str, content: str, read_only: bool) -> str:
    """Build the Python check executed inside the Linux sandbox."""
    script = (
        "import errno\nfrom pathlib import Path\n"
        f"path = Path({('/mnt/remote/' + marker_name)!r})\n"
        f"expected = {content!r}\n"
    )
    if not read_only:
        script += "with path.open('x', encoding='utf-8') as marker:\n    marker.write(expected)\n"
    script += (
        "if path.read_text(encoding='utf-8') != expected:\n"
        "    raise RuntimeError('Remote marker content does not match')\n"
    )
    if read_only:
        script += (
            "probe = path.with_suffix('.readonly-check')\n"
            "try:\n"
            "    with probe.open('x', encoding='utf-8') as output:\n"
            "        output.write('This write should be rejected')\n"
            "except OSError as exc:\n"
            "    if exc.errno != errno.EROFS:\n"
            "        raise\n"
            "else:\n"
            "    probe.unlink()\n"
            "    raise RuntimeError('Read-only mount unexpectedly allowed a write')\n"
        )
    return script + "print('volume check passed')\n"


async def verify_mount(
    config: ConnectionConfig,
    image: str,
    volume_name: str,
    marker_name: str,
    content: str,
    *,
    read_only: bool,
) -> None:
    sandbox = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=5),
        ready_timeout=timedelta(minutes=3),
        volumes=[
            Volume(
                name="remote-storage",
                pvc=PVC(
                    claimName=volume_name,
                    createIfNotExists=False,
                    deleteOnSandboxTermination=False,
                ),
                mountPath="/mnt/remote",
                readOnly=read_only,
            ),
        ],
    )
    async with sandbox:
        try:
            script = verification_script(marker_name, content, read_only)
            result = await sandbox.commands.run(f"python3 -c {shlex.quote(script)}")
            if result.error or result.exit_code not in (None, 0):
                raise RuntimeError(
                    f"Volume check failed: exit_code={result.exit_code}, "
                    f"error={result.error}, "
                    f"stderr={''.join(message.text for message in result.logs.stderr)}"
                )
            output = "".join(message.text for message in result.logs.stdout).strip()
            if output != "volume check passed":
                raise RuntimeError(f"Volume check did not confirm success: {output!r}")
        finally:
            await sandbox.kill()


async def main() -> None:
    volume_name = os.environ.get("SANDBOX_VOLUME_NAME", "").strip()
    if not volume_name:
        raise ValueError("Set SANDBOX_VOLUME_NAME to a pre-created Docker volume or PVC")
    image = os.environ.get("SANDBOX_IMAGE", "python:3.11")
    config = ConnectionConfig(request_timeout=timedelta(minutes=3))
    marker_name = f"opensandbox-{uuid4().hex}.txt"
    content = f"OpenSandbox remote volume verification: {marker_name}\n"
    print(f"Volume: {volume_name}")
    print(f"Marker filename: {marker_name}")
    print(f"Expected content: {content!r}")

    await verify_mount(config, image, volume_name, marker_name, content, read_only=False)
    print("Read-write check passed; first sandbox removed.")
    await verify_mount(config, image, volume_name, marker_name, content, read_only=True)
    print("Cross-sandbox read and read-only protection passed; second sandbox removed.")
    print("The volume and marker are retained. Verify the marker independently on the remote.")


if __name__ == "__main__":
    asyncio.run(main())
