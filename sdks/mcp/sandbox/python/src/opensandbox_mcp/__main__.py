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

from __future__ import annotations

import argparse
import sys
from datetime import timedelta

from opensandbox.config import ConnectionConfig
from starlette.applications import Starlette

from opensandbox_mcp.auth import OAuthConfigError, load_oauth_config
from opensandbox_mcp.server import create_server, create_streamable_http_app


def _serve_streamable_http_app(
    app: Starlette, *, host: str, port: int, log_level: str
) -> None:  # pragma: no cover
    import uvicorn

    uvicorn.run(app, host=host, port=port, log_level=log_level.lower())


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
        "--host",
        default="127.0.0.1",
        help="HTTP bind host for streamable-http transport.",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=8000,
        help="HTTP bind port for streamable-http transport.",
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
        help=(
            "Deprecated: the server-proxied data path is in maintenance mode "
            "(security and bug fixes only); route data-plane traffic through the "
            "ingress gateway instead. Routes sandbox traffic through the "
            "OpenSandbox server proxy (use when direct port access is blocked)."
        ),
    )

    oauth_group = parser.add_argument_group(
        "OAuth 2.1 Resource Server (streamable-http only)"
    )
    oauth_group.add_argument(
        "--oauth-mode",
        default=None,
        help=(
            "OAuth Resource Server mode: 'none' (default) or 'jwt' "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_MODE)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-issuer-url",
        default=None,
        help=(
            "Trusted external Authorization Server issuer URL "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_ISSUER_URL)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-resource-url",
        default=None,
        help=(
            "Canonical public URL of this MCP endpoint for RFC 9728/8707 "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_RESOURCE_URL)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-audience",
        default=None,
        help=(
            "Expected JWT aud claim; defaults to --oauth-resource-url "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_AUDIENCE)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-jwks-url",
        default=None,
        help=(
            "Explicit trusted JWKS URL; discovered from issuer metadata if omitted "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_JWKS_URL)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-required-scope",
        action="append",
        dest="oauth_required_scopes",
        default=None,
        help=(
            "Required OAuth scope (repeatable; overrides "
            "OPEN_SANDBOX_MCP_OAUTH_REQUIRED_SCOPES)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-allowed-algs",
        default=None,
        help=(
            "Comma-separated allowed asymmetric JWS algorithms "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_ALLOWED_ALGS)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-clock-skew-seconds",
        type=float,
        default=None,
        help=(
            "Clock-skew tolerance in seconds for JWT exp/nbf "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_CLOCK_SKEW_SECONDS)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-http-timeout-seconds",
        type=float,
        default=None,
        help=(
            "Timeout in seconds for outbound metadata/JWKS requests "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_HTTP_TIMEOUT_SECONDS)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-jwks-cache-ttl-seconds",
        type=float,
        default=None,
        help=(
            "JWKS cache TTL in seconds "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_JWKS_CACHE_TTL_SECONDS)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-jwks-refresh-cooldown-seconds",
        type=float,
        default=None,
        help=(
            "Minimum interval in seconds between forced JWKS refreshes on unknown kid "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_JWKS_REFRESH_COOLDOWN_SECONDS)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-allowed-host",
        action="append",
        dest="oauth_allowed_hosts",
        default=None,
        help=(
            "Additional allowed Host header value for DNS-rebinding protection "
            "(repeatable; overrides OPEN_SANDBOX_MCP_OAUTH_ALLOWED_HOSTS)."
        ),
    )
    oauth_group.add_argument(
        "--oauth-allow-insecure-http",
        action="store_true",
        default=None,
        help=(
            "Allow http:// OAuth URLs strictly on loopback hosts for local testing "
            "(overrides OPEN_SANDBOX_MCP_OAUTH_ALLOW_INSECURE_HTTP)."
        ),
    )

    args = parser.parse_args()
    try:
        oauth_config = load_oauth_config(
            transport=args.transport,
            mode=args.oauth_mode,
            issuer_url=args.oauth_issuer_url,
            resource_url=args.oauth_resource_url,
            audience=args.oauth_audience,
            jwks_url=args.oauth_jwks_url,
            required_scopes=args.oauth_required_scopes,
            allowed_algorithms=args.oauth_allowed_algs,
            clock_skew_seconds=args.oauth_clock_skew_seconds,
            http_timeout_seconds=args.oauth_http_timeout_seconds,
            jwks_cache_ttl_seconds=args.oauth_jwks_cache_ttl_seconds,
            jwks_refresh_cooldown_seconds=args.oauth_jwks_refresh_cooldown_seconds,
            allowed_hosts=args.oauth_allowed_hosts,
            allow_insecure_http=args.oauth_allow_insecure_http,
        )
    except OAuthConfigError as exc:
        parser.error(str(exc))

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
        print(
            "warning: --use-server-proxy is deprecated: the server proxy data path "
            "is in maintenance mode (security and bug fixes only); route data-plane "
            "traffic through the ingress gateway instead.",
            file=sys.stderr,
        )
        config_values["use_server_proxy"] = True

    connection_config = ConnectionConfig(**config_values) if config_values else None
    if oauth_config is not None:
        mcp = create_server(
            connection_config=connection_config,
            oauth_config=oauth_config,
        )
        app = create_streamable_http_app(
            mcp,
            host=args.host,
            oauth_config=oauth_config,
        )
        _serve_streamable_http_app(
            app,
            host=args.host,
            port=args.port,
            log_level=mcp.settings.log_level,
        )
        return

    mcp = create_server(connection_config=connection_config)

    if args.transport == "streamable-http":
        mcp.run(
            transport="streamable-http",
            host=args.host,
            port=args.port,
        )
        return

    if args.transport == "stdio":
        mcp.run(transport="stdio")
        return

    mcp.run()


if __name__ == "__main__":
    main()
