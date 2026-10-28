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

"""Type-check the public download cleanup examples without a live sandbox."""

from contextlib import aclosing

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.adapters.isolated_filesystem_adapter import IsolatedFilesystemAdapter
from opensandbox.services.filesystem import Filesystem


async def close_download(
    files: Filesystem | FilesystemAdapter | IsolatedFilesystemAdapter,
) -> None:
    chunks = await files.read_bytes_stream("/tmp/large.bin")
    await chunks.aclose()

    async with aclosing(await files.read_bytes_stream("/tmp/large.bin")) as chunks:
        async for _chunk in chunks:
            break
