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

"""Exercise stdout aliases with real file descriptors, outside CliRunner capture."""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

import pytest

pytestmark = pytest.mark.skipif(os.name == "nt", reason="POSIX stdout device aliases")

_PAYLOAD = b"\x00\xfffirstsecond"
_DOWNLOAD = """
import sys
from unittest.mock import MagicMock, patch

from opensandbox.exceptions import SandboxApiException
from opensandbox_cli.main import cli

scenario = sys.argv.pop(1)

def stream(path):
    assert path == '/remote.bin'
    if scenario == 'not_found':
        raise SandboxApiException('File not found', status_code=404)
    if scenario == 'empty':
        return
    yield b'\\x00\\xfffirst'
    if scenario == 'disconnect':
        raise ConnectionError('Download disconnected')
    if scenario == 'interrupt':
        raise KeyboardInterrupt
    yield b'second'

sandbox = MagicMock()
sandbox.files.read_bytes_stream.side_effect = stream
with patch('opensandbox_cli.client.ClientContext.connect_sandbox', return_value=sandbox):
    try:
        cli()
    finally:
        sandbox.close.assert_called_once()
"""


def _download(
    tmp_path: Path, target: str, stdout_kind: str, fmt: str, scenario: str
) -> tuple[subprocess.CompletedProcess[bytes], bytes]:
    if target == "symlink":
        link = tmp_path / "stdout-link"
        link.symlink_to("/dev/stdout")
        target = str(link)
    command = [
        sys.executable, "-c", _DOWNLOAD, scenario,
        "--config", str(tmp_path / "unused-config.toml"),
        "--api-key", "test-key", "--domain", "localhost:8080", "--no-color",
        "file", "download", "sb-1", "/remote.bin", target, "-o", fmt,
    ]
    if stdout_kind == "pipe":
        result = subprocess.run(command, capture_output=True, timeout=10)
        received = result.stdout
    else:
        output = tmp_path / "stdout.bin"
        if stdout_kind == "append":
            output.write_bytes(b"existing prefix")
        # Use a write-only descriptor, as a shell redirect would.
        with output.open("ab" if stdout_kind == "append" else "wb") as stdout:
            original_inode = output.stat().st_ino
            result = subprocess.run(command, stdout=stdout, stderr=subprocess.PIPE, timeout=10)
        assert output.stat().st_ino == original_inode
        received = output.read_bytes()
    if target == str(tmp_path / "stdout-link"):
        assert Path(target).is_symlink()
    assert not list(tmp_path.glob(".osb-download-*"))
    return result, received


@pytest.mark.parametrize("target", ["/dev/stdout", "/dev/fd/1", "symlink"])
@pytest.mark.parametrize("stdout_kind", ["pipe", "file", "append"])
@pytest.mark.parametrize("fmt", ["table", "json", "yaml"])
def test_stdout_download_contains_only_payload(
    tmp_path: Path, target: str, stdout_kind: str, fmt: str
) -> None:
    result, received = _download(tmp_path, target, stdout_kind, fmt, "success")

    assert result.returncode == 0, result.stderr
    prefix = b"existing prefix" if stdout_kind == "append" else b""
    assert received == prefix + _PAYLOAD
    assert result.stderr == b""


@pytest.mark.parametrize("scenario", ["empty", "not_found", "disconnect", "interrupt"])
@pytest.mark.parametrize("stdout_kind", ["pipe", "file"])
@pytest.mark.parametrize("fmt", ["table", "json", "yaml"])
def test_stdout_download_empty_or_failed_stream(
    tmp_path: Path, stdout_kind: str, fmt: str, scenario: str
) -> None:
    result, received = _download(tmp_path, "/dev/stdout", stdout_kind, fmt, scenario)

    assert (result.returncode == 0) == (scenario == "empty"), result.stderr
    assert received == (b"\x00\xfffirst" if scenario in ("disconnect", "interrupt") else b"")
    assert bool(result.stderr) == (scenario != "empty")
    if scenario != "empty":
        expected_error = {
            "not_found": b"File not found",
            "disconnect": b"Download disconnected",
            "interrupt": b"Aborted!",
        }[scenario]
        assert expected_error in result.stderr
        assert b"Traceback" not in result.stderr
