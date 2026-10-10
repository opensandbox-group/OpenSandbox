#
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
#

from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field
from dateutil.parser import isoparse

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.create_sandbox_response_extensions import CreateSandboxResponseExtensions
    from ..models.create_sandbox_response_metadata import CreateSandboxResponseMetadata
    from ..models.platform_spec import PlatformSpec
    from ..models.sandbox_status import SandboxStatus


T = TypeVar("T", bound="CreateSandboxResponse")


@_attrs_define
class CreateSandboxResponse:
    """Response from creating a new sandbox. Contains essential information without startup source details and updatedAt.

    Attributes:
        id (str): Unique sandbox identifier
        status (SandboxStatus): Detailed status information with lifecycle state and transition details
        created_at (datetime.datetime): Sandbox creation timestamp
        entrypoint (list[str]): Entry process specification for the sandbox. For image-created sandboxes,
            this is copied from the creation request. For snapshot-created sandboxes,
            this is restored from the snapshot.
        resolved_image_digest (None | str | Unset): Registry artifact digest independently observed from runtime
            metadata for
            the primary sandbox container, distinct from the existing image reference
            and from image/config IDs. May identify a multi-platform index or a
            platform manifest; platform-manifest identity is not guaranteed.
            Optional: absent or null when unavailable, ambiguous, or unsupported,
            including Pool and snapshot-backed sandboxes. May become available after
            creation. Does not attest to current filesystem contents or provide fencing.
        metadata (CreateSandboxResponseMetadata | Unset): Custom metadata from creation request
        extensions (CreateSandboxResponseExtensions | Unset): Opaque extension data restored from provider-specific
            storage
        platform (PlatformSpec | Unset): Runtime platform constraint used for scheduling/provisioning.

            This field is independent from `image` and expresses the expected target
            OS and CPU architecture for sandbox execution.

            Behavioral notes:
            - If omitted, the runtime applies its own default platform selection behavior.
              For Docker, requests are created without an explicit platform override.
              For Kubernetes, no `kubernetes.io/os` or `kubernetes.io/arch` constraint
              is injected unless provided by request or workload template.
            - If provided and cannot be satisfied by runtime/template/pool constraints,
              request must fail explicitly.
        expires_at (datetime.datetime | Unset): Timestamp when sandbox will auto-terminate. Omitted when manual cleanup
            is enabled.
    """

    id: str
    status: SandboxStatus
    created_at: datetime.datetime
    entrypoint: list[str]
    resolved_image_digest: None | str | Unset = UNSET
    metadata: CreateSandboxResponseMetadata | Unset = UNSET
    extensions: CreateSandboxResponseExtensions | Unset = UNSET
    platform: PlatformSpec | Unset = UNSET
    expires_at: datetime.datetime | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        id = self.id

        status = self.status.to_dict()

        created_at = self.created_at.isoformat()

        entrypoint = self.entrypoint

        resolved_image_digest: None | str | Unset
        if isinstance(self.resolved_image_digest, Unset):
            resolved_image_digest = UNSET
        else:
            resolved_image_digest = self.resolved_image_digest

        metadata: dict[str, Any] | Unset = UNSET
        if not isinstance(self.metadata, Unset):
            metadata = self.metadata.to_dict()

        extensions: dict[str, Any] | Unset = UNSET
        if not isinstance(self.extensions, Unset):
            extensions = self.extensions.to_dict()

        platform: dict[str, Any] | Unset = UNSET
        if not isinstance(self.platform, Unset):
            platform = self.platform.to_dict()

        expires_at: str | Unset = UNSET
        if not isinstance(self.expires_at, Unset):
            expires_at = self.expires_at.isoformat()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "id": id,
                "status": status,
                "createdAt": created_at,
                "entrypoint": entrypoint,
            }
        )
        if resolved_image_digest is not UNSET:
            field_dict["resolvedImageDigest"] = resolved_image_digest
        if metadata is not UNSET:
            field_dict["metadata"] = metadata
        if extensions is not UNSET:
            field_dict["extensions"] = extensions
        if platform is not UNSET:
            field_dict["platform"] = platform
        if expires_at is not UNSET:
            field_dict["expiresAt"] = expires_at

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.create_sandbox_response_extensions import CreateSandboxResponseExtensions
        from ..models.create_sandbox_response_metadata import CreateSandboxResponseMetadata
        from ..models.platform_spec import PlatformSpec
        from ..models.sandbox_status import SandboxStatus

        d = dict(src_dict)
        id = d.pop("id")

        status = SandboxStatus.from_dict(d.pop("status"))

        created_at = isoparse(d.pop("createdAt"))

        entrypoint = cast(list[str], d.pop("entrypoint"))

        def _parse_resolved_image_digest(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        resolved_image_digest = _parse_resolved_image_digest(d.pop("resolvedImageDigest", UNSET))

        _metadata = d.pop("metadata", UNSET)
        metadata: CreateSandboxResponseMetadata | Unset
        if isinstance(_metadata, Unset):
            metadata = UNSET
        else:
            metadata = CreateSandboxResponseMetadata.from_dict(_metadata)

        _extensions = d.pop("extensions", UNSET)
        extensions: CreateSandboxResponseExtensions | Unset
        if isinstance(_extensions, Unset):
            extensions = UNSET
        else:
            extensions = CreateSandboxResponseExtensions.from_dict(_extensions)

        _platform = d.pop("platform", UNSET)
        platform: PlatformSpec | Unset
        if isinstance(_platform, Unset):
            platform = UNSET
        else:
            platform = PlatformSpec.from_dict(_platform)

        _expires_at = d.pop("expiresAt", UNSET)
        expires_at: datetime.datetime | Unset
        if isinstance(_expires_at, Unset):
            expires_at = UNSET
        else:
            expires_at = isoparse(_expires_at)

        create_sandbox_response = cls(
            id=id,
            status=status,
            created_at=created_at,
            entrypoint=entrypoint,
            resolved_image_digest=resolved_image_digest,
            metadata=metadata,
            extensions=extensions,
            platform=platform,
            expires_at=expires_at,
        )

        create_sandbox_response.additional_properties = d
        return create_sandbox_response

    @property
    def additional_keys(self) -> list[str]:
        return list(self.additional_properties.keys())

    def __getitem__(self, key: str) -> Any:
        return self.additional_properties[key]

    def __setitem__(self, key: str, value: Any) -> None:
        self.additional_properties[key] = value

    def __delitem__(self, key: str) -> None:
        del self.additional_properties[key]

    def __contains__(self, key: str) -> bool:
        return key in self.additional_properties
