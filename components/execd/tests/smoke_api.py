#!/usr/bin/env python3

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

"""
Simple smoke tests for execd APIs.

Prerequisites:
- execd server running locally (default http://localhost:44772)
- Optional: set env BASE_URL to override
- Optional: set env API_TOKEN if server expects X-EXECD-ACCESS-TOKEN
"""

import json
import os
import sys
import time
import uuid
import tempfile
import pathlib

import requests

BASE_URL = os.environ.get("BASE_URL", "http://localhost:44772").rstrip("/")
API_TOKEN = os.environ.get("API_TOKEN")

HEADERS = {}
if API_TOKEN:
    HEADERS["X-EXECD-ACCESS-TOKEN"] = API_TOKEN

session = requests.Session()
session.headers.update(HEADERS)


def expect(cond: bool, msg: str):
    if not cond:
        raise SystemExit(msg)


def sse_get_command_id() -> str:
    url = f"{BASE_URL}/command"
    payload = {"command": "echo smoke-command && sleep 1", "background": True}
    with session.post(url, json=payload, stream=True, timeout=15) as resp:
        expect(resp.status_code == 200, f"SSE start failed: {resp.status_code} {resp.text}")
        for line in resp.iter_lines():
            if not line or not line.startswith(b"data:"):
                # controller emits raw JSON lines without SSE 'data:' prefix
                try:
                    data = json.loads(line.decode())
                except Exception:
                    continue
            else:
                data = json.loads(line[len(b"data:") :].decode())
            if data.get("type") == "init":
                cmd_id = data.get("text")
                expect(cmd_id, "missing command id in init event")
                return cmd_id
    raise SystemExit("Failed to obtain command id from SSE")


def wait_status(cmd_id: str, timeout: float = 15.0) -> dict:
    url = f"{BASE_URL}/command/status/{cmd_id}"
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        r = session.get(url, timeout=5)
        expect(r.status_code == 200, f"status failed: {r.status_code} {r.text}")
        last = r.json()
        if not last.get("running", True):
            return last
        time.sleep(0.3)
    return last


def fetch_logs(cmd_id: str, cursor: int = 0):
    url = f"{BASE_URL}/command/{cmd_id}/logs"
    r = session.get(url, params={"cursor": cursor}, timeout=10)
    expect(r.status_code == 200, f"logs failed: {r.status_code} {r.text}")
    return r.text, r.headers.get("EXECD-COMMANDS-TAIL-CURSOR")


def run_command_blank_lines():
    """
    Foreground command whose stdout contains consecutive newlines must surface
    blank-line events instead of dropping them. Regression test for the
    readFromPos fix that preserves empty lines (a\n\nb -> ["a", "\n", "b"]).
    """
    url = f"{BASE_URL}/command"
    # Pick a shell-native command per platform so the regression covers both
    # POSIX (LF-only) and Windows cmd (CRLF) byte streams without depending on
    # Git for Windows / MSYS argv mangling. The execd reader collapses CRLF to
    # LF, so both produce ["a", "\n", "b", "\n", "\n", "c"].
    if os.name == "nt":
        # cmd /C echo chain: each segment writes "<text>\r\n"; "echo." writes
        # a bare "\r\n". Order is deterministic because "&" is sequential.
        command = "echo a&echo.&echo b&echo.&echo.&echo c"
    else:
        # printf emits exact bytes: a\n\nb\n\n\nc\n
        command = "printf 'a\\n\\nb\\n\\n\\nc\\n'"
    payload = {
        "command": command,
        "background": False,
    }

    stdout_texts = []
    saw_complete = False
    with session.post(url, json=payload, stream=True, timeout=15) as resp:
        expect(resp.status_code == 200, f"SSE start failed: {resp.status_code} {resp.text}")
        for line in resp.iter_lines():
            if not line:
                continue
            try:
                if line.startswith(b"data:"):
                    data = json.loads(line[len(b"data:") :].decode())
                else:
                    data = json.loads(line.decode())
            except Exception:
                continue
            event_type = data.get("type")
            if event_type == "stdout":
                stdout_texts.append(data.get("text", ""))
            elif event_type == "execution_complete":
                saw_complete = True
                break

    expect(saw_complete, "did not observe execution_complete")
    want = ["a", "\n", "b", "\n", "\n", "c"]
    expect(
        stdout_texts == want,
        f"blank-line stdout sequence mismatch: got {stdout_texts!r}, want {want!r}",
    )


def sse_disconnect_should_stop_ping():
    """
    Open an SSE stream for a long-running command, receive init, then close the
    client side early to ensure the server handles disconnects (ping loop should
    stop). We verify the server is still responsive afterwards.
    """
    url = f"{BASE_URL}/command"
    payload = {
        # long command so the server would keep pinging if not cancelled
        "command": "sh -c 'echo long-run-start && sleep 20 && echo long-run-end'",
        "background": False,
    }

    with session.post(url, json=payload, stream=True, timeout=10) as resp:
        expect(resp.status_code == 200, f"SSE start failed: {resp.status_code} {resp.text}")
        for line in resp.iter_lines():
            if not line:
                continue
            try:
                if line.startswith(b"data:"):
                    data = json.loads(line[len(b"data:") :].decode())
                else:
                    data = json.loads(line.decode())
            except Exception:
                continue
            if data.get("type") == "init":
                break
        # explicitly close to simulate client drop
        resp.close()

    # Give server a moment to observe disconnect and ensure API remains healthy
    time.sleep(1)
    pong = session.get(f"{BASE_URL}/ping", timeout=5)
    expect(pong.status_code == 200, "ping failed after SSE disconnect")


def upload_and_download():
    tmp_dir = f"/tmp/execd-smoke-{uuid.uuid4().hex}"
    path = f"{tmp_dir}/hello.txt"
    metadata = json.dumps({"path": path})
    files = {
        "metadata": ("metadata", metadata, "application/json"),
        "file": ("file", b"hello execd\n", "application/octet-stream"),
    }
    up = session.post(f"{BASE_URL}/files/upload", files=files, timeout=10)
    expect(up.status_code == 200, f"upload failed: {up.status_code} {up.text}")

    down = session.get(f"{BASE_URL}/files/download", params={"path": path}, timeout=10)
    expect(down.status_code == 200, f"download failed: {down.status_code} {down.text}")
    expect(down.content == b"hello execd\n", "downloaded content mismatch")


def filesystem_smoke():
    base_dir = os.path.join(tempfile.gettempdir(), f"execd-smoke-{uuid.uuid4().hex}")
    sub_dir = os.path.join(base_dir, "sub")
    file_path = os.path.join(sub_dir, "hello.txt")
    renamed_path = os.path.join(sub_dir, "hello_renamed.txt")
    home_dir = os.path.expanduser("~")
    home_file_name = f"execd-smoke-home-{uuid.uuid4().hex}.txt"
    home_file_abs = os.path.join(home_dir, home_file_name)
    # Windows uses backslash path style by default; keep smoke path style aligned
    # with platform so "~" expansion is exercised in a realistic way.
    home_file_tilde = f"~\\{home_file_name}" if os.name == "nt" else f"~/{home_file_name}"

    # create dirs
    mk = session.post(f"{BASE_URL}/directories", json={sub_dir: {"mode": 0}}, timeout=10)
    expect(mk.status_code == 200, f"mkdir failed: {mk.status_code} {mk.text}")

    # upload a file
    metadata = json.dumps({"path": file_path})
    files = {
        "metadata": ("metadata", metadata, "application/json"),
        "file": ("file", b"hello execd\n", "application/octet-stream"),
    }
    up = session.post(f"{BASE_URL}/files/upload", files=files, timeout=10)
    expect(up.status_code == 200, f"upload failed: {up.status_code} {up.text}")

    # get info
    info = session.get(f"{BASE_URL}/files/info", params={"path": [file_path]}, timeout=10)
    expect(info.status_code == 200, f"info failed: {info.status_code} {info.text}")

    # search
    search = session.get(f"{BASE_URL}/files/search", params={"path": base_dir, "pattern": "*.txt"}, timeout=10)
    expect(search.status_code == 200, f"search failed: {search.status_code} {search.text}")
    found = False
    for f in search.json():
        p = f.get("path")
        if not p:
            continue
        if pathlib.Path(p).resolve() == pathlib.Path(file_path).resolve():
            found = True
            break
    expect(found, "search did not find file")

    # replace content
    rep = session.post(
        f"{BASE_URL}/files/replace",
        json={file_path: {"old": "hello", "new": "hi"}},
        timeout=10,
    )
    expect(rep.status_code == 200, f"replace failed: {rep.status_code} {rep.text}")

    # download to verify replace
    down = session.get(f"{BASE_URL}/files/download", params={"path": file_path}, timeout=10)
    expect(down.status_code == 200, f"download failed: {down.status_code} {down.text}")
    expect(down.content == b"hi execd\n", "replace content mismatch")

    # chmod (mode only)
    chmod = session.post(f"{BASE_URL}/files/permissions", json={file_path: {"mode": 644}}, timeout=10)
    expect(chmod.status_code == 200, f"chmod failed: {chmod.status_code} {chmod.text}")

    # rename
    mv = session.post(
        f"{BASE_URL}/files/mv",
        json=[{"src": file_path, "dest": renamed_path}],
        timeout=10,
    )
    expect(mv.status_code == 200, f"rename failed: {mv.status_code} {mv.text}")

    # remove file
    rm_file = session.delete(f"{BASE_URL}/files", params={"path": [renamed_path]}, timeout=10)
    expect(rm_file.status_code == 200, f"remove file failed: {rm_file.status_code} {rm_file.text}")

    # read file using "~/<file>" style path
    home_metadata = json.dumps({"path": home_file_abs})
    home_files = {
        "metadata": ("metadata", home_metadata, "application/json"),
        "file": ("file", b"home path content\n", "application/octet-stream"),
    }
    home_up = session.post(f"{BASE_URL}/files/upload", files=home_files, timeout=10)
    expect(home_up.status_code == 200, f"home upload failed: {home_up.status_code} {home_up.text}")

    home_down = session.get(f"{BASE_URL}/files/download", params={"path": home_file_tilde}, timeout=10)
    # On Windows, also accept "~/" form as a compatibility fallback.
    if home_down.status_code != 200 and os.name == "nt":
        alt_tilde = f"~/{home_file_name}"
        home_down = session.get(f"{BASE_URL}/files/download", params={"path": alt_tilde}, timeout=10)
    expect(home_down.status_code == 200, f"home download via tilde failed: {home_down.status_code} {home_down.text}")
    expect(home_down.content == b"home path content\n", "home download content mismatch")

    home_rm = session.delete(f"{BASE_URL}/files", params={"path": [home_file_tilde]}, timeout=10)
    expect(home_rm.status_code == 200, f"home remove failed: {home_rm.status_code} {home_rm.text}")

    # remove dir
    rm_dir = session.delete(f"{BASE_URL}/directories", params={"path": [base_dir]}, timeout=10)
    expect(rm_dir.status_code == 200, f"remove dir failed: {rm_dir.status_code} {rm_dir.text}")


def main():
    print(f"[+] base: {BASE_URL}")
    r = session.get(f"{BASE_URL}/ping", timeout=5)
    expect(r.status_code == 200, "ping failed")
    print("[+] ping ok")

    sse_disconnect_should_stop_ping()
    print("[+] SSE disconnect handled")

    run_command_blank_lines()
    print("[+] run_command preserves blank lines")

    cmd_id = sse_get_command_id()
    print(f"[+] command id: {cmd_id}")

    status = wait_status(cmd_id)
    print(f"[+] status: {status}")

    logs, cursor = fetch_logs(cmd_id, cursor=0)
    print(f"[+] logs (cursor={cursor}):\n{logs}")

    filesystem_smoke()
    print("[+] filesystem APIs ok")

    print("[+] smoke tests PASS")


if __name__ == "__main__":
    try:
        main()
    except SystemExit as exc:
        print(f"[!] smoke tests FAIL: {exc}", file=sys.stderr)
        sys.exit(1)