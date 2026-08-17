#
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
#

from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.endpoint_headers import EndpointHeaders


T = TypeVar("T", bound="Endpoint")


@_attrs_define
class Endpoint:
    """Endpoint for accessing a service running in the sandbox.
    The service must be listening on the specified port inside the sandbox for the endpoint to be available.

        Attributes:
            endpoint (str): Public URL to access the service from outside the sandbox.
                The exact shape depends on the runtime and network mode, e.g. direct
                `{endpoint-host}:{port}`, execd proxy
                `{endpoint-host}:{hostProxyPort}/proxy/{port}`, or server proxy
                `{endpoint-host}/sandboxes/{sandboxId}/proxy/{port}`.
                Example: endpoint.opensandbox.io/sandboxes/abc123/proxy/8080
            headers (EndpointHeaders | Unset): Requests targeting the sandbox must include the corresponding header(s).
    """

    endpoint: str
    headers: EndpointHeaders | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        endpoint = self.endpoint

        headers: dict[str, Any] | Unset = UNSET
        if not isinstance(self.headers, Unset):
            headers = self.headers.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "endpoint": endpoint,
            }
        )
        if headers is not UNSET:
            field_dict["headers"] = headers

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.endpoint_headers import EndpointHeaders

        d = dict(src_dict)
        endpoint = d.pop("endpoint")

        _headers = d.pop("headers", UNSET)
        headers: EndpointHeaders | Unset
        if isinstance(_headers, Unset):
            headers = UNSET
        else:
            headers = EndpointHeaders.from_dict(_headers)

        endpoint = cls(
            endpoint=endpoint,
            headers=headers,
        )

        return endpoint
