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
from attrs import field as _attrs_field

if TYPE_CHECKING:
    from ..models.command_pagination import CommandPagination
    from ..models.running_command_summary import RunningCommandSummary
    from ..models.terminal_command_summary import TerminalCommandSummary


T = TypeVar("T", bound="ListCommandsResponse")


@_attrs_define
class ListCommandsResponse:
    """
    Attributes:
        commands (list[RunningCommandSummary | TerminalCommandSummary]):
        pagination (CommandPagination):
    """

    commands: list[RunningCommandSummary | TerminalCommandSummary]
    pagination: CommandPagination
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        from ..models.running_command_summary import RunningCommandSummary

        commands = []
        for commands_item_data in self.commands:
            commands_item: dict[str, Any]
            if isinstance(commands_item_data, RunningCommandSummary):
                commands_item = commands_item_data.to_dict()
            else:
                commands_item = commands_item_data.to_dict()

            commands.append(commands_item)

        pagination = self.pagination.to_dict()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "commands": commands,
                "pagination": pagination,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.command_pagination import CommandPagination
        from ..models.running_command_summary import RunningCommandSummary
        from ..models.terminal_command_summary import TerminalCommandSummary

        d = dict(src_dict)
        commands = []
        _commands = d.pop("commands")
        for commands_item_data in _commands:

            def _parse_commands_item(data: object) -> RunningCommandSummary | TerminalCommandSummary:
                try:
                    if not isinstance(data, dict):
                        raise TypeError()
                    componentsschemas_command_summary_type_0 = RunningCommandSummary.from_dict(data)

                    return componentsschemas_command_summary_type_0
                except (TypeError, ValueError, AttributeError, KeyError):
                    pass
                if not isinstance(data, dict):
                    raise TypeError()
                componentsschemas_command_summary_type_1 = TerminalCommandSummary.from_dict(data)

                return componentsschemas_command_summary_type_1

            commands_item = _parse_commands_item(commands_item_data)

            commands.append(commands_item)

        pagination = CommandPagination.from_dict(d.pop("pagination"))

        list_commands_response = cls(
            commands=commands,
            pagination=pagination,
        )

        list_commands_response.additional_properties = d
        return list_commands_response

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
