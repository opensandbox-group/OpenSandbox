#
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
#
from __future__ import annotations

import pytest

from opensandbox.models.execd import (
    RunningCommandSummary,
    TerminalCommandSummary,
    parse_list_commands_page,
)


def test_parser_selects_running_and_terminal_branches() -> None:
    page = parse_list_commands_page(
        {
            "commands": [
                {
                    "session": "running",
                    "running": True,
                    "background": True,
                    "started_at": "2026-07-24T01:02:03Z",
                },
                {
                    "session": "terminal",
                    "running": False,
                    "background": False,
                    "started_at": "2026-07-24T01:02:03Z",
                    "finished_at": "2026-07-24T01:03:03Z",
                    "exit_code": None,
                    "error": "killed",
                },
            ],
            "pagination": {"limit": 2, "nextCursor": "cursor", "future": True},
            "future": True,
        }
    )

    assert isinstance(page.commands[0], RunningCommandSummary)
    assert isinstance(page.commands[1], TerminalCommandSummary)
    assert page.pagination.next_cursor == "cursor"


@pytest.mark.parametrize(
    "started_at",
    [
        "2026-07-24T01:02:03Z",
        "2026-07-24t01:02:03z",
        "2026-07-24T01:02:03.123456Z",
        "2026-07-24T01:02:03+08:00",
    ],
)
def test_parser_accepts_rfc3339_command_started_at(started_at: str) -> None:
    page = parse_list_commands_page(
        {
            "commands": [
                {
                    "session": "running",
                    "running": True,
                    "background": True,
                    "started_at": started_at,
                }
            ],
            "pagination": {"limit": 1},
        }
    )

    assert page.commands[0].started_at.tzinfo is not None


@pytest.mark.parametrize(
    ("started_at", "microsecond"),
    [
        ("2026-07-24T01:02:03.1Z", 100_000),
        ("2026-07-24T01:02:03.12Z", 120_000),
        ("2026-07-24T01:02:03.12345Z", 123_450),
        ("2026-07-24T01:02:03.123456789Z", 123_456),
    ],
)
def test_parser_normalizes_rfc3339_fractional_seconds(
    started_at: str, microsecond: int
) -> None:
    page = parse_list_commands_page(
        {
            "commands": [
                {
                    "session": "running",
                    "running": True,
                    "background": True,
                    "started_at": started_at,
                }
            ],
            "pagination": {"limit": 1},
        }
    )

    assert page.commands[0].started_at.microsecond == microsecond


@pytest.mark.parametrize(
    "started_at",
    [
        "2026-07-24",
        "2026-07-24T01:02:03",
        "2026-07-24 01:02:03Z",
        "20260724T010203Z",
        "2026-13-24T01:02:03Z",
        "2026-07-24T01:02:03+25:00",
        "2026-07-24T01:02:03+01:60",
        123,
    ],
)
def test_parser_rejects_non_rfc3339_command_started_at(started_at: object) -> None:
    with pytest.raises(ValueError):
        parse_list_commands_page(
            {
                "commands": [
                    {
                        "session": "running",
                        "running": True,
                        "background": True,
                        "started_at": started_at,
                    }
                ],
                "pagination": {"limit": 1},
            }
        )


def test_parser_accepts_rfc3339_terminal_timestamps() -> None:
    page = parse_list_commands_page(
        {
            "commands": [
                {
                    "session": "terminal",
                    "running": False,
                    "background": False,
                    "started_at": "2026-07-24t01:02:03z",
                    "finished_at": "2026-07-24T01:03:03.123456+08:00",
                    "exit_code": 0,
                }
            ],
            "pagination": {"limit": 1},
        }
    )

    command = page.commands[0]
    assert command.started_at.tzinfo is not None
    assert command.finished_at.tzinfo is not None


@pytest.mark.parametrize("exit_code", [-2_147_483_649, 2_147_483_648])
def test_parser_rejects_terminal_exit_code_outside_int32_range(exit_code: int) -> None:
    with pytest.raises(ValueError, match="command.exit_code"):
        parse_list_commands_page(
            {
                "commands": [
                    {
                        "session": "terminal",
                        "running": False,
                        "background": False,
                        "started_at": "2026-07-24T01:02:03Z",
                        "finished_at": "2026-07-24T01:03:03Z",
                        "exit_code": exit_code,
                    }
                ],
                "pagination": {"limit": 1},
            }
        )


def test_parser_normalizes_terminal_rfc3339_fractional_seconds() -> None:
    page = parse_list_commands_page(
        {
            "commands": [
                {
                    "session": "terminal",
                    "running": False,
                    "background": False,
                    "started_at": "2026-07-24T01:02:03.12345Z",
                    "finished_at": "2026-07-24T01:03:03.123456789Z",
                    "exit_code": 0,
                }
            ],
            "pagination": {"limit": 1},
        }
    )

    command = page.commands[0]
    assert command.started_at.microsecond == 123_450
    assert command.finished_at.microsecond == 123_456


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("started_at", "2026-07-24T01:02:03"),
        ("finished_at", "2026-07-24T01:03:03+01:60"),
    ],
)
def test_parser_rejects_non_rfc3339_terminal_timestamps(
    field: str, value: str
) -> None:
    command = {
        "session": "terminal",
        "running": False,
        "background": False,
        "started_at": "2026-07-24T01:02:03Z",
        "finished_at": "2026-07-24T01:03:03Z",
        "exit_code": 0,
    }
    command[field] = value

    with pytest.raises(ValueError):
        parse_list_commands_page(
            {"commands": [command], "pagination": {"limit": 1}}
        )


@pytest.mark.parametrize(
    "payload",
    [
        {"commands": [], "pagination": None},
        {"commands": {}, "pagination": {"limit": 1}},
        {"commands": [], "pagination": {"limit": True}},
        {"commands": [], "pagination": {"limit": 0}},
        {"commands": [], "pagination": {"limit": 101}},
        {"commands": [], "pagination": {"limit": 1, "nextCursor": None}},
        {"commands": [], "pagination": {"limit": 1, "nextCursor": 1}},
        {"commands": [], "pagination": {"limit": 1, "nextCursor": ""}},
        {"commands": [], "pagination": {"limit": 1, "nextCursor": " \t "}},
        {
            "commands": [
                {
                    "session": "bad-running",
                    "running": "true",
                    "background": True,
                    "started_at": "2026-07-24T01:02:03Z",
                }
            ],
            "pagination": {"limit": 1},
        },
        {
            "commands": [
                {
                    "session": "running-terminal",
                    "running": True,
                    "background": True,
                    "started_at": "2026-07-24T01:02:03Z",
                    "finished_at": "2026-07-24T01:03:03Z",
                }
            ],
            "pagination": {"limit": 1},
        },
        {
            "commands": [
                {
                    "session": "running-extra",
                    "running": True,
                    "background": True,
                    "started_at": "2026-07-24T01:02:03Z",
                    "unknown": True,
                }
            ],
            "pagination": {"limit": 1},
        },
        {
            "commands": [
                {
                    "session": "terminal-missing",
                    "running": False,
                    "background": False,
                    "started_at": "2026-07-24T01:02:03Z",
                    "exit_code": None,
                }
            ],
            "pagination": {"limit": 1},
        },
        {
            "commands": [
                {
                    "session": "terminal-error",
                    "running": False,
                    "background": False,
                    "started_at": "2026-07-24T01:02:03Z",
                    "finished_at": "2026-07-24T01:03:03Z",
                    "exit_code": None,
                    "error": 1,
                }
            ],
            "pagination": {"limit": 1},
        },
    ],
)
def test_parser_rejects_invalid_command_inventory_payload(payload: object) -> None:
    with pytest.raises(ValueError):
        parse_list_commands_page(payload)
