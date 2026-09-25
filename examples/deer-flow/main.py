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

"""DeerFlow + OpenSandbox example.

Drives DeerFlow's own OpenSandbox sandbox provider -- the backend selected by
``sandbox.use: deerflow.community.opensandbox:OpenSandboxProvider`` in DeerFlow
2.1.0 and later -- against an OpenSandbox server. The provider is used directly
so the sandbox contract can be exercised without starting the DeerFlow gateway
or configuring a model.

Prerequisites:
    DeerFlow 2.1.0 or later checked out with the provider extra. The harness is
    not published on PyPI, so it is installed from the checkout:

        git clone https://github.com/bytedance/deer-flow.git
        cd deer-flow/backend && uv sync --all-packages --extra opensandbox

    Run this script with that environment, from the OpenSandbox checkout:

        uv run --project ../deer-flow/backend python examples/deer-flow/main.py

Environment:
    SANDBOX_DOMAIN     OpenSandbox server address. Defaults to "localhost:8080".
    SANDBOX_PROTOCOL   "http" (default) or "https".
    SANDBOX_API_KEY    API key, if the server requires authentication.
    SANDBOX_IMAGE      Sandbox image. Defaults to DeerFlow's "python:3.11".
"""

import os
import tempfile
from pathlib import Path

from deerflow.community.opensandbox import OpenSandboxProvider

DEFAULT_IMAGE = "python:3.11"
WORKSPACE = "/mnt/user-data/workspace"
FIB_SCRIPT = f"{WORKSPACE}/fib.py"
FIB_SOURCE = """\
def fibonacci(count):
    first, second = 0, 1
    for _ in range(count):
        yield first
        first, second = second, first + second


print(list(fibonacci(10)))
"""
FIB_EXPECTED = "[0, 1, 1, 2, 3, 5, 8, 13, 21, 34]"

# DeerFlow's AppConfig requires only the `sandbox` section -- every other
# section defaults -- so a provider-only run needs no model or gateway settings.
# `ready_timeout` covers a first-run image pull. `idle_timeout: 0` keeps the
# reaper out of the way; this script destroys the sandbox via shutdown().
CONFIG_TEMPLATE = """\
sandbox:
  use: deerflow.community.opensandbox:OpenSandboxProvider
  image: {image}
  domain: {domain}
  protocol: {protocol}
  ready_timeout: 120
  sandbox_timeout: 600
  idle_timeout: 0
"""


def build_config() -> str:
    """Render the DeerFlow config that points its sandbox provider at the server."""
    api_key = os.getenv("SANDBOX_API_KEY")
    if api_key:
        # The SDK reads OPEN_SANDBOX_API_KEY when the config carries no key, so
        # the example never writes the secret to disk.
        os.environ["OPEN_SANDBOX_API_KEY"] = api_key

    return CONFIG_TEMPLATE.format(
        image=os.getenv("SANDBOX_IMAGE", DEFAULT_IMAGE),
        domain=os.getenv("SANDBOX_DOMAIN", "localhost:8080"),
        protocol=os.getenv("SANDBOX_PROTOCOL", "http"),
    )


def exercise_sandbox(sandbox) -> None:
    """Walk the DeerFlow sandbox surface the agent's tools call into."""
    print(f"[create] DeerFlow sandbox {sandbox.id} bound to remote {sandbox.remote_id}")

    platform_info = sandbox.execute_command("python3 -c 'import platform; print(platform.platform())'")
    print(f"[command] {platform_info.strip()}")

    sandbox.write_file(FIB_SCRIPT, FIB_SOURCE)
    print(f"[file] Wrote {FIB_SCRIPT}")

    output = sandbox.execute_command(f"python3 {FIB_SCRIPT}")
    print(f"[command] {output.strip()}")
    if FIB_EXPECTED not in output:
        raise RuntimeError(f"Unexpected output from {FIB_SCRIPT}: {output!r}")

    print(f"[file] Read back: {sandbox.read_file(FIB_SCRIPT, start_line=1, end_line=1).strip()}")
    print(f"[list_dir] {sandbox.list_dir(WORKSPACE)}")

    matches, _ = sandbox.glob("/mnt/user-data", "*.py")
    print(f"[glob] {matches}")

    hits, _ = sandbox.grep(WORKSPACE, "fibonacci", literal=True)
    if not hits:
        raise RuntimeError("grep found no match for 'fibonacci' in the sandbox")
    print(f"[grep] {hits[0].path}:{hits[0].line_number}: {hits[0].line.strip()}")

    # Artifact downloads are restricted to /mnt/user-data.
    download = sandbox.download_file(FIB_SCRIPT)
    print(f"[download] {len(download)} bytes fetched from {FIB_SCRIPT}")


def main() -> None:
    config = build_config()

    with tempfile.TemporaryDirectory(prefix="opensandbox-deerflow-") as tmp:
        config_path = Path(tmp) / "config.yaml"
        config_path.write_text(config, encoding="utf-8")
        # The provider reads this when it is constructed, below.
        os.environ["DEER_FLOW_CONFIG_PATH"] = str(config_path)

        print(f"[config] DeerFlow config: {config_path}")
        provider = OpenSandboxProvider()
        try:
            sandbox_id = provider.acquire(thread_id="opensandbox-example", user_id="example-user")
            sandbox = provider.get(sandbox_id)
            if sandbox is None:
                raise RuntimeError(f"DeerFlow returned no sandbox for scope {sandbox_id}")
            try:
                exercise_sandbox(sandbox)
            finally:
                # release() parks the sandbox in the warm pool; shutdown()
                # destroys it. Releasing first mirrors a completed agent turn.
                provider.release(sandbox_id)
        finally:
            provider.shutdown()

    print("[cleanup] Sandbox destroyed")
    print(f"\nDeerFlow config used for this run:\n\n{config}")
    print("Run a DeerFlow agent turn whose tools execute in the sandbox with:")
    print("  deerflow --print \"Run python3 -c 'print(2 ** 16)' and report the output\"")


if __name__ == "__main__":
    main()
