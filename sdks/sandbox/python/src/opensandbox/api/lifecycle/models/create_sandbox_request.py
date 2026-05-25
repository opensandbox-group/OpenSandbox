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
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.create_sandbox_request_env import CreateSandboxRequestEnv
    from ..models.create_sandbox_request_extensions import CreateSandboxRequestExtensions
    from ..models.create_sandbox_request_metadata import CreateSandboxRequestMetadata
    from ..models.image_spec import ImageSpec
    from ..models.network_policy import NetworkPolicy
    from ..models.platform_spec import PlatformSpec
    from ..models.resource_limits import ResourceLimits
    from ..models.volume import Volume


T = TypeVar("T", bound="CreateSandboxRequest")


@_attrs_define
class CreateSandboxRequest:
    """Request to create a new sandbox from either a container image or a snapshot.
    Exactly one of `image` or `snapshotId` must be provided.

    When `image` is provided, `entrypoint` is required. When `snapshotId` is
    provided, `entrypoint` is optional. If omitted, the server defaults the
    sandbox entrypoint to `["tail", "-f", "/dev/null"]`.

    **Note**: API Key authentication is required via the `OPEN-SANDBOX-API-KEY` header.

        Attributes:
            resource_limits (ResourceLimits): Runtime resource constraints as key-value pairs. Similar to Kubernetes
                resource specifications,
                allows flexible definition of resource limits. Common resource types include:
                - `cpu`: CPU allocation in millicores (e.g., "250m" for 0.25 CPU cores)
                - `memory`: Memory allocation in bytes or human-readable format (e.g., "512Mi", "1Gi")
                - `gpu`: Number of GPU devices (e.g., "1")

                New resource types can be added without API changes.
                 Example: {'cpu': '500m', 'memory': '512Mi', 'gpu': '1'}.
            image (ImageSpec | Unset): Container image specification for sandbox provisioning.

                Supports public registry images and private registry images with authentication.
            snapshot_id (str | Unset): Snapshot identifier to restore from.
                Mutually exclusive with `image`.
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
            timeout (int | None | Unset): Sandbox timeout in seconds. The sandbox will automatically terminate after this
                duration.
                The maximum is controlled by the server configuration (`server.max_sandbox_timeout_seconds`).
                Omit this field or set it to null to disable automatic expiration and require explicit cleanup.
                Note: manual cleanup support is runtime-dependent; Kubernetes providers may reject
                omitted or null timeout when the underlying workload provider does not support non-expiring sandboxes.
            env (CreateSandboxRequestEnv | Unset): Environment variables to inject into the sandbox runtime. Example:
                {'API_KEY': 'secret-key', 'DEBUG': 'true', 'LOG_LEVEL': 'info'}.
            metadata (CreateSandboxRequestMetadata | Unset): Custom key-value metadata for management, filtering, and
                tagging.
                Use "name" key for a human-readable identifier.
                 Example: {'name': 'Data Processing Sandbox', 'project': 'data-processing', 'team': 'ml', 'environment':
                'staging'}.
            entrypoint (list[str] | Unset): The command to execute as the sandbox's entry process.

                Required when `image` is provided.

                Optional when `snapshotId` is provided. If omitted for snapshot
                restore, the server defaults to `["tail", "-f", "/dev/null"]`.

                Explicitly specifies the user's expected main process, allowing the sandbox management
                service to reliably inject control processes before executing this command.

                Format: [executable, arg1, arg2, ...]

                Examples:
                - ["python", "/app/main.py"]
                - ["/bin/bash"]
                - ["java", "-jar", "/app/app.jar"]
                - ["node", "server.js"]
                 Example: ['python', '/app/main.py'].
            network_policy (NetworkPolicy | Unset): Egress network policy matching the sidecar `/policy` request body.
                If `defaultAction` is omitted, the sidecar defaults to "deny"; passing an empty
                object or null results in allow-all behavior at startup.
            secure_access (bool | Unset): Opts the sandbox into secured access for endpoint access.
                This is currently supported only for Kubernetes sandboxes exposed
                through ingress gateway mode. When enabled, the server provisions
                access credentials and returns the required request headers with
                endpoint responses. Clients must include those endpoint headers when
                calling the sandbox. When omitted or false, endpoints remain
                accessible without the additional access token for backward
                compatibility.
                 Default: False.
            volumes (list[Volume] | Unset): Storage mounts for the sandbox. Each volume entry specifies a named backend-
                specific
                storage source and common mount settings. Exactly one backend type must be specified
                per volume entry.
            extensions (CreateSandboxRequestExtensions | Unset): Opaque container for provider-specific or transient
                parameters not supported by the core API.

                **Note**: This field is reserved for internal features, experimental flags, or temporary behaviors. Standard
                parameters should be proposed as core API fields.

                **Best Practices**:
                - **Namespacing**: Use prefixed keys (e.g., `storage.id`) to prevent collisions.
                - **Pass-through**: SDKs and middleware must treat this object as opaque and pass it through transparently.

                **Well-known keys**:
                - `access.renew.extend.seconds` (optional): Decimal integer string from **300** to **86400** (5 minutes to 24
                hours inclusive). Opts the sandbox into OSEP-0009 renew-on-access and sets per-renewal extension seconds. Omit
                to disable. Invalid values are rejected at creation with HTTP 400 (validated on the lifecycle create endpoint
                via `validate_extensions` in server `src/extensions/validation.py`).
    """

    resource_limits: ResourceLimits
    image: ImageSpec | Unset = UNSET
    snapshot_id: str | Unset = UNSET
    platform: PlatformSpec | Unset = UNSET
    timeout: int | None | Unset = UNSET
    env: CreateSandboxRequestEnv | Unset = UNSET
    metadata: CreateSandboxRequestMetadata | Unset = UNSET
    entrypoint: list[str] | Unset = UNSET
    network_policy: NetworkPolicy | Unset = UNSET
    secure_access: bool | Unset = False
    volumes: list[Volume] | Unset = UNSET
    extensions: CreateSandboxRequestExtensions | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        resource_limits = self.resource_limits.to_dict()

        image: dict[str, Any] | Unset = UNSET
        if not isinstance(self.image, Unset):
            image = self.image.to_dict()

        snapshot_id = self.snapshot_id

        platform: dict[str, Any] | Unset = UNSET
        if not isinstance(self.platform, Unset):
            platform = self.platform.to_dict()

        timeout: int | None | Unset
        if isinstance(self.timeout, Unset):
            timeout = UNSET
        else:
            timeout = self.timeout

        env: dict[str, Any] | Unset = UNSET
        if not isinstance(self.env, Unset):
            env = self.env.to_dict()

        metadata: dict[str, Any] | Unset = UNSET
        if not isinstance(self.metadata, Unset):
            metadata = self.metadata.to_dict()

        entrypoint: list[str] | Unset = UNSET
        if not isinstance(self.entrypoint, Unset):
            entrypoint = self.entrypoint

        network_policy: dict[str, Any] | Unset = UNSET
        if not isinstance(self.network_policy, Unset):
            network_policy = self.network_policy.to_dict()

        secure_access = self.secure_access

        volumes: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.volumes, Unset):
            volumes = []
            for volumes_item_data in self.volumes:
                volumes_item = volumes_item_data.to_dict()
                volumes.append(volumes_item)

        extensions: dict[str, Any] | Unset = UNSET
        if not isinstance(self.extensions, Unset):
            extensions = self.extensions.to_dict()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "resourceLimits": resource_limits,
            }
        )
        if image is not UNSET:
            field_dict["image"] = image
        if snapshot_id is not UNSET:
            field_dict["snapshotId"] = snapshot_id
        if platform is not UNSET:
            field_dict["platform"] = platform
        if timeout is not UNSET:
            field_dict["timeout"] = timeout
        if env is not UNSET:
            field_dict["env"] = env
        if metadata is not UNSET:
            field_dict["metadata"] = metadata
        if entrypoint is not UNSET:
            field_dict["entrypoint"] = entrypoint
        if network_policy is not UNSET:
            field_dict["networkPolicy"] = network_policy
        if secure_access is not UNSET:
            field_dict["secureAccess"] = secure_access
        if volumes is not UNSET:
            field_dict["volumes"] = volumes
        if extensions is not UNSET:
            field_dict["extensions"] = extensions

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.create_sandbox_request_env import CreateSandboxRequestEnv
        from ..models.create_sandbox_request_extensions import CreateSandboxRequestExtensions
        from ..models.create_sandbox_request_metadata import CreateSandboxRequestMetadata
        from ..models.image_spec import ImageSpec
        from ..models.network_policy import NetworkPolicy
        from ..models.platform_spec import PlatformSpec
        from ..models.resource_limits import ResourceLimits
        from ..models.volume import Volume

        d = dict(src_dict)
        resource_limits = ResourceLimits.from_dict(d.pop("resourceLimits"))

        _image = d.pop("image", UNSET)
        image: ImageSpec | Unset
        if isinstance(_image, Unset):
            image = UNSET
        else:
            image = ImageSpec.from_dict(_image)

        snapshot_id = d.pop("snapshotId", UNSET)

        _platform = d.pop("platform", UNSET)
        platform: PlatformSpec | Unset
        if isinstance(_platform, Unset):
            platform = UNSET
        else:
            platform = PlatformSpec.from_dict(_platform)

        def _parse_timeout(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        timeout = _parse_timeout(d.pop("timeout", UNSET))

        _env = d.pop("env", UNSET)
        env: CreateSandboxRequestEnv | Unset
        if isinstance(_env, Unset):
            env = UNSET
        else:
            env = CreateSandboxRequestEnv.from_dict(_env)

        _metadata = d.pop("metadata", UNSET)
        metadata: CreateSandboxRequestMetadata | Unset
        if isinstance(_metadata, Unset):
            metadata = UNSET
        else:
            metadata = CreateSandboxRequestMetadata.from_dict(_metadata)

        entrypoint = cast(list[str], d.pop("entrypoint", UNSET))

        _network_policy = d.pop("networkPolicy", UNSET)
        network_policy: NetworkPolicy | Unset
        if isinstance(_network_policy, Unset):
            network_policy = UNSET
        else:
            network_policy = NetworkPolicy.from_dict(_network_policy)

        secure_access = d.pop("secureAccess", UNSET)

        _volumes = d.pop("volumes", UNSET)
        volumes: list[Volume] | Unset = UNSET
        if _volumes is not UNSET:
            volumes = []
            for volumes_item_data in _volumes:
                volumes_item = Volume.from_dict(volumes_item_data)

                volumes.append(volumes_item)

        _extensions = d.pop("extensions", UNSET)
        extensions: CreateSandboxRequestExtensions | Unset
        if isinstance(_extensions, Unset):
            extensions = UNSET
        else:
            extensions = CreateSandboxRequestExtensions.from_dict(_extensions)

        create_sandbox_request = cls(
            resource_limits=resource_limits,
            image=image,
            snapshot_id=snapshot_id,
            platform=platform,
            timeout=timeout,
            env=env,
            metadata=metadata,
            entrypoint=entrypoint,
            network_policy=network_policy,
            secure_access=secure_access,
            volumes=volumes,
            extensions=extensions,
        )

        create_sandbox_request.additional_properties = d
        return create_sandbox_request

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
