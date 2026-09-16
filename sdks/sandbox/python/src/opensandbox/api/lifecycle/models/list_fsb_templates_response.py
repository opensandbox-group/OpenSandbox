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
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define

if TYPE_CHECKING:
    from ..models.fsb_template import FsbTemplate
    from ..models.pagination_info import PaginationInfo


T = TypeVar("T", bound="ListFsbTemplatesResponse")


@_attrs_define
class ListFsbTemplatesResponse:
    """Paginated collection of fsb templates.

    Attributes:
        items (list[FsbTemplate]):
        pagination (PaginationInfo): Pagination metadata for list responses
    """

    items: list[FsbTemplate]
    pagination: PaginationInfo

    def to_dict(self) -> dict[str, Any]:
        items = []
        for items_item_data in self.items:
            items_item = items_item_data.to_dict()
            items.append(items_item)

        pagination = self.pagination.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "items": items,
                "pagination": pagination,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.fsb_template import FsbTemplate
        from ..models.pagination_info import PaginationInfo

        d = dict(src_dict)
        items = []
        _items = d.pop("items")
        for items_item_data in _items:
            items_item = FsbTemplate.from_dict(items_item_data)

            items.append(items_item)

        pagination = PaginationInfo.from_dict(d.pop("pagination"))

        list_fsb_templates_response = cls(
            items=items,
            pagination=pagination,
        )

        return list_fsb_templates_response
