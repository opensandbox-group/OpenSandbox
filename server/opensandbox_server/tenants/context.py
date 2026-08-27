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

from __future__ import annotations

from contextvars import ContextVar
from typing import Optional

from opensandbox_server.tenants.models import TenantEntry

_current_tenant: ContextVar[Optional[TenantEntry]] = ContextVar("current_tenant", default=None)


def get_current_tenant() -> Optional[TenantEntry]:
    return _current_tenant.get()


def set_current_tenant(tenant: Optional[TenantEntry]) -> None:
    _current_tenant.set(tenant)
