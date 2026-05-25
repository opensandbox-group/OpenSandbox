# Copyright 2026 Alibaba Group Holding Ltd.
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

import logging
import os
import sys
import threading

logger = logging.getLogger(__name__)

INSECURE_SERVER_ENV_VAR = "OPENSANDBOX_INSECURE_SERVER"
ALLOW_NO_API_KEY_CONFIRMATION = "YES"
ANSI_RED = "\033[31m"
ANSI_RESET = "\033[0m"
API_KEY_CONFIRM_TIMEOUT_SECONDS = 30


class _InputResult:
    def __init__(self) -> None:
        self.value: str | None = None
        self.error: BaseException | None = None


def _read_with_timeout(prompt: str, input_func, timeout_seconds: int) -> str:
    result = _InputResult()

    def _worker() -> None:
        try:
            result.value = input_func(prompt)
        except BaseException as exc:  # pragma: no cover
            result.error = exc

    t = threading.Thread(target=_worker, daemon=True)
    t.start()
    t.join(timeout_seconds)

    if t.is_alive():
        raise TimeoutError(f"confirmation input timed out after {timeout_seconds} seconds")
    if result.error is not None:
        raise result.error
    return result.value or ""


def api_key_confirm(
    *,
    configured_api_key: str | None,
    stdin=None,
    environ=None,
    input_func=input,
) -> None:
    """
    Enforce explicit confirmation before starting without server.api_key.

    Confirmation sources:
    1) OPENSANDBOX_INSECURE_SERVER=YES (non-interactive safe path)
    2) Interactive TTY prompt requiring exact input 'YES'
    """
    if configured_api_key and configured_api_key.strip():
        return

    env = environ if environ is not None else os.environ

    if env.get(INSECURE_SERVER_ENV_VAR) == ALLOW_NO_API_KEY_CONFIRMATION:
        logger.warning(
            "server.api_key is not configured. Proceeding because %s explicitly acknowledges "
            "the insecure server mode.",
            INSECURE_SERVER_ENV_VAR,
        )
        return

    stdin_stream = stdin if stdin is not None else sys.stdin
    if stdin_stream is not None and hasattr(stdin_stream, "isatty") and stdin_stream.isatty():
        try:
            confirmation = _read_with_timeout(
                f"{ANSI_RED}"
                "SECURITY WARNING: server.api_key is empty; API authentication is disabled. "
                "Type 'YES' to continue startup without API key. "
                "Strongly recommend setting server.api_key. "
                "See: https://github.com/alibaba/OpenSandbox/issues/750 "
                ": "
                f"{ANSI_RESET}",
                input_func,
                API_KEY_CONFIRM_TIMEOUT_SECONDS,
            )
        except TimeoutError as exc:
            raise RuntimeError(
                "Startup aborted: confirmation timed out waiting for YES. "
                "Strongly recommend setting server.api_key. "
                "See: https://github.com/alibaba/OpenSandbox/issues/750"
            ) from exc
        if confirmation == ALLOW_NO_API_KEY_CONFIRMATION:
            logger.warning(
                "server.api_key is not configured. Proceeding after interactive confirmation."
            )
            return
        raise RuntimeError(
            "Startup aborted: missing explicit confirmation for empty server.api_key. "
            "Strongly recommend setting server.api_key. "
            "See: https://github.com/alibaba/OpenSandbox/issues/750"
        )

    raise RuntimeError(
        "Startup blocked: server.api_key is empty in non-interactive mode. "
        f"Set {INSECURE_SERVER_ENV_VAR}={ALLOW_NO_API_KEY_CONFIRMATION} to acknowledge the risk. "
        "Strongly recommend setting server.api_key. "
        "See: https://github.com/alibaba/OpenSandbox/issues/750"
    )
