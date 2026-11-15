# Copyright 2026 The OpenSandbox Authors.
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

"""Tests for opensandbox_cli.utils — duration parsing, key-value type, error handling."""

from __future__ import annotations

import json
from datetime import timedelta
from pathlib import Path

import pytest

from opensandbox_cli.utils import (
    DURATION,
    KEY_VALUE,
    load_json_object,
    parse_duration,
    validation_message,
)


class TestParseDuration:
    @pytest.mark.parametrize(
        "input_str, expected",
        [
            ("10", timedelta(seconds=10)),
            ("0", timedelta(seconds=0)),
            ("10s", timedelta(seconds=10)),
            ("5m", timedelta(minutes=5)),
            ("2h", timedelta(hours=2)),
            ("1h30m", timedelta(hours=1, minutes=30)),
            ("1h30m45s", timedelta(hours=1, minutes=30, seconds=45)),
            ("90s", timedelta(seconds=90)),
        ],
    )
    def test_valid_durations(self, input_str: str, expected: timedelta) -> None:
        assert parse_duration(input_str) == expected

    def test_strips_whitespace(self) -> None:
        assert parse_duration("  10m  ") == timedelta(minutes=10)


class TestDurationType:
    def test_converts_string(self) -> None:
        result = DURATION.convert("5m", None, None)
        assert result == timedelta(minutes=5)

    def test_passes_through_timedelta(self) -> None:
        td = timedelta(hours=1)
        result = DURATION.convert(td, None, None)  # type: ignore[arg-type]
        assert result is td


class TestKeyValueType:
    def test_parses_simple_kv(self) -> None:
        assert KEY_VALUE.convert("FOO=bar", None, None) == ("FOO", "bar")

    def test_value_can_contain_equals(self) -> None:
        assert KEY_VALUE.convert("key=a=b=c", None, None) == ("key", "a=b=c")

    def test_empty_value(self) -> None:
        assert KEY_VALUE.convert("key=", None, None) == ("key", "")

    def test_passes_through_tuple(self) -> None:
        t = ("key", "val")
        result = KEY_VALUE.convert(t, None, None)  # type: ignore[arg-type]
        assert result is t


class TestLoadJsonObject:
    def test_loads_object(self, tmp_path: Path) -> None:
        path = tmp_path / "req.json"
        path.write_text(json.dumps({"image": "python:3.12"}))
        assert load_json_object(str(path)) == {"image": "python:3.12"}


class TestValidationMessage:
    def test_joins_field_errors(self) -> None:
        from pydantic import BaseModel, ValidationError

        class _Model(BaseModel):
            image: str

        with pytest.raises(ValidationError) as exc_info:
            _Model()
        message = validation_message(exc_info.value)
        assert "image" in message
        assert ": " in message
