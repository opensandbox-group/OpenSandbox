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
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define

from ..models.credential_match_schemes_item import CredentialMatchSchemesItem
from ..types import UNSET, Unset

T = TypeVar("T", bound="CredentialMatch")


@_attrs_define
class CredentialMatch:
    """
    Attributes:
        hosts (list[str]):
        schemes (list[CredentialMatchSchemesItem] | Unset):
        ports (list[int] | Unset): Deprecated. Port is derived from scheme (https→443, http→80). Values other than 80 or
            443 are rejected with a validation error. Standard values (80/443) are accepted but ignored.
        methods (list[str] | Unset):
        paths (list[str] | Unset):
    """

    hosts: list[str]
    schemes: list[CredentialMatchSchemesItem] | Unset = UNSET
    ports: list[int] | Unset = UNSET
    methods: list[str] | Unset = UNSET
    paths: list[str] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        hosts = self.hosts

        schemes: list[str] | Unset = UNSET
        if not isinstance(self.schemes, Unset):
            schemes = []
            for schemes_item_data in self.schemes:
                schemes_item = schemes_item_data.value
                schemes.append(schemes_item)

        ports: list[int] | Unset = UNSET
        if not isinstance(self.ports, Unset):
            ports = self.ports

        methods: list[str] | Unset = UNSET
        if not isinstance(self.methods, Unset):
            methods = self.methods

        paths: list[str] | Unset = UNSET
        if not isinstance(self.paths, Unset):
            paths = self.paths

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "hosts": hosts,
            }
        )
        if schemes is not UNSET:
            field_dict["schemes"] = schemes
        if ports is not UNSET:
            field_dict["ports"] = ports
        if methods is not UNSET:
            field_dict["methods"] = methods
        if paths is not UNSET:
            field_dict["paths"] = paths

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        hosts = cast(list[str], d.pop("hosts"))

        _schemes = d.pop("schemes", UNSET)
        schemes: list[CredentialMatchSchemesItem] | Unset = UNSET
        if _schemes is not UNSET:
            schemes = []
            for schemes_item_data in _schemes:
                schemes_item = CredentialMatchSchemesItem(schemes_item_data)

                schemes.append(schemes_item)

        ports = cast(list[int], d.pop("ports", UNSET))

        methods = cast(list[str], d.pop("methods", UNSET))

        paths = cast(list[str], d.pop("paths", UNSET))

        credential_match = cls(
            hosts=hosts,
            schemes=schemes,
            ports=ports,
            methods=methods,
            paths=paths,
        )

        return credential_match
