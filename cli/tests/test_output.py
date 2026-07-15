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

"""Tests for opensandbox_cli.output — table, JSON, YAML rendering."""

from __future__ import annotations

import json

import pytest
from pydantic import BaseModel

from opensandbox_cli.output import OutputFormatter

# ---------------------------------------------------------------------------
# Test models
# ---------------------------------------------------------------------------


class FakeItem(BaseModel):
    id: str
    name: str
    score: int


# ---------------------------------------------------------------------------
# JSON output
# ---------------------------------------------------------------------------


class TestJsonOutput:
    def test_print_dict(self, capsys: pytest.CaptureFixture[str]) -> None:
        fmt = OutputFormatter("json", color=False)
        fmt.print_dict({"key": "value", "num": 42})
        captured = capsys.readouterr()
        data = json.loads(captured.out)
        assert data == {"key": "value", "num": 42}

    def test_print_model(self, capsys: pytest.CaptureFixture[str]) -> None:
        fmt = OutputFormatter("json", color=False)
        item = FakeItem(id="abc", name="test", score=100)
        fmt.print_model(item)
        captured = capsys.readouterr()
        data = json.loads(captured.out)
        assert data["id"] == "abc"
        assert data["name"] == "test"
        assert data["score"] == 100

    def test_print_models(self, capsys: pytest.CaptureFixture[str]) -> None:
        fmt = OutputFormatter("json", color=False)
        items = [
            FakeItem(id="1", name="a", score=10),
            FakeItem(id="2", name="b", score=20),
        ]
        fmt.print_models(items, columns=["id", "name", "score"])
        captured = capsys.readouterr()
        data = json.loads(captured.out)
        assert len(data) == 2
        assert data[0]["id"] == "1"
        assert data[1]["name"] == "b"

    def test_success_renders_structured_json(self, capsys: pytest.CaptureFixture[str]) -> None:
        fmt = OutputFormatter("json", color=False)
        fmt.success("done")
        captured = capsys.readouterr()
        data = json.loads(captured.out)
        assert data == {"status": "ok", "message": "done"}


# ---------------------------------------------------------------------------
# YAML output
# ---------------------------------------------------------------------------


class TestYamlOutput:
    def test_print_dict(self, capsys: pytest.CaptureFixture[str]) -> None:
        fmt = OutputFormatter("yaml", color=False)
        fmt.print_dict({"key": "value"})
        captured = capsys.readouterr()
        assert "key: value" in captured.out

    def test_print_model(self, capsys: pytest.CaptureFixture[str]) -> None:
        fmt = OutputFormatter("yaml", color=False)
        item = FakeItem(id="x", name="y", score=5)
        fmt.print_model(item)
        captured = capsys.readouterr()
        assert "id: x" in captured.out
        assert "name: y" in captured.out
        assert "score: 5" in captured.out


# ---------------------------------------------------------------------------
# Table output
# ---------------------------------------------------------------------------


class TestTableOutput:
    def test_print_dict_contains_values(self, capsys: pytest.CaptureFixture[str]) -> None:
        fmt = OutputFormatter("table", color=False)
        fmt.print_dict({"host": "example.com", "port": 8080}, title="Config")
        captured = capsys.readouterr()
        assert any(cell == "example.com" for cell in captured.out.split())
        assert "8080" in captured.out
        assert "Config" in captured.out

    def test_print_dict_none_renders_dash(self, capsys: pytest.CaptureFixture[str]) -> None:
        fmt = OutputFormatter("table", color=False)
        fmt.print_dict({"key": None})
        captured = capsys.readouterr()
        assert "-" in captured.out

    def test_print_models_shows_headers(self, capsys: pytest.CaptureFixture[str]) -> None:
        fmt = OutputFormatter("table", color=False)
        items = [FakeItem(id="1", name="a", score=10)]
        fmt.print_models(items, columns=["id", "name", "score"], title="Items")
        captured = capsys.readouterr()
        assert "ID" in captured.out
        assert "NAME" in captured.out
        assert "SCORE" in captured.out

    def test_print_text_ignores_format(self, capsys: pytest.CaptureFixture[str]) -> None:
        fmt = OutputFormatter("json", color=False)
        fmt.print_text("hello world")
        captured = capsys.readouterr()
        assert captured.out.strip() == "hello world"
