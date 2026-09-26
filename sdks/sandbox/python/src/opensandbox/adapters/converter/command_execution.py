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

"""Helpers shared by the async and sync command adapters."""

import json
import logging
from datetime import timedelta

from opensandbox.adapters.converter.event_node import EventNode
from opensandbox.exceptions import InvalidArgumentException
from opensandbox.models.execd import Execution, RunCommandOpts

logger = logging.getLogger(__name__)


def resolve_run_in_session_timeout(timeout: timedelta | None) -> int | None:
    if timeout is None:
        return None
    if isinstance(timeout, timedelta):
        # The execd API validates this timeout with `gte=0`.
        if timeout < timedelta(0):
            raise InvalidArgumentException("timeout must not be negative")
        return int(timeout.total_seconds() * 1000)
    raise InvalidArgumentException("timeout must be a datetime.timedelta or None")


def infer_foreground_exit_code(execution: Execution) -> int | None:
    if execution.error is not None:
        try:
            return int(execution.error.value)
        except (TypeError, ValueError):
            return None
    if execution.complete is not None:
        return 0
    return None


def build_run_command_request_body(command: str | list[str], opts: RunCommandOpts):
    from opensandbox.adapters.converter.execution_converter import ExecutionConverter

    return ExecutionConverter.to_api_run_command_request(command, opts)


def build_run_in_session_request_body(
    command: str,
    working_directory: str | None,
    timeout: int | None,
):
    from opensandbox.api.execd.models.run_in_session_request import (
        RunInSessionRequest,
    )
    from opensandbox.api.execd.types import UNSET

    return RunInSessionRequest(
        command=command,
        cwd=working_directory if working_directory else UNSET,
        timeout=timeout if timeout is not None else UNSET,
    )


def decode_sse_event_data(data: str) -> EventNode | None:
    if not data.strip():
        return None

    try:
        event_dict = json.loads(data)
        return EventNode(**event_dict)
    except Exception as e:
        logger.error(f"Failed to parse SSE event data: {data}", exc_info=e)
        return None
