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

from __future__ import annotations

import argparse
from datetime import timedelta

from opensandbox.config import ConnectionConfig

from opensandbox_mcp.server import create_server


def main() -> None:
    parser = argparse.ArgumentParser(
        description="OpenSandbox MCP Sandbox server entrypoint."
    )
    parser.add_argument(
        "--transport",
        choices=("stdio", "streamable-http"),
        default="stdio",
        help="Transport to use. Default uses the MCP SDK default.",
    )
    parser.add_argument(
        "--api-key",
        default=None,
        help="OpenSandbox API key (overrides OPEN_SANDBOX_API_KEY).",
    )
    parser.add_argument(
        "--domain",
        default=None,
        help="OpenSandbox API domain (overrides OPEN_SANDBOX_DOMAIN).",
    )
    parser.add_argument(
        "--protocol",
        choices=("http", "https"),
        default="http",
        help="Protocol to use for API requests.",
    )
    parser.add_argument(
        "--request-timeout-seconds",
        type=float,
        default=30,
        help="HTTP request timeout in seconds.",
    )
    parser.add_argument(
        "--use-server-proxy",
        action="store_true",
        default=False,
        help="Route sandbox traffic through the OpenSandbox server proxy (use when direct port access is blocked).",
    )

    args = parser.parse_args()
    config_values = {}
    if args.api_key:
        config_values["api_key"] = args.api_key
    if args.domain:
        config_values["domain"] = args.domain
    if args.protocol:
        config_values["protocol"] = args.protocol
    if args.request_timeout_seconds is not None:
        config_values["request_timeout"] = timedelta(
            seconds=args.request_timeout_seconds
        )
    if args.use_server_proxy:
        config_values["use_server_proxy"] = True

    connection_config = ConnectionConfig(**config_values) if config_values else None
    mcp = create_server(connection_config=connection_config)

    if args.transport == "streamable-http":
        mcp.run(
            transport="streamable-http"
        )
        return

    if args.transport == "stdio":
        mcp.run(transport="stdio")
        return

    mcp.run()


if __name__ == "__main__":
    main()
