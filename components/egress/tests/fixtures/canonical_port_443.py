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
``request.data.port`` to the canonical 443 in ``requestheaders`` — loaded
BEFORE ``system.py`` — so the live H1 identity gate sees the same port a
real transparent-mode HTTPS/443 interception produces. It is test evidence
plumbing only: it does not relax any gate (the authority must still equal
the SNI and the admission token SNI), it adds no production configuration,
and upstream delivery still uses the already-established tunnel.
"""

from mitmproxy import http

_METADATA_KEY = "_canonical_port_real"


def requestheaders(flow: http.HTTPFlow) -> None:
    flow.metadata[_METADATA_KEY] = flow.request.data.port
    flow.request.data.port = 443


def request(flow: http.HTTPFlow) -> None:
    # The gate has already run inside requestheaders; restore the real
    # upstream port before the lazy upstream connection is opened.
    real = flow.metadata.pop(_METADATA_KEY, None)
    if real is not None:
        flow.request.data.port = real
