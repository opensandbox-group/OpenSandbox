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

from datetime import datetime
from typing import Any

from opensandbox_server.api.schema import (
    ListSandboxesRequest,
    ListSandboxesResponse,
    PaginationInfo,
    Sandbox,
)
from opensandbox_server.services.helpers import matches_filter


def _build_list_sandboxes_response(
    sandboxes: list[Sandbox],
    request: ListSandboxesRequest,
) -> ListSandboxesResponse:
    filtered = _apply_filters(sandboxes, request.filter)
    filtered.sort(key=lambda s: s.created_at or datetime.min, reverse=True)

    total_items = len(filtered)
    page = request.pagination.page
    page_size = request.pagination.page_size
    start_idx = (page - 1) * page_size
    end_idx = start_idx + page_size
    paginated_items = filtered[start_idx:end_idx]

    total_pages = (total_items + page_size - 1) // page_size
    has_next = page < total_pages

    return ListSandboxesResponse(
        items=paginated_items,
        pagination=PaginationInfo(
            page=page,
            page_size=page_size,
            total_items=total_items,
            total_pages=total_pages,
            has_next_page=has_next,
        ),
    )


def _apply_filters(sandboxes: list[Sandbox], filter_spec: Any) -> list[Sandbox]:
    if not filter_spec:
        return sandboxes
    return [sandbox for sandbox in sandboxes if matches_filter(sandbox, filter_spec)]