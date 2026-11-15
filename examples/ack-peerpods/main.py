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

"""Exercise the standard SDK against an ACK PeerPods sandbox."""

import argparse
import asyncio
import os
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models import WriteEntry


async def main(keep: bool) -> None:
    api_key = os.environ.get("SANDBOX_API_KEY")
    if not api_key:
        raise RuntimeError("Set SANDBOX_API_KEY to the installed server's API key")

    print(
        "Creating an ACK PeerPods sandbox (cloud startup can take several minutes)...",
        flush=True,
    )
    sandbox = await Sandbox.create(
        os.environ.get("SANDBOX_IMAGE", "ubuntu:22.04"),
        connection_config=ConnectionConfig(
            domain=os.environ.get("SANDBOX_DOMAIN", "127.0.0.1:18080"),
            api_key=api_key,
            use_server_proxy=True,
            request_timeout=timedelta(minutes=10),
        ),
        ready_timeout=timedelta(minutes=10),
        timeout=timedelta(minutes=30),
        metadata={"example": "ack-peerpods"},
    )
    print(f"Sandbox ID: {sandbox.id}", flush=True)
    succeeded = False
    try:
        execution = await sandbox.commands.run(
            "uname -sr && printf 'hello from PeerPods\\n'"
        )
        for message in execution.logs.stdout:
            print(message.text, end="" if message.text.endswith("\n") else "\n")
        if execution.error:
            raise RuntimeError(f"Command failed: {execution.error}")

        path = "/tmp/opensandbox-peerpods.txt"
        content = "OpenSandbox file round-trip on ACK PeerPods\n"
        await sandbox.files.write_files([WriteEntry(path=path, data=content, mode=644)])
        if await sandbox.files.read_file(path) != content:
            raise RuntimeError("File round-trip returned unexpected content")
        print("File round-trip: OK", flush=True)
        succeeded = True
    finally:
        if keep and succeeded:
            await sandbox.close()
            print("Sandbox retained for inspection; expires after 30 minutes.")
        else:
            await sandbox.destroy()
            print("Sandbox deletion requested; check PodVM cleanup in the cluster.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--keep", action="store_true", help="Keep a successful sandbox for inspection"
    )
    asyncio.run(main(parser.parse_args().keep))
