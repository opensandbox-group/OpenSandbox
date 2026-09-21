#
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
#

"""
Converters for execd command-related models.
"""

from typing import TypeVar, cast

from opensandbox.api.execd.models import CommandStatusResponse
from opensandbox.api.execd.types import Unset
from opensandbox.models.execd import CommandStatus

T = TypeVar("T")


def _unwrap_optional(value: Unset | T) -> T | None:
    if isinstance(value, Unset):
        return None
    return cast(T, value)


def to_command_status(raw: CommandStatusResponse) -> CommandStatus:
    """
    Convert OpenAPI CommandStatusResponse to SDK CommandStatus.
    """

    return CommandStatus(
        id=_unwrap_optional(raw.id),
        content=_unwrap_optional(raw.content),
        running=_unwrap_optional(raw.running),
        exit_code=_unwrap_optional(raw.exit_code),
        error=_unwrap_optional(raw.error),
        started_at=_unwrap_optional(raw.started_at),
        finished_at=_unwrap_optional(raw.finished_at),
    )
