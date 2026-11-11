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

import click
import pytest
from click.testing import CliRunner
from pydantic import BaseModel, ValidationError, model_validator

from opensandbox_cli.output import OutputFormatter
from opensandbox_cli.utils import (
    DURATION,
    KEY_VALUE,
    handle_errors,
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

    @pytest.mark.parametrize(
        "input_str",
        [
            "",
            "abc",
            "10x",
            "m10",
            "-5m",
        ],
    )
    def test_invalid_durations(self, input_str: str) -> None:
        with pytest.raises(click.BadParameter):
            parse_duration(input_str)

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

    def test_invalid_raises_bad_parameter(self) -> None:
        with pytest.raises(click.exceptions.BadParameter):
            DURATION.convert("invalid", None, None)


class TestKeyValueType:
    def test_parses_simple_kv(self) -> None:
        assert KEY_VALUE.convert("FOO=bar", None, None) == ("FOO", "bar")

    def test_value_can_contain_equals(self) -> None:
        assert KEY_VALUE.convert("key=a=b=c", None, None) == ("key", "a=b=c")

    def test_empty_value(self) -> None:
        assert KEY_VALUE.convert("key=", None, None) == ("key", "")

    def test_missing_equals_fails(self) -> None:
        with pytest.raises(click.exceptions.BadParameter):
            KEY_VALUE.convert("no-equals", None, None)

    def test_passes_through_tuple(self) -> None:
        t = ("key", "val")
        result = KEY_VALUE.convert(t, None, None)  # type: ignore[arg-type]
        assert result is t


class _UniqueNames(BaseModel):
    names: list[str] = []

    @model_validator(mode="after")
    def _names_unique(self) -> _UniqueNames:
        if len(self.names) != len(set(self.names)):
            raise ValueError("names must be unique")
        return self


class TestLoadJsonObject:
    def test_loads_object(self, tmp_path: Path) -> None:
        path = tmp_path / "req.json"
        path.write_text(json.dumps({"image": "python:3.12"}))
        assert load_json_object(str(path)) == {"image": "python:3.12"}

    def test_invalid_json_names_the_file(self, tmp_path: Path) -> None:
        path = tmp_path / "req.json"
        path.write_text("{not json")
        with pytest.raises(click.ClickException, match="Invalid JSON in request file"):
            load_json_object(str(path))

    def test_non_object_rejected(self, tmp_path: Path) -> None:
        path = tmp_path / "req.json"
        path.write_text(json.dumps([1, 2]))
        with pytest.raises(click.ClickException, match="must contain a JSON object"):
            load_json_object(str(path))

    def test_unreadable_file_names_the_file(self, tmp_path: Path) -> None:
        path = tmp_path / "req.json"
        path.write_bytes(b'{"image": "\xff\xfe"}')
        with pytest.raises(click.ClickException, match="Cannot read request file"):
            load_json_object(str(path))

    def test_missing_file_names_the_file(self, tmp_path: Path) -> None:
        with pytest.raises(click.ClickException, match="Cannot read request file"):
            load_json_object(str(tmp_path / "missing.json"))


class TestValidationMessage:
    def test_joins_field_errors(self) -> None:
        class _Model(BaseModel):
            image: str

        with pytest.raises(ValidationError) as exc_info:
            _Model()
        message = validation_message(exc_info.value)
        assert "image" in message
        assert ": " in message

    def test_model_level_error_has_no_dangling_prefix(self) -> None:
        with pytest.raises(ValidationError) as exc_info:
            _UniqueNames(names=["a", "a"])
        message = validation_message(exc_info.value)
        assert "names must be unique" in message
        assert message.index("Value error") == 0


class TestHandleErrors:
    @pytest.mark.parametrize("color", [True, False])
    def test_unexpected_error_prints_message_as_is(self, color: bool) -> None:
        @click.command()
        @click.pass_context
        @handle_errors
        def fail(ctx: click.Context) -> None:
            raise RuntimeError("cannot open /data/[old]/a[/]b")

        obj = type("Obj", (), {"output": OutputFormatter("table", color=color)})()
        result = CliRunner().invoke(fail, obj=obj)

        assert result.exit_code == 1
        assert "cannot open /data/[old]/a[/]b" in result.stderr
        assert "Type: RuntimeError" in result.stderr
        assert "[dim]" not in result.stderr
