#!/usr/bin/env python3

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

"""
Smoke tests for the execd runtime-init API (POST /internal/init, GET /ready).

Prerequisites:
- execd server running locally with the mode under test
- Environment:
    MODE          "gated" (EXECD_RUNTIME_INIT=1) or "legacy" (fallback path)
    BASE_URL      e.g. http://localhost:44773
    LEGACY_TOKEN  value of the server's EXECD_ACCESS_TOKEN
    NEW_TOKEN     raw token whose sha256 the /internal/init payload delivers
"""

import hashlib
import json
import os
import sys
import time
import uuid

import requests

BASE_URL = os.environ.get("BASE_URL", "http://localhost:44773").rstrip("/")
MODE = os.environ.get("MODE", "gated")
LEGACY_TOKEN = os.environ.get("LEGACY_TOKEN", "")
NEW_TOKEN = os.environ.get("NEW_TOKEN", "runtime-init-smoke-new-token")
NEW_TOKEN_HASH = "sha256:" + hashlib.sha256(NEW_TOKEN.encode()).hexdigest()
SANDBOX_ID = "sandbox-smoke-" + uuid.uuid4().hex[:8]
GENERATION = 7

INIT_HEADER = "X-EXECD-ACCESS-TOKEN"


def expect(cond: bool, msg: str):
    if not cond:
        raise SystemExit(f"FAIL ({MODE}): {msg}")


def auth(token: str) -> dict:
    return {INIT_HEADER: token} if token else {}


def wait_ping(timeout: float = 30.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            r = requests.get(f"{BASE_URL}/ping", timeout=2)
            if r.status_code == 200:
                return
        except requests.RequestException:
            pass
        time.sleep(0.2)
    raise SystemExit(f"FAIL ({MODE}): /ping never became ready")


def init_payload(**overrides) -> dict:
    payload = {
        "sandboxId": SANDBOX_ID,
        "generation": GENERATION,
        "accessTokenHash": NEW_TOKEN_HASH,
        "envs": {"SMOKE_INIT_VAR": "bound-by-init"},
    }
    payload.update(overrides)
    return payload


def run_background_command(token: str, command: str) -> str:
    headers = auth(token)
    cmd_id = ""
    with requests.post(
        f"{BASE_URL}/command",
        json={"command": command, "background": True},
        headers=headers,
        stream=True,
        timeout=10,
    ) as resp:
        expect(resp.status_code == 200, f"/command failed: {resp.status_code} {resp.text}")
        for line in resp.iter_lines():
            if not line:
                continue
            try:
                if line.startswith(b"data:"):
                    data = json.loads(line[len(b"data:"):].decode())
                else:
                    data = json.loads(line.decode())
            except Exception:
                continue
            if data.get("type") == "init":
                cmd_id = data.get("text") or ""
                break
    expect(cmd_id, "missing command id in init event")

    deadline = time.time() + 15
    while time.time() < deadline:
        s = requests.get(
            f"{BASE_URL}/command/status/{cmd_id}", headers=headers, timeout=5
        )
        expect(s.status_code == 200, f"/command/status failed: {s.status_code}")
        if not s.json().get("running", True):
            break
        time.sleep(0.2)

    logs = requests.get(
        f"{BASE_URL}/command/{cmd_id}/logs", headers=headers, timeout=10
    )
    expect(logs.status_code == 200, f"/command logs failed: {logs.status_code}")
    return logs.text


def smoke_gated():
    # Liveness up, readiness down, business APIs gated.
    wait_ping()
    r = requests.get(f"{BASE_URL}/ready", timeout=5)
    expect(r.status_code == 503, f"/ready expected 503, got {r.status_code}")
    expect(r.json().get("initialized") is False, "ready body must be uninitialized")

    r = requests.get(f"{BASE_URL}/metrics", timeout=5)
    expect(r.status_code == 503, f"gated /metrics expected 503, got {r.status_code}")
    r = requests.get(
        f"{BASE_URL}/metrics", headers={INIT_HEADER: LEGACY_TOKEN}, timeout=5
    )
    expect(
        r.status_code == 503,
        f"gated /metrics with legacy token expected 503, got {r.status_code}",
    )

    # A malformed /internal/init must not consume the one-shot slot.
    r = requests.post(f"{BASE_URL}/internal/init", json={"sandboxId": "x", "generation": 0}, timeout=5)
    expect(r.status_code == 400, f"invalid /internal/init expected 400, got {r.status_code}")

    # The valid call initializes.
    r = requests.post(f"{BASE_URL}/internal/init", json=init_payload(), timeout=30)
    expect(r.status_code == 200, f"/internal/init failed: {r.status_code} {r.text}")
    body = r.json()
    expect(body.get("status") == "initialized", f"unexpected init body: {body}")

    r = requests.get(f"{BASE_URL}/ready", timeout=5)
    expect(r.status_code == 200, f"/ready expected 200, got {r.status_code}")
    ready = r.json()
    expect(ready.get("initialized") is True, "ready must be initialized")
    expect(ready.get("sandboxId") == SANDBOX_ID, f"ready sandboxId mismatch: {ready}")
    expect(ready.get("generation") == GENERATION, f"ready generation mismatch: {ready}")

    # Token rotation: the binding hash replaced the legacy container token.
    r = requests.get(f"{BASE_URL}/metrics", timeout=5)
    expect(r.status_code == 401, f"unauthenticated /metrics expected 401, got {r.status_code}")
    r = requests.get(f"{BASE_URL}/metrics", headers=auth(LEGACY_TOKEN), timeout=5)
    expect(r.status_code == 401, f"legacy-token /metrics expected 401, got {r.status_code}")
    r = requests.get(f"{BASE_URL}/metrics", headers=auth(NEW_TOKEN), timeout=5)
    expect(r.status_code == 200, f"new-token /metrics expected 200, got {r.status_code}")

    # /internal/init envs must reach user processes.
    out = run_background_command(NEW_TOKEN, 'printf %s "$SMOKE_INIT_VAR"')
    expect("bound-by-init" in out, f"/internal/init envs did not reach the command: {out!r}")

    # Strictly one-shot: identical and different retries all conflict.
    r = requests.post(f"{BASE_URL}/internal/init", json=init_payload(), timeout=5)
    expect(r.status_code == 409, f"identical retry expected 409, got {r.status_code}")
    expect(r.json().get("code") == "ALREADY_INITIALIZED", f"unexpected 409 body: {r.text}")
    r = requests.post(
        f"{BASE_URL}/internal/init", json=init_payload(sandboxId="sandbox-other", generation=9), timeout=5
    )
    expect(r.status_code == 409, f"different-identity retry expected 409, got {r.status_code}")


def smoke_legacy():
    # Legacy fallback: ready once the template-driven startup completes.
    wait_ping()
    deadline = time.time() + 30
    while True:
        r = requests.get(f"{BASE_URL}/ready", timeout=5)
        if r.status_code == 200:
            break
        expect(r.status_code == 503, f"/ready unexpected status {r.status_code}")
        if time.time() > deadline:
            raise SystemExit("FAIL (legacy): /ready never became ready")
        time.sleep(0.2)
    expect(r.json().get("initialized") is True, "legacy ready must be initialized")

    # Legacy auth still active before /internal/init.
    r = requests.get(f"{BASE_URL}/metrics", timeout=5)
    expect(r.status_code == 401, f"unauthenticated /metrics expected 401, got {r.status_code}")
    r = requests.get(f"{BASE_URL}/metrics", headers=auth(LEGACY_TOKEN), timeout=5)
    expect(r.status_code == 200, f"legacy-token /metrics expected 200, got {r.status_code}")

    # A late /internal/init is accepted and becomes authoritative.
    r = requests.post(f"{BASE_URL}/internal/init", json=init_payload(), timeout=30)
    expect(r.status_code == 200, f"/internal/init failed: {r.status_code} {r.text}")

    r = requests.get(f"{BASE_URL}/metrics", headers=auth(LEGACY_TOKEN), timeout=5)
    expect(r.status_code == 401, f"legacy-token /metrics expected 401 after init, got {r.status_code}")
    r = requests.get(f"{BASE_URL}/metrics", headers=auth(NEW_TOKEN), timeout=5)
    expect(r.status_code == 200, f"new-token /metrics expected 200 after init, got {r.status_code}")

    # Still strictly one-shot.
    r = requests.post(f"{BASE_URL}/internal/init", json=init_payload(), timeout=5)
    expect(r.status_code == 409, f"second /internal/init expected 409, got {r.status_code}")


def main():
    expect(MODE in ("gated", "legacy"), f"unknown MODE {MODE!r}")
    if MODE == "gated":
        smoke_gated()
    else:
        smoke_legacy()
    print(f"runtime-init smoke ({MODE}) OK")


if __name__ == "__main__":
    main()
