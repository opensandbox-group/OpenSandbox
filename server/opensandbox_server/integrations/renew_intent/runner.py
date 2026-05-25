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

"""Backward-compatible aliases for the unified renew-intent consumer."""

from __future__ import annotations

from typing import Optional

from opensandbox_server.config import AppConfig
from opensandbox_server.integrations.renew_intent.consumer import (
    RenewIntentConsumer,
    start_renew_intent_consumer,
)
from opensandbox_server.services.extension_service import ExtensionService
from opensandbox_server.services.sandbox_service import SandboxService

RenewIntentRunner = RenewIntentConsumer


async def start_renew_intent_runner(
    app_config: AppConfig,
    sandbox_service: SandboxService | None = None,
    extension_service: ExtensionService | None = None,
) -> Optional[RenewIntentConsumer]:
    return await start_renew_intent_consumer(
        app_config, sandbox_service, extension_service
    )
