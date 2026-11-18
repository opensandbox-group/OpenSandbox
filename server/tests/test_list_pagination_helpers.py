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

"""Shared list-response helpers used by every sandbox backend, templates and snapshots."""

from datetime import datetime, timedelta, timezone

import pytest

from opensandbox_server.api.schema import (
    ListSandboxesRequest,
    PaginationRequest,
    Sandbox,
    SandboxFilter,
    SandboxStatus,
)
from opensandbox_server.services.helpers import (
    build_list_sandboxes_response,
    build_pagination_info,
)

_EPOCH = datetime(2026, 1, 1, tzinfo=timezone.utc)


def _sandbox(index: int, state: str = "Running") -> Sandbox:
    return Sandbox(
        id=f"sbx-{index}",
        status=SandboxStatus(state=state),
        created_at=_EPOCH + timedelta(minutes=index),
    )


@pytest.mark.parametrize(
    ("page", "page_size", "total_items", "total_pages", "has_next"),
    [
        (1, 20, 0, 0, False),
        (1, 20, 20, 1, False),
        (1, 20, 21, 2, True),
        (2, 20, 21, 2, False),
        (3, 20, 21, 2, False),
    ],
)
def test_build_pagination_info(page, page_size, total_items, total_pages, has_next):
    info = build_pagination_info(page, page_size, total_items)

    assert (info.page, info.page_size, info.total_items) == (page, page_size, total_items)
    assert info.total_pages == total_pages
    assert info.has_next_page is has_next


def test_list_response_filters_then_sorts_newest_first_then_pages():
    sandboxes = [_sandbox(i, "Paused" if i % 2 else "Running") for i in range(5)]
    request = ListSandboxesRequest(
        filter=SandboxFilter(state=["running"]),
        pagination=PaginationRequest(page=1, page_size=2),
    )

    response = build_list_sandboxes_response(sandboxes, request)

    assert [s.id for s in response.items] == ["sbx-4", "sbx-2"]
    assert response.pagination.total_items == 3
    assert response.pagination.total_pages == 2
    assert response.pagination.has_next_page is True


def test_list_response_without_pagination_uses_the_request_defaults():
    sandboxes = [_sandbox(i) for i in range(25)]

    response = build_list_sandboxes_response(sandboxes, ListSandboxesRequest(pagination=None))

    defaults = PaginationRequest()
    assert response.pagination.page == defaults.page
    assert response.pagination.page_size == defaults.page_size
    assert len(response.items) == defaults.page_size
    assert response.items[0].id == "sbx-24"
