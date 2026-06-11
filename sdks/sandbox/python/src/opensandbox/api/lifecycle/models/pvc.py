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

from ..types import UNSET, Unset

T = TypeVar("T", bound="PVC")


@_attrs_define
class PVC:
    """Platform-managed named volume backend. A runtime-neutral abstraction
    for referencing a platform-managed named volume. If `createIfNotExists`
    is true (the default) and the volume does not yet exist, it will be
    created automatically using the provisioning hints below.

    - Kubernetes: maps to a PersistentVolumeClaim in the same namespace.
    - Docker: maps to a Docker named volume (created via `docker volume create`).

        Attributes:
            claim_name (str): Name of the volume on the target platform.
                In Kubernetes this is the PVC name; in Docker this is the named
                volume name. Must be a valid DNS label.
            create_if_not_exists (bool | Unset): When true (the default), the volume is automatically created if
                it does not exist. When false, referencing a non-existent volume
                fails with an error.
                 Default: True.
            delete_on_sandbox_termination (bool | Unset): When true, the volume is automatically removed when the sandbox
                is deleted. Only applies to volumes that were auto-created by the
                server on this request; pre-existing volumes are never removed.
                For Kubernetes, the resulting PVC delete triggers the bound PV's
                StorageClass reclaim policy (`Retain`/`Delete`).
                 Default: False.
            storage_class (None | str | Unset): Kubernetes StorageClass name for auto-created PVCs. Null means
                use the cluster default. Ignored for Docker volumes.
            storage (None | str | Unset): Storage capacity request for auto-created PVCs (e.g. "1Gi",
                "10Gi"). Defaults to the server-configured `volume_default_size`
                when omitted. Ignored for Docker volumes.
            access_modes (list[str] | None | Unset): Access modes for auto-created PVCs (e.g. ["ReadWriteOnce"]).
                Defaults to ["ReadWriteOnce"] when omitted. Ignored for Docker
                volumes.
    """

    claim_name: str
    create_if_not_exists: bool | Unset = True
    delete_on_sandbox_termination: bool | Unset = False
    storage_class: None | str | Unset = UNSET
    storage: None | str | Unset = UNSET
    access_modes: list[str] | None | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        claim_name = self.claim_name

        create_if_not_exists = self.create_if_not_exists

        delete_on_sandbox_termination = self.delete_on_sandbox_termination

        storage_class: None | str | Unset
        if isinstance(self.storage_class, Unset):
            storage_class = UNSET
        else:
            storage_class = self.storage_class

        storage: None | str | Unset
        if isinstance(self.storage, Unset):
            storage = UNSET
        else:
            storage = self.storage

        access_modes: list[str] | None | Unset
        if isinstance(self.access_modes, Unset):
            access_modes = UNSET
        elif isinstance(self.access_modes, list):
            access_modes = self.access_modes

        else:
            access_modes = self.access_modes

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "claimName": claim_name,
            }
        )
        if create_if_not_exists is not UNSET:
            field_dict["createIfNotExists"] = create_if_not_exists
        if delete_on_sandbox_termination is not UNSET:
            field_dict["deleteOnSandboxTermination"] = delete_on_sandbox_termination
        if storage_class is not UNSET:
            field_dict["storageClass"] = storage_class
        if storage is not UNSET:
            field_dict["storage"] = storage
        if access_modes is not UNSET:
            field_dict["accessModes"] = access_modes

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        claim_name = d.pop("claimName")

        create_if_not_exists = d.pop("createIfNotExists", UNSET)

        delete_on_sandbox_termination = d.pop("deleteOnSandboxTermination", UNSET)

        def _parse_storage_class(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        storage_class = _parse_storage_class(d.pop("storageClass", UNSET))

        def _parse_storage(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        storage = _parse_storage(d.pop("storage", UNSET))

        def _parse_access_modes(data: object) -> list[str] | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, list):
                    raise TypeError()
                access_modes_type_0 = cast(list[str], data)

                return access_modes_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(list[str] | None | Unset, data)

        access_modes = _parse_access_modes(d.pop("accessModes", UNSET))

        pvc = cls(
            claim_name=claim_name,
            create_if_not_exists=create_if_not_exists,
            delete_on_sandbox_termination=delete_on_sandbox_termination,
            storage_class=storage_class,
            storage=storage,
            access_modes=access_modes,
        )

        return pvc
