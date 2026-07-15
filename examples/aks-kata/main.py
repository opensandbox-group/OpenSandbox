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

"""
AKS Kata example — run all steps or one at a time.

Usage:
    # Run everything end to end (create → … → delete):
    python main.py all

    # Run one step at a time:
    python main.py create                          # prints SANDBOX_ID
    python main.py credentials --sandbox-id <ID>
    python main.py llm          --sandbox-id <ID>
    python main.py http         --sandbox-id <ID>
    python main.py exec         --sandbox-id <ID>
    python main.py pause        --sandbox-id <ID>
    python main.py resume       --sandbox-id <ID>
    python main.py status       --sandbox-id <ID>
    python main.py delete       --sandbox-id <ID>
"""

import argparse
import json
import os
import shlex
import time
from datetime import timedelta
from urllib.parse import urlparse

import requests
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.sandboxes import (
    Credential,
    CredentialBinding,
    CredentialProxyConfig,
    NetworkPolicy,
    NetworkRule,
    SandboxState,
)

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

DEFAULT_DOMAIN = os.getenv("SANDBOX_DOMAIN", "http://127.0.0.1:18080")
DEFAULT_API_KEY = os.getenv("SANDBOX_API_KEY", "aks-kata-demo-key")
DEFAULT_IMAGE = os.getenv(
    "SANDBOX_IMAGE",
    "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0",
)
DEFAULT_TIMEOUT_SECONDS = int(os.getenv("SANDBOX_TIMEOUT_SECONDS", "1800"))

AZURE_OPENAI_DEPLOYMENT = os.getenv("AZURE_OPENAI_DEPLOYMENT", "gpt-4o-mini")
AZURE_OPENAI_API_VERSION = os.getenv("AZURE_OPENAI_API_VERSION", "2024-12-01-preview") 

# Shared connection config.  request_timeout is raised because Kata VM
# sandbox creation involves image pulls that can take well over the default
# 30 s.  use_server_proxy is left at the default (False) — the SDK reaches
# the execd and egress sidecars through the ingress gateway port-forward
# rather than through the server's internal proxy.
CONNECTION_CONFIG = ConnectionConfigSync(
    domain=DEFAULT_DOMAIN,
    api_key=DEFAULT_API_KEY,
    request_timeout=timedelta(seconds=180),
)

HTTP_PORT = int(os.getenv("SANDBOX_HTTP_PORT", "8080"))
HTTP_ROOT = "/tmp/www"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _required_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


def _print_execution(prefix: str, execution) -> None:
    print(f"[{prefix}] exit_code={execution.exit_code}")
    for message in execution.logs.stdout:
        print(f"[{prefix}][stdout] {message.text}")
    for message in execution.logs.stderr:
        print(f"[{prefix}][stderr] {message.text}")
    if execution.error:
        print(f"[{prefix}][error] {execution.error.name}: {execution.error.value}")


def _wait_http_ready(base_url: str, headers: dict, attempts: int = 60) -> bool:
    for _ in range(attempts):
        try:
            resp = requests.get(base_url, headers=headers, timeout=2)
            if resp.status_code < 500:
                return True
        except Exception:
            pass
        time.sleep(1)
    return False


def _http_get(base_url: str, path: str, headers: dict) -> requests.Response:
    if not path.startswith("/"):
        path = "/" + path
    url = base_url.rstrip("/") + path
    resp = requests.get(url, headers=headers, timeout=10)
    print(f"[http] GET {path} -> {resp.status_code} ({len(resp.content)} bytes)")
    return resp


def _wait_sandbox_ready(sandbox, timeout_seconds: int = 300) -> None:
    start = time.time()
    while time.time() - start < timeout_seconds:
        try:
            status = sandbox.get_info().status
        except Exception:
            # Transient API error while the sandbox is starting; retry.
            time.sleep(3)
            continue

        if status.state == SandboxState.RUNNING:
            # Let the execd / egress sidecars finish initializing.
            time.sleep(15)
            return
        if status.state == SandboxState.FAILED:
            detail = status.message or status.reason or "no detail provided"
            raise RuntimeError(f"Sandbox failed to start: {detail}")
        print(f"[setup] sandbox state: {status.state}, waiting...")
        time.sleep(3)

    raise RuntimeError(
        f"Sandbox did not reach Running state within {timeout_seconds}s"
    )


def _retry(fn, description: str, attempts: int = 10, delay: int = 5):
    last_exc = None
    for i in range(attempts):
        try:
            return fn()
        except Exception as exc:
            last_exc = exc
            if i < attempts - 1:
                print(f"[retry] {description} attempt {i + 1} failed, retrying in {delay}s...")
                time.sleep(delay)
    raise RuntimeError(f"{description} failed after {attempts} attempts: {last_exc}") from last_exc


def _connect(sandbox_id: str) -> SandboxSync:
    """Connect to an existing sandbox by ID."""
    return SandboxSync.connect(
        sandbox_id,
        connection_config=CONNECTION_CONFIG,
        skip_health_check=True,
    )


def _ingress(sandbox) -> tuple:
    """Return (base_url, headers) for the sandbox HTTP server via the ingress gateway."""
    endpoint = sandbox.get_endpoint(HTTP_PORT)
    base_url = f"http://{endpoint.endpoint}"
    headers = dict(endpoint.headers or {})
    return base_url, headers


# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------


def step_create() -> str:
    """Create a Kata-isolated sandbox and return its ID."""
    azure_endpoint = _required_env("AZURE_OPENAI_ENDPOINT").rstrip("/")
    azure_host = urlparse(azure_endpoint).hostname
    if not azure_host:
        raise RuntimeError("AZURE_OPENAI_ENDPOINT must be a full URL")

    print("Creating Kata-isolated sandbox on AKS...")
    print(f"  OpenSandbox API: {DEFAULT_DOMAIN}")
    print(f"  Sandbox image:   {DEFAULT_IMAGE}")

    sandbox = SandboxSync.create(
        image=DEFAULT_IMAGE,
        timeout=timedelta(seconds=DEFAULT_TIMEOUT_SECONDS),
        metadata={"example": "aks-kata", "runtime": "kata"},
        entrypoint=[
            "bash", "-lc",
            f"mkdir -p {HTTP_ROOT} && cd {HTTP_ROOT} && "
            f"exec python3 -m http.server {HTTP_PORT} --bind 0.0.0.0",
        ],
        connection_config=CONNECTION_CONFIG,
        skip_health_check=True,
        env={
            "AZURE_OPENAI_API_KEY": "fake-key-inside-sandbox",
            "AZURE_OPENAI_ENDPOINT": azure_endpoint,
            "AZURE_OPENAI_DEPLOYMENT": AZURE_OPENAI_DEPLOYMENT,
            "AZURE_OPENAI_API_VERSION": AZURE_OPENAI_API_VERSION,
            "IS_SANDBOX": "1",
        },
        network_policy=NetworkPolicy(
            defaultAction="deny",
            egress=[
                NetworkRule(action="allow", target=azure_host),
                NetworkRule(action="allow", target="pypi.org"),
                NetworkRule(action="allow", target="files.pythonhosted.org"),
            ],
        ),
        credential_proxy=CredentialProxyConfig(enabled=True),
        secure_access=True,
    )

    # Print the ID before waiting so the sandbox can still be identified and
    # deleted even if readiness polling times out (it already has a TTL here).
    print(f"\nSANDBOX_ID={sandbox.id}")
    print(f"Use --sandbox-id {sandbox.id} for subsequent steps.\n")

    _wait_sandbox_ready(sandbox)
    return sandbox.id


def step_credentials(sandbox_id: str) -> None:
    """Store the Azure OpenAI key in Credential Vault."""
    azure_endpoint = _required_env("AZURE_OPENAI_ENDPOINT").rstrip("/")
    azure_api_key = _required_env("AZURE_OPENAI_API_KEY")
    azure_host = urlparse(azure_endpoint).hostname
    if not azure_host:
        raise RuntimeError(
            "AZURE_OPENAI_ENDPOINT must be a full URL, e.g. "
            "https://my-resource.openai.azure.com"
        )

    sandbox = _connect(sandbox_id)

    def _create():
        sandbox.credential_vault.create(
            credentials=[
                Credential(name="azure-openai-key", source={"value": azure_api_key})
            ],
            bindings=[
                CredentialBinding(
                    name="azure-openai",
                    match={
                        "schemes": ["https"], "ports": [443],
                        "hosts": [azure_host],
                        "methods": ["GET", "POST"],
                        "paths": ["/openai/*"],
                    },
                    auth={
                        "type": "apiKey", "name": "api-key",
                        "credential": "azure-openai-key",
                    },
                )
            ],
        )

    _retry(_create, "credential vault setup")
    print("[credentials] Credential Vault configured.")


def step_llm(sandbox_id: str, question: str | None = None) -> None:
    """Call Azure OpenAI from inside the sandbox via Credential Vault."""
    sandbox = _connect(sandbox_id)

    prompt = question or "Compute 1+1. Reply with only the number."

    chat_url = (
        "$AZURE_OPENAI_ENDPOINT/openai/deployments/$AZURE_OPENAI_DEPLOYMENT"
        "/chat/completions?api-version=$AZURE_OPENAI_API_VERSION"
    )
    chat_payload = json.dumps({
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": 256,
    })
    # The `api-key` header is intentionally NOT set here: Credential Vault
    # injects the real key at the egress sidecar, so the command stays
    # secret-free. shlex.quote keeps the JSON payload safe even when the
    # prompt contains apostrophes, while the $AZURE_OPENAI_* variables stay
    # unquoted so the in-sandbox shell still expands them.
    chat_cmd = (
        f'curl -sS -X POST "{chat_url}" '
        '-H "Content-Type: application/json" '
        f"-d {shlex.quote(chat_payload)}"
    )
    exec_result = sandbox.commands.run(chat_cmd)

    # Parse and print just the answer if possible
    if exec_result.exit_code == 0 and exec_result.logs.stdout:
        # Command output can be split across multiple log messages, so join
        # them all before parsing the JSON response.
        raw = "".join(msg.text for msg in exec_result.logs.stdout)
        try:
            resp = json.loads(raw)
            answer = resp["choices"][0]["message"]["content"]
            model = resp.get("model", "unknown")
            print(f"[llm] Question: {prompt}")
            print(f"[llm] Model:    {model}")
            print(f"[llm] Answer:   {answer}")
        except (json.JSONDecodeError, KeyError, IndexError):
            # Couldn't parse — show raw output
            _print_execution("azure-openai", exec_result)
    else:
        _print_execution("azure-openai", exec_result)


def step_http(sandbox_id: str, path: str | None = None) -> None:
    """HTTP client operations against the sandbox HTTP server via the ingress gateway.

    With --path: fetch a single path.  Without: run the full demo (publish files, fetch, 404).
    """
    sandbox = _connect(sandbox_id)
    base_url, headers = _ingress(sandbox)

    print(f"Ingress endpoint: {base_url}")
    for k, v in headers.items():
        print(f"  {k}: {v}")

    if not _wait_http_ready(base_url, headers):
        raise RuntimeError("Sandbox HTTP server did not become reachable.")

    if path is not None:
        resp = _http_get(base_url, path, headers)
        print(resp.text)
        return

    # Full demo
    _http_get(base_url, "/", headers)

    sandbox.files.write_file(
        f"{HTTP_ROOT}/index.html",
        "<!doctype html><html><head><title>aks-kata</title></head>"
        "<body><h1>Hello from a Kata-isolated sandbox</h1></body></html>",
        mode=644,
    )
    sandbox.files.write_file(
        f"{HTTP_ROOT}/data.json",
        json.dumps({"example": "aks-kata", "runtime": "kata", "port": HTTP_PORT}),
        mode=644,
    )

    listing = _http_get(base_url, "/", headers)
    if "index.html" in listing.text:
        print("[http] directory listing includes index.html")

    html_resp = _http_get(base_url, "/index.html", headers)
    print(f"[http] index.html first line: {html_resp.text.splitlines()[0][:60]}")

    json_resp = _http_get(base_url, "/data.json", headers)
    print(f"[http] data.json parsed: {json_resp.json()}")

    _http_get(base_url, "/missing.txt", headers)


def step_exec(sandbox_id: str, command: str | None = None) -> None:
    """Execute a command in the sandbox.

    With -c: run the given command.  Without: run the full demo (write file, uname, delete).
    """
    sandbox = _connect(sandbox_id)

    if command is not None:
        _print_execution("exec", sandbox.commands.run(command))
        return

    # Full demo
    base_url, headers = _ingress(sandbox)
    if not _wait_http_ready(base_url, headers):
        raise RuntimeError("Sandbox HTTP server did not become reachable.")

    sandbox.commands.run(f"echo 'generated in sandbox' > {HTTP_ROOT}/exec.txt")
    _http_get(base_url, "/exec.txt", headers)

    _print_execution("uname", sandbox.commands.run("uname -a"))

    sandbox.files.delete_files([f"{HTTP_ROOT}/data.json"])
    _http_get(base_url, "/data.json", headers)


def step_pause(sandbox_id: str) -> None:
    """Pause the sandbox (snapshot + push to registry)."""
    sandbox = _connect(sandbox_id)
    print("[lifecycle] pausing sandbox...")
    sandbox.pause()

    # Poll for PAUSED, but abort as soon as the controller reports FAILED
    # (e.g. missing controller.snapshot.registry or a bad push secret) so the
    # failure surfaces instead of silently exiting 0 after the timeout.
    for _ in range(300):
        try:
            info = sandbox.get_info()
        except Exception:
            time.sleep(1)
            continue
        state = info.status.state
        if state == SandboxState.PAUSED:
            print("[lifecycle] sandbox is PAUSED")
            return
        if state == SandboxState.FAILED:
            message = info.status.message or "no detail provided"
            raise RuntimeError(f"Pause failed: {message}")
        time.sleep(1)

    print("[lifecycle] pause requested (state not confirmed PAUSED)")


def step_resume(sandbox_id: str) -> str:
    """Resume a paused sandbox and return its ID (stable across pause/resume)."""
    print("[lifecycle] resuming sandbox...")
    resumed = SandboxSync.resume(
        sandbox_id,
        connection_config=CONNECTION_CONFIG,
        skip_health_check=True,
    )
    _wait_sandbox_ready(resumed)
    print(f"[lifecycle] state after resume: {resumed.get_info().status.state}")

    base_url, headers = _ingress(resumed)
    if _wait_http_ready(base_url, headers):
        _http_get(base_url, "/index.html", headers)

    return resumed.id


def step_status(sandbox_id: str) -> None:
    """Print the current sandbox state."""
    sandbox = _connect(sandbox_id)
    info = sandbox.get_info()
    print(f"Sandbox:  {sandbox_id}")
    print(f"State:    {info.status.state}")
    if info.image:
        print(f"Image:    {info.image}")
    if info.snapshot_id:
        print(f"Snapshot: {info.snapshot_id}")


def step_delete(sandbox_id: str) -> None:
    """Delete (terminate) the sandbox."""
    sandbox = _connect(sandbox_id)
    sandbox.kill()
    print(f"[lifecycle] sandbox {sandbox_id} deleted.")


# ---------------------------------------------------------------------------
# "all" — original end-to-end flow
# ---------------------------------------------------------------------------


def run_all() -> None:
    """Run every step end to end, then delete the sandbox."""
    sandbox_id = step_create()
    sandbox = _connect(sandbox_id)
    active_sandbox = sandbox

    try:
        step_credentials(sandbox_id)
        step_llm(sandbox_id)
        step_http(sandbox_id)
        step_exec(sandbox_id)

        try:
            step_pause(sandbox_id)
            step_resume(sandbox_id)
        except Exception as exc:  # noqa: BLE001
            print(f"[lifecycle] pause/resume skipped: {exc}")

    finally:
        print("[lifecycle] deleting sandbox...")
        active_sandbox.kill()


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def main() -> None:
    parser = argparse.ArgumentParser(
        description="AKS Kata example — run all steps or one at a time.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
steps:
  all          Run everything end to end (create → … → delete)
  create       Create a Kata-isolated sandbox, print its ID
  credentials  Store Azure OpenAI key in Credential Vault
  llm          Call Azure OpenAI from inside the sandbox
  http         HTTP operations via the ingress gateway
  exec         Run commands, delete files
  pause        Pause the sandbox (snapshot to registry)
  resume       Resume a paused sandbox
  status       Print current sandbox state
  delete       Delete the sandbox

example — step by step:
  export AZURE_OPENAI_ENDPOINT=https://my.openai.azure.com
  export AZURE_OPENAI_API_KEY=sk-...

  python main.py create
  # prints SANDBOX_ID=abc-123

  python main.py credentials --sandbox-id abc-123
  python main.py llm         --sandbox-id abc-123
  python main.py llm         --sandbox-id abc-123 -q "What is Kubernetes?"
  python main.py http        --sandbox-id abc-123
  python main.py http        --sandbox-id abc-123 -p /index.html
  python main.py exec        --sandbox-id abc-123
  python main.py exec        --sandbox-id abc-123 -c "uname -a"
  python main.py pause       --sandbox-id abc-123
  python main.py resume      --sandbox-id abc-123
  python main.py status      --sandbox-id abc-123
  python main.py delete      --sandbox-id abc-123
""",
    )
    parser.add_argument(
        "step",
        choices=[
            "all", "create", "credentials", "llm", "http",
            "exec", "pause", "resume", "status", "delete",
        ],
        help="Which step to run.",
    )
    parser.add_argument(
        "--sandbox-id",
        help="Sandbox ID (required for all steps except 'create' and 'all').",
    )
    parser.add_argument(
        "-q", "--question",
        help="Question to ask the LLM (used with the 'llm' step). "
             "Defaults to 'Compute 1+1. Reply with only the number.'",
    )
    parser.add_argument(
        "-c", "--command",
        help="Shell command to run inside the sandbox (used with the 'exec' step). "
             "Without this flag, 'exec' runs the built-in demo.",
    )
    parser.add_argument(
        "-p", "--path",
        help="URL path to fetch from the sandbox HTTP server (used with the 'http' step). "
             "Without this flag, 'http' runs the built-in demo.",
    )
    args = parser.parse_args()

    if args.step == "all":
        run_all()
        return

    if args.step == "create":
        step_create()
        return

    # All other steps require --sandbox-id
    if not args.sandbox_id:
        parser.error(f"--sandbox-id is required for the '{args.step}' step")

    dispatch = {
        "credentials": lambda sid: step_credentials(sid),
        "llm": lambda sid: step_llm(sid, question=args.question),
        "http": lambda sid: step_http(sid, path=args.path),
        "exec": lambda sid: step_exec(sid, command=args.command),
        "pause": lambda sid: step_pause(sid),
        "resume": lambda sid: step_resume(sid),
        "status": lambda sid: step_status(sid),
        "delete": lambda sid: step_delete(sid),
    }
    dispatch[args.step](args.sandbox_id)


if __name__ == "__main__":
    main()
