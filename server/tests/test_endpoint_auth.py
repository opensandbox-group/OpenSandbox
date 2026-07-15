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

from opensandbox_server.services.constants import OPEN_SANDBOX_EGRESS_AUTH_HEADER
from opensandbox_server.services.endpoint_auth import (
    build_egress_auth_headers,
    generate_egress_token,
    merge_endpoint_headers,
)


def test_generate_egress_token_returns_random_urlsafe_strings() -> None:
    first = generate_egress_token()
    second = generate_egress_token()

    assert first
    assert second
    assert first != second


def test_build_egress_auth_headers_uses_expected_header_name() -> None:
    token = "egress-token"

    assert build_egress_auth_headers(token) == {
        OPEN_SANDBOX_EGRESS_AUTH_HEADER: token,
    }


def test_merge_endpoint_headers_preserves_existing_headers() -> None:
    existing = {"OpenSandbox-Ingress-To": "sbx-1-18080"}
    extra = {OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token"}

    merged = merge_endpoint_headers(existing, extra)

    assert merged == {
        "OpenSandbox-Ingress-To": "sbx-1-18080",
        OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token",
    }
    assert existing == {"OpenSandbox-Ingress-To": "sbx-1-18080"}


def test_merge_endpoint_headers_handles_missing_existing_headers() -> None:
    merged = merge_endpoint_headers(None, {OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token"})

    assert merged == {OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token"}
