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

import asyncio
import os
from datetime import timedelta

from novnc_url import (
    browser_proxy_auth_warning,
    build_novnc_url,
    normalize_domain,
    parse_bool,
    resolve_api_key,
    resolve_novnc_protocol,
    resolve_protocol,
    validate_connection_mode,
)
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models.execd import RunCommandOpts


def _required_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


def _bool_env(name: str, default: bool = False) -> bool:
    value = os.getenv(name)
    if value is None:
        return default
    return parse_bool(value, name)


async def _print_logs(label: str, execution) -> None:
    for msg in execution.logs.stdout:
        print(f"[{label} stdout] {msg.text}")
    for msg in execution.logs.stderr:
        print(f"[{label} stderr] {msg.text}")
    if execution.error:
        print(f"[{label} error] {execution.error.name}: {execution.error.value}")


async def main() -> None:
    domain = normalize_domain(os.getenv("SANDBOX_DOMAIN", "localhost:8080"))
    protocol = resolve_protocol(domain, os.getenv("SANDBOX_PROTOCOL"))
    use_server_proxy = _bool_env("SANDBOX_USE_SERVER_PROXY")
    validate_connection_mode(protocol, use_server_proxy)
    api_key = resolve_api_key(
        os.getenv("SANDBOX_API_KEY"),
        os.getenv("OPEN_SANDBOX_API_KEY"),
    )
    image = os.getenv(
        "SANDBOX_IMAGE",
        "opensandbox/desktop:latest",
    )
    python_version = os.getenv("PYTHON_VERSION", "3.11")
    vnc_password = _required_env("VNC_PASSWORD")

    config = ConnectionConfig(
        domain=domain,
        protocol=protocol,
        api_key=api_key,
        request_timeout=timedelta(seconds=60),
        use_server_proxy=use_server_proxy,
    )

    sandbox = await Sandbox.create(
        image,
        connection_config=config,
        env={
            "PYTHON_VERSION": python_version,
            "VNC_PASSWORD": vnc_password,
        },
    )

    async with sandbox:
        # Desktop and VNC components are pre-installed in the image, just start them
        # Start virtual display, window manager, and VNC server (in background)
        xvfb_exec = await sandbox.commands.run(
            "Xvfb :0 -screen 0 1280x800x24",
            opts=RunCommandOpts(background=True),
        )
        await _print_logs("xvfb", xvfb_exec)

        # Start XFCE session (provides panel, file manager, terminal)
        xfce_exec = await sandbox.commands.run(
            "DISPLAY=:0 dbus-launch startxfce4",
            opts=RunCommandOpts(background=True),
        )
        await _print_logs("xfce", xfce_exec)

        vnc_exec = await sandbox.commands.run(
            'x11vnc -display :0 -passwd "$VNC_PASSWORD" -forever -shared -rfbport 5900',
            opts=RunCommandOpts(background=True),
        )
        await _print_logs("x11vnc", vnc_exec)

        # Start noVNC/websockify to expose VNC over WebSocket/HTTP
        novnc_exec = await sandbox.commands.run(
            "/usr/bin/websockify --web=/usr/share/novnc 6080 localhost:5900",
            opts=RunCommandOpts(background=True),
        )
        await _print_logs("novnc", novnc_exec)

        endpoint_novnc = await sandbox.get_endpoint(6080)

        # noVNC uses the page's scheme to select ws:// or wss://. Only server
        # proxy endpoints inherit the management API's TLS origin; direct
        # websockify endpoints do not terminate TLS.
        novnc_protocol = resolve_novnc_protocol(config.protocol, use_server_proxy)
        novnc_url = build_novnc_url(endpoint_novnc.endpoint, novnc_protocol)
        auth_warning = browser_proxy_auth_warning(use_server_proxy, api_key)

        if not use_server_proxy:
            endpoint_vnc = await sandbox.get_endpoint(5900)
            print("\nVNC endpoint (native clients):")
            print(f"  {endpoint_vnc.endpoint}")
            print(f"Password: {vnc_password}")

        print("\nnoVNC (browser):")
        print(f"  {novnc_url}")
        print(f"Password: {vnc_password}")
        if auth_warning:
            print(f"Authentication note: {auth_warning}")

        print("\nKeeping sandbox alive for 5 minutes. Press Ctrl+C to exit sooner.")
        try:
            await asyncio.sleep(300)
        except KeyboardInterrupt:
            print("Stopping...")
        finally:
            await sandbox.kill()


if __name__ == "__main__":
    asyncio.run(main())
