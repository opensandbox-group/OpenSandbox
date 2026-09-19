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

# OpenSandbox egress chained upstream-proxy addon.
#
# Loaded by the egress mitmproxy launcher (after system.py, before user addons)
# only when OPENSANDBOX_EGRESS_UPSTREAM_PROXY is set. Routes every
# mitmproxy-handled connection through a chained HTTP CONNECT proxy.
#
# Behavior:
#   1. Sets server_conn.via on each flow, so mitmproxy's upstream-proxy layer
#      dials the configured proxy and issues "CONNECT <request.host>:<port>".
#      For TLS-intercepted traffic request.host is the SNI/Host-derived FQDN,
#      keeping CONNECT authority, SNI and Host consistent; for flows where only
#      the original destination IP is known the authority stays IP:port.
#   2. Optionally injects Proxy-Authorization on the upstream CONNECT from
#      OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH (complete header value, never
#      logged).
#   3. Fail-closed: server_connect refuses any direct dial that is not the
#      proxy's own tunnel connection. TLS pass-through flows (no-SNI,
#      ignore_hosts/tcp_hosts matches) and UDP/QUIC dials cannot be chained,
#      so they are refused rather than silently bypassing the proxy.
#
# Requirements (validated at load; violations fail closed):
#   - connection_strategy must be "lazy" (the shipped config.yaml default).
#      Eager connects upstream before request headers arrive, so no via can be
#      applied and every flow would be refused at runtime.
#   - ignore_hosts / tcp_hosts / udp_hosts must be empty: pass-through traffic
#      cannot be chained and enabling both options would silently exempt
#      destinations from the proxy.
#
# For https:// proxies, TLS to the proxy is verified against
# ssl_verify_upstream_trusted_confdir / _trusted_ca (default /etc/ssl/certs,
# overridable via OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_TRUST_DIR) with SNI and
# hostname verification against the proxy host.

from __future__ import annotations

import os
from urllib.parse import urlsplit

from mitmproxy import ctx, http

UPSTREAM_PROXY_ENV = "OPENSANDBOX_EGRESS_UPSTREAM_PROXY"
UPSTREAM_PROXY_AUTH_ENV = "OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH"

_via: tuple[str, tuple[str, int]] | None = None
_proxy_address: tuple[str, int] | None = None
_proxy_auth: str | None = None


def _parse_upstream(raw: str) -> tuple[str, str, int]:
    """Parse "scheme://host[:port]"; port defaults to the scheme default.

    Userinfo, query and fragment are rejected: the value may only carry an
    address. Credentials belong exclusively in UPSTREAM_PROXY_AUTH_ENV.
    """
    raw = raw.strip()
    if not raw:
        raise ValueError("value is empty")
    if "://" not in raw:
        raise ValueError("missing scheme, want http://host:port or https://host:port")
    try:
        url = urlsplit(raw)
        port = url.port
    except ValueError as e:
        raise ValueError(f"invalid URL: {e}") from e
    if url.scheme not in ("http", "https"):
        raise ValueError(f"unsupported scheme {url.scheme!r}, want http or https")
    if url.username is not None or url.password is not None:
        raise ValueError(
            f"userinfo is not allowed, use {UPSTREAM_PROXY_AUTH_ENV} for credentials"
        )
    host = url.hostname
    if not host:
        raise ValueError("missing host")
    if url.query or url.fragment:
        raise ValueError("query and fragment are not allowed")
    if port is None:
        port = 443 if url.scheme == "https" else 80
    elif not 1 <= port <= 65535:
        raise ValueError(f"invalid port {port}")
    if any(c in host for c in " \t\r\n/@"):
        raise ValueError(f"invalid host {host!r}")
    return url.scheme, host.lower(), port


def load(loader) -> None:
    global _via, _proxy_address, _proxy_auth
    raw = os.environ.get(UPSTREAM_PROXY_ENV, "").strip()
    auth = os.environ.get(UPSTREAM_PROXY_AUTH_ENV, "").strip()
    if not raw:
        if auth:
            raise ValueError(
                f"{UPSTREAM_PROXY_AUTH_ENV} is set but {UPSTREAM_PROXY_ENV} is empty"
            )
        return
    scheme, host, port = _parse_upstream(raw)
    if getattr(ctx.options, "connection_strategy", "lazy") != "lazy":
        raise ValueError(
            f"{UPSTREAM_PROXY_ENV} requires connection_strategy=lazy: eager "
            "connects upstream before a flow exists, so no via can be applied"
        )
    passthrough = [
        name
        for name in ("ignore_hosts", "tcp_hosts", "udp_hosts")
        if getattr(ctx.options, name, [])
    ]
    if passthrough:
        raise ValueError(
            f"{UPSTREAM_PROXY_ENV} is incompatible with {', '.join(passthrough)}: "
            "pass-through traffic cannot be chained and would bypass the proxy"
        )
    _proxy_address = (host, port)
    _via = (scheme, _proxy_address)
    _proxy_auth = auth or None
    ctx.log.info(
        f"upstream proxy: chaining enabled via {scheme}://{host}:{port}"
        + (" with Proxy-Authorization" if _proxy_auth else "")
    )


def _set_via_on_conn(conn) -> None:
    if conn is None or getattr(conn, "connected", False):
        return
    conn.via = _via


def _set_via(flow: http.HTTPFlow) -> None:
    _set_via_on_conn(getattr(flow, "server_conn", None))


def tls_clienthello(data) -> None:
    # Anchor via on the (not yet connected) server placeholder while the client
    # TLS hello is parsed — the earliest point a server connection object
    # exists for an intercepted TLS flow.
    if _via is not None:
        _set_via_on_conn(getattr(data.context, "server", None))


def requestheaders(flow: http.HTTPFlow) -> None:
    # Fires before the server connection is opened (lazy strategy); setting via
    # here makes the connection-spec fork chain through the upstream proxy.
    if _via is not None:
        _set_via(flow)


def http_connect(flow: http.HTTPFlow) -> None:
    # Regular-mode client CONNECT: same chaining point before the upstream
    # connection is established.
    if _via is not None:
        _set_via(flow)


def http_connect_upstream(flow: http.HTTPFlow) -> None:
    # The CONNECT mitmproxy sends to our upstream proxy. Attach credentials.
    if _proxy_auth:
        flow.request.headers["Proxy-Authorization"] = _proxy_auth


def server_connect(data) -> None:
    """Refuse any direct dial while chaining is enabled (fail closed).

    Chained flows never reach this hook for the real destination: the
    upstream-proxy layer dials the proxy itself, so the only legitimate
    connection here is that tunnel. Everything else — TLS pass-through, UDP —
    would silently bypass the proxy, so it is refused.
    """
    if _via is None:
        return
    server = data.server
    address = getattr(server, "address", None)
    # A hook exception is logged by mitmproxy but does not stop the dial, so the
    # comparison must be total: anything we cannot positively identify as the
    # proxy's own tunnel connection is refused.
    try:
        is_proxy_conn = (
            getattr(server, "transport_protocol", "tcp") == "tcp"
            and address is not None
            and len(address) >= 2
            and str(address[0]).lower() == _proxy_address[0]
            and int(address[1]) == _proxy_address[1]
        )
    except (TypeError, ValueError):
        is_proxy_conn = False
    if not is_proxy_conn:
        server.error = (
            "upstream proxy required: direct egress dial refused "
            f"({getattr(server, 'transport_protocol', 'tcp')} to {address})"
        )
