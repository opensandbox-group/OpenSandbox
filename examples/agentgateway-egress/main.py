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

"""Verify OpenSandbox Egress Forward Mode enforced by agentgateway."""

import argparse
import asyncio
import json
import os
import shlex
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


def _env_bool(name: str, default: bool) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    return raw.strip().lower() in {"1", "true", "yes", "on"}


async def _run_python(sandbox: Sandbox, code: str) -> tuple[str, str, str | None]:
    execution = await sandbox.commands.run(f"python3 -c {shlex.quote(code)}")
    stdout = "".join(msg.text for msg in execution.logs.stdout).strip()
    stderr = "".join(msg.text for msg in execution.logs.stderr).strip()
    err_detail = str(execution.error) if execution.error is not None else None
    return stdout, stderr, err_detail


async def main(keep: bool) -> None:
    domain = os.getenv("SANDBOX_DOMAIN", "127.0.0.1:8080")
    api_key = os.getenv("SANDBOX_API_KEY", "opensandbox-demo-key")
    image = os.getenv("SANDBOX_IMAGE", "python:3.12-slim")
    use_server_proxy = _env_bool("SANDBOX_USE_SERVER_PROXY", False)
    verify_https = _env_bool("VERIFY_HTTPS", True)
    verify_http = _env_bool("VERIFY_HTTP", True)
    expected_namespace = os.getenv("EXPECTED_NAMESPACE")

    sandbox = await Sandbox.create(
        image,
        connection_config=ConnectionConfig(
            domain=domain,
            api_key=api_key,
            use_server_proxy=use_server_proxy,
            request_timeout=timedelta(minutes=2),
        ),
        ready_timeout=timedelta(minutes=2),
        timeout=timedelta(minutes=10),
        metadata={"example": "agentgateway-egress"},
    )
    print(f"Created sandbox: {sandbox.id}", flush=True)

    succeeded = False
    try:
        if verify_https:
            # 1. Allowed HTTPS passthrough (TLS stays end-to-end encrypted; routed by SNI).
            out, err, exec_err = await _run_python(
                sandbox,
                "import urllib.request; "
                "r = urllib.request.urlopen('https://example.com', timeout=15); "
                "print(r.status)",
            )
            if exec_err is not None or out != "200":
                raise RuntimeError(
                    f"Expected 200 from https://example.com, got exec_err={exec_err!r} out={out!r} err={err!r}"
                )
            print("[PASS] HTTPS passthrough allowed: https://example.com -> 200", flush=True)

            # 2. Denied HTTPS domain fails closed without falling back to direct dial.
            out, err, exec_err = await _run_python(
                sandbox,
                "import urllib.request; "
                "urllib.request.urlopen('https://example.org', timeout=10)",
            )
            if exec_err is None:
                raise RuntimeError(f"Expected https://example.org to be blocked, got out={out!r}")
            print("[PASS] HTTPS denied domain blocked: https://example.org", flush=True)

            # 3. Anti-spoofing: connecting to an arbitrary IP with an allowed SNI must
            # still dial the SNI hostname via DNS rather than the raw CONNECT IP.
            out, err, exec_err = await _run_python(
                sandbox,
                "import socket, ssl; "
                "s = socket.create_connection(('1.1.1.1', 443), timeout=15); "
                "t = ssl.create_default_context().wrap_socket(s, server_hostname='example.com'); "
                "t.sendall(b'GET / HTTP/1.1\\r\\nHost: example.com\\r\\nConnection: close\\r\\n\\r\\n'); "
                "print(t.recv(128).decode(errors='replace').splitlines()[0])",
            )
            if exec_err is not None or "200 OK" not in out:
                raise RuntimeError(
                    f"Expected SNI routing to example.com, got exec_err={exec_err!r} out={out!r} err={err!r}"
                )
            print(f"[PASS] Anti-spoofing SNI routing: 1.1.1.1:443 (SNI=example.com) -> {out}", flush=True)

        if verify_http:
            # 4. Allowed HTTP + header transformation from outer CONNECT identity headers.
            out, err, exec_err = await _run_python(
                sandbox,
                "import json, urllib.request; "
                "r = urllib.request.urlopen('http://example.com', timeout=15); "
                "print(json.dumps({k.lower(): v for k, v in r.headers.items()}))",
            )
            if exec_err is not None:
                raise RuntimeError(
                    f"Expected 200 from http://example.com, exec_err={exec_err!r} err={err!r}"
                )
            headers = json.loads(out)
            if headers.get("x-opensandbox-sandbox-id") != sandbox.id:
                raise RuntimeError(
                    f"Expected x-opensandbox-sandbox-id={sandbox.id!r}, got {headers}"
                )
            if headers.get("x-injected-by") != "agentgateway":
                raise RuntimeError(f"Expected x-injected-by='agentgateway', got {headers}")
            if expected_namespace and headers.get("x-opensandbox-sandbox-namespace") != expected_namespace:
                raise RuntimeError(
                    f"Expected x-opensandbox-sandbox-namespace={expected_namespace!r}, got {headers}"
                )
            print(
                "[PASS] HTTP allowed + CONNECT identity headers injected: "
                f"x-opensandbox-sandbox-id={headers['x-opensandbox-sandbox-id']}",
                flush=True,
            )

            # 5. Denied HTTP domain returns 404 from agentgateway.
            out, err, exec_err = await _run_python(
                sandbox,
                "import urllib.error, urllib.request\n"
                "try:\n"
                "    urllib.request.urlopen('http://example.org', timeout=10)\n"
                "    print('UNEXPECTED_200')\n"
                "except urllib.error.HTTPError as e:\n"
                "    print(f'HTTP_{e.code}')\n",
            )
            if exec_err is not None or out != "HTTP_404":
                raise RuntimeError(
                    f"Expected HTTP_404 from http://example.org, got exec_err={exec_err!r} out={out!r} err={err!r}"
                )
            print("[PASS] HTTP denied domain rejected by agentgateway: http://example.org -> 404", flush=True)

        # 6. Link-local metadata address is dropped by the egress sidecar.
        out, err, exec_err = await _run_python(
            sandbox,
            "import socket\n"
            "s = socket.socket()\n"
            "s.settimeout(2)\n"
            "try:\n"
            "    s.connect(('169.254.169.254', 80))\n"
            "    print('CONNECTED')\n"
            "except OSError as e:\n"
            "    print(f'BLOCKED:{type(e).__name__}')\n",
        )
        if exec_err is not None or not out.startswith("BLOCKED:"):
            raise RuntimeError(
                f"Expected 169.254.169.254 to be blocked, got exec_err={exec_err!r} out={out!r} err={err!r}"
            )
        print(f"[PASS] Link-local metadata address dropped: 169.254.169.254 ({out})", flush=True)

        succeeded = True
        print("All agentgateway egress checks passed.", flush=True)
    finally:
        if keep and succeeded:
            await sandbox.close()
            print(f"Sandbox {sandbox.id} retained for inspection.", flush=True)
        else:
            await sandbox.destroy()
            print(f"Sandbox {sandbox.id} destroyed.", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--keep", action="store_true", help="Keep a successful sandbox for inspection"
    )
    asyncio.run(main(parser.parse_args().keep))
