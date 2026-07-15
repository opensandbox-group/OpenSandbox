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
from typing import Any, TypeVar

from attrs import define as _attrs_define

from ..types import UNSET, Unset

T = TypeVar("T", bound="CredentialProxyConfig")


@_attrs_define
class CredentialProxyConfig:
    """Credential Vault proxy startup settings. This is an explicit opt-in for
    transparent MITM support used by credential injection. Credential Vault
    requires `dns+nft` enforcement and a network policy. A deny-default policy
    is strongly recommended; default-allow remains temporarily supported for
    backward compatibility and emits a security warning.

        Attributes:
            enabled (bool | Unset): When true, the server starts the egress sidecar with transparent
                MITM enabled and installs the runtime-managed MITM CA bundle into
                the sandbox container. Requires `networkPolicy` and server
                `[egress].mode = "dns+nft"`. `defaultAction: deny` is strongly
                recommended; default-allow support is deprecated.
                 Default: False.
    """

    enabled: bool | Unset = False

    def to_dict(self) -> dict[str, Any]:
        enabled = self.enabled

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if enabled is not UNSET:
            field_dict["enabled"] = enabled

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        enabled = d.pop("enabled", UNSET)

        credential_proxy_config = cls(
            enabled=enabled,
        )

        return credential_proxy_config
