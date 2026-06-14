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
Pool service adapter implementation.

Implements the Pools Protocol by calling the lifecycle API over HTTP,
following the same patterns as SandboxesAdapter.
"""

import logging

import httpx

from opensandbox.adapters.converter.exception_converter import ExceptionConverter
from opensandbox.adapters.converter.response_handler import handle_api_error, require_parsed
from opensandbox.api.lifecycle import AuthenticatedClient
from opensandbox.api.lifecycle.models.pool_capacity_spec import ApiPoolCapacitySpec
from opensandbox.api.lifecycle.models.create_pool_request import ApiCreatePoolRequest
from opensandbox.api.lifecycle.models.update_pool_request import ApiUpdatePoolRequest
from opensandbox.api.lifecycle.models.pool_response import ApiPoolResponse
from opensandbox.api.lifecycle.models.list_pools_response import ApiListPoolsResponse
from opensandbox.api.lifecycle.types import Unset
from opensandbox.config import ConnectionConfig
from opensandbox.models.pools import (
    CreatePoolParams,
    PoolCapacitySpec,
    PoolInfo,
    PoolListResponse,
    PoolStatus,
    UpdatePoolParams,
)
from opensandbox.services.pool import Pools

logger = logging.getLogger(__name__)


class PoolsAdapter(Pools):
    """
    HTTP adapter that implements the Pools protocol.

    Calls the lifecycle API's /pools endpoints and converts between
    API (attrs) models and domain (Pydantic) models.
    """

    def __init__(self, connection_config: ConnectionConfig) -> None:
        self._connection_config = connection_config
        api_key = connection_config.get_api_key()
        timeout_seconds = connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)
        headers = {
            "User-Agent": connection_config.user_agent,
            **connection_config.headers,
        }
        if api_key:
            headers["OPEN-SANDBOX-API-KEY"] = api_key

        self._client = AuthenticatedClient(
            base_url=connection_config.get_base_url(),
            token=api_key or "",
            prefix="",
            auth_header_name="OPEN-SANDBOX-API-KEY",
            timeout=timeout,
        )
        self._httpx_client = httpx.AsyncClient(
            base_url=connection_config.get_base_url(),
            headers=headers,
            timeout=timeout,
            transport=connection_config.transport,
        )
        self._client.set_async_httpx_client(self._httpx_client)

    # ------------------------------------------------------------------
    # Conversion helpers
    # ------------------------------------------------------------------

    @staticmethod
    def _to_api_capacity(spec: PoolCapacitySpec) -> ApiPoolCapacitySpec:
        return ApiPoolCapacitySpec(
            buffer_max=spec.buffer_max,
            buffer_min=spec.buffer_min,
            pool_max=spec.pool_max,
            pool_min=spec.pool_min,
        )

    @staticmethod
    def _from_api_pool(raw: ApiPoolResponse) -> PoolInfo:
        cap = raw.capacity_spec
        capacity_spec = PoolCapacitySpec(
            bufferMax=cap.buffer_max,
            bufferMin=cap.buffer_min,
            poolMax=cap.pool_max,
            poolMin=cap.pool_min,
        )
        status = None
        if not isinstance(raw.status, Unset) and raw.status is not None:
            s = raw.status
            status = PoolStatus(
                total=s.total,
                allocated=s.allocated,
                available=s.available,
                revision=s.revision,
            )
        created_at = None
        if not isinstance(raw.created_at, Unset):
            created_at = raw.created_at

        return PoolInfo(
            name=raw.name,
            capacitySpec=capacity_spec,
            status=status,
            createdAt=created_at,
        )

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def create_pool(self, params: CreatePoolParams) -> PoolInfo:
        logger.info("Creating pool: name=%s", params.name)
        try:
            from opensandbox.api.lifecycle.api.pools import post_pools

            body = ApiCreatePoolRequest(
                name=params.name,
                template=params.template,
                capacity_spec=self._to_api_capacity(params.capacity_spec),
            )
            response_obj = await post_pools.asyncio_detailed(
                client=self._client, body=body
            )
            handle_api_error(response_obj, f"Create pool '{params.name}'")
            parsed = require_parsed(response_obj, ApiPoolResponse, f"Create pool '{params.name}'")
            result = self._from_api_pool(parsed)
            logger.info("Successfully created pool: %s", result.name)
            return result
        except Exception as e:
            logger.error("Failed to create pool '%s'", params.name, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def get_pool(self, pool_name: str) -> PoolInfo:
        logger.debug("Getting pool: %s", pool_name)
        try:
            from opensandbox.api.lifecycle.api.pools import get_pools_pool_name

            response_obj = await get_pools_pool_name.asyncio_detailed(
                pool_name, client=self._client
            )
            handle_api_error(response_obj, f"Get pool '{pool_name}'")
            parsed = require_parsed(response_obj, ApiPoolResponse, f"Get pool '{pool_name}'")
            return self._from_api_pool(parsed)
        except Exception as e:
            logger.error("Failed to get pool '%s'", pool_name, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def list_pools(self) -> PoolListResponse:
        logger.debug("Listing pools")
        try:
            from opensandbox.api.lifecycle.api.pools import get_pools

            response_obj = await get_pools.asyncio_detailed(client=self._client)
            handle_api_error(response_obj, "List pools")
            parsed = require_parsed(response_obj, ApiListPoolsResponse, "List pools")
            items = [self._from_api_pool(item) for item in parsed.items]
            return PoolListResponse(items=items)
        except Exception as e:
            logger.error("Failed to list pools", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def update_pool(self, pool_name: str, params: UpdatePoolParams) -> PoolInfo:
        logger.info("Updating pool capacity: %s", pool_name)
        try:
            from opensandbox.api.lifecycle.api.pools import put_pools_pool_name

            body = ApiUpdatePoolRequest(
                capacity_spec=self._to_api_capacity(params.capacity_spec)
            )
            response_obj = await put_pools_pool_name.asyncio_detailed(
                pool_name, client=self._client, body=body
            )
            handle_api_error(response_obj, f"Update pool '{pool_name}'")
            parsed = require_parsed(response_obj, ApiPoolResponse, f"Update pool '{pool_name}'")
            result = self._from_api_pool(parsed)
            logger.info("Successfully updated pool: %s", pool_name)
            return result
        except Exception as e:
            logger.error("Failed to update pool '%s'", pool_name, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def delete_pool(self, pool_name: str) -> None:
        logger.info("Deleting pool: %s", pool_name)
        try:
            from opensandbox.api.lifecycle.api.pools import delete_pools_pool_name

            response_obj = await delete_pools_pool_name.asyncio_detailed(
                pool_name, client=self._client
            )
            handle_api_error(response_obj, f"Delete pool '{pool_name}'")
            logger.info("Successfully deleted pool: %s", pool_name)
        except Exception as e:
            logger.error("Failed to delete pool '%s'", pool_name, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e
