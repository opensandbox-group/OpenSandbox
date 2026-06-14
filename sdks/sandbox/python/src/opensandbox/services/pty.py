#
# Copyright 2025 Alibaba Group Holding Ltd.
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
#
"""
PTY service interface.

Protocol for sandbox interactive pseudo-terminal (PTY) session lifecycle.
"""

from typing import Protocol

from opensandbox.models.execd import PtySession, PtySessionStatus


class Pty(Protocol):
    """
    Interactive PTY session lifecycle for a sandbox.

    Manages the session lifecycle over execd's REST API (create / status / delete).
    Attaching to the interactive ``/pty/{sessionId}/ws`` WebSocket stream is a separate
    concern and is not part of this service. PTY is only supported on Unix-like platforms.
    """

    async def create_session(
        self,
        cwd: str | None = None,
        command: str | None = None,
    ) -> PtySession:
        """
        Create a new PTY session. The shell does not start until the first WebSocket attaches.

        Args:
            cwd: Optional working directory for the shell
            command: Optional command to run instead of the default login shell

        Returns:
            The created session
        """
        ...

    async def get_session(self, session_id: str) -> PtySessionStatus:
        """
        Retrieve the current status of a PTY session.

        Args:
            session_id: Identifier of the PTY session

        Returns:
            Session status, including the output offset usable for replay
        """
        ...

    async def delete_session(self, session_id: str) -> None:
        """
        Tear down a PTY session on the server side.

        Args:
            session_id: Identifier of the PTY session
        """
        ...
