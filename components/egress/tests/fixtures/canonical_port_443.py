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

"""Loopback-test port canonicalization for the live credential-bound addon.

Loopback runtime tests cannot bind privileged port 443, so the test origin
listens on an ephemeral high port. This fixture-only addon rewrites
the ClientHello destination and ``request.data.port`` to canonical 443 —
loaded BEFORE ``system.py`` — so TLS admission and the live H1 identity gate
see the canonical identity. Each hook restores the real port before upstream
delivery. This is fixture semantics, not real port-443/nft E2E evidence.
It does not relax any gate (the authority must still equal
the SNI and the admission token SNI), it adds no production configuration,
and upstream delivery still uses the already-established tunnel.
"""

from mitmproxy import ctx, http, tls

_METADATA_KEY = "_canonical_port_real"
_clienthello_addresses = {}


def tls_clienthello(data: tls.ClientHelloData) -> None:
    server = data.context.server
    _clienthello_addresses[id(data)] = server.address
    server.address = (server.address[0], 443)


class RestoreClientHelloDestination:
    def tls_clienthello(self, data: tls.ClientHelloData) -> None:
        data.context.server.address = _clienthello_addresses.pop(id(data))


def running() -> None:
    # Scripts are now loaded: append restoration AFTER system.py's TLS gate,
    # before mitmproxy resumes processing and opens the real upstream socket.
    ctx.master.addons.add(RestoreClientHelloDestination())


def requestheaders(flow: http.HTTPFlow) -> None:
    flow.metadata[_METADATA_KEY] = flow.request.data.port
    flow.request.data.port = 443


def request(flow: http.HTTPFlow) -> None:
    # The gate has already run inside requestheaders; restore the real
    # upstream port before the lazy upstream connection is opened.
    real = flow.metadata.pop(_METADATA_KEY, None)
    if real is not None:
        flow.request.data.port = real
