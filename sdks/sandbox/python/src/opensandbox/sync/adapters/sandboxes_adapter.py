#
# Copyright 2025 Alibaba Group Holding Ltd.
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
"""
Synchronous sandbox service adapter implementation.
"""

import logging
from datetime import datetime, timedelta

import httpx

from opensandbox.adapters.converter.exception_converter import (
    ExceptionConverter,
)
from opensandbox.adapters.converter.response_handler import (
    handle_api_error,
    require_parsed,
)
from opensandbox.adapters.converter.sandbox_model_converter import (
    SandboxModelConverter,
)
from opensandbox.api.lifecycle.types import UNSET
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.sandboxes import (
    CreateSnapshotRequest,
    NetworkPolicy,
    PagedSandboxInfos,
    PagedSnapshotInfos,
    PlatformSpec,
    SandboxCreateResponse,
    SandboxEndpoint,
    SandboxFilter,
    SandboxImageSpec,
    SandboxInfo,
    SandboxRenewResponse,
    SnapshotFilter,
    SnapshotInfo,
    Volume,
)
from opensandbox.sync.services.sandbox import SandboxesSync

logger = logging.getLogger(__name__)


class SandboxesAdapterSync(SandboxesSync):
    def __init__(self, connection_config: ConnectionConfigSync) -> None:
        self.connection_config = connection_config
        from opensandbox.api.lifecycle import AuthenticatedClient

        api_key = self.connection_config.get_api_key()
        timeout_seconds = self.connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)

        headers = {
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
        }
        if api_key:
            headers["OPEN-SANDBOX-API-KEY"] = api_key

        self._client = AuthenticatedClient(
            base_url=self.connection_config.get_base_url(),
            token=api_key or "",
            prefix="",
            auth_header_name="OPEN-SANDBOX-API-KEY",
            timeout=timeout,
        )

        self._httpx_client = httpx.Client(
            base_url=self.connection_config.get_base_url(),
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client.set_httpx_client(self._httpx_client)

    def _get_client(self):
        return self._client

    def create_sandbox(
        self,
        spec: SandboxImageSpec | None,
        entrypoint: list[str] | None,
        env: dict[str, str],
        metadata: dict[str, str],
        timeout: timedelta | None,
        resource: dict[str, str],
        network_policy: NetworkPolicy | None,
        extensions: dict[str, str],
        volumes: list[Volume] | None,
        platform: PlatformSpec | None = None,
        secure_access: bool = False,
        snapshot_id: str | None = None,
    ) -> SandboxCreateResponse:
        logger.info(
            "Creating sandbox with startup source: %s",
            spec.image if spec is not None else snapshot_id,
        )
        try:
            from opensandbox.api.lifecycle.api.sandboxes import post_sandboxes
            from opensandbox.api.lifecycle.models import (
                CreateSandboxResponse as ApiCreateSandboxResponse,
            )

            create_request = SandboxModelConverter.to_api_create_sandbox_request(
                spec=spec,
                entrypoint=entrypoint,
                env=env,
                metadata=metadata,
                timeout=timeout,
                resource=resource,
                platform=platform,
                network_policy=network_policy,
                extensions=extensions,
                volumes=volumes,
                secure_access=secure_access,
                snapshot_id=snapshot_id,
            )
            response_obj = post_sandboxes.sync_detailed(client=self._get_client(), body=create_request)
            handle_api_error(response_obj, "Create sandbox")

            parsed = require_parsed(response_obj, ApiCreateSandboxResponse, "Create sandbox")
            return SandboxModelConverter.to_sandbox_create_response(parsed)
        except Exception as e:
            logger.error(
                "Failed to create sandbox with startup source: %s",
                spec.image if spec is not None else snapshot_id,
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_sandbox_info(self, sandbox_id: str) -> SandboxInfo:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import get_sandboxes_sandbox_id
            from opensandbox.api.lifecycle.models import Sandbox as ApiSandbox

            response_obj = get_sandboxes_sandbox_id.sync_detailed(
                client=self._get_client(),
                sandbox_id=sandbox_id,
            )
            handle_api_error(response_obj, f"Get sandbox {sandbox_id}")
            parsed = require_parsed(response_obj, ApiSandbox, f"Get sandbox {sandbox_id}")
            return SandboxModelConverter.to_sandbox_info(parsed)
        except Exception as e:
            logger.error("Failed to get sandbox info: %s", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def list_sandboxes(self, filter: SandboxFilter) -> PagedSandboxInfos:
        # metadata double-encoding logic kept identical to async adapter
        metadata = UNSET
        if filter.metadata:
            from urllib.parse import quote

            metadata_parts: list[str] = []
            for key, value in filter.metadata.items():
                k1 = quote(key, safe="")
                v1 = quote(value, safe="")
                k2 = quote(k1, safe="")
                v2 = quote(v1, safe="")
                metadata_parts.append(f"{k2}={v2}")
            metadata = "&".join(metadata_parts)

        try:
            from opensandbox.api.lifecycle.api.sandboxes import get_sandboxes
            from opensandbox.api.lifecycle.models import (
                ListSandboxesResponse as ApiListSandboxesResponse,
            )
            from opensandbox.api.lifecycle.types import UNSET as API_UNSET

            response_obj = get_sandboxes.sync_detailed(
                client=self._get_client(),
                state=filter.states if filter.states else API_UNSET,
                metadata=metadata,
                page=filter.page if filter.page is not None else API_UNSET,
                page_size=filter.page_size if filter.page_size is not None else API_UNSET,
            )
            handle_api_error(response_obj, "List sandboxes")
            parsed = require_parsed(response_obj, ApiListSandboxesResponse, "List sandboxes")
            return SandboxModelConverter.to_paged_sandbox_infos(parsed)
        except Exception as e:
            logger.error("Failed to list sandboxes", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def patch_sandbox_metadata(
        self, sandbox_id: str, patch: dict[str, str | None]
    ) -> SandboxInfo:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import (
                patch_sandboxes_sandbox_id_metadata,
            )
            from opensandbox.api.lifecycle.models import PatchSandboxMetadataRequest
            from opensandbox.api.lifecycle.models import Sandbox as ApiSandbox

            response_obj = patch_sandboxes_sandbox_id_metadata.sync_detailed(
                client=self._get_client(),
                sandbox_id=sandbox_id,
                body=PatchSandboxMetadataRequest.from_dict(patch),
            )
            handle_api_error(response_obj, f"Patch sandbox {sandbox_id} metadata")
            parsed = require_parsed(
                response_obj,
                ApiSandbox,
                f"Patch sandbox {sandbox_id} metadata",
            )
            return SandboxModelConverter.to_sandbox_info(parsed)
        except Exception as e:
            logger.error("Failed to patch sandbox %s metadata", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_sandbox_endpoint(
        self, sandbox_id: str, port: int, use_server_proxy: bool = False
    ) -> SandboxEndpoint:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import (
                get_sandboxes_sandbox_id_endpoints_port,
            )
            from opensandbox.api.lifecycle.models import Endpoint as ApiEndpoint

            response_obj = get_sandboxes_sandbox_id_endpoints_port.sync_detailed(
                sandbox_id=sandbox_id,
                port=port,
                client=self._get_client(),
                use_server_proxy=use_server_proxy,
            )
            handle_api_error(response_obj, f"Get endpoint for sandbox {sandbox_id} port {port}")
            parsed = require_parsed(response_obj, ApiEndpoint, "Get endpoint")
            return SandboxModelConverter.to_sandbox_endpoint(parsed)
        except Exception as e:
            logger.error("Failed to retrieve sandbox endpoint for sandbox %s", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_signed_sandbox_endpoint(
        self, sandbox_id: str, port: int, expires: int,
        use_server_proxy: bool = False,
    ) -> SandboxEndpoint:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import (
                get_sandboxes_sandbox_id_endpoints_port,
            )
            from opensandbox.api.lifecycle.models import Endpoint as ApiEndpoint

            response_obj = get_sandboxes_sandbox_id_endpoints_port.sync_detailed(
                sandbox_id=sandbox_id,
                port=port,
                client=self._get_client(),
                use_server_proxy=use_server_proxy,
                expires=str(expires),
            )
            handle_api_error(response_obj, f"Get signed endpoint for sandbox {sandbox_id} port {port}")
            parsed = require_parsed(response_obj, ApiEndpoint, "Get signed endpoint")
            return SandboxModelConverter.to_sandbox_endpoint(parsed)
        except Exception as e:
            logger.error("Failed to retrieve signed sandbox endpoint for sandbox %s", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def pause_sandbox(self, sandbox_id: str) -> None:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import (
                post_sandboxes_sandbox_id_pause,
            )

            response_obj = post_sandboxes_sandbox_id_pause.sync_detailed(
                client=self._get_client(), sandbox_id=sandbox_id
            )
            handle_api_error(response_obj, f"Pause sandbox {sandbox_id}")
        except Exception as e:
            logger.error("Failed to pause sandbox: %s", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def resume_sandbox(self, sandbox_id: str) -> None:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import (
                post_sandboxes_sandbox_id_resume,
            )

            response_obj = post_sandboxes_sandbox_id_resume.sync_detailed(
                client=self._get_client(), sandbox_id=sandbox_id
            )
            handle_api_error(response_obj, f"Resume sandbox {sandbox_id}")
        except Exception as e:
            logger.error("Failed to resume sandbox: %s", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def renew_sandbox_expiration(
        self, sandbox_id: str, new_expiration_time: datetime
    ) -> SandboxRenewResponse:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import (
                post_sandboxes_sandbox_id_renew_expiration,
            )
            from opensandbox.api.lifecycle.models.renew_sandbox_expiration_response import (
                RenewSandboxExpirationResponse,
            )

            renew_request = SandboxModelConverter.to_api_renew_request(new_expiration_time)
            response_obj = post_sandboxes_sandbox_id_renew_expiration.sync_detailed(
                client=self._get_client(),
                sandbox_id=sandbox_id,
                body=renew_request,
            )
            handle_api_error(response_obj, f"Renew sandbox {sandbox_id} expiration")
            parsed = require_parsed(
                response_obj,
                RenewSandboxExpirationResponse,
                f"Renew sandbox {sandbox_id} expiration",
            )
            return SandboxModelConverter.to_sandbox_renew_response(parsed)
        except Exception as e:
            logger.error("Failed to renew sandbox %s expiration", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def kill_sandbox(self, sandbox_id: str) -> None:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import (
                delete_sandboxes_sandbox_id,
            )

            response_obj = delete_sandboxes_sandbox_id.sync_detailed(
                client=self._get_client(), sandbox_id=sandbox_id
            )
            handle_api_error(response_obj, f"Kill sandbox {sandbox_id}")
        except Exception as e:
            logger.error("Failed to kill sandbox: %s", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def create_snapshot(
        self, sandbox_id: str, request: CreateSnapshotRequest | None = None
    ) -> SnapshotInfo:
        try:
            from opensandbox.api.lifecycle.api.snapshots import (
                post_sandboxes_sandbox_id_snapshots,
            )
            from opensandbox.api.lifecycle.models import Snapshot as ApiSnapshot

            response_obj = post_sandboxes_sandbox_id_snapshots.sync_detailed(
                client=self._get_client(),
                sandbox_id=sandbox_id,
                body=SandboxModelConverter.to_api_create_snapshot_request(request),
            )
            handle_api_error(response_obj, f"Create snapshot for sandbox {sandbox_id}")
            parsed = require_parsed(response_obj, ApiSnapshot, "Create snapshot")
            return SandboxModelConverter.to_snapshot_info(parsed)
        except Exception as e:
            logger.error("Failed to create snapshot for sandbox %s", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_snapshot(self, snapshot_id: str) -> SnapshotInfo:
        try:
            from opensandbox.api.lifecycle.api.snapshots import (
                get_snapshots_snapshot_id,
            )
            from opensandbox.api.lifecycle.models import Snapshot as ApiSnapshot

            response_obj = get_snapshots_snapshot_id.sync_detailed(
                client=self._get_client(),
                snapshot_id=snapshot_id,
            )
            handle_api_error(response_obj, f"Get snapshot {snapshot_id}")
            parsed = require_parsed(response_obj, ApiSnapshot, f"Get snapshot {snapshot_id}")
            return SandboxModelConverter.to_snapshot_info(parsed)
        except Exception as e:
            logger.error("Failed to get snapshot info: %s", snapshot_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def list_snapshots(self, filter: SnapshotFilter) -> PagedSnapshotInfos:
        try:
            from opensandbox.api.lifecycle.api.snapshots import get_snapshots
            from opensandbox.api.lifecycle.models import (
                ListSnapshotsResponse as ApiListSnapshotsResponse,
            )
            from opensandbox.api.lifecycle.types import UNSET as API_UNSET

            response_obj = get_snapshots.sync_detailed(
                client=self._get_client(),
                sandbox_id=filter.sandbox_id if filter.sandbox_id is not None else API_UNSET,
                state=filter.states if filter.states else API_UNSET,
                page=filter.page if filter.page is not None else API_UNSET,
                page_size=filter.page_size if filter.page_size is not None else API_UNSET,
            )
            handle_api_error(response_obj, "List snapshots")
            parsed = require_parsed(response_obj, ApiListSnapshotsResponse, "List snapshots")
            return SandboxModelConverter.to_paged_snapshot_infos(parsed)
        except Exception as e:
            logger.error("Failed to list snapshots", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete_snapshot(self, snapshot_id: str) -> None:
        try:
            from opensandbox.api.lifecycle.api.snapshots import (
                delete_snapshots_snapshot_id,
            )

            response_obj = delete_snapshots_snapshot_id.sync_detailed(
                client=self._get_client(),
                snapshot_id=snapshot_id,
            )
            handle_api_error(response_obj, f"Delete snapshot {snapshot_id}")
        except Exception as e:
            logger.error("Failed to delete snapshot: %s", snapshot_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e
