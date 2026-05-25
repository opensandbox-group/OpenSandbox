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

import time

import pytest

from opensandbox_server.startup_guard import (
    ALLOW_NO_API_KEY_CONFIRMATION,
    INSECURE_SERVER_ENV_VAR,
    api_key_confirm,
)


class _TTY:
    def isatty(self) -> bool:
        return True


class _NonTTY:
    def isatty(self) -> bool:
        return False


def test_api_key_configured_skips_confirmation(monkeypatch):
    monkeypatch.delenv(INSECURE_SERVER_ENV_VAR, raising=False)

    def _fail_prompt(_: str) -> str:
        raise AssertionError("prompt should not be called when api_key is configured")

    api_key_confirm(
        configured_api_key="secret",
        stdin=_NonTTY(),
        input_func=_fail_prompt,
    )


def test_non_interactive_requires_env_ack(monkeypatch):
    monkeypatch.delenv(INSECURE_SERVER_ENV_VAR, raising=False)

    with pytest.raises(RuntimeError) as exc_info:
        api_key_confirm(
            configured_api_key=None,
            stdin=_NonTTY(),
        )

    assert "Startup blocked" in str(exc_info.value)
    assert INSECURE_SERVER_ENV_VAR in str(exc_info.value)


def test_env_ack_allows_non_interactive_start(monkeypatch):
    monkeypatch.setenv(INSECURE_SERVER_ENV_VAR, ALLOW_NO_API_KEY_CONFIRMATION)

    api_key_confirm(
        configured_api_key=None,
        stdin=_NonTTY(),
    )


def test_env_ack_warning_does_not_log_confirmation_value(monkeypatch):
    monkeypatch.setenv(INSECURE_SERVER_ENV_VAR, ALLOW_NO_API_KEY_CONFIRMATION)
    calls = []

    def _capture_warning(message: str, *args) -> None:
        calls.append(message % args)

    monkeypatch.setattr(
        "opensandbox_server.startup_guard.logger.warning",
        _capture_warning,
    )

    api_key_confirm(
        configured_api_key=None,
        stdin=_NonTTY(),
    )

    assert len(calls) == 1
    assert INSECURE_SERVER_ENV_VAR in calls[0]
    assert f"{INSECURE_SERVER_ENV_VAR}={ALLOW_NO_API_KEY_CONFIRMATION}" not in calls[0]


def test_tty_requires_exact_yes(monkeypatch):
    monkeypatch.delenv(INSECURE_SERVER_ENV_VAR, raising=False)

    with pytest.raises(RuntimeError) as exc_info:
        api_key_confirm(
            configured_api_key=None,
            stdin=_TTY(),
            input_func=lambda _: "yes",
        )

    assert "Startup aborted" in str(exc_info.value)


def test_tty_yes_allows_start(monkeypatch):
    monkeypatch.delenv(INSECURE_SERVER_ENV_VAR, raising=False)

    api_key_confirm(
        configured_api_key=None,
        stdin=_TTY(),
        input_func=lambda _: ALLOW_NO_API_KEY_CONFIRMATION,
    )


def test_tty_confirmation_timeout(monkeypatch):
    monkeypatch.delenv(INSECURE_SERVER_ENV_VAR, raising=False)
    monkeypatch.setattr(
        "opensandbox_server.startup_guard.API_KEY_CONFIRM_TIMEOUT_SECONDS",
        1,
    )

    def _slow_input(_: str) -> str:
        time.sleep(2)
        return ALLOW_NO_API_KEY_CONFIRMATION

    with pytest.raises(RuntimeError) as exc_info:
        api_key_confirm(
            configured_api_key=None,
            stdin=_TTY(),
            input_func=_slow_input,
        )

    assert "timed out" in str(exc_info.value)
