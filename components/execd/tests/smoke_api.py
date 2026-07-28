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


def iter_sse_events(lines):
    for line in lines:
        if not line:
            continue
        try:
            if line.startswith(b"data:"):
                event = json.loads(line[len(b"data:") :].decode())
            else:
                # controller emits raw JSON lines without SSE 'data:' prefix
                event = json.loads(line.decode())
        except Exception:
            continue
        if isinstance(event, dict):
            yield event


def wait_for_command_barrier(events, *, background: bool, succeeds: bool) -> str:
    command_id = None
    expected = "execution_complete" if background or succeeds else "error"
    for event in events:
        event_type = event.get("type")
        if event_type == "init":
            command_id = event.get("text")
            expect(command_id, "missing command id in init event")
            continue
        if command_id is None:
            if event_type in ("execution_complete", "error"):
                raise SystemExit("command terminated before init")
            continue
        if event_type == expected:
            return command_id
        if event_type in ("execution_complete", "error"):
            raise SystemExit(f"unexpected command event after init: {event_type}")
    raise SystemExit("command stream ended before readiness barrier")


def sse_get_command_id() -> str:
    url = f"{BASE_URL}/command"
    if os.name == "nt":
        command = "echo smoke-command & ping -n 2 127.0.0.1 >nul"
    else:
        command = "echo smoke-command && sleep 1"
    payload = {"command": command, "background": True}
    with session.post(url, json=payload, stream=True, timeout=15) as resp:
        expect(resp.status_code == 200, f"SSE start failed: {resp.status_code} {resp.text}")
        return wait_for_command_barrier(
            iter_sse_events(resp.iter_lines()), background=True, succeeds=True
        )


def command_for(success: bool, label: str) -> str:
    if os.name == "nt":
        code = "0" if success else "17"
        return f"echo {label} & exit /b {code}"
    code = "0" if success else "17"
    return f"printf '{label}\\n'; exit {code}"


def start_command(command: str, background: bool, succeeds: bool) -> str:
    url = f"{BASE_URL}/command"
    payload = {"command": command, "background": background}
    with session.post(url, json=payload, stream=True, timeout=15) as resp:
        expect(resp.status_code == 200, f"command start failed: {resp.status_code} {resp.text}")
        return wait_for_command_barrier(
            iter_sse_events(resp.iter_lines()), background=background, succeeds=succeeds
        )


def list_commands(timeout: float = 10) -> list[dict]:
    response = session.get(f"{BASE_URL}/command", timeout=timeout)
    expect(response.status_code == 200, f"command list failed: {response.status_code} {response.text}")
    payload = response.json()
    commands = payload.get("commands")
    expect(isinstance(commands, list), f"command list missing commands array: {payload}")
    return commands


INVENTORY_CONDITION_TIMEOUT = 5.0
INVENTORY_CONDITION_INTERVAL = 0.05


def wait_for_inventory_condition(
    predicate,
    *,
    description: str,
    timeout: float = INVENTORY_CONDITION_TIMEOUT,
) -> list[dict]:
    deadline = time.monotonic() + timeout
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise SystemExit(
                f"inventory condition not reached before {timeout:g}s deadline: {description}"
            )
        commands = list_commands(timeout=min(10.0, remaining))
        if predicate(commands):
            return commands
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise SystemExit(
                f"inventory condition not reached before {timeout:g}s deadline: {description}"
            )
        time.sleep(min(INVENTORY_CONDITION_INTERVAL, remaining))


def expect_terminal_delete_legacy_error(command_id: str):
    response = session.delete(f"{BASE_URL}/command", params={"id": command_id}, timeout=10)
    expect(response.status_code == 500, f"terminal DELETE changed legacy status: {response.status_code} {response.text}")
    expect("not running" in response.text, f"terminal DELETE changed legacy error: {response.text}")


def expect_known_session_compatibility(command_id: str, background: bool, expected_label: str | None = None):
    status = wait_status(command_id)
    expect(not status.get("running", True), f"terminal status still running: {status}")
    logs = session.get(f"{BASE_URL}/command/{command_id}/logs", params={"cursor": 0}, timeout=10)
    if background:
        expect(logs.status_code == 200, f"known background logs unavailable: {logs.status_code} {logs.text}")
        if expected_label:
            expect(expected_label in logs.text, f"known background logs lost expected output {expected_label!r}: {logs.text!r}")
    else:
        expect(logs.status_code == 400, f"foreground logs must be rejected: {logs.status_code} {logs.text}")
        expect("not running in background" in logs.text, f"foreground logs error changed: {logs.text}")
    expect_terminal_delete_legacy_error(command_id)


def command_lifecycle_regression():
    commands = []
    for background in (False, True):
        for success in (True, False):
            label = f"inventory-{'bg' if background else 'fg'}-{'ok' if success else 'fail'}"
            command_id = start_command(command_for(success, label), background, success)
            status = wait_status(command_id)
            expect(not status.get("running", True), f"{label} did not become terminal: {status}")
            expected_exit = 0 if success else 17
            expect(status.get("exit_code") == expected_exit, f"{label} exit code mismatch: {status}")
            commands.append((command_id, background, label))

    listed = {entry.get("session") for entry in list_commands()}
    for command_id, background, label in commands:
        expect(command_id in listed, f"terminal command absent from inventory: {command_id}")
        expect_known_session_compatibility(command_id, background, label)


def inventory_cap_regression():
    sessions = []
    for index in range(3):
        label = f"inventory-cap-{index}"
        command_id = start_command(command_for(True, label), True, True)
        wait_status(command_id)
        sessions.append((command_id, label))
    earliest_id, earliest_label = sessions[0]
    second_id, _ = sessions[1]
    third_id, _ = sessions[2]

    def cap_evicted(entries: list[dict]) -> bool:
        listed_sessions = {entry.get("session") for entry in entries}
        return (
            earliest_id not in listed_sessions
            and second_id in listed_sessions
            and third_id in listed_sessions
        )

    listed = {
        entry.get("session")
        for entry in wait_for_inventory_condition(
            cap_evicted,
            description=(
                f"earliest terminal summary {earliest_id} was not evicted while "
                f"{second_id} and {third_id} remained visible"
            ),
        )
    }
    expect(earliest_id not in listed, f"cap did not evict earliest terminal summary: {earliest_id}")
    expect(second_id in listed, f"cap unexpectedly removed second terminal summary: {second_id}")
    expect(third_id in listed, f"cap unexpectedly removed third terminal summary: {third_id}")
    expect_known_session_compatibility(earliest_id, True, earliest_label)


def command_for_inventory_observation(label: str) -> str:
    if os.name == "nt":
        return f"echo {label} & ping -n 2 127.0.0.1 >nul"
    return f"printf '{label}\\n' && sleep 1"


def start_background_command_for_inventory_observation(command: str) -> str:
    url = f"{BASE_URL}/command"
    payload = {"command": command, "background": True}
    with session.post(url, json=payload, stream=True, timeout=15) as resp:
        expect(resp.status_code == 200, f"command start failed: {resp.status_code} {resp.text}")
        for event in iter_sse_events(resp.iter_lines()):
            if event.get("type") != "init":
                continue
            command_id = event.get("text")
            expect(command_id, "missing command id in init event")
            return command_id
    raise SystemExit("command stream ended before inventory observation barrier")


def inventory_expiry_regression():
    label = "inventory-expiry"
    command_id = start_background_command_for_inventory_observation(
        command_for_inventory_observation(label)
    )
    wait_for_inventory_condition(
        lambda entries: any(
            entry.get("session") == command_id and entry.get("running") is True
            for entry in entries
        ),
        description=f"running command summary {command_id} was not visible",
    )
    deadline = time.monotonic() + 5
    seen_terminal_summary = False
    while time.monotonic() < deadline:
        remaining = deadline - time.monotonic()
        expect(remaining > 0, f"terminal summary did not expire before 5s deadline: {command_id}")
        # Bound each request by the remaining deadline so a slow list endpoint
        # cannot extend this regression beyond its declared five seconds.
        entries = list_commands(timeout=min(10, remaining))
        if any(
            entry.get("session") == command_id and entry.get("running") is False
            for entry in entries
        ):
            seen_terminal_summary = True
        elif seen_terminal_summary and not any(
            entry.get("session") == command_id for entry in entries
        ):
            expect_known_session_compatibility(command_id, True, label)
            return
        time.sleep(0.05)
    if not seen_terminal_summary:
        raise SystemExit(
            f"terminal summary was not visible before expiry check deadline: {command_id}"
        )
    raise SystemExit(f"terminal summary did not expire before 5s deadline: {command_id}")


def wait_status(cmd_id: str, timeout: float = 15.0, poll_interval: float = 0.3) -> dict:
    url = f"{BASE_URL}/command/status/{cmd_id}"
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        r = session.get(url, timeout=5)
        expect(r.status_code == 200, f"status failed: {r.status_code} {r.text}")
        last = r.json()
        if not last.get("running", True):
            return last
        time.sleep(poll_interval)
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

    # list directory contents
    listed = session.get(f"{BASE_URL}/directories/list", params={"path": sub_dir}, timeout=10)
    expect(listed.status_code == 200, f"list directory failed: {listed.status_code} {listed.text}")
    listed_file = None
    for entry in listed.json():
        p = entry.get("path")
        if p and pathlib.Path(p).resolve() == pathlib.Path(file_path).resolve():
            listed_file = entry
            break
    expect(listed_file is not None, "directory list did not find file")
    expect(listed_file.get("type") == "file", f"directory list file type mismatch: {listed_file}")

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

    inventory_mode = os.environ.get("SMOKE_INVENTORY_MODE", "")
    if inventory_mode == "":
        command_lifecycle_regression()
        print("[+] command inventory lifecycle compatibility ok")
    elif inventory_mode == "cap":
        inventory_cap_regression()
        print("[+] command inventory cap eviction compatibility ok")
    elif inventory_mode == "expiry":
        inventory_expiry_regression()
        print("[+] command inventory TTL expiry compatibility ok")
    else:
        raise SystemExit(f"unknown SMOKE_INVENTORY_MODE: {inventory_mode}")

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
